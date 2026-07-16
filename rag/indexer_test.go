package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newMockEmbedServer returns an httptest.Server that serves mock embeddings.
func newMockEmbedServer(dim int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Return a simple embedding vector
		emb := make([]float64, dim)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func testManagedIndexer(t *testing.T, store *SQLiteStore, vectorSpaceID string, embedErr error) *Indexer {
	t.Helper()
	idx, err := NewIndexerWithEmbedder(EmbedderFunc(func(_ context.Context, _ string, inputs []string) (EmbedResult, error) {
		if embedErr != nil {
			return EmbedResult{}, embedErr
		}
		embeddings := make([][]float64, len(inputs))
		for i := range embeddings {
			embeddings[i] = []float64{float64(i + 1), 1}
		}
		return EmbedResult{Embeddings: embeddings, VectorSpaceID: vectorSpaceID}, nil
	}), store, WithEmbeddingModel("test"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	return idx
}

func TestIndexerIndexTextUsesExistingPipeline(t *testing.T) {
	store := newTestStore(t)
	idx := testManagedIndexer(t, store, "test/v1", nil)
	if err := idx.IndexText(context.Background(), "notes.md", "# Runbook\nrestart safely"); err != nil {
		t.Fatal(err)
	}
	chunks, err := store.GetBySource(context.Background(), "notes.md")
	if err != nil || len(chunks) == 0 {
		t.Fatalf("chunks=%d err=%v", len(chunks), err)
	}
	if chunks[0].Chunk.Metadata["vector_space_id"] != "test/v1" {
		t.Fatalf("metadata=%v", chunks[0].Chunk.Metadata)
	}
}

func TestIndexerIndexFile(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	testFile := filepath.Join("..", "testdata", "sample.go")
	if err := idx.IndexFile(context.Background(), testFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.TotalChunks == 0 {
		t.Error("expected at least one chunk after indexing")
	}
	if stats.EmbeddingDim != 4 {
		t.Errorf("embedding dim = %d, want 4", stats.EmbeddingDim)
	}
}

func TestIndexerReindex(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)
	testFile := filepath.Join("..", "testdata", "sample.go")

	// Index twice — should not duplicate
	_ = idx.IndexFile(context.Background(), testFile)
	stats1, _ := store.Stats(context.Background())

	_ = idx.IndexFile(context.Background(), testFile)
	stats2, _ := store.Stats(context.Background())

	if stats2.TotalChunks != stats1.TotalChunks {
		t.Errorf("re-indexing changed chunk count from %d to %d", stats1.TotalChunks, stats2.TotalChunks)
	}
}

func TestIndexerIndexDirectory(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	// Create a temp directory with some files
	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "util.py"), []byte("def helper():\n    pass\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "data.csv"), []byte("a,b,c\n1,2,3\n"), 0644) // should be skipped

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected chunks after indexing directory")
	}
	// CSV should not be indexed since .csv is not in default extensions
	if stats.TotalSources > 2 {
		t.Errorf("expected at most 2 sources (go + py), got %d", stats.TotalSources)
	}
}

func TestIndexerContextCancellation(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n"), 0644)

	err := idx.IndexDirectory(ctx, tmpDir)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestIndexerPreservesDataOnEmbedFailure(t *testing.T) {
	// First, index successfully with a working server
	goodSrv := newMockEmbedServer(4)
	defer goodSrv.Close()

	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	goodClient := ollama.NewClient(ollama.WithBaseURL(goodSrv.URL))
	idx := NewIndexer(goodClient, store, WithEmbeddingModel("test-embed"))

	testFile := filepath.Join("..", "testdata", "sample.go")
	if err := idx.IndexFile(context.Background(), testFile); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalChunks == 0 {
		t.Fatal("expected chunks after initial indexing")
	}

	// Now try to re-index with a broken embed server
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer badSrv.Close()

	badClient := ollama.NewClient(ollama.WithBaseURL(badSrv.URL))
	badIdx := NewIndexer(badClient, store, WithEmbeddingModel("nonexistent"))

	err := badIdx.IndexFile(context.Background(), testFile)
	if err == nil {
		t.Fatal("expected error from broken embed server")
	}

	// Old data should still be intact
	statsAfter, _ := store.Stats(context.Background())
	if statsAfter.TotalChunks != statsBefore.TotalChunks {
		t.Errorf("embed failure changed chunk count from %d to %d — data was lost",
			statsBefore.TotalChunks, statsAfter.TotalChunks)
	}
}

func TestIndexerReindexEmptyFileRemovesChunks(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	idx := NewIndexer(client, store, WithEmbeddingModel("test-embed"))

	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		t.Fatalf("write initial file: %v", err)
	}

	if err := idx.IndexFile(context.Background(), file); err != nil {
		t.Fatalf("initial IndexFile() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalChunks == 0 {
		t.Fatal("expected chunks after initial indexing")
	}

	if err := os.WriteFile(file, []byte(""), 0644); err != nil {
		t.Fatalf("truncate file: %v", err)
	}

	if err := idx.IndexFile(context.Background(), file); err != nil {
		t.Fatalf("re-index empty file error: %v", err)
	}

	statsAfter, _ := store.Stats(context.Background())
	if statsAfter.TotalChunks != 0 {
		t.Errorf("expected 0 chunks after re-indexing empty file, got %d", statsAfter.TotalChunks)
	}
}

func TestIndexerDuplicateContentDifferentPositions(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	chunker, _ := NewSlidingWindowChunker(30, 0)
	idx := NewIndexer(client, store, WithChunker(chunker), WithEmbeddingModel("test"))

	// Create a file with duplicate lines that will end up in different chunks
	tmpDir := t.TempDir()
	content := "return nil\nreturn nil\nreturn nil\nreturn nil\nreturn nil\n"
	_ = os.WriteFile(filepath.Join(tmpDir, "dup.go"), []byte(content), 0644)

	if err := idx.IndexFile(context.Background(), filepath.Join(tmpDir, "dup.go")); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// With duplicate content at different positions, we should still get
	// multiple chunks stored (not collapsed into one)
	if stats.TotalChunks < 2 {
		t.Errorf("expected multiple chunks for duplicate content, got %d", stats.TotalChunks)
	}
}

func TestIndexerWithCustomChunker(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	chunker, _ := NewSlidingWindowChunker(50, 10)
	idx := NewIndexer(client, store, WithChunker(chunker))

	testFile := filepath.Join("..", "testdata", "sample.go")
	if err := idx.IndexFile(context.Background(), testFile); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected chunks with custom chunker")
	}
}

// newCountingEmbedServer returns a mock server that counts embed requests.
func newCountingEmbedServer(dim int, counter *atomic.Int32) *httptest.Server {
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

func TestIndexerConcurrentIndexDirectory(t *testing.T) {
	var reqCount atomic.Int32
	srv := newCountingEmbedServer(4, &reqCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	numFiles := 10
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("file%d.go", i)
		content := fmt.Sprintf("package main\n\nfunc F%d() {}\n", i)
		_ = os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644)
	}

	err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(3))
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalSources != numFiles {
		t.Errorf("expected %d sources, got %d", numFiles, stats.TotalSources)
	}
	if int(reqCount.Load()) < numFiles {
		t.Errorf("expected at least %d embed requests, got %d", numFiles, reqCount.Load())
	}
}

func TestIndexerConcurrencyValidation(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	// WithConcurrency(0) should not panic or deadlock — clamped to 1
	if err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(0)); err != nil {
		t.Fatalf("IndexDirectory with concurrency 0: %v", err)
	}

	// WithConcurrency(-1) should also work
	if err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(-1)); err != nil {
		t.Fatalf("IndexDirectory with concurrency -1: %v", err)
	}
}

