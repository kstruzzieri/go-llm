package rag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// --- canDoIncremental tests ---

func TestCanDoIncremental_RequiresStoreCapability(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store := &minimalVectorStore{}
	idx := NewIndexer(client, store, WithWorkspaceRoot("/tmp/test"))

	chunks := []Chunk{
		{StableKey: "file.go::Foo#0"},
		{StableKey: "file.go::Bar#0"},
	}

	if idx.canDoIncremental(chunks) {
		t.Error("canDoIncremental should return false for store without sourceChunkLoader")
	}
}

func TestCanDoIncremental_RequiresWorkspaceRoot(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	// No WithWorkspaceRoot.
	idx := NewIndexer(client, store)

	chunks := []Chunk{
		{StableKey: "file.go::Foo#0"},
	}

	if idx.canDoIncremental(chunks) {
		t.Error("canDoIncremental should return false without workspace root")
	}
}

func TestCanDoIncremental_RequiresStableKeys(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store, WithWorkspaceRoot("/tmp/test"))

	// More than 50% empty StableKeys.
	chunks := []Chunk{
		{StableKey: "file.go::Foo#0"},
		{StableKey: ""},
		{StableKey: ""},
	}

	if idx.canDoIncremental(chunks) {
		t.Error("canDoIncremental should return false when <50%% have StableKeys")
	}
}

func TestCanDoIncremental_EmptyChunks(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store, WithWorkspaceRoot("/tmp/test"))

	if idx.canDoIncremental(nil) {
		t.Error("canDoIncremental should return false for nil chunks")
	}
	if idx.canDoIncremental([]Chunk{}) {
		t.Error("canDoIncremental should return false for empty chunks")
	}
}

func TestCanDoIncremental_Success(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store, WithWorkspaceRoot("/tmp/test"))

	chunks := []Chunk{
		{StableKey: "file.go::Foo#0"},
		{StableKey: "file.go::Bar#0"},
		{StableKey: ""},
	}

	if !idx.canDoIncremental(chunks) {
		t.Error("canDoIncremental should return true when >50%% have StableKeys")
	}
}

// --- minimalVectorStore: satisfies VectorStore but nothing else ---

type minimalVectorStore struct{}

func (m *minimalVectorStore) Store(_ context.Context, _ []Chunk, _ [][]float64) error { return nil }
func (m *minimalVectorStore) Search(_ context.Context, _ []float64, _ int) ([]SearchResult, error) {
	return nil, nil
}
func (m *minimalVectorStore) DeleteBySource(_ context.Context, _ string) error { return nil }
func (m *minimalVectorStore) Stats(_ context.Context) (StoreStats, error)      { return StoreStats{}, nil }
func (m *minimalVectorStore) Close() error                                     { return nil }

// --- IndexFileIncremental tests ---

// newBatchCountingEmbedServer returns a mock server that counts each individual
// text embedded (one count per /api/embed call).
func newBatchCountingEmbedServer(dim int, counter *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		counter.Add(1)
		emb := make([]float64, dim)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestIndexFileIncremental_BasicUpdate(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	// Initial content: two functions.
	initial := "package main\n\nfunc Hello() {\n\treturn 1\n}\n\nfunc World() {\n\treturn 2\n}\n"
	if err := os.WriteFile(goFile, []byte(initial), 0644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	// Full index first.
	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}
	initialEmbeds := embedCount.Load()
	if initialEmbeds == 0 {
		t.Fatal("expected at least 1 embed call during initial index")
	}

	// Reset counter.
	embedCount.Store(0)

	// Edit: change only the body of Hello.
	edited := "package main\n\nfunc Hello() {\n\treturn 42\n}\n\nfunc World() {\n\treturn 2\n}\n"
	if err := os.WriteFile(goFile, []byte(edited), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}

	// Incremental index.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	incrementalEmbeds := embedCount.Load()
	if incrementalEmbeds >= initialEmbeds {
		t.Errorf("incremental embeds (%d) should be less than initial (%d)", incrementalEmbeds, initialEmbeds)
	}

	// Verify store has correct chunks.
	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("store has no chunks after incremental index")
	}
}

func TestIndexFileIncremental_NoChange(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	content := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	// Full index first.
	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	// Reset counter.
	embedCount.Store(0)

	// Re-index same content incrementally.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if embedCount.Load() != 0 {
		t.Errorf("expected 0 embed calls for unchanged file, got %d", embedCount.Load())
	}
}

func TestIndexFileIncremental_FallbackNoOldChunks(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn 1\n}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	// First-time index via IndexFileIncremental -- no old chunks exist.
	// Should fall back to full indexing.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if embedCount.Load() == 0 {
		t.Error("expected embed calls for first-time index (fallback to full)")
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected chunks after first-time incremental (fallback)")
	}
}

func TestIndexFileIncremental_FallbackNoStableKeys(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	txtFile := filepath.Join(tmpDir, "data.txt")

	// Plain text content that produces no structural symbols -> no StableKeys.
	content := "line one\nline two\nline three\nline four\nline five\n"
	if err := os.WriteFile(txtFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Use a sliding window chunker to produce chunks without symbol metadata.
	chunker, _ := NewSlidingWindowChunker(30, 0)
	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)

	// First full index.
	if err := idx.IndexFile(context.Background(), txtFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}
	initialEmbeds := embedCount.Load()
	embedCount.Store(0)

	// Try incremental -- should fall back because StableKeys are empty.
	// (SlidingWindowChunker produces anchor_hash metadata, but ComputeStableKey
	// may fail if no symbol_path, section_path, or anchor_hash is found.)
	if err := idx.IndexFileIncremental(context.Background(), txtFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	// Either falls back to full (embeds everything again) or succeeds
	// incrementally if anchor_hash provides StableKeys. Either way,
	// the store should have chunks.
	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected chunks after incremental index (with fallback)")
	}
	_ = initialEmbeds // used for context only
}

func TestIndexFileIncremental_FallbackNoWorkspaceRoot(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// No workspace root -- should fall back to full re-index.
	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	// Should still work (falls back to full), just not incrementally.
	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected chunks after fallback index")
	}
}

