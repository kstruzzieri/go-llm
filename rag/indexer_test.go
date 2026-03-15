package rag

import (
	"context"
	"encoding/json"
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
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestIndexerIndexFile(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	defer store.Close()

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
	defer store.Close()

	idx := NewIndexer(client, store)
	testFile := filepath.Join("..", "testdata", "sample.go")

	// Index twice — should not duplicate
	idx.IndexFile(context.Background(), testFile)
	stats1, _ := store.Stats(context.Background())

	idx.IndexFile(context.Background(), testFile)
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
	defer store.Close()

	idx := NewIndexer(client, store)

	// Create a temp directory with some files
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.py"), []byte("def helper():\n    pass\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "data.csv"), []byte("a,b,c\n1,2,3\n"), 0644) // should be skipped

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
	defer store.Close()

	idx := NewIndexer(client, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n"), 0644)

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
	defer store.Close()

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
		w.Write([]byte(`{"error":"model not found"}`))
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
	defer store.Close()

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
	defer store.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	chunker, _ := NewSlidingWindowChunker(30, 0)
	idx := NewIndexer(client, store, WithChunker(chunker), WithEmbeddingModel("test"))

	// Create a file with duplicate lines that will end up in different chunks
	tmpDir := t.TempDir()
	content := "return nil\nreturn nil\nreturn nil\nreturn nil\nreturn nil\n"
	os.WriteFile(filepath.Join(tmpDir, "dup.go"), []byte(content), 0644)

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
	defer store.Close()

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
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestIndexerConcurrentIndexDirectory(t *testing.T) {
	var reqCount atomic.Int32
	srv := newCountingEmbedServer(4, &reqCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	numFiles := 10
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("file%d.go", i)
		content := fmt.Sprintf("package main\n\nfunc F%d() {}\n", i)
		os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644)
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
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

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
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer failSrv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(failSrv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "c.go"), []byte("package c\n\nfunc C() {}\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root .gitignore ignores *.log
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)

	// src/.gitignore ignores *.tmp
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.tmp\n"), 0644)

	// lib/ has no .gitignore
	os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// Create test files
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log data\n"), 0644)          // ignored by root
	os.WriteFile(filepath.Join(tmpDir, "src", "util.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "cache.tmp"), []byte("temp\n"), 0644)     // ignored by src
	os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("log\n"), 0644)      // ignored by root
	os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore all .gen.go, but un-ignore important.gen.go
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.gen.go\n!important.gen.go\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "schema.gen.go"), []byte("package main\n"), 0644)    // ignored
	os.WriteFile(filepath.Join(tmpDir, "important.gen.go"), []byte("package main\n"), 0644) // NOT ignored

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore the build/ directory
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "build", "deep", "nested"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "build", "deep", "nested", "inner.go"), []byte("package nested\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// .gitignore tries to un-ignore vendor/
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("!vendor/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "vendor"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "vendor", "dep.go"), []byte("package dep\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Path-scoped rule: only ignore generated/ under src/
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("src/generated/\n"), 0644)

	os.MkdirAll(filepath.Join(tmpDir, "src", "generated"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "lib", "generated"), 0755)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "generated", "proto.go"), []byte("package gen\n"), 0644) // ignored
	os.WriteFile(filepath.Join(tmpDir, "lib", "generated", "types.go"), []byte("package gen\n"), 0644) // NOT ignored

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// No .gitignore file
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.py"), []byte("def helper():\n    pass\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// src/.gitignore ignores *.gen.go
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.gen.go\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "schema.gen.go"), []byte("package src\n"), 0644) // ignored by src rule
	os.WriteFile(filepath.Join(tmpDir, "lib", "types.gen.go"), []byte("package lib\n"), 0644)  // NOT ignored
	os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore build/, then un-ignore it — subtree should be walked
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n!build/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore *.log, then un-ignore *.log
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n!*.log\n"), 0644)

	// src/: re-ignore *.log (overrides parent negation)
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.log\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "root.log"), []byte("root log\n"), 0644)        // NOT ignored (root negation)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("src log\n"), 0644) // ignored (nested re-ignore)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// First run: no .gitignore, index everything
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.go"), []byte("package main\n\nfunc util() {}\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalSources != 2 {
		t.Fatalf("expected 2 sources before gitignore, got %d", statsBefore.TotalSources)
	}

	// Second run: add .gitignore that ignores util.go
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("util.go\n"), 0644)

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
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore build/ directory, then try to un-ignore a file inside it.
	// The negation cannot work because SkipDir prevents the walk from reaching the file.
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n!build/keep.go\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "build", "keep.go"), []byte("package build\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "build", "other.go"), []byte("package build\n"), 0644)

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