func TestIndexerConcurrentErrorAggregation(t *testing.T) {
	// Server that always fails embedding
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer failSrv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(failSrv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "c.go"), []byte("package c\n\nfunc C() {}\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(2))
	if err == nil {
		t.Fatal("expected error when all files fail embedding")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "3 errors") {
		t.Errorf("expected '3 errors' in message, got: %s", errMsg)
	}
}

func TestIndexerGitignoreNestedScoping(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root .gitignore ignores *.log
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)

	// src/.gitignore ignores *.tmp
	_ = os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.tmp\n"), 0644)

	// lib/ has no .gitignore
	_ = os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// Create test files
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log data\n"), 0644) // ignored by root
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "util.go"), []byte("package src\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "cache.tmp"), []byte("temp\n"), 0644) // ignored by src
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("log\n"), 0644)  // ignored by root
	_ = os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected indexed: main.go, src/util.go, lib/helper.go = 3 sources
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources, got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreNegation(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore all .gen.go, but un-ignore important.gen.go
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.gen.go\n!important.gen.go\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "schema.gen.go"), []byte("package main\n"), 0644)    // ignored
	_ = os.WriteFile(filepath.Join(tmpDir, "important.gen.go"), []byte("package main\n"), 0644) // NOT ignored

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go + important.gen.go = 2 sources
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (negation should un-ignore important.gen.go), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreDirectorySkip(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore the build/ directory
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmpDir, "build", "deep", "nested"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "build", "deep", "nested", "inner.go"), []byte("package nested\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Only main.go — entire build/ subtree skipped
	if stats.TotalSources != 1 {
		t.Errorf("expected 1 source (build/ dir skipped), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreWithExcludePriority(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// .gitignore tries to un-ignore vendor/
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("!vendor/\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmpDir, "vendor"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "vendor", "dep.go"), []byte("package dep\n"), 0644)

	// Default WithExclude includes "vendor"
	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Only main.go — WithExclude("vendor") takes priority over .gitignore negation
	if stats.TotalSources != 1 {
		t.Errorf("expected 1 source (WithExclude overrides .gitignore negation), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignorePathScoped(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Path-scoped rule: only ignore generated/ under src/
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("src/generated/\n"), 0644)

	_ = os.MkdirAll(filepath.Join(tmpDir, "src", "generated"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "lib", "generated"), 0755)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "generated", "proto.go"), []byte("package gen\n"), 0644) // ignored
	_ = os.WriteFile(filepath.Join(tmpDir, "lib", "generated", "types.go"), []byte("package gen\n"), 0644) // NOT ignored

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go, src/app.go, lib/generated/types.go = 3
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (path-scoped ignore), got %d", stats.TotalSources)
	}
}

func TestIndexerNoGitignore(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// No .gitignore file
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "util.py"), []byte("def helper():\n    pass\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (no gitignore), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreSiblingScoping(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	_ = os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	_ = os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// src/.gitignore ignores *.gen.go
	_ = os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.gen.go\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "schema.gen.go"), []byte("package src\n"), 0644) // ignored by src rule
	_ = os.WriteFile(filepath.Join(tmpDir, "lib", "types.gen.go"), []byte("package lib\n"), 0644)  // NOT ignored
	_ = os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: src/app.go, lib/types.gen.go, lib/helper.go = 3
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (src/.gitignore should not affect lib/), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreDirectoryUnignore(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore build/, then un-ignore it — subtree should be walked
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n!build/\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go + build/output.go = 2 (directory un-ignored, subtree walked)
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (build/ un-ignored), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreOverlappingNestedRules(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore *.log, then un-ignore *.log
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n!*.log\n"), 0644)

	// src/: re-ignore *.log (overrides parent negation)
	_ = os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.log\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "root.log"), []byte("root log\n"), 0644) // NOT ignored (root negation)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("src log\n"), 0644) // ignored (nested re-ignore)

	err := idx.IndexDirectory(context.Background(), tmpDir,
		WithExtensions(".go", ".log")) // include .log in extensions
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go, root.log, src/app.go = 3 (src/debug.log re-ignored by nested rule)
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (nested re-ignore overrides parent negation), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreAlreadyIndexedFileSurvives(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// First run: no .gitignore, index everything
	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "util.go"), []byte("package main\n\nfunc util() {}\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalSources != 2 {
		t.Fatalf("expected 2 sources before gitignore, got %d", statsBefore.TotalSources)
	}

	// Second run: add .gitignore that ignores util.go
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("util.go\n"), 0644)

	err = idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("second IndexDirectory() error: %v", err)
	}

	statsAfter, _ := store.Stats(context.Background())
	// util.go chunks should still be in the store (prospective filtering only)
	if statsAfter.TotalSources != 2 {
		t.Errorf("expected 2 sources still (already-indexed data preserved), got %d", statsAfter.TotalSources)
	}
}

