package rag

import (
	"context"
	"strings"
	"testing"
)

func TestSQLiteStore_Search_RejectsDimensionMismatch(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()

	// Seed a chunk with a 4-dimensional embedding.
	chunks := []Chunk{{ID: "c1", Content: "x", Source: "f.go", StartLine: 1, EndLine: 1}}
	embeddings := [][]float64{{0.1, 0.2, 0.3, 0.4}}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	// Query with a 3-dimensional vector — must error, not silently return zero score.
	_, err = store.Search(ctx, []float64{0.1, 0.2, 0.3}, 5)
	if err == nil {
		t.Fatal("expected dimension-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "dimension mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "dimension mismatch")
	}
}