func TestIndexFileIncremental_FallbackCustomStore(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store := &minimalVectorStore{}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	// Should fall back to full re-index since minimalVectorStore
	// does not implement sourceChunkLoader.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if embedCount.Load() == 0 {
		t.Error("expected embed calls for fallback full index")
	}
}

func TestIndexFileIncremental_NewFunction(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	initial := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(initial), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}
	initialEmbeds := embedCount.Load()
	embedCount.Store(0)

	// Add a new function.
	edited := "package main\n\nfunc Hello() {\n\treturn 1\n}\n\nfunc NewFunc() {\n\treturn 42\n}\n"
	if err := os.WriteFile(goFile, []byte(edited), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}

	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	incrementalEmbeds := embedCount.Load()
	// Should embed only the new function, not the unchanged Hello.
	if incrementalEmbeds == 0 {
		t.Error("expected at least 1 embed call for new function")
	}
	if incrementalEmbeds >= initialEmbeds+1 {
		t.Errorf("incremental embeds (%d) should be fewer than initial+1 (%d)", incrementalEmbeds, initialEmbeds+1)
	}
}

func TestIndexFileIncremental_DeleteFunction(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	initial := "package main\n\nfunc Hello() {\n\treturn 1\n}\n\nfunc ToBeRemoved() {\n\treturn 2\n}\n"
	if err := os.WriteFile(goFile, []byte(initial), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	embedCount.Store(0)

	// Remove the second function.
	edited := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(edited), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}

	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	statsAfter, _ := store.Stats(context.Background())
	if statsAfter.TotalChunks >= statsBefore.TotalChunks {
		t.Errorf("expected fewer chunks after deletion: before=%d, after=%d",
			statsBefore.TotalChunks, statsAfter.TotalChunks)
	}

	// No new content was added, so embed count should be 0.
	if embedCount.Load() != 0 {
		t.Errorf("expected 0 embed calls (only deletion), got %d", embedCount.Load())
	}
}

func TestIndexFileIncremental_EmbedFailurePreservesData(t *testing.T) {
	// Initial index with good server.
	goodSrv := newMockEmbedServer(4)
	defer goodSrv.Close()

	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	goodClient := ollama.NewClient(ollama.WithBaseURL(goodSrv.URL))

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn 1\n}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(goodClient, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())

	// Now try incremental with a broken embed server.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer badSrv.Close()

	badClient := ollama.NewClient(ollama.WithBaseURL(badSrv.URL))
	badIdx := NewIndexer(badClient, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	// Edit the file so incremental path has something to embed.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn 42\n}\n"), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}

	err := badIdx.IndexFileIncremental(context.Background(), goFile)
	if err == nil {
		t.Fatal("expected error from broken embed server")
	}

	// Old data should still be intact.
	statsAfter, _ := store.Stats(context.Background())
	if statsAfter.TotalChunks != statsBefore.TotalChunks {
		t.Errorf("embed failure changed chunk count from %d to %d",
			statsBefore.TotalChunks, statsAfter.TotalChunks)
	}
}

func TestIndexFileIncremental_ConsistentWithFull(t *testing.T) {
	dim := 4
	srv := newMockEmbedServer(dim)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	initial := "package main\n\nfunc Hello() {\n\treturn 1\n}\n\nfunc World() {\n\treturn 2\n}\n"
	if err := os.WriteFile(goFile, []byte(initial), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Store 1: full index, then edit, then incremental.
	store1, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store1.Close() }()
	idx1 := NewIndexer(client, store1, WithEmbeddingModel("test-embed"), WithWorkspaceRoot(tmpDir))
	if err := idx1.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("store1 initial IndexFile() error: %v", err)
	}

	edited := "package main\n\nfunc Hello() {\n\treturn 42\n}\n\nfunc World() {\n\treturn 2\n}\n"
	if err := os.WriteFile(goFile, []byte(edited), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}
	if err := idx1.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("store1 IndexFileIncremental() error: %v", err)
	}

	// Store 2: full index of the edited file from scratch.
	store2, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store2.Close() }()
	idx2 := NewIndexer(client, store2, WithEmbeddingModel("test-embed"), WithWorkspaceRoot(tmpDir))
	if err := idx2.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("store2 IndexFile() error: %v", err)
	}

	// Compare: both stores should have the same chunks.
	results1, _ := store1.GetBySource(context.Background(), goFile)
	results2, _ := store2.GetBySource(context.Background(), goFile)

	if len(results1) != len(results2) {
		t.Fatalf("chunk count mismatch: incremental=%d, full=%d", len(results1), len(results2))
	}

	for i := range results1 {
		if results1[i].Chunk.Content != results2[i].Chunk.Content {
			t.Errorf("chunk %d content mismatch:\n  incremental: %q\n  full: %q",
				i, results1[i].Chunk.Content, results2[i].Chunk.Content)
		}
		if results1[i].Chunk.StableKey != results2[i].Chunk.StableKey {
			t.Errorf("chunk %d StableKey mismatch:\n  incremental: %q\n  full: %q",
				i, results1[i].Chunk.StableKey, results2[i].Chunk.StableKey)
		}
		if results1[i].Chunk.StartLine != results2[i].Chunk.StartLine {
			t.Errorf("chunk %d StartLine mismatch: incremental=%d, full=%d",
				i, results1[i].Chunk.StartLine, results2[i].Chunk.StartLine)
		}
	}
}

func TestIndexFileIncremental_EmptyFile(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	// First, index a file with content.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
	)

	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalChunks == 0 {
		t.Fatal("expected chunks after initial indexing")
	}

	// Truncate file to empty.
	if err := os.WriteFile(goFile, []byte(""), 0644); err != nil {
		t.Fatalf("truncate file: %v", err)
	}

	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	statsAfter, _ := store.Stats(context.Background())
	if statsAfter.TotalChunks != 0 {
		t.Errorf("expected 0 chunks after indexing empty file, got %d", statsAfter.TotalChunks)
	}
}

// --- Source hash fast path tests ---

