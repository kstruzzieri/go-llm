package rag

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSQLiteStoreStoreAndSearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "goodbye world", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
		{ID: "c3", Content: "foo bar baz", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Search for vector closest to [1, 0, 0]
	results, err := store.Search(ctx, []float64{1.0, 0.0, 0.0}, 2)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Chunk.ID != "c1" {
		t.Errorf("top result ID = %q, want %q", results[0].Chunk.ID, "c1")
	}
	if math.Abs(results[0].Score-1.0) > 0.001 {
		t.Errorf("top result score = %f, want ~1.0", results[0].Score)
	}
}

func TestNewSQLiteStoreAcceptsRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	store, err := NewSQLiteStore("rel.db")
	if err != nil {
		t.Fatalf("NewSQLiteStore(relative) error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The DSN must resolve to a real file: URI, not treat the relative path
	// as a URI host, and the per-connection pragmas must take effect.
	var timeout int
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", timeout)
	}
	chunk := Chunk{ID: "c", Content: "x", Source: "s", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}
	if err := store.Store(context.Background(), []Chunk{chunk}, [][]float64{{1}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}
	if _, err := os.Stat("rel.db"); err != nil {
		t.Fatalf("relative database file missing: %v", err)
	}
}

func TestSQLiteStoreConcurrentWritersWaitInsteadOfFailingBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rag.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	tx, err := store.beginWriteTx(ctx)
	if err != nil {
		t.Fatalf("beginWriteTx() error: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE source = 'none'`); err != nil {
		t.Fatalf("write inside held transaction: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		done <- tx.Commit()
	}()

	// Without the pool-wide busy_timeout DSN pragma this fails in
	// microseconds with SQLITE_BUSY instead of waiting for the held lock:
	// modernc.org/sqlite applies PRAGMAs per connection, and this write runs
	// on a different pooled connection than the one holding the lock.
	if err := store.DeleteBySource(ctx, "other"); err != nil {
		t.Fatalf("concurrent DeleteBySource() error: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("commit held transaction: %v", err)
	}
}

func TestSQLiteStoreDeleteBySource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "two", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	_ = store.Store(ctx, chunks, embeddings)

	if err := store.DeleteBySource(ctx, "a.go"); err != nil {
		t.Fatalf("DeleteBySource() error: %v", err)
	}

	stats, _ := store.Stats(ctx)
	if stats.TotalChunks != 1 {
		t.Errorf("expected 1 chunk after delete, got %d", stats.TotalChunks)
	}
}

func TestSQLiteStoreListSources(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	empty, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources() on empty store error: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("ListSources() on empty store = %v, want empty", empty)
	}

	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "two", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "three", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1, 0}, {0, 1}, {1, 1}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	sources, err := store.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources() error: %v", err)
	}
	want := []string{"a.go", "b.go"}
	if !reflect.DeepEqual(sources, want) {
		t.Errorf("ListSources() = %v, want %v", sources, want)
	}
}

func TestSQLiteStoreStats(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "two", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
		{ID: "c3", Content: "three", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}}

	_ = store.Store(ctx, chunks, embeddings)

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.TotalChunks != 3 {
		t.Errorf("TotalChunks = %d, want 3", stats.TotalChunks)
	}
	if stats.TotalSources != 2 {
		t.Errorf("TotalSources = %d, want 2", stats.TotalSources)
	}
	if stats.EmbeddingDim != 4 {
		t.Errorf("EmbeddingDim = %d, want 4", stats.EmbeddingDim)
	}
}

func TestOpenSQLiteStoreReadOnlyReadsWithoutWalShm(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "idx.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	ro, err := OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	stats, err := ro.Stats(ctx)
	if err != nil {
		t.Fatalf("read-only Stats: %v", err)
	}
	if stats.TotalChunks != 1 {
		t.Fatalf("TotalChunks = %d, want 1", stats.TotalChunks)
	}
	if _, err := ro.Search(ctx, []float64{1, 0, 0}, 1); err != nil {
		t.Fatalf("read-only Search: %v", err)
	}
	if err := ro.Store(ctx, chunks, [][]float64{{1, 0, 0}}); err == nil {
		t.Fatal("read-only store should reject writes")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("read-only open should not create %s, stat err=%v", p, err)
		}
	}
}

func TestSQLiteStoreEmptySearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	results, err := store.Search(ctx, []float64{1.0, 0.0}, 5)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on empty store, got %d", len(results))
	}
}

func TestSQLiteStoreLengthMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "one", Source: "a.go", Metadata: map[string]string{}}}
	embeddings := [][]float64{{1.0}, {2.0}} // wrong length

	err := store.Store(ctx, chunks, embeddings)
	if err == nil {
		t.Fatal("expected error for length mismatch")
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{"orthogonal", []float64{1, 0, 0}, []float64{0, 1, 0}, 0.0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1.0},
		{"similar", []float64{1, 1}, []float64{1, 0}, 1.0 / math.Sqrt(2)},
		{"empty", []float64{}, []float64{}, 0.0},
		{"length mismatch", []float64{1}, []float64{1, 2}, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("cosineSimilarity() = %f, want %f", got, tt.want)
			}
		})
	}
}

func TestSQLiteStoreUpsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert initial chunk
	chunks := []Chunk{{ID: "c1", Content: "original", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	_ = store.Store(ctx, chunks, [][]float64{{1.0, 0.0}})

	// Upsert with same ID, different content
	chunks[0].Content = "updated"
	_ = store.Store(ctx, chunks, [][]float64{{0.0, 1.0}})

	stats, _ := store.Stats(ctx)
	if stats.TotalChunks != 1 {
		t.Errorf("expected 1 chunk after upsert, got %d", stats.TotalChunks)
	}

	results, _ := store.Search(ctx, []float64{0.0, 1.0}, 1)
	if results[0].Chunk.Content != "updated" {
		t.Errorf("expected content %q, got %q", "updated", results[0].Chunk.Content)
	}
}

func TestSQLiteStoreReplaceSourceRollsBackOnInsertFailure(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Seed existing data for the source.
	initial := []Chunk{
		{ID: "c1", Content: "original", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, initial, [][]float64{{1.0}}); err != nil {
		t.Fatalf("seed Store() error: %v", err)
	}

	// NaN fails JSON marshaling, forcing ReplaceSource to error after DELETE has run.
	replacement := []Chunk{
		{ID: "c2", Content: "replacement", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	err := store.ReplaceSource(ctx, "a.go", replacement, [][]float64{{math.NaN()}})
	if err == nil {
		t.Fatal("expected ReplaceSource() to fail on NaN embedding")
	}

	// Existing data should still be present because delete+insert is transactional.
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.TotalChunks != 1 {
		t.Fatalf("expected 1 chunk after failed replace, got %d", stats.TotalChunks)
	}

	results, err := store.Search(ctx, []float64{1.0}, 1)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].Chunk.Content != "original" {
		t.Fatalf("expected original chunk to remain after failed replace, got %#v", results)
	}
}

// --- ExportChunks tests ---

func seedExportStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "func main()", Source: "/src/main.go", StartLine: 1, EndLine: 5, Language: "go", Metadata: map[string]string{}},
		{ID: "c2", Content: "func helper()", Source: "/src/main.go", StartLine: 6, EndLine: 10, Language: "go", Metadata: map[string]string{}},
		{ID: "c3", Content: "def train()", Source: "/src/train.py", StartLine: 1, EndLine: 20, Language: "python", Metadata: map[string]string{}},
		{ID: "c4", Content: "import React", Source: "/src/app.tsx", StartLine: 1, EndLine: 3, Language: "typescript", Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
		{1.0, 1.0, 0.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("seed Store() error: %v", err)
	}
	return store
}

func TestExportChunksRoundTrip(t *testing.T) {
	store := seedExportStore(t)
	ctx := context.Background()

	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var results []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		results = append(results, ec)
	}

	if len(results) != 4 {
		t.Fatalf("got %d chunks, want 4", len(results))
	}

	// Verify data integrity for first result.
	found := false
	for _, ec := range results {
		if ec.Chunk.ID == "c1" {
			found = true
			if ec.Chunk.Content != "func main()" {
				t.Errorf("Content = %q, want %q", ec.Chunk.Content, "func main()")
			}
			if ec.Chunk.Source != "/src/main.go" {
				t.Errorf("Source = %q, want %q", ec.Chunk.Source, "/src/main.go")
			}
			if ec.Chunk.Language != "go" {
				t.Errorf("Language = %q, want %q", ec.Chunk.Language, "go")
			}
			if len(ec.Embedding) != 3 {
				t.Errorf("Embedding dim = %d, want 3", len(ec.Embedding))
			}
			if ec.Embedding[0] != 1.0 || ec.Embedding[1] != 0.0 || ec.Embedding[2] != 0.0 {
				t.Errorf("Embedding = %v, want [1 0 0]", ec.Embedding)
			}
			// Metadata should be nil (not exported).
			if ec.Chunk.Metadata != nil {
				t.Errorf("Metadata should be nil, got %v", ec.Chunk.Metadata)
			}
			break
		}
	}
	if !found {
		t.Error("chunk c1 not found in results")
	}
}

func TestExportChunksLanguageFilter(t *testing.T) {
	store := seedExportStore(t)
	ctx := context.Background()

	filter := &ExportFilter{Language: "go"}
	seq, err := store.ExportChunks(ctx, filter)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var results []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		results = append(results, ec)
	}

	if len(results) != 2 {
		t.Fatalf("got %d chunks, want 2 (go only)", len(results))
	}
	for _, ec := range results {
		if ec.Chunk.Language != "go" {
			t.Errorf("got language %q, want %q", ec.Chunk.Language, "go")
		}
	}
}

