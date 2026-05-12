package rag

import (
	"context"
	"errors"
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

func TestNewRetrieverWithEmbedder_NilStoreRejected(t *testing.T) {
	emb := &recordingEmbedder{}

	tests := []struct {
		name  string
		store VectorStore
	}{
		{name: "nil interface", store: nil},
		{name: "typed nil", store: (*SQLiteStore)(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRetrieverWithEmbedder(emb, tt.store)
			if err == nil {
				t.Fatal("expected error for nil store, got nil")
			}
			if r != nil {
				t.Errorf("expected nil retriever on error, got %v", r)
			}
			if !strings.Contains(err.Error(), "store") {
				t.Errorf("error = %q, want it to mention store", err.Error())
			}
		})
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

func TestRetriever_VectorSpaceMismatchFailsClosed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "stored", Source: "s.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "s.go", chunks, [][]float64{{1, 0}}, "hash", "ollama/indexed"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	emb := &recordingEmbedder{result: EmbedResult{
		Embeddings:    [][]float64{{1, 0}},
		Provider:      "ollama",
		Model:         "query",
		VectorSpaceID: "ollama/query",
	}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrVectorSpaceMismatch) {
		t.Fatalf("Retrieve error = %v, want ErrVectorSpaceMismatch", err)
	}
}

func TestRetriever_MissingQueryVectorSpaceFailsClosed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "stored", Source: "s.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, "s.go", chunks, [][]float64{{1, 0}}, "hash", "ollama/indexed"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	emb := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	r, err := NewRetrieverWithEmbedder(emb, store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}

	_, err = r.Retrieve(ctx, "question", 1)
	if !errors.Is(err, ErrVectorSpaceMismatch) {
		t.Fatalf("Retrieve error = %v, want ErrVectorSpaceMismatch", err)
	}
}

func TestRetriever_MixedCorpusVectorSpacesFailClosed(t *testing.T) {
	for _, tt := range []struct {
		name string
		ids  []string
	}{
		{name: "two known", ids: []string{"A", "B"}},
		{name: "known plus legacy", ids: []string{"A", ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			for i, id := range tt.ids {
				insertVSIDRow(t, store, string(rune('a'+i)), id)
			}

			emb := &recordingEmbedder{result: EmbedResult{
				Embeddings:    [][]float64{{1}},
				Provider:      "fake",
				Model:         "A",
				VectorSpaceID: "A",
			}}
			r, err := NewRetrieverWithEmbedder(emb, store)
			if err != nil {
				t.Fatalf("NewRetrieverWithEmbedder: %v", err)
			}

			_, err = r.Retrieve(ctx, "question", 1)
			if !errors.Is(err, ErrCorpusMixedVectorSpaces) {
				t.Fatalf("Retrieve error = %v, want ErrCorpusMixedVectorSpaces", err)
			}
		})
	}
}
