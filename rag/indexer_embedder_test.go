package rag

import (
	"context"
	"strings"
	"testing"
)

func TestNewIndexerWithEmbedder_NilEmbedderRejected(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	idx, err := NewIndexerWithEmbedder(nil, store)
	if err == nil {
		t.Fatal("expected error for nil embedder, got nil")
	}
	if idx != nil {
		t.Errorf("expected nil indexer on error, got %v", idx)
	}
	if !strings.Contains(err.Error(), "NewIndexerWithEmbedder") {
		t.Errorf("error = %q, want it to identify the constructor", err.Error())
	}
}

// TestNewIndexerWithEmbedder_AppliesEmbeddingModel verifies WithEmbeddingModel
// surfaces the configured model in the embedder call (observable via
// recordingEmbedder), rather than asserting on private struct fields.
func TestNewIndexerWithEmbedder_AppliesEmbeddingModel(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings: [][]float64{{1, 2, 3}},
	}}
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("test-model"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	idx.chunker = chunkerFunc(func(path string, content string) ([]Chunk, error) {
		return []Chunk{{ID: "c", Content: content, Source: path, StartLine: 1, EndLine: 1}}, nil
	})
	tmp := t.TempDir()
	path := tmp + "/f.go"
	if err := writeFileImpl(path, "package x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := idx.IndexFile(context.Background(), path); err != nil {
		t.Fatalf("IndexFile: %v", err)
	}

	if emb.calls != 1 {
		t.Errorf("embedder calls = %d, want 1", emb.calls)
	}
	if emb.model != "test-model" {
		t.Errorf("embedder saw model = %q, want %q", emb.model, "test-model")
	}
}