func TestExportChunksSourceGlob(t *testing.T) {
	store := seedExportStore(t)
	ctx := context.Background()

	filter := &ExportFilter{SourcePattern: "*.py"}
	seq, err := store.ExportChunks(ctx, filter)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var results []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		results = append(results, ec)
	}

	if len(results) != 1 {
		t.Fatalf("got %d chunks, want 1 (python only)", len(results))
	}
	if results[0].Chunk.ID != "c3" {
		t.Errorf("got chunk %q, want c3", results[0].Chunk.ID)
	}
}

func TestExportChunksOrdering(t *testing.T) {
	store := seedExportStore(t)
	ctx := context.Background()

	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var results []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		results = append(results, ec)
	}

	// Should be ordered by source, then start_line.
	for i := 1; i < len(results); i++ {
		prev, curr := results[i-1], results[i]
		if prev.Chunk.Source > curr.Chunk.Source {
			t.Errorf("out of order: %q > %q", prev.Chunk.Source, curr.Chunk.Source)
		}
		if prev.Chunk.Source == curr.Chunk.Source && prev.Chunk.StartLine > curr.Chunk.StartLine {
			t.Errorf("out of order within source %q: line %d > %d",
				prev.Chunk.Source, prev.Chunk.StartLine, curr.Chunk.StartLine)
		}
	}
}

func TestExportChunksEmptyStore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	count := 0
	for _, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		count++
	}

	if count != 0 {
		t.Errorf("got %d chunks from empty store, want 0", count)
	}
}

func TestExportChunksContextCancellation(t *testing.T) {
	store := seedExportStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seq, err := store.ExportChunks(ctx, nil)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	count := 0
	for _, iterErr := range seq {
		if iterErr != nil {
			// Context cancellation may surface as an error during iteration.
			break
		}
		count++
		if count == 1 {
			cancel()
			// Continue — the iterator should stop on next check or yield an error.
		}
	}

	// We cancelled after 1 row. The store has 4 rows total.
	// With context cancellation, we should get fewer than all 4.
	// (The iterator may yield 1-2 more rows before checking context.)
	if count >= 4 {
		t.Errorf("got all %d chunks despite cancel after 1, cancellation not effective", count)
	}
}

