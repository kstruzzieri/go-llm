package rag

import (
	"context"
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
}

func TestNewRetrieverWithEmbedder_AppliesOptions(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	emb := EmbedderFunc(func(_ context.Context, _ string, _ []string) (EmbedResult, error) {
		return EmbedResult{}, nil
	})
	r, err := NewRetrieverWithEmbedder(emb, store, WithRetrieverModel("test-model"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil retriever")
	}
	if r.model != "test-model" {
		t.Errorf("model = %q, want %q", r.model, "test-model")
	}
}
