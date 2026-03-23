package rag

import (
	"context"
	"math"
	"testing"
)

// newScorerTestStore creates an in-memory SQLite store with the full schema
// (including FTS5 and indexed_at from migration v2). Delegates to newTestStore
// which is defined in sqlite_store_test.go.
func newScorerTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return newTestStore(t)
}

func TestKeywordScorerName(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewKeywordScorer(store.DB())
	if got := scorer.Name(); got != "keyword" {
		t.Errorf("Name() = %q, want %q", got, "keyword")
	}
}

func TestKeywordScorerMatchingChunk(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "the golang gopher is a friendly mascot", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "python snake is another animal", Source: "b.py", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "java coffee beans and enterprise code", Source: "c.java", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "gopher", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if len(scores) != 3 {
		t.Fatalf("len(scores) = %d, want 3", len(scores))
	}

	// "gopher" appears only in chunk c1 — it should score highest.
	if scores[0] <= 0 {
		t.Errorf("scores[0] (matching chunk) = %f, want > 0", scores[0])
	}
	if scores[1] != 0 {
		t.Errorf("scores[1] (non-matching) = %f, want 0", scores[1])
	}
	if scores[2] != 0 {
		t.Errorf("scores[2] (non-matching) = %f, want 0", scores[2])
	}

	// The matching chunk should have the max score of 1.0 (max-normalized).
	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("scores[0] = %f, want 1.0 (max-normalized)", scores[0])
	}
}

func TestKeywordScorerNoMatches(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "goodbye world", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0},
		{0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "xyzzyx", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	for i, score := range scores {
		if score != 0 {
			t.Errorf("scores[%d] = %f, want 0 (no match)", i, score)
		}
	}
}

func TestKeywordScorerEmptyQuery(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "some content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if len(scores) != 1 {
		t.Fatalf("len(scores) = %d, want 1", len(scores))
	}
	if scores[0] != 0 {
		t.Errorf("scores[0] = %f, want 0 for empty query", scores[0])
	}
}

func TestKeywordScorerEmptyChunks(t *testing.T) {
	store := newScorerTestStore(t)

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(context.Background(), nil, "query", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("len(scores) = %d, want 0", len(scores))
	}
}

func TestKeywordScorerNormalizedRange(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	// Create chunks where "function" appears in multiple chunks with
	// different frequencies, ensuring scores are normalized to [0, 1].
	chunks := []Chunk{
		{ID: "c1", Content: "function function function compute", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "function helper utility code", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "no matching terms here at all", Source: "c.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "function", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	for i, score := range scores {
		if score < 0 || score > 1.0 {
			t.Errorf("scores[%d] = %f, want in [0, 1]", i, score)
		}
	}

	// At least one matching chunk should have the maximum score of 1.0.
	var hasMax bool
	for _, score := range scores {
		if math.Abs(score-1.0) < 0.001 {
			hasMax = true
			break
		}
	}
	if !hasMax {
		t.Errorf("no score reached 1.0, scores = %v", scores)
	}

	// Non-matching chunk should score 0.
	if scores[2] != 0 {
		t.Errorf("non-matching chunk scores[2] = %f, want 0", scores[2])
	}
}

func TestKeywordScorerMalformedQuery(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())

	// FTS5 special syntax characters that may cause MATCH to fail.
	malformedQueries := []string{
		`"unclosed quote`,
		`(unbalanced`,
		`col:value AND`,
		`NOT`,
	}

	for _, q := range malformedQueries {
		t.Run(q, func(t *testing.T) {
			scores, err := scorer.ScoreBatch(ctx, chunks, q, nil, QueryContext{})
			if err != nil {
				t.Errorf("ScoreBatch(%q) returned error: %v, want graceful zero scores", q, err)
			}
			if len(scores) != 1 {
				t.Fatalf("len(scores) = %d, want 1", len(scores))
			}
			// Malformed queries should return zero scores (graceful degradation).
			if scores[0] != 0 {
				// Note: some "malformed" queries may actually work in FTS5.
				// We only verify no error is returned.
				t.Logf("scores[0] = %f for query %q (may be valid FTS5)", scores[0], q)
			}
		})
	}
}

func TestKeywordScorerInterfaceCompliance(t *testing.T) {
	store := newScorerTestStore(t)
	var _ SignalScorer = NewKeywordScorer(store.DB())
}