// countingChunker wraps a Chunker and counts how many times Chunk is called.
type countingChunker struct {
	inner   Chunker
	counter *atomic.Int32
}

func (c *countingChunker) Chunk(source string, content string) ([]Chunk, error) {
	c.counter.Add(1)
	return c.inner.Chunk(source, content)
}

// countingGetBySourceStore wraps a *SQLiteStore and counts GetBySource calls.
type countingGetBySourceStore struct {
	*SQLiteStore
	getBySourceCount *atomic.Int32
}

func (s *countingGetBySourceStore) GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error) {
	s.getBySourceCount.Add(1)
	return s.SQLiteStore.GetBySource(ctx, source)
}

func TestIndexFileIncremental_HashSkip(t *testing.T) {
	// This test proves the source-hash fast path exists and works.
	// On the second call with unchanged content:
	//   - 0 embed calls (no embedding work)
	//   - 0 Chunk() calls (no chunking work)
	//   - 0 GetBySource calls (no store lookup for old chunks)
	// This distinguishes the fast path from the incremental "all chunks unchanged"
	// path, which would still chunk and call GetBySource.

	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	innerStore, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = innerStore.Close() }()

	var getBySourceCount atomic.Int32
	store := &countingGetBySourceStore{
		SQLiteStore:      innerStore,
		getBySourceCount: &getBySourceCount,
	}

	var chunkCount atomic.Int32
	chunker := &countingChunker{
		inner:   NewCodeChunker(),
		counter: &chunkCount,
	}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	content := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)

	// First call: full index (stores source_content_hash).
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFileIncremental() error: %v", err)
	}

	// Verify hash was stored.
	hash, err := innerStore.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty source_content_hash after indexing")
	}

	// Reset all counters.
	embedCount.Store(0)
	chunkCount.Store(0)
	getBySourceCount.Store(0)

	// Second call: same content -- should hit hash fast path.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("second IndexFileIncremental() error: %v", err)
	}

	// Verify the fast path: no embedding, no chunking, no GetBySource.
	if got := embedCount.Load(); got != 0 {
		t.Errorf("expected 0 embed calls for hash-matched file, got %d", got)
	}
	if got := chunkCount.Load(); got != 0 {
		t.Errorf("expected 0 Chunk() calls for hash-matched file, got %d", got)
	}
	if got := getBySourceCount.Load(); got != 0 {
		t.Errorf("expected 0 GetBySource calls for hash-matched file, got %d", got)
	}
}

func TestIndexFileIncremental_EmbeddingModelDriftFailsClosed(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	innerStore, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = innerStore.Close() }()

	var getBySourceCount atomic.Int32
	store := &countingGetBySourceStore{
		SQLiteStore:      innerStore,
		getBySourceCount: &getBySourceCount,
	}

	var chunkCount atomic.Int32
	chunker := &countingChunker{
		inner:   NewCodeChunker(),
		counter: &chunkCount,
	}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	content := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx1 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed-a"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)
	if err := idx1.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	hashBefore, err := innerStore.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hashBefore == "" {
		t.Fatal("expected non-empty source hash after initial index")
	}

	embedCount.Store(0)
	chunkCount.Store(0)
	getBySourceCount.Store(0)

	idx2 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed-b"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)
	err = idx2.IndexFileIncremental(context.Background(), goFile)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("IndexFileIncremental() error = %v, want ErrVectorSpaceDrift", err)
	}

	hashAfter, err := innerStore.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() after model change error: %v", err)
	}
	if hashAfter != hashBefore {
		t.Error("expected source hash to remain unchanged after rejected embedding-model drift")
	}
	if got := embedCount.Load(); got == 0 {
		t.Error("expected embed calls after embedding model change")
	}
	if got := chunkCount.Load(); got == 0 {
		t.Error("expected chunking after embedding model change")
	}
	if got := getBySourceCount.Load(); got != 0 {
		t.Errorf("expected 0 GetBySource calls on forced full re-embed, got %d", got)
	}
}

func TestIndexFileIncremental_EmptyStoredHashForcesFullReembed(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	innerStore, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = innerStore.Close() }()

	var getBySourceCount atomic.Int32
	store := &countingGetBySourceStore{
		SQLiteStore:      innerStore,
		getBySourceCount: &getBySourceCount,
	}

	var chunkCount atomic.Int32
	chunker := &countingChunker{
		inner:   NewCodeChunker(),
		counter: &chunkCount,
	}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	content := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx1 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed-a"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)
	if err := idx1.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	if err := innerStore.UpdateSourceHash(context.Background(), goFile, ""); err != nil {
		t.Fatalf("UpdateSourceHash() error: %v", err)
	}
	hashBefore, err := innerStore.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() before reindex error: %v", err)
	}
	if hashBefore != "" {
		t.Fatalf("expected empty stored hash after backfill reset, got %q", hashBefore)
	}

	embedCount.Store(0)
	chunkCount.Store(0)
	getBySourceCount.Store(0)

	idx2 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed-a"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)
	if err := idx2.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	hashAfter, err := innerStore.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() after reindex error: %v", err)
	}
	if hashAfter == "" {
		t.Fatal("expected non-empty source hash after forced full re-embed")
	}
	if got := embedCount.Load(); got == 0 {
		t.Error("expected embed calls when stored source hash is empty")
	}
	if got := chunkCount.Load(); got == 0 {
		t.Error("expected chunking when stored source hash is empty")
	}
	if got := getBySourceCount.Load(); got != 0 {
		t.Errorf("expected 0 GetBySource calls on forced full re-embed, got %d", got)
	}
}

