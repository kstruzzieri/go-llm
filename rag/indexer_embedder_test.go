package rag

import (
	"context"
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
	if err.Error() == "" {
		t.Errorf("expected non-empty error, got %q", err)
	}
}

func TestNewIndexerWithEmbedder_AppliesOptions(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := EmbedderFunc(func(_ context.Context, _ string, _ []string) (EmbedResult, error) {
		return EmbedResult{}, nil
	})
	idx, err := NewIndexerWithEmbedder(emb, store, WithEmbeddingModel("test-model"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if idx == nil {
		t.Fatal("expected non-nil indexer")
	}
	if idx.model != "test-model" {
		t.Errorf("model = %q, want %q", idx.model, "test-model")
	}
}
