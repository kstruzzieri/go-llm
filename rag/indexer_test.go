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