func TestSchemaMigrationIndexes(t *testing.T) {
	store := newTestStore(t)

	// Verify the new indexes exist.
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='chunks' ORDER BY name`)
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()

	indexes := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		indexes[name] = true
	}

	if !indexes["idx_chunks_source_line"] {
		t.Error("missing idx_chunks_source_line index")
	}
	if !indexes["idx_chunks_lang_source_line"] {
		t.Error("missing idx_chunks_lang_source_line index")
	}
	// Old index should be gone.
	if indexes["idx_chunks_source"] {
		t.Error("old idx_chunks_source index should have been dropped")
	}
}

func TestExportChunksCombinedFilter(t *testing.T) {
	store := seedExportStore(t)
	ctx := context.Background()

	filter := &ExportFilter{SourcePattern: "/src/main*", Language: "go"}
	seq, err := store.ExportChunks(ctx, filter)
	if err != nil {
		t.Fatalf("ExportChunks() error: %v", err)
	}

	var results []ExportedChunk
	for ec, iterErr := range seq {
		if iterErr != nil {
			t.Fatalf("iteration error: %v", iterErr)
		}
		results = append(results, ec)
	}

	if len(results) != 2 {
		t.Fatalf("got %d chunks, want 2 (go + main.go)", len(results))
	}
}

// Verify the new store returns *SQLiteStore that satisfies Exportable.
func TestSQLiteStoreImplementsExportable(t *testing.T) {
	store := newTestStore(t)
	var _ Exportable = store // compile-time check
}

// --- GetBySource tests ---

func TestGetBySourceBasic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store 3 chunks: 2 for "main.go" (out of order by start_line), 1 for "other.go".
	chunks := []Chunk{
		{
			ID: "m2", Content: "func helper() {}", Source: "main.go",
			StartLine: 10, EndLine: 15, Language: "go",
			Metadata: map[string]string{"kind": "function"}, StableKey: "main.go::helper",
		},
		{
			ID: "m1", Content: "package main", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go",
			Metadata: map[string]string{"kind": "package"}, StableKey: "main.go::package",
		},
		{
			ID: "o1", Content: "package other", Source: "other.go",
			StartLine: 1, EndLine: 1, Language: "go",
			Metadata: map[string]string{}, StableKey: "other.go::package",
		},
	}
	embeddings := [][]float64{
		{0.1, 0.2, 0.3},
		{0.4, 0.5, 0.6},
		{0.7, 0.8, 0.9},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// GetBySource for "main.go" should return 2 results ordered by start_line.
	results, err := store.GetBySource(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetBySource() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}

	// First result should be m1 (start_line=1), second m2 (start_line=10).
	if results[0].Chunk.ID != "m1" {
		t.Errorf("results[0].Chunk.ID = %q, want %q", results[0].Chunk.ID, "m1")
	}
	if results[1].Chunk.ID != "m2" {
		t.Errorf("results[1].Chunk.ID = %q, want %q", results[1].Chunk.ID, "m2")
	}
}

func TestGetBySourceEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	results, err := store.GetBySource(ctx, "nonexistent.go")
	if err != nil {
		t.Fatalf("GetBySource() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results for nonexistent source, want 0", len(results))
	}
}

func TestGetBySourceImplementsInterface(t *testing.T) {
	store := newTestStore(t)
	var _ sourceChunkLoader = store
}

// --- GetSourceHash tests ---

func TestGetSourceHashBasic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Manually insert a chunk with source_content_hash set.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash)
		 VALUES ('h1', 'func main()', 'main.go', 1, 5, 'go', '{}', '[0.1,0.2]', 12345, 'main.go::main', 'deadbeef1234')`)
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	hash, err := store.GetSourceHash(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash != "deadbeef1234" {
		t.Errorf("GetSourceHash() = %q, want %q", hash, "deadbeef1234")
	}
}

func TestGetSourceHashEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	hash, err := store.GetSourceHash(ctx, "nonexistent.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash != "" {
		t.Errorf("GetSourceHash() = %q, want empty string", hash)
	}
}