func TestIndexerV2ForcesReembedOnV1SourceSignature(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "fake/m"
	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{
			Embeddings:    embeddings,
			Provider:      "fake",
			Model:         "m",
			VectorSpaceID: wantVSID,
		}, nil
	})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "f.go")
	if err := os.WriteFile(path, []byte("same content"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmpDir))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "same content")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmpDir)

	legacySig := idx.currentSourceSignature("same content")
	legacySig.Version = 1
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(
		context.Background(),
		path,
		oldChunks,
		[][]float64{{1, 0}, {0, 1}},
		legacySig.String(),
		wantVSID,
	); err != nil {
		t.Fatalf("seed v1 rows: %v", err)
	}

	embedInputs = nil
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if len(embedInputs) != 1 || len(embedInputs[0]) != len(oldChunks) {
		t.Fatalf("embed calls = %#v, want one full re-embed of %d chunks", embedInputs, len(oldChunks))
	}

	hashAfter, err := store.GetSourceHash(context.Background(), path)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	afterSig, ok := parseSourceSignature(hashAfter)
	if !ok {
		t.Fatalf("parse source signature after reindex failed for %q", hashAfter)
	}
	if afterSig.Version != sourceSignatureVersion {
		t.Fatalf("source signature version after reindex = %d, want %d", afterSig.Version, sourceSignatureVersion)
	}
	if hashAfter == legacySig.String() {
		t.Fatal("source hash still has v1 signature after forced re-embed")
	}
}

func TestIndexerV2RepairsV1LegacyUnknownVSID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "fake/m"
	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{
			Embeddings:    embeddings,
			Provider:      "fake",
			Model:         "m",
			VectorSpaceID: wantVSID,
		}, nil
	})

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "f.go")
	if err := os.WriteFile(path, []byte("same content"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmpDir))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "same content")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmpDir)

	legacySig := idx.currentSourceSignature("same content")
	legacySig.Version = 1
	if err := store.ReplaceSourceWithHash(context.Background(), path, oldChunks, [][]float64{{1, 0}, {0, 1}}, legacySig.String()); err != nil {
		t.Fatalf("seed v1 legacy rows: %v", err)
	}

	embedInputs = nil
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if len(embedInputs) != 1 || len(embedInputs[0]) != len(oldChunks) {
		t.Fatalf("embed calls = %#v, want one full re-embed of %d chunks", embedInputs, len(oldChunks))
	}

	var count int
	var minVSID, maxVSID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), MIN(vector_space_id), MAX(vector_space_id) FROM chunks WHERE source = ?`, path).
		Scan(&count, &minVSID, &maxVSID); err != nil {
		t.Fatalf("query vector_space_id summary: %v", err)
	}
	if count != len(oldChunks) || minVSID != wantVSID || maxVSID != wantVSID {
		t.Fatalf("vector_space_id summary = count %d min %q max %q, want %d/%q/%q", count, minVSID, maxVSID, len(oldChunks), wantVSID, wantVSID)
	}
}

func TestIndexFileIncremental_HashInvalidatedByChunkerConfig(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	content := "alpha\nbeta\ngamma\ndelta\nepsilon\nzeta\neta\ntheta\n"
	tmpDir := t.TempDir()
	txtFile := filepath.Join(tmpDir, "notes.txt")
	if err := os.WriteFile(txtFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	chunkerA, err := NewSlidingWindowChunker(18, 0)
	if err != nil {
		t.Fatalf("NewSlidingWindowChunker() error: %v", err)
	}
	idx1 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunkerA),
	)
	if err := idx1.IndexFile(context.Background(), txtFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	hashBefore, err := store.GetSourceHash(context.Background(), txtFile)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hashBefore == "" {
		t.Fatal("expected non-empty source hash after initial index")
	}

	embedCount.Store(0)

	var chunkCount atomic.Int32
	chunkerB, err := NewSlidingWindowChunker(28, 0)
	if err != nil {
		t.Fatalf("NewSlidingWindowChunker() error: %v", err)
	}
	idx2 := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(&countingChunker{
			inner:   chunkerB,
			counter: &chunkCount,
		}),
	)
	if err := idx2.IndexFileIncremental(context.Background(), txtFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	hashAfter, err := store.GetSourceHash(context.Background(), txtFile)
	if err != nil {
		t.Fatalf("GetSourceHash() after config change error: %v", err)
	}
	if hashAfter == hashBefore {
		t.Error("expected source hash to change after chunker config change")
	}
	if got := chunkCount.Load(); got == 0 {
		t.Error("expected chunking after chunker config change")
	}
	if got := embedCount.Load(); got == 0 {
		t.Error("expected embed calls after chunker config change")
	}
}

func TestIndexFileIncremental_HashMismatch(t *testing.T) {
	// After editing a file, the source hash should differ and the incremental
	// path should run (chunking + selective embedding).

	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	var chunkCount atomic.Int32
	chunker := &countingChunker{
		inner:   NewCodeChunker(),
		counter: &chunkCount,
	}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")

	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn 1\n}\n"), 0644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)

	// Full index.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("initial IndexFileIncremental() error: %v", err)
	}

	// Verify the stored hash matches initial content.
	hashBefore, err := store.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hashBefore == "" {
		t.Fatal("expected non-empty hash after initial index")
	}

	// Reset counters.
	embedCount.Store(0)
	chunkCount.Store(0)

	// Edit the file.
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc Hello() {\n\treturn 42\n}\n"), 0644); err != nil {
		t.Fatalf("write edited file: %v", err)
	}

	// Incremental index with changed content.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() after edit error: %v", err)
	}

	// The hash should have changed.
	hashAfter, err := store.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() after edit error: %v", err)
	}
	if hashAfter == hashBefore {
		t.Error("expected source hash to change after file edit")
	}
	if hashAfter == "" {
		t.Error("expected non-empty source hash after re-index")
	}

	// Should have chunked (hash mismatch means no fast path).
	if got := chunkCount.Load(); got == 0 {
		t.Error("expected at least 1 Chunk() call for changed file")
	}

	// Should have embedded changed chunk(s).
	if got := embedCount.Load(); got == 0 {
		t.Error("expected embed calls for changed file")
	}
}

func TestIndexFile_StoresHash(t *testing.T) {
	// Verify that a full IndexFile (not incremental) stores the source
	// signature so that subsequent IndexFileIncremental calls can use the
	// fast path only when the indexing inputs still match.

	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	var chunkCount atomic.Int32
	chunker := &countingChunker{
		inner:   NewCodeChunker(),
		counter: &chunkCount,
	}

	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "main.go")
	content := "package main\n\nfunc Hello() {\n\treturn 1\n}\n"
	if err := os.WriteFile(goFile, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	idx := NewIndexer(client, store,
		WithEmbeddingModel("test-embed"),
		WithWorkspaceRoot(tmpDir),
		WithChunker(chunker),
	)

	// Full IndexFile (not incremental).
	if err := idx.IndexFile(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	// Verify hash was stored.
	hash, err := store.GetSourceHash(context.Background(), goFile)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hash == "" {
		t.Fatal("expected non-empty source_content_hash after IndexFile")
	}

	// Verify the stored signature matches the expected value.
	expectedHash := idx.currentSourceSignature(content).String()
	if hash != expectedHash {
		t.Errorf("stored hash = %q, want %q", hash, expectedHash)
	}

	// Reset counters.
	embedCount.Store(0)
	chunkCount.Store(0)

	// Now IndexFileIncremental should skip via hash fast path.
	if err := idx.IndexFileIncremental(context.Background(), goFile); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if got := embedCount.Load(); got != 0 {
		t.Errorf("expected 0 embed calls after hash match, got %d", got)
	}
	if got := chunkCount.Load(); got != 0 {
		t.Errorf("expected 0 Chunk() calls after hash match, got %d", got)
	}
}

func TestIndexDirectory_WithIncremental(t *testing.T) {
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc Hello() {\n\treturn\n}\n"), 0644)

	// First index: full.
	if err := idx.IndexDirectory(context.Background(), tmpDir); err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}
	firstEmbedCount := embedCount.Load()
	if firstEmbedCount == 0 {
		t.Fatal("expected embed calls on first index")
	}

	// Reset counter.
	embedCount.Store(0)

	// Second index with WithIncremental: same content → no embeds.
	if err := idx.IndexDirectory(context.Background(), tmpDir, WithIncremental()); err != nil {
		t.Fatalf("incremental IndexDirectory() error: %v", err)
	}
	if got := embedCount.Load(); got != 0 {
		t.Errorf("expected 0 embed calls with WithIncremental on unchanged files, got %d", got)
	}
}

// TestIndexerIncremental_fullReplacement_writesVSID is the §10.2 refinement
// guard for the second indexer write path: IndexFileIncremental's
// full-replacement fall-back. The incremental indexer falls back to full
// re-embed under several conditions (no workspace root, no pre-existing
// chunks, version bump, incremental path error), and that fall-back must
// route through the same vsid-aware funnel as IndexFile. Without the guard,
// vsid silently drops through this path and corrupts the corpus.
//
// Red until indexer_incremental.go's full re-index call site is migrated
// from replaceSourceWithHash to replaceSourceWithProvenance with vsid
// resolution.
func TestIndexerIncremental_fullReplacement_writesVSID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "ollama/qwen3-embedding:8b"
	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0, 0, 0}},
		Model:         "qwen3-embedding:8b",
		Provider:      "ollama",
		VectorSpaceID: wantVSID,
	}}

	// No WithWorkspaceRoot → canDoIncremental returns false → straight to
	// the full re-index path inside IndexFileIncremental.
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("qwen3-embedding:8b"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{{ID: "c1", Content: content, Source: path, StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	if err := os.WriteFile(path, []byte("package x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	var vsidCol string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT vector_space_id FROM chunks WHERE source = ? LIMIT 1`, path).Scan(&vsidCol); err != nil {
		t.Fatalf("query: %v", err)
	}
	if vsidCol != wantVSID {
		t.Errorf("vector_space_id after IndexFileIncremental fallback = %q, want %q (full re-index path must route vsid)", vsidCol, wantVSID)
	}
}