func TestIndexerGitignoreNegationInsideIgnoredDir(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer func() { _ = store.Close() }()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore build/ directory, then try to un-ignore a file inside it.
	// The negation cannot work because SkipDir prevents the walk from reaching the file.
	_ = os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n!build/keep.go\n"), 0644)

	_ = os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	_ = os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)
	_ = os.WriteFile(filepath.Join(tmpDir, "build", "keep.go"), []byte("package build\n"), 0644)
	_ = os.WriteFile(filepath.Join(tmpDir, "build", "other.go"), []byte("package build\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Only main.go — build/ dir is skipped entirely, negation of build/keep.go has no effect
	if stats.TotalSources != 1 {
		t.Errorf("expected 1 source (cannot un-ignore file inside ignored dir), got %d", stats.TotalSources)
	}
}

// --- Persistent drift signature tests (#82 Phase 2, spec §5.6 / §5.7) ---

// TestIndexer_writesMetadataMirror is the indexer-level end-to-end for the
// store-layer mirror written by Test 1: when IndexFile runs with an embedder
// that resolves a non-empty VectorSpaceID, the indexer MUST route that vsid
// into the vsid-aware store method so each persisted chunk ends up with the
// value on both the chunks.vector_space_id column AND in the per-chunk
// metadata JSON.
//
// Red until replaceSourceWithHash grows a vsid-aware dispatch arm that
// invokes atomicSourceReplacerWithVectorSpaceID.
func TestIndexer_writesMetadataMirror(t *testing.T) {
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

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("qwen3-embedding:8b"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{{
			ID: "c1", Content: content, Source: path,
			StartLine: 1, EndLine: 1, Language: "go",
			Metadata: map[string]string{"kind": "fn"},
		}}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	if err := os.WriteFile(path, []byte("package x"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	rows, err := store.db.QueryContext(context.Background(),
		`SELECT id, vector_space_id, metadata FROM chunks WHERE source = ? ORDER BY id`, path)
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
		if vsidCol != wantVSID {
			t.Errorf("chunk %q vector_space_id column = %q, want %q", id, vsidCol, wantVSID)
		}
		var meta map[string]string
		if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
			t.Fatalf("chunk %q metadata unmarshal: %v (raw=%q)", id, err, metaJSON)
		}
		if got := meta["vector_space_id"]; got != wantVSID {
			t.Errorf("chunk %q metadata[\"vector_space_id\"] = %q, want %q", id, got, wantVSID)
		}
		// Pre-existing metadata keys must survive the mirror.
		if got := meta["kind"]; got != "fn" {
			t.Errorf("chunk %q metadata[\"kind\"] = %q, want %q (mirror must not clobber caller metadata)", id, got, "fn")
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if seen != 1 {
		t.Fatalf("expected 1 chunk, got %d", seen)
	}
}

// TestIndexer_vsidFallbackProviderModel covers spec §5.6 fallback derivation:
// when the embedder returns an empty VectorSpaceID but populates Provider and
// Model, the indexer MUST synthesize the vsid as "Provider/Model" rather than
// persisting an empty column. This preserves vsid drift detection for older
// Embedder implementations that did not adopt the explicit VectorSpaceID field.
//
// Red until IndexFile computes the fallback before calling
// replaceSourceWithProvenance.
func TestIndexer_vsidFallbackProviderModel(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0, 0, 0}},
		Provider:      "fake",
		Model:         "m",
		VectorSpaceID: "", // empty — indexer must fall back to Provider/Model
	}}

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"))
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

	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("IndexFile() error: %v", err)
	}

	const wantVSID = "fake/m"
	var vsidCol string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT vector_space_id FROM chunks WHERE source = ? LIMIT 1`, path).Scan(&vsidCol); err != nil {
		t.Fatalf("query: %v", err)
	}
	if vsidCol != wantVSID {
		t.Errorf("vector_space_id = %q, want %q (derived from Provider/Model fallback)", vsidCol, wantVSID)
	}
}

// TestIndexer_vsidCapableStore_emptyVSID_fails covers spec §5.6 fail-closed
// behavior. When the underlying store is vsid-capable and the embedder
// returns embeddings without any way to derive a vsid (empty
// VectorSpaceID/Provider/Model), IndexFile MUST refuse the write before any
// rows land. This prevents fresh vector_space_id = "" rows from being
// silently mixed into a corpus that drift detection cannot recover from.
//
// Red until IndexFile gains a pre-dispatch fail-closed check for vsid-capable
// stores receiving an empty resolved vsid on a non-empty chunk batch.
func TestIndexer_vsidCapableStore_emptyVSID_fails(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings: [][]float64{{1, 0, 0, 0}},
		// VectorSpaceID, Provider, Model all empty — resolveVectorSpaceID returns ""
	}}

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"))
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

	err = idx.IndexFile(context.Background(), path)
	if err == nil {
		t.Fatal("IndexFile() with empty vsid against capable store: expected error, got nil")
	}

	// No fresh empty-vsid rows must have been written.
	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM chunks WHERE source = ?`, path).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("chunks rows after failed IndexFile = %d, want 0 (must not write empty-vsid rows)", count)
	}
}

