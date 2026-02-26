package rag

import (
	"context"
	"math"
	"testing"
)

func newTestStore(t *testing.T) VectorStore {
	t.Helper()
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore() error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSQLiteStoreStoreAndSearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "goodbye world", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
		{ID: "c3", Content: "foo bar baz", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Search for vector closest to [1, 0, 0]
	results, err := store.Search(ctx, []float64{1.0, 0.0, 0.0}, 2)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Chunk.ID != "c1" {
		t.Errorf("top result ID = %q, want %q", results[0].Chunk.ID, "c1")
	}
	if math.Abs(results[0].Score-1.0) > 0.001 {
		t.Errorf("top result score = %f, want ~1.0", results[0].Score)
	}
}

func TestSQLiteStoreDeleteBySource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "two", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	store.Store(ctx, chunks, embeddings)

	if err := store.DeleteBySource(ctx, "a.go"); err != nil {
		t.Fatalf("DeleteBySource() error: %v", err)
	}

	stats, _ := store.Stats(ctx)
	if stats.TotalChunks != 1 {
		t.Errorf("expected 1 chunk after delete, got %d", stats.TotalChunks)
	}
}

func TestSQLiteStoreStats(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "one", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "two", Source: "a.go", StartLine: 2, EndLine: 2, Metadata: map[string]string{}},
		{ID: "c3", Content: "three", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}}

	store.Store(ctx, chunks, embeddings)

	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.TotalChunks != 3 {
		t.Errorf("TotalChunks = %d, want 3", stats.TotalChunks)
	}
	if stats.TotalSources != 2 {
		t.Errorf("TotalSources = %d, want 2", stats.TotalSources)
	}
	if stats.EmbeddingDim != 4 {
		t.Errorf("EmbeddingDim = %d, want 4", stats.EmbeddingDim)
	}
}

func TestSQLiteStoreEmptySearch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	results, err := store.Search(ctx, []float64{1.0, 0.0}, 5)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results on empty store, got %d", len(results))
	}
}

func TestSQLiteStoreLengthMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{{ID: "c1", Content: "one", Source: "a.go", Metadata: map[string]string{}}}
	embeddings := [][]float64{{1.0}, {2.0}} // wrong length

	err := store.Store(ctx, chunks, embeddings)
	if err == nil {
		t.Fatal("expected error for length mismatch")
	}
}

func TestCosineSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []float64
		want float64
	}{
		{"identical", []float64{1, 0, 0}, []float64{1, 0, 0}, 1.0},
		{"orthogonal", []float64{1, 0, 0}, []float64{0, 1, 0}, 0.0},
		{"opposite", []float64{1, 0}, []float64{-1, 0}, -1.0},
		{"similar", []float64{1, 1}, []float64{1, 0}, 1.0 / math.Sqrt(2)},
		{"empty", []float64{}, []float64{}, 0.0},
		{"length mismatch", []float64{1}, []float64{1, 2}, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cosineSimilarity(tt.a, tt.b)
			if math.Abs(got-tt.want) > 0.001 {
				t.Errorf("cosineSimilarity() = %f, want %f", got, tt.want)
			}
		})
	}
}

func TestSQLiteStoreUpsert(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Insert initial chunk
	chunks := []Chunk{{ID: "c1", Content: "original", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}}}
	store.Store(ctx, chunks, [][]float64{{1.0, 0.0}})

	// Upsert with same ID, different content
	chunks[0].Content = "updated"
	store.Store(ctx, chunks, [][]float64{{0.0, 1.0}})

	stats, _ := store.Stats(ctx)
	if stats.TotalChunks != 1 {
		t.Errorf("expected 1 chunk after upsert, got %d", stats.TotalChunks)
	}

	results, _ := store.Search(ctx, []float64{0.0, 1.0}, 1)
	if results[0].Chunk.Content != "updated" {
		t.Errorf("expected content %q, got %q", "updated", results[0].Chunk.Content)
	}
}

func TestSQLiteStoreReplaceSourceRollsBackOnInsertFailure(t *testing.T) {
	vs := newTestStore(t)
	store, ok := vs.(*SQLiteStore)
	if !ok {
		t.Fatal("expected *SQLiteStore")
	}
	ctx := context.Background()

	// Seed existing data for the source.
	initial := []Chunk{
		{ID: "c1", Content: "original", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, initial, [][]float64{{1.0}}); err != nil {
		t.Fatalf("seed Store() error: %v", err)
	}

	// NaN fails JSON marshaling, forcing ReplaceSource to error after DELETE has run.
	replacement := []Chunk{
		{ID: "c2", Content: "replacement", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	err := store.ReplaceSource(ctx, "a.go", replacement, [][]float64{{math.NaN()}})
	if err == nil {
		t.Fatal("expected ReplaceSource() to fail on NaN embedding")
	}

	// Existing data should still be present because delete+insert is transactional.
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.TotalChunks != 1 {
		t.Fatalf("expected 1 chunk after failed replace, got %d", stats.TotalChunks)
	}

	results, err := store.Search(ctx, []float64{1.0}, 1)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].Chunk.Content != "original" {
		t.Fatalf("expected original chunk to remain after failed replace, got %#v", results)
	}
}