func TestIndexerIncremental_successfulReusePathPreservesVSID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "ollama/qwen3-embedding:8b"
	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		copied := append([]string(nil), inputs...)
		embedInputs = append(embedInputs, copied)

		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0, 0, 0}
		}
		return EmbedResult{
			Embeddings:    embeddings,
			Model:         "qwen3-embedding:8b",
			Provider:      "ollama",
			VectorSpaceID: wantVSID,
		}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(
		emb,
		store,
		WithEmbeddingModel("qwen3-embedding:8b"),
		WithWorkspaceRoot(tmp),
	)
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		aContent := "func A() { return 1 }"
		if content == "changed" {
			aContent = "func A() { return 42 }"
		}
		return []Chunk{
			{
				ID: "a-" + contentHash(aContent), Content: aContent, Source: path,
				StartLine: 1, EndLine: 1, Language: "go",
				Metadata: map[string]string{"symbol_path": "A", "chunk_ordinal": "0"},
			},
			{
				ID: "b", Content: "func B() { return 2 }", Source: path,
				StartLine: 3, EndLine: 3, Language: "go",
				Metadata: map[string]string{"symbol_path": "B", "chunk_ordinal": "0"},
			},
		}, nil
	})

	if err := os.WriteFile(path, []byte("initial"), 0644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	embedInputs = nil
	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if len(embedInputs) != 1 {
		t.Fatalf("embed calls during incremental update = %d, want 1", len(embedInputs))
	}
	if got := len(embedInputs[0]); got != 1 {
		t.Fatalf("embedded chunks during incremental update = %d, want 1 (successful reuse path, not full fallback)", got)
	}

	var count int
	var minVSID, maxVSID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), MIN(vector_space_id), MAX(vector_space_id) FROM chunks WHERE source = ?`, path).
		Scan(&count, &minVSID, &maxVSID); err != nil {
		t.Fatalf("query vector_space_id summary: %v", err)
	}
	if count != 2 {
		t.Fatalf("chunk rows after incremental update = %d, want 2", count)
	}
	if minVSID != wantVSID || maxVSID != wantVSID {
		t.Errorf("vector_space_id after successful incremental path = min %q max %q, want both %q", minVSID, maxVSID, wantVSID)
	}
}

func TestIndexerIncremental_noDiffRewritePreservesVSID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "ollama/qwen3-embedding:8b"
	var embedCalls int
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedCalls++
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0, 0, 0}
		}
		return EmbedResult{
			Embeddings:    embeddings,
			Model:         "qwen3-embedding:8b",
			Provider:      "ollama",
			VectorSpaceID: wantVSID,
		}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(
		emb,
		store,
		WithEmbeddingModel("qwen3-embedding:8b"),
		WithWorkspaceRoot(tmp),
	)
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, _ string) ([]Chunk, error) {
		return []Chunk{
			{
				ID: "a", Content: "func A() { return 1 }", Source: path,
				StartLine: 1, EndLine: 1, Language: "go",
				Metadata: map[string]string{"symbol_path": "A", "chunk_ordinal": "0"},
			},
			{
				ID: "b", Content: "func B() { return 2 }", Source: path,
				StartLine: 3, EndLine: 3, Language: "go",
				Metadata: map[string]string{"symbol_path": "B", "chunk_ordinal": "0"},
			},
		}, nil
	})

	if err := os.WriteFile(path, []byte("initial file bytes"), 0644); err != nil {
		t.Fatalf("write initial: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	embedCalls = 0
	if err := os.WriteFile(path, []byte("changed file bytes"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}
	if embedCalls != 0 {
		t.Fatalf("embed calls during no-diff incremental rewrite = %d, want 0", embedCalls)
	}

	var count int
	var minVSID, maxVSID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), MIN(vector_space_id), MAX(vector_space_id) FROM chunks WHERE source = ?`, path).
		Scan(&count, &minVSID, &maxVSID); err != nil {
		t.Fatalf("query vector_space_id summary: %v", err)
	}
	if count != 2 {
		t.Fatalf("chunk rows after no-diff incremental rewrite = %d, want 2", count)
	}
	if minVSID != wantVSID || maxVSID != wantVSID {
		t.Errorf("vector_space_id after no-diff incremental rewrite = min %q max %q, want both %q", minVSID, maxVSID, wantVSID)
	}
}

