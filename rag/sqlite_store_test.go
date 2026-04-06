package rag

import (
	"context"
	"math"
	"testing"
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

	// Verify start_line ordering.
	if results[0].Chunk.StartLine != 1 {
		t.Errorf("results[0].Chunk.StartLine = %d, want 1", results[0].Chunk.StartLine)
	}
	if results[1].Chunk.StartLine != 10 {
		t.Errorf("results[1].Chunk.StartLine = %d, want 10", results[1].Chunk.StartLine)
	}

	// Verify embeddings are correct.
	// m1 was stored with embedding [0.4, 0.5, 0.6].
	if len(results[0].Embedding) != 3 {
		t.Fatalf("results[0].Embedding length = %d, want 3", len(results[0].Embedding))
	}
	if results[0].Embedding[0] != 0.4 || results[0].Embedding[1] != 0.5 || results[0].Embedding[2] != 0.6 {
		t.Errorf("results[0].Embedding = %v, want [0.4 0.5 0.6]", results[0].Embedding)
	}
	// m2 was stored with embedding [0.1, 0.2, 0.3].
	if results[1].Embedding[0] != 0.1 || results[1].Embedding[1] != 0.2 || results[1].Embedding[2] != 0.3 {
		t.Errorf("results[1].Embedding = %v, want [0.1 0.2 0.3]", results[1].Embedding)
	}

	// Verify StableKeys.
	if results[0].Chunk.StableKey != "main.go::package" {
		t.Errorf("results[0].Chunk.StableKey = %q, want %q", results[0].Chunk.StableKey, "main.go::package")
	}
	if results[1].Chunk.StableKey != "main.go::helper" {
		t.Errorf("results[1].Chunk.StableKey = %q, want %q", results[1].Chunk.StableKey, "main.go::helper")
	}

	// Verify metadata is populated.
	if results[0].Chunk.Metadata["kind"] != "package" {
		t.Errorf("results[0].Chunk.Metadata[\"kind\"] = %q, want %q", results[0].Chunk.Metadata["kind"], "package")
	}
	if results[1].Chunk.Metadata["kind"] != "function" {
		t.Errorf("results[1].Chunk.Metadata[\"kind\"] = %q, want %q", results[1].Chunk.Metadata["kind"], "function")
	}

	// Verify other fields.
	if results[0].Chunk.Content != "package main" {
		t.Errorf("results[0].Chunk.Content = %q, want %q", results[0].Chunk.Content, "package main")
	}
	if results[0].Chunk.Language != "go" {
		t.Errorf("results[0].Chunk.Language = %q, want %q", results[0].Chunk.Language, "go")
	}
	if results[0].Chunk.Source != "main.go" {
		t.Errorf("results[0].Chunk.Source = %q, want %q", results[0].Chunk.Source, "main.go")
	}
}

func TestGetBySourceEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// GetBySource for a nonexistent source returns empty slice, no error.
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
	// Compile-time check that *SQLiteStore satisfies sourceChunkLoader.
	var _ sourceChunkLoader = store
}