func TestIndexer_replaceSourceWithProvenance_emptyVSIDRejectsCapableStore(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	idx, err := NewIndexerWithEmbedder(EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
		return EmbedResult{}, nil
	}), store)
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	chunks := []Chunk{{
		ID: "c1", Content: "package x", Source: "main.go",
		StartLine: 1, EndLine: 1, Language: "go", Metadata: map[string]string{},
	}}

	err = idx.replaceSourceWithProvenance(context.Background(), "main.go", chunks, [][]float64{{1, 0, 0, 0}}, "hash", "")
	if err == nil {
		t.Fatal("replaceSourceWithProvenance() with empty vsid against capable store: expected error, got nil")
	}

	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM chunks WHERE source = 'main.go'`).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("chunks rows after rejected replace = %d, want 0", count)
	}
}

func TestIndexer_IndexFileExistingVectorSpaceDriftFailsClosed(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	if err := os.WriteFile(path, []byte("package old"), 0644); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	chunker := chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{{
			ID: "c1", Content: content, Source: path,
			StartLine: 1, EndLine: 1, Language: "go",
			Metadata: map[string]string{},
		}}, nil
	})
	seedEmb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0}},
		Provider:      "fake",
		Model:         "old",
		VectorSpaceID: "fake/old",
	}}
	idx, err := NewIndexerWithEmbedder(seedEmb, store, WithEmbeddingModel("old"), WithChunker(chunker))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() seed error: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("seed IndexFile() error: %v", err)
	}

	if err := os.WriteFile(path, []byte("package new"), 0644); err != nil {
		t.Fatalf("write changed: %v", err)
	}
	driftEmb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{0, 1}},
		Provider:      "fake",
		Model:         "new",
		VectorSpaceID: "fake/new",
	}}
	idx, err = NewIndexerWithEmbedder(driftEmb, store, WithEmbeddingModel("new"), WithChunker(chunker))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() drift error: %v", err)
	}
	err = idx.IndexFile(context.Background(), path)
	if !errors.Is(err, ErrVectorSpaceDrift) {
		t.Fatalf("IndexFile() error = %v, want ErrVectorSpaceDrift", err)
	}

	var content, vectorSpaceID string
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT content, vector_space_id FROM chunks WHERE source = ?`, path).
		Scan(&content, &vectorSpaceID); err != nil {
		t.Fatalf("query stored row: %v", err)
	}
	if content != "package old" || vectorSpaceID != "fake/old" {
		t.Fatalf("stored row = content %q vsid %q, want unchanged old row/fake/old", content, vectorSpaceID)
	}
}