func TestIndexerIncremental_legacyEmptyVSIDFallsBackAndRepairs(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "fake/m"
	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{Embeddings: embeddings, Provider: "fake", Model: "m", VectorSpaceID: wantVSID}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "initial")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmp)
	if err := store.ReplaceSourceWithHash(context.Background(), path, oldChunks, [][]float64{{1, 0}, {0, 1}}, idx.currentSourceSignature("initial").String()); err != nil {
		t.Fatalf("seed legacy rows: %v", err)
	}

	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	if len(embedInputs) != 1 || len(embedInputs[0]) != 2 {
		t.Fatalf("embed calls = %#v, want one full re-embed of 2 chunks", embedInputs)
	}

	var count int
	var minVSID, maxVSID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), MIN(vector_space_id), MAX(vector_space_id) FROM chunks WHERE source = ?`, path).
		Scan(&count, &minVSID, &maxVSID); err != nil {
		t.Fatalf("query vector_space_id summary: %v", err)
	}
	if count != 2 || minVSID != wantVSID || maxVSID != wantVSID {
		t.Fatalf("vector_space_id summary = count %d min %q max %q, want 2/%q/%q", count, minVSID, maxVSID, wantVSID, wantVSID)
	}
}

func TestIndexerIncremental_staleSourceDoesNotFallback(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	const wantVSID = "fake/m"
	var embedInputs [][]string

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	emb := EmbedderFunc(func(ctx context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		if len(embedInputs) == 1 {
			if err := store.UpdateSourceHash(ctx, path, "raced"); err != nil {
				t.Fatalf("UpdateSourceHash() during embed: %v", err)
			}
		}
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{Embeddings: embeddings, Provider: "fake", Model: "m", VectorSpaceID: wantVSID}, nil
	})

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "initial")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmp)
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), path, oldChunks, [][]float64{{1, 0}, {0, 1}}, idx.currentSourceSignature("initial").String(), wantVSID); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	err = idx.IndexFileIncremental(context.Background(), path)
	if !errors.Is(err, ErrIncrementalStaleSource) {
		t.Fatalf("IndexFileIncremental() error = %v, want ErrIncrementalStaleSource", err)
	}
	if len(embedInputs) != 1 || len(embedInputs[0]) != 1 {
		t.Fatalf("embed calls = %#v, want one incremental embed of 1 chunk and no full fallback", embedInputs)
	}

	hashAfter, err := store.GetSourceHash(context.Background(), path)
	if err != nil {
		t.Fatalf("GetSourceHash() error: %v", err)
	}
	if hashAfter != "raced" {
		t.Fatalf("source hash after stale CAS = %q, want raced", hashAfter)
	}
	var changedRows int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM chunks WHERE source = ? AND content = ?`, path, "func A() { return 42 }").
		Scan(&changedRows); err != nil {
		t.Fatalf("query changed rows: %v", err)
	}
	if changedRows != 0 {
		t.Fatalf("changed chunk rows after stale CAS = %d, want 0", changedRows)
	}
}

