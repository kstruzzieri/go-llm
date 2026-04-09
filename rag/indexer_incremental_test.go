package rag

import (
	"context"
	"encoding/json"
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
