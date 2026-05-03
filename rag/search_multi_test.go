package rag

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSearchMultiBasic(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Store chunks with distinct content and embeddings.
	chunks := []Chunk{
		{ID: "c1", Content: "golang concurrency patterns", Source: "main.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c2", Content: "python machine learning", Source: "ml.py", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
		{ID: "c3", Content: "golang error handling", Source: "errors.go", StartLine: 1, EndLine: 5, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.8, 0.0, 0.6},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Query with an embedding similar to c1 and c3, and keyword "golang".
	queryEmb := []float64{0.9, 0.0, 0.1}
	qCtx := QueryContext{
		CurrentFile: "main.go",
		Timestamp:   time.Now(),
	}

	results, err := store.SearchMulti(ctx, queryEmb, "golang", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank highest: best semantic match + keyword match + structural (same file).
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 as top result, got %s", results[0].Chunk.ID)
	}

	// Verify signal breakdowns are populated.
	for i, r := range results {
		if r.Signals == nil {
			t.Errorf("result %d has nil Signals", i)
			continue
		}
		if _, ok := r.Signals["semantic"]; !ok {
			t.Errorf("result %d missing 'semantic' signal", i)
		}
		if _, ok := r.Signals["keyword"]; !ok {
			t.Errorf("result %d missing 'keyword' signal", i)
		}
		if _, ok := r.Signals["temporal"]; !ok {
			t.Errorf("result %d missing 'temporal' signal", i)
		}
		if _, ok := r.Signals["structural"]; !ok {
			t.Errorf("result %d missing 'structural' signal", i)
		}
	}
}

func TestSearchMultiEmptyStore(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	results, err := store.SearchMulti(ctx, []float64{1.0, 0.0}, "test", 5, QueryContext{})
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty store, got %d", len(results))
	}
}

func TestSearchMultiRejectsDimensionMismatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "test content", Source: "test.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{0.1, 0.2, 0.3, 0.4}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	_, err := store.SearchMulti(ctx, []float64{0.1, 0.2, 0.3}, "test", 5, QueryContext{})
	if err == nil {
		t.Fatal("expected dimension-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "dimension mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "dimension mismatch")
	}
}

func TestSearchMultiKeywordBoostAffectsRanking(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two chunks with identical embeddings but different content.
	chunks := []Chunk{
		{ID: "c1", Content: "database optimization query performance", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "unrelated topic discussion", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	// Identical embeddings — semantic signal alone cannot distinguish them.
	embeddings := [][]float64{
		{0.5, 0.5},
		{0.5, 0.5},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	qCtx := QueryContext{Timestamp: time.Now()}
	results, err := store.SearchMulti(ctx, []float64{0.5, 0.5}, "database", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank higher due to keyword match on "database".
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 ranked first (keyword match), got %s", results[0].Chunk.ID)
	}

	// c1 should have a higher keyword signal than c2.
	if results[0].Signals["keyword"] <= results[1].Signals["keyword"] {
		t.Errorf("expected c1 keyword score > c2 keyword score, got %f <= %f",
			results[0].Signals["keyword"], results[1].Signals["keyword"])
	}
}

func TestSearchMultiStructuralBoost(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "function handler", Source: "pkg/handler.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "function handler", Source: "other/handler.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0},
		{1.0, 0.0},
	}
	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	// Query from the same directory as c1.
	qCtx := QueryContext{
		CurrentFile: "pkg/main.go",
		Timestamp:   time.Now(),
	}
	results, err := store.SearchMulti(ctx, []float64{1.0, 0.0}, "handler", 2, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// c1 should rank higher: same directory as current file.
	if results[0].Chunk.ID != "c1" {
		t.Errorf("expected c1 ranked first (structural proximity), got %s", results[0].Chunk.ID)
	}
	if results[0].Signals["structural"] <= results[1].Signals["structural"] {
		t.Errorf("expected c1 structural > c2 structural, got %f <= %f",
			results[0].Signals["structural"], results[1].Signals["structural"])
	}
}

func TestSearchMultiReturnsAllSignals(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "test content", Source: "test.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	if err := store.Store(ctx, chunks, [][]float64{{1.0}}); err != nil {
		t.Fatalf("Store() error: %v", err)
	}

	qCtx := QueryContext{
		CurrentFile: "test.go",
		Timestamp:   time.Now(),
	}
	results, err := store.SearchMulti(ctx, []float64{1.0}, "test", 1, qCtx)
	if err != nil {
		t.Fatalf("SearchMulti() error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	expectedSignals := []string{"semantic", "keyword", "temporal", "structural"}
	for _, sig := range expectedSignals {
		if _, ok := results[0].Signals[sig]; !ok {
			t.Errorf("missing signal %q in results", sig)
		}
	}

	// Semantic should be 1.0 (identical embedding).
	if results[0].Signals["semantic"] != 1.0 {
		t.Errorf("expected semantic=1.0, got %f", results[0].Signals["semantic"])
	}
	// Structural should be 1.0 (same file).
	if results[0].Signals["structural"] != 1.0 {
		t.Errorf("expected structural=1.0, got %f", results[0].Signals["structural"])
	}
}

func TestSearchMultiImplementsInterface(t *testing.T) {
	// Compile-time check is in search_multi.go, but verify at runtime too.
	store := newTestStore(t)
	var ms MultiSignalSearcher = store
	_ = ms
}

func TestComputeRanks(t *testing.T) {
	tests := []struct {
		name   string
		scores []float64
		want   []int
	}{
		{"descending", []float64{0.9, 0.7, 0.5}, []int{1, 2, 3}},
		{"ascending", []float64{0.1, 0.5, 0.9}, []int{3, 2, 1}},
		{"single", []float64{0.5}, []int{1}},
		{"empty", []float64{}, []int{}},
		{"equal", []float64{0.5, 0.5, 0.5}, []int{1, 1, 1}}, // ties get the same rank
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeRanks(tt.scores)
			if len(got) != len(tt.want) {
				t.Fatalf("len(ranks) = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("rank[%d] = %d, want %d", i, got[i], tt.want[i])
				}
			}
		})
	}
}