func TestIndexerIncremental_vectorSpaceDriftDoesNotFallback(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{Embeddings: embeddings, Provider: "fake", Model: "new", VectorSpaceID: "fake/new"}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "initial")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmp)
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), path, oldChunks, [][]float64{{1, 0}, {0, 1}}, idx.currentSourceSignature("initial").String(), "fake/old"); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	err = idx.IndexFileIncremental(context.Background(), path)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("IndexFileIncremental() error = %v, want ErrVectorSpaceDrift", err)
	}
	if len(embedInputs) != 1 || len(embedInputs[0]) != 1 {
		t.Fatalf("embed calls = %#v, want one incremental embed of 1 chunk and no full fallback", embedInputs)
	}

	var minVSID, maxVSID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT MIN(vector_space_id), MAX(vector_space_id) FROM chunks WHERE source = ?`, path).
		Scan(&minVSID, &maxVSID); err != nil {
		t.Fatalf("query vector_space_id summary: %v", err)
	}
	if minVSID != "fake/old" || maxVSID != "fake/old" {
		t.Fatalf("stored vector_space_id = min %q max %q, want fake/old unchanged", minVSID, maxVSID)
	}
}

func TestIndexerIncremental_mixedCachedVSIDDoesNotFallback(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		t.Fatalf("embedder should not be called for mixed cached VSID; inputs=%v", inputs)
		return EmbedResult{}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 1 }")

	oldChunks, err := idx.chunker.Chunk(path, "initial")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmp)
	seedIncrementalFixtureRows(t, store, path, oldChunks, [][]float64{{1, 0}, {0, 1}}, idx.currentSourceSignature("initial").String(), []string{"fake/a", "fake/b"})

	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	err = idx.IndexFileIncremental(context.Background(), path)
	if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
		t.Fatalf("IndexFileIncremental() error = %v, want ErrCorpusMixedVectorSpaces", err)
	}
}

func TestIndexerIncremental_emptyChunkOutputClearsWithoutVSID(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1}},
		Provider:      "fake",
		Model:         "m",
		VectorSpaceID: "fake/m",
	}}
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		if content == "empty-by-chunker" {
			return nil, nil
		}
		return []Chunk{{ID: "c", Content: content, Source: path, StartLine: 1, EndLine: 1, Metadata: map[string]string{"anchor_hash": "seed"}}}, nil
	})

	if err := os.WriteFile(path, []byte("seed"), 0644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("seed IndexFile() error: %v", err)
	}
	if err := os.WriteFile(path, []byte("empty-by-chunker"), 0644); err != nil {
		t.Fatalf("write empty-by-chunker: %v", err)
	}
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}

	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM chunks WHERE source = ?`, path).Scan(&count); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if count != 0 {
		t.Fatalf("chunk rows after empty chunk output = %d, want 0", count)
	}
}

func TestIndexerIncremental_vsidStoreWithoutCASFallsBackToFullReembed(t *testing.T) {
	inner, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = inner.Close() }()
	store := &vsidNoCASStore{inner: inner}

	const wantVSID = "fake/m"
	var embedInputs [][]string
	emb := EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		embedInputs = append(embedInputs, append([]string(nil), inputs...))
		embeddings := make([][]float64, len(inputs))
		for i := range inputs {
			embeddings[i] = []float64{float64(i + 1), 0}
		}
		return EmbedResult{Embeddings: embeddings, Provider: "fake", Model: "m", VectorSpaceID: wantVSID}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"), WithWorkspaceRoot(tmp))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = incrementalVSIDFixtureChunker("func A() { return 1 }", "func A() { return 42 }")

	oldChunks, err := idx.chunker.Chunk(path, "initial")
	if err != nil {
		t.Fatalf("chunk seed: %v", err)
	}
	populateFixtureStableKeys(t, oldChunks, tmp)
	if err := inner.ReplaceSourceWithHashAndVectorSpaceID(context.Background(), path, oldChunks, [][]float64{{1, 0}, {0, 1}}, idx.currentSourceSignature("initial").String(), wantVSID); err != nil {
		t.Fatalf("seed rows: %v", err)
	}

	if err := os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	if err := idx.IndexFileIncremental(context.Background(), path); err != nil {
		t.Fatalf("IndexFileIncremental() error: %v", err)
	}
	if store.getBySourceCalls != 0 {
		t.Fatalf("GetBySource calls = %d, want 0 when CAS capability is missing", store.getBySourceCalls)
	}
	if len(embedInputs) != 1 || len(embedInputs[0]) != 2 {
		t.Fatalf("embed calls = %#v, want one full re-embed of 2 chunks", embedInputs)
	}
}

func incrementalVSIDFixtureChunker(initialA, changedA string) chunkerFunc {
	return chunkerFunc(func(path string, content string) ([]Chunk, error) {
		aContent := initialA
		if content == "changed" {
			aContent = changedA
		}
		return []Chunk{
			{
				ID: "a-" + contentHash(aContent), Content: aContent, Source: path,
				StartLine: 1, EndLine: 1, Language: "go",
				Metadata: map[string]string{"symbol_path": "A", "chunk_ordinal": "0"},
			},
			{
				ID: "b", Content: "func B() { return 2 }", Source: path,
				StartLine: 3, EndLine: 3, Language: "go",
				Metadata: map[string]string{"symbol_path": "B", "chunk_ordinal": "0"},
			},
		}, nil
	})
}

type vsidNoCASStore struct {
	inner            *SQLiteStore
	getBySourceCalls int
}

func (s *vsidNoCASStore) Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error {
	return s.inner.Store(ctx, chunks, embeddings)
}

func (s *vsidNoCASStore) Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	return s.inner.Search(ctx, queryEmbedding, k)
}

func (s *vsidNoCASStore) DeleteBySource(ctx context.Context, source string) error {
	return s.inner.DeleteBySource(ctx, source)
}

func (s *vsidNoCASStore) Stats(ctx context.Context) (StoreStats, error) {
	return s.inner.Stats(ctx)
}

func (s *vsidNoCASStore) Close() error {
	return s.inner.Close()
}

func (s *vsidNoCASStore) GetSourceHash(ctx context.Context, source string) (string, error) {
	return s.inner.GetSourceHash(ctx, source)
}

func (s *vsidNoCASStore) GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error) {
	s.getBySourceCalls++
	return s.inner.GetBySource(ctx, source)
}

func (s *vsidNoCASStore) ReplaceSourceWithHashAndVectorSpaceID(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID string) error {
	return s.inner.ReplaceSourceWithHashAndVectorSpaceID(ctx, source, chunks, embeddings, sourceHash, vectorSpaceID)
}

func populateFixtureStableKeys(t *testing.T, chunks []Chunk, workspaceRoot string) {
	t.Helper()
	for i := range chunks {
		key, err := ComputeStableKey(chunks[i], workspaceRoot)
		if err != nil {
			t.Fatalf("ComputeStableKey(%q): %v", chunks[i].ID, err)
		}
		chunks[i].StableKey = key
	}
}