// TestIndexer_vsidCapableStore_emptyContent_clearsCleanly is the §10.2
// refinement sibling of the empty-vsid fail-closed test. The fail-closed
// check must NOT fire on legitimate empty-content / no-chunks paths, which
// all four call sites (rag/indexer.go and rag/indexer_incremental.go) drive
// through idx.replaceSource(ctx, path, nil, nil). Those paths intentionally
// pass empty vsid with empty chunks, and must continue to work.
func TestIndexer_vsidCapableStore_emptyContent_clearsCleanly(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Seed a pre-existing row to confirm empty-content clears it.
	seedEmb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0, 0, 0}},
		VectorSpaceID: "ollama/seed",
	}}
	idx, err := NewIndexerWithEmbedder(seedEmb, store, WithEmbeddingModel("seed"))
	if err != nil {
		t.Fatalf("NewIndexerWithEmbedder() error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{{ID: "c1", Content: content, Source: path, StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}, nil
	})

	tmp := t.TempDir()
	path := filepath.Join(tmp, "f.go")
	if err := os.WriteFile(path, []byte("package x"), 0644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("seed IndexFile: %v", err)
	}

	// Now truncate the file and re-index. This drives the empty-content path
	// at indexer.go's `if content == ""` branch, which calls
	// idx.replaceSource(ctx, path, nil, nil) — empty vsid, no chunks.
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("IndexFile on empty content: expected clean clear, got error: %v", err)
	}

	var count int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM chunks WHERE source = ?`, path).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("chunks rows after empty-content re-index = %d, want 0 (seeded row should be cleared)", count)
	}
}

// hashOnlyStore is a test double satisfying VectorStore and
// atomicSourceReplacerWithHash but NOT atomicSourceReplacerWithVectorSpaceID.
// It records which write path was invoked so Test 5 can assert the indexer
// falls through to the hash-only arm and silently drops vsid.
type hashOnlyStore struct {
	withVSIDCalls int
	withHashCalls int
	bareCalls     int
	lastHash      string
	lastChunks    []Chunk
}

func (h *hashOnlyStore) Store(_ context.Context, _ []Chunk, _ [][]float64) error {
	return nil
}
func (h *hashOnlyStore) Search(_ context.Context, _ []float64, _ int) ([]SearchResult, error) {
	return nil, nil
}
func (h *hashOnlyStore) DeleteBySource(_ context.Context, _ string) error { return nil }
func (h *hashOnlyStore) Stats(_ context.Context) (StoreStats, error)      { return StoreStats{}, nil }
func (h *hashOnlyStore) Close() error                                     { return nil }

func (h *hashOnlyStore) ReplaceSourceWithHash(_ context.Context, _ string, chunks []Chunk, _ [][]float64, hash string) error {
	h.withHashCalls++
	h.lastHash = hash
	h.lastChunks = chunks
	return nil
}

// TestIndexer_externalHashOnlyStore_dropsVSID covers spec §5.6 silent-drop
// dispatch: an external VectorStore that opts into the hash-tier interface
// but NOT the vsid-tier interface must still accept IndexFile writes; the
// indexer dispatches to the hash arm and the resolved vsid is silently
// dropped. This preserves the additive capability tier contract — external
// consumers (Firn IDE, Flux ML, Quantum Trader) that implement only the
// pre-#82 interfaces keep working without code changes.
func TestIndexer_externalHashOnlyStore_dropsVSID(t *testing.T) {
	store := &hashOnlyStore{}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0, 0, 0}},
		Provider:      "ollama",
		Model:         "m",
		VectorSpaceID: "ollama/m",
	}}

	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("m"))
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

	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("IndexFile against hash-only store: expected success, got %v", err)
	}

	if store.withHashCalls != 1 {
		t.Errorf("ReplaceSourceWithHash calls = %d, want 1 (hash arm should have been chosen)", store.withHashCalls)
	}
	if store.withVSIDCalls != 0 {
		t.Errorf("vsid-aware calls = %d, want 0 (store does not implement that arm)", store.withVSIDCalls)
	}
	if store.bareCalls != 0 {
		t.Errorf("bare ReplaceSource calls = %d, want 0 (hash arm should win over bare)", store.bareCalls)
	}
	if store.lastHash == "" {
		t.Errorf("ReplaceSourceWithHash received empty hash; expected non-empty source signature")
	}
}