func TestGetSourceHashNoHashStored(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert a chunk with default empty source_content_hash (simulating pre-V4 data).
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key)
		 VALUES ('h2', 'package foo', 'foo.go', 1, 1, 'go', '{}', '[0.5]', 12345, 'foo.go::package')`)
	if err != nil {
		t.Fatalf("insert chunk: %v", err)
	}

	hash, err := store.GetSourceHash(ctx, "foo.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash != "" {
		t.Errorf("GetSourceHash() = %q, want empty string for default", hash)
	}
}

func TestGetSourceHashImplementsInterface(t *testing.T) {
	store := newTestStore(t)
	var _ sourceHashChecker = store
}

// --- ReplaceSourceWithHashAndVectorSpaceID tests (#82 Phase 2) ---

// TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceID_writesColumnAndMetadata
// is the foundational store-layer test for #82 Phase 2 (spec §10.2, TDD test 1).
//
// Asserts the store owns the metadata-mirror invariant: when given a non-empty
// vsid, it writes the value to both the chunks.vector_space_id column (primary)
// AND injects "vector_space_id": "X" into each chunk's metadata JSON (mirror).
// Both assertions go through direct SQL — the verification gate's integration
// test reads vector_space_id via SELECT, so the unit test mirrors that observable.
func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceID_writesColumnAndMetadata(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID: "c1", Content: "func Hello() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
		{
			ID: "c2", Content: "func World() {}", Source: "main.go",
			StartLine: 5, EndLine: 7, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}
	const (
		sourceHash    = "abc123def456abc123def456"
		vectorSpaceID = "ollama/qwen3-embedding:8b"
	)

	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", chunks, embeddings, sourceHash, vectorSpaceID); err != nil {
		t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID() error: %v", err)
	}

	rows, err := store.db.QueryContext(ctx,
		`SELECT id, vector_space_id, metadata FROM chunks WHERE source = 'main.go' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var seen int
	for rows.Next() {
		var id, vsidCol, metaJSON string
		if err := rows.Scan(&id, &vsidCol, &metaJSON); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if vsidCol != vectorSpaceID {
			t.Errorf("chunk %q vector_space_id column = %q, want %q", id, vsidCol, vectorSpaceID)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
			t.Fatalf("chunk %q metadata unmarshal: %v (raw=%q)", id, err, metaJSON)
		}
		if got := meta["vector_space_id"]; got != vectorSpaceID {
			t.Errorf("chunk %q metadata[\"vector_space_id\"] = %q, want %q", id, got, vectorSpaceID)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if seen != 2 {
		t.Fatalf("expected 2 chunks, got %d", seen)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceID_requiresVSIDBeforeDelete(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	initial := []Chunk{{
		ID: "old", Content: "original", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", initial, [][]float64{{1}}, "h1", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "")
	if !errors.Is(err, ErrMissingVectorSpaceID) {
		t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID error = %v, want ErrMissingVectorSpaceID", err)
	}

	var id, content string
	if err := store.db.QueryRowContext(ctx,
		`SELECT id, content FROM chunks WHERE source = 'main.go'`).Scan(&id, &content); err != nil {
		t.Fatalf("query original row: %v", err)
	}
	if id != "old" || content != "original" {
		t.Fatalf("row after rejected replace = %q/%q, want old/original", id, content)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceID_rejectsExistingVectorSpaceDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	initial := []Chunk{{
		ID: "old", Content: "original", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", initial, [][]float64{{1}}, "h1", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "Y")
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID error = %v, want ErrVectorSpaceDrift", err)
	}
}

func TestSQLiteStore_ForceReplaceSourceWithHashAndVectorSpaceID_allowsExistingVectorSpaceDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	initial := []Chunk{{
		ID: "old", Content: "original", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", initial, [][]float64{{1}}, "h1", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ForceReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "Y"); err != nil {
		t.Fatalf("ForceReplaceSourceWithHashAndVectorSpaceID error: %v", err)
	}

	var id, vsid string
	if err := store.db.QueryRowContext(ctx,
		`SELECT id, vector_space_id FROM chunks WHERE source = 'main.go'`).Scan(&id, &vsid); err != nil {
		t.Fatalf("query replacement row: %v", err)
	}
	if id != "new" || vsid != "Y" {
		t.Fatalf("forced replacement = id %q vsid %q, want new/Y", id, vsid)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash_rejectsStaleSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	initial := []Chunk{{
		ID: "old", Content: "original", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", initial, [][]float64{{1}}, "h1", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	err := store.ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "X", "stale")
	if !errors.Is(err, ErrIncrementalStaleSource) {
		t.Fatalf("CAS replace error = %v, want ErrIncrementalStaleSource", err)
	}

	var id string
	if err := store.db.QueryRowContext(ctx,
		`SELECT id FROM chunks WHERE source = 'main.go'`).Scan(&id); err != nil {
		t.Fatalf("query original row: %v", err)
	}
	if id != "old" {
		t.Fatalf("row after stale CAS = %q, want old", id)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash_rejectsVectorSpaceDrift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	initial := []Chunk{{
		ID: "old", Content: "original", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", initial, [][]float64{{1}}, "h1", "X"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	err := store.ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "Y", "h1")
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("CAS replace error = %v, want ErrVectorSpaceDrift", err)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash_rejectsMixedExistingVSID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	embedding, err := encodeEmbedding([]float64{1})
	if err != nil {
		t.Fatal(err)
	}

	// Intentional schema-coupled seed: public replace APIs enforce one VSID per
	// source, so this test owns the raw INSERT needed to simulate corruption.
	for _, row := range []struct {
		id   string
		vsid string
	}{
		{id: "a", vsid: "X"},
		{id: "b", vsid: "Y"},
	} {
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			 VALUES (?, 'x', 'main.go', 1, 1, 'go', '{}', ?, 1, ?, 'h1', ?)`,
			row.id, embedding, row.id, row.vsid); err != nil {
			t.Fatalf("insert mixed row %q: %v", row.id, err)
		}
	}

	replacement := []Chunk{{
		ID: "new", Content: "replacement", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}
	err = store.ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx, "main.go", replacement, [][]float64{{2}}, "h2", "X", "h1")
	if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
		t.Fatalf("CAS replace error = %v, want ErrCorpusMixedVectorSpaces", err)
	}
}

func TestSQLiteStore_ReplaceSourceWithHashAndVectorSpaceID_metadataMirrorStoreValueWinsConflict(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{
		ID: "c1", Content: "content", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go",
		Metadata: map[string]string{"vector_space_id": "caller-value", "kind": "test"},
	}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "main.go", chunks, [][]float64{{1}}, "h1", "store-value"); err != nil {
		t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID error: %v", err)
	}

	var metaJSON string
	if err := store.db.QueryRowContext(ctx,
		`SELECT metadata FROM chunks WHERE id = 'c1'`).Scan(&metaJSON); err != nil {
		t.Fatalf("query metadata: %v", err)
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if got := meta["vector_space_id"]; got != "store-value" {
		t.Fatalf("metadata vector_space_id = %q, want store-value", got)
	}
	if got := meta["kind"]; got != "test" {
		t.Fatalf("metadata kind = %q, want test", got)
	}
}

// --- ReplaceSourceWithHash tests (Task 1.5) ---

func TestReplaceSourceWithHashPopulatesHash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID: "c1", Content: "func Hello() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
		{
			ID: "c2", Content: "func World() {}", Source: "main.go",
			StartLine: 5, EndLine: 7, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}
	sourceHash := "abc123def456abc123def456"

	if err := store.ReplaceSourceWithHash(ctx, "main.go", chunks, embeddings, sourceHash); err != nil {
		t.Fatalf("ReplaceSourceWithHash() error: %v", err)
	}

	// All chunks from the same source should have the same source_content_hash.
	rows, err := store.db.QueryContext(ctx,
		`SELECT id, source_content_hash FROM chunks WHERE source = 'main.go' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var count int
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if hash != sourceHash {
			t.Errorf("chunk %q source_content_hash = %q, want %q", id, hash, sourceHash)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2 chunks, got %d", count)
	}
}

func TestReplaceSourceWithoutHashLeavesEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID: "c1", Content: "func Hello() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings := [][]float64{{1.0, 0.0}}

	// Standard ReplaceSource (no hash).
	if err := store.ReplaceSource(ctx, "main.go", chunks, embeddings); err != nil {
		t.Fatalf("ReplaceSource() error: %v", err)
	}

	var hash string
	err := store.db.QueryRowContext(ctx,
		`SELECT source_content_hash FROM chunks WHERE id = 'c1'`).Scan(&hash)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if hash != "" {
		t.Errorf("source_content_hash = %q, want empty (no hash provided)", hash)
	}
}

func TestInsertChunksPopulatesSourceContentHash(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{
			ID: "c1", Content: "func Hello() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
		{
			ID: "c2", Content: "func World() {}", Source: "main.go",
			StartLine: 5, EndLine: 7, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	// Compute hash the same way the indexer would.
	combined := chunks[0].Content + chunks[1].Content
	expectedHash := contentHash(combined)

	// Use ReplaceSourceWithHash which flows through insertChunksTx.
	if err := store.ReplaceSourceWithHash(ctx, "main.go", chunks, embeddings, expectedHash); err != nil {
		t.Fatalf("ReplaceSourceWithHash() error: %v", err)
	}

	// All chunks from the same source should have the same source_content_hash.
	rows, err := store.db.QueryContext(ctx,
		`SELECT id, source_content_hash FROM chunks WHERE source = 'main.go' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var hashes []string
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if hash == "" {
			t.Errorf("chunk %q has empty source_content_hash", id)
		}
		hashes = append(hashes, hash)
	}

	if len(hashes) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(hashes))
	}
	if hashes[0] != hashes[1] {
		t.Errorf("chunks from same source have different hashes: %q vs %q", hashes[0], hashes[1])
	}
	if hashes[0] != expectedHash {
		t.Errorf("source_content_hash = %q, want %q", hashes[0], expectedHash)
	}
}

func TestSourceContentHashChangesOnUpdate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// First insert.
	chunks1 := []Chunk{
		{
			ID: "c1", Content: "func Hello() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings1 := [][]float64{{1.0, 0.0}}
	hash1 := contentHash("func Hello() {}")

	if err := store.ReplaceSourceWithHash(ctx, "main.go", chunks1, embeddings1, hash1); err != nil {
		t.Fatalf("first ReplaceSourceWithHash() error: %v", err)
	}

	// Verify first hash.
	gotHash1, err := store.GetSourceHash(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if gotHash1 != hash1 {
		t.Fatalf("first hash = %q, want %q", gotHash1, hash1)
	}

	// Second insert with different content.
	chunks2 := []Chunk{
		{
			ID: "c1", Content: "func Goodbye() {}", Source: "main.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddings2 := [][]float64{{0.0, 1.0}}
	hash2 := contentHash("func Goodbye() {}")

	if err := store.ReplaceSourceWithHash(ctx, "main.go", chunks2, embeddings2, hash2); err != nil {
		t.Fatalf("second ReplaceSourceWithHash() error: %v", err)
	}

	// Verify hash changed.
	gotHash2, err := store.GetSourceHash(ctx, "main.go")
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if gotHash2 == gotHash1 {
		t.Errorf("hash did not change after update: %q", gotHash2)
	}
	if gotHash2 != hash2 {
		t.Errorf("second hash = %q, want %q", gotHash2, hash2)
	}
}

func TestDifferentSourcesDifferentHashes(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert chunks for source A.
	chunksA := []Chunk{
		{
			ID: "a1", Content: "func Alpha() {}", Source: "alpha.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddingsA := [][]float64{{1.0, 0.0}}
	hashA := contentHash("func Alpha() {}")

	if err := store.ReplaceSourceWithHash(ctx, "alpha.go", chunksA, embeddingsA, hashA); err != nil {
		t.Fatalf("ReplaceSourceWithHash(alpha) error: %v", err)
	}

	// Insert chunks for source B.
	chunksB := []Chunk{
		{
			ID: "b1", Content: "func Beta() {}", Source: "beta.go",
			StartLine: 1, EndLine: 3, Language: "go", Metadata: map[string]string{},
		},
	}
	embeddingsB := [][]float64{{0.0, 1.0}}
	hashB := contentHash("func Beta() {}")

	if err := store.ReplaceSourceWithHash(ctx, "beta.go", chunksB, embeddingsB, hashB); err != nil {
		t.Fatalf("ReplaceSourceWithHash(beta) error: %v", err)
	}

	// Verify different hashes.
	gotHashA, err := store.GetSourceHash(ctx, "alpha.go")
	if err != nil {
		t.Fatalf("GetSourceHash(alpha) error: %v", err)
	}
	gotHashB, err := store.GetSourceHash(ctx, "beta.go")
	if err != nil {
		t.Fatalf("GetSourceHash(beta) error: %v", err)
	}

	if gotHashA == "" {
		t.Error("alpha hash is empty")
	}
	if gotHashB == "" {
		t.Error("beta hash is empty")
	}
	if gotHashA == gotHashB {
		t.Errorf("different sources have same hash: %q", gotHashA)
	}
	if gotHashA != hashA {
		t.Errorf("alpha hash = %q, want %q", gotHashA, hashA)
	}
	if gotHashB != hashB {
		t.Errorf("beta hash = %q, want %q", gotHashB, hashB)
	}
}

// insertVSIDRow writes a chunk row with the given vector_space_id directly,
// bypassing the public Store API which does not yet accept vsid.
func insertVSIDRow(t *testing.T, store *SQLiteStore, id, vsid string) {
	t.Helper()
	if _, err := store.db.Exec(
		`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
		 VALUES (?, 'x', 's.go', 1, 1, 'go', '{}', '[0.1]', 1, '', '', ?)`,
		id, vsid,
	); err != nil {
		t.Fatalf("insert vsid row %q/%q: %v", id, vsid, err)
	}
}

func TestProbeVectorSpaces_empty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: nil, HasUnknown: false}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

func TestProbeVectorSpaces_singleKnown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		insertVSIDRow(t, store, id, "X")
	}

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: []string{"X"}, HasUnknown: false}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

func TestProbeVectorSpaces_singleKnown_plusUnknown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	insertVSIDRow(t, store, "a", "X")
	insertVSIDRow(t, store, "b", "X")
	insertVSIDRow(t, store, "c", "")
	insertVSIDRow(t, store, "d", "")

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: []string{"X"}, HasUnknown: true}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

func TestProbeVectorSpaces_twoKnown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert in non-alphabetical order to verify bookend probe sorts.
	insertVSIDRow(t, store, "a", "Y")
	insertVSIDRow(t, store, "b", "A")

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: []string{"A", "Y"}, HasUnknown: false}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

func TestProbeVectorSpaces_twoKnown_plusUnknown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	insertVSIDRow(t, store, "a", "Y")
	insertVSIDRow(t, store, "b", "A")
	insertVSIDRow(t, store, "c", "")

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: []string{"A", "Y"}, HasUnknown: true}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

func TestProbeVectorSpaces_fullyLegacy(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		insertVSIDRow(t, store, id, "")
	}

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: nil, HasUnknown: true}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}

// TestProbeVectorSpaces_threeKnown_returnsBookends documents that the bookend
// strategy returns only the lex min/max even if a middle vsid exists. This
// is intentional — Retriever's decision matrix only cares whether ≥2 distinct
// values exist; deeper enumeration would not change the outcome.
func TestProbeVectorSpaces_threeKnown_returnsBookends(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	insertVSIDRow(t, store, "a", "A")
	insertVSIDRow(t, store, "b", "M")
	insertVSIDRow(t, store, "c", "Z")

	probe, err := store.ProbeVectorSpaces(ctx)
	if err != nil {
		t.Fatalf("ProbeVectorSpaces: %v", err)
	}
	want := VectorSpaceProbe{KnownIDs: []string{"A", "Z"}, HasUnknown: false}
	if !reflect.DeepEqual(probe, want) {
		t.Errorf("probe = %+v, want %+v", probe, want)
	}
}