func seedIncrementalFixtureRows(t *testing.T, store *SQLiteStore, source string, chunks []Chunk, embeddings [][]float64, sourceHash string, vectorSpaceIDs []string) {
	t.Helper()
	// Intentional schema-coupled seed helper: public replace APIs enforce a
	// uniform VSID, while these tests need legacy and mixed rows. Keep raw
	// INSERTs centralized here so future migrations update one fixture path.
	if len(chunks) != len(embeddings) || len(chunks) != len(vectorSpaceIDs) {
		t.Fatalf("bad seed lengths: chunks=%d embeddings=%d vsids=%d", len(chunks), len(embeddings), len(vectorSpaceIDs))
	}
	ctx := context.Background()
	for i, chunk := range chunks {
		embJSON, err := json.Marshal(embeddings[i])
		if err != nil {
			t.Fatalf("marshal seed embedding: %v", err)
		}
		if _, err := store.db.ExecContext(ctx,
			`INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
			 VALUES (?, ?, ?, ?, ?, ?, '{}', ?, 1, ?, ?, ?)`,
			chunk.ID, chunk.Content, source, chunk.StartLine, chunk.EndLine, chunk.Language, string(embJSON), chunk.StableKey, sourceHash, vectorSpaceIDs[i]); err != nil {
			t.Fatalf("seed row %q: %v", chunk.ID, err)
		}
	}
}

// --- WithPruneDeleted tests ---

// newPruneTestIndexer builds an indexer backed by an in-memory SQLiteStore
// and a mock embed server, for exercising directory prune behavior.
func newPruneTestIndexer(t *testing.T) (*Indexer, *SQLiteStore) {
	t.Helper()
	var embedCount atomic.Int32
	srv := newBatchCountingEmbedServer(4, &embedCount)
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	return NewIndexer(client, store, WithEmbeddingModel("test-embed")), store
}

// sourceSet lists stored sources as a membership set.
func sourceSet(t *testing.T, store *SQLiteStore) map[string]bool {
	t.Helper()
	sources, err := store.ListSources(context.Background())
	if err != nil {
		t.Fatalf("ListSources() error: %v", err)
	}
	set := make(map[string]bool, len(sources))
	for _, s := range sources {
		set[s] = true
	}
	return set
}

func TestIndexDirectory_PruneDeleted_RemovesDeletedFile(t *testing.T) {
	idx, store := newPruneTestIndexer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	keptFile := filepath.Join(tmpDir, "main.go")
	deletedFile := filepath.Join(tmpDir, "old.go")
	_ = os.WriteFile(keptFile, []byte("package main\n\nfunc Hello() {}\n"), 0644)
	_ = os.WriteFile(deletedFile, []byte("package main\n\nfunc Old() {}\n"), 0644)

	if err := idx.IndexDirectory(ctx, tmpDir); err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}
	set := sourceSet(t, store)
	if !set[keptFile] || !set[deletedFile] {
		t.Fatalf("expected both files indexed, got sources %v", set)
	}

	// Seed a source outside the workspace root: prune must never touch it.
	outside := filepath.Join(t.TempDir(), "elsewhere.go")
	outsideChunks := []Chunk{{ID: "out1", Content: "outside", Source: outside, StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.Store(ctx, outsideChunks, [][]float64{{1, 0, 0, 0}}); err != nil {
		t.Fatalf("Store() outside chunk error: %v", err)
	}

	if err := os.Remove(deletedFile); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	if err := idx.IndexDirectory(ctx, tmpDir, WithIncremental(), WithPruneDeleted()); err != nil {
		t.Fatalf("prune IndexDirectory() error: %v", err)
	}

	set = sourceSet(t, store)
	if set[deletedFile] {
		t.Errorf("deleted file %q still stored after prune", deletedFile)
	}
	if !set[keptFile] {
		t.Errorf("kept file %q missing after prune", keptFile)
	}
	if !set[outside] {
		t.Errorf("outside-root source %q was pruned; must be preserved", outside)
	}
}

func TestIndexDirectory_PruneDeleted_RemovesExcludedSource(t *testing.T) {
	idx, store := newPruneTestIndexer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	keptFile := filepath.Join(tmpDir, "main.go")
	genDir := filepath.Join(tmpDir, "gen")
	_ = os.MkdirAll(genDir, 0755)
	excludedFile := filepath.Join(genDir, "out.go")
	_ = os.WriteFile(keptFile, []byte("package main\n\nfunc Hello() {}\n"), 0644)
	_ = os.WriteFile(excludedFile, []byte("package gen\n\nfunc Out() {}\n"), 0644)

	if err := idx.IndexDirectory(ctx, tmpDir); err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}
	if set := sourceSet(t, store); !set[excludedFile] {
		t.Fatalf("expected %q indexed on first run, got sources %v", excludedFile, set)
	}

	// Second run excludes gen/: its stored source must be pruned.
	if err := idx.IndexDirectory(ctx, tmpDir, WithIncremental(), WithPruneDeleted(), WithExclude("gen")); err != nil {
		t.Fatalf("prune IndexDirectory() error: %v", err)
	}

	set := sourceSet(t, store)
	if set[excludedFile] {
		t.Errorf("excluded file %q still stored after prune", excludedFile)
	}
	if !set[keptFile] {
		t.Errorf("kept file %q missing after prune", keptFile)
	}
}

func TestIndexDirectory_WithIncremental_PreservesDeletedWithoutPrune(t *testing.T) {
	idx, store := newPruneTestIndexer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	keptFile := filepath.Join(tmpDir, "main.go")
	deletedFile := filepath.Join(tmpDir, "old.go")
	_ = os.WriteFile(keptFile, []byte("package main\n\nfunc Hello() {}\n"), 0644)
	_ = os.WriteFile(deletedFile, []byte("package main\n\nfunc Old() {}\n"), 0644)

	if err := idx.IndexDirectory(ctx, tmpDir); err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}
	if err := os.Remove(deletedFile); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}

	// No WithPruneDeleted: deleted sources stay (existing manual semantics).
	if err := idx.IndexDirectory(ctx, tmpDir, WithIncremental()); err != nil {
		t.Fatalf("incremental IndexDirectory() error: %v", err)
	}

	set := sourceSet(t, store)
	if !set[deletedFile] {
		t.Errorf("deleted file %q was removed without WithPruneDeleted", deletedFile)
	}
	if !set[keptFile] {
		t.Errorf("kept file %q missing", keptFile)
	}
}
