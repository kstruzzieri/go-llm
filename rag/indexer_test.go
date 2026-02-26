package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
