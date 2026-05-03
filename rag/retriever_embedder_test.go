package rag

import (
	"context"
	"strings"
	"testing"
)

func TestNewRetrieverWithEmbedder_NilEmbedderRejected(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	r, err := NewRetrieverWithEmbedder(nil, store)
	if err == nil {
		t.Fatal("expected error for nil embedder, got nil")
	}
	if r != nil {
		t.Errorf("expected nil retriever on error, got %v", r)
	}
	if !strings.Contains(err.Error(), "NewRetrieverWithEmbedder") {
		t.Errorf("error = %q, want it to identify the constructor", err.Error())
	}
}

// TestNewRetrieverWithEmbedder_AppliesRetrieverModel verifies WithRetrieverModel
// surfaces the configured model in the embedder call (observable via
// recordingEmbedder), rather than asserting on private struct fields.
func TestNewRetrieverWithEmbedder_AppliesRetrieverModel(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings: [][]float64{{1, 2, 3}},
	}}
	r, err := NewRetrieverWithEmbedder(emb, store, WithRetrieverModel("test-model"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := r.Retrieve(context.Background(), "query", 5); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if emb.calls != 1 {
		t.Errorf("embedder calls = %d, want 1", emb.calls)
	}
	if emb.model != "test-model" {
		t.Errorf("embedder saw model = %q, want %q", emb.model, "test-model")
	}
}
