package rag

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestTemporalScorerName(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewTemporalScorer(store.DB(), 0)
	if got := scorer.Name(); got != "temporal" {
		t.Errorf("Name() = %q, want %q", got, "temporal")
	}
}

func TestTemporalScorerDefaultHalfLife(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewTemporalScorer(store.DB(), 0)
	if scorer.halfLife != 604800 {
		t.Errorf("halfLife = %f, want 604800 (7 days)", scorer.halfLife)
	}
}

func TestTemporalScorerNegativeHalfLife(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewTemporalScorer(store.DB(), -100)
	if scorer.halfLife != 604800 {
		t.Errorf("halfLife = %f, want 604800 (default for negative input)", scorer.halfLife)
	}
}

func TestTemporalScorerRecentChunksScoreHigher(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	// Store chunks through the store API first.
	chunks := []Chunk{
		{ID: "c1", Content: "old content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "recent content", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "newest content", Source: "c.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Set specific indexed_at timestamps via SQL.
	now := time.Now().Unix()
	oneDay := int64(86400)
	sevenDays := int64(604800)

	// c1: 30 days ago, c2: 1 day ago, c3: now
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now-30*oneDay, "c1"); err != nil {
		t.Fatalf("update indexed_at c1: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now-oneDay, "c2"); err != nil {
		t.Fatalf("update indexed_at c2: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now, "c3"); err != nil {
		t.Fatalf("update indexed_at c3: %v", err)
	}

	scorer := NewTemporalScorer(store.DB(), float64(sevenDays))
	qCtx := QueryContext{Timestamp: time.Unix(now, 0)}
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if len(scores) != 3 {
		t.Fatalf("len(scores) = %d, want 3", len(scores))
	}

	// Newest chunk (c3, age=0) should score 1.0.
	if math.Abs(scores[2]-1.0) > 0.001 {
		t.Errorf("scores[2] (newest) = %f, want 1.0", scores[2])
	}

	// Recent chunk (c2, age=1 day) should score higher than old chunk (c1, age=30 days).
	if scores[1] <= scores[0] {
		t.Errorf("scores[1] (1 day old, %f) should be > scores[0] (30 days old, %f)", scores[1], scores[0])
	}

	// All scores should be in [0, 1].
	for i, score := range scores {
		if score < 0 || score > 1.0 {
			t.Errorf("scores[%d] = %f, want in [0, 1]", i, score)
		}
	}
}

func TestTemporalScorerChunkIndexedNow(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "fresh content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	now := time.Now().Unix()
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now, "c1"); err != nil {
		t.Fatalf("update indexed_at: %v", err)
	}

	scorer := NewTemporalScorer(store.DB(), 604800)
	qCtx := QueryContext{Timestamp: time.Unix(now, 0)}
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("score = %f, want 1.0 for chunk indexed at query time", scores[0])
	}
}

func TestTemporalScorerVeryOldChunks(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "ancient content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	now := time.Now().Unix()
	halfLife := int64(604800) // 7 days
	// Set indexed_at to 10 half-lives ago. Score should be 2^(-10) ~ 0.001.
	veryOld := now - 10*halfLife
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, veryOld, "c1"); err != nil {
		t.Fatalf("update indexed_at: %v", err)
	}

	scorer := NewTemporalScorer(store.DB(), float64(halfLife))
	qCtx := QueryContext{Timestamp: time.Unix(now, 0)}
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	expected := math.Pow(2, -10.0)
	if math.Abs(scores[0]-expected) > 0.0001 {
		t.Errorf("score = %f, want %f for chunk 10 half-lives old", scores[0], expected)
	}

	// Score should be very close to 0.
	if scores[0] > 0.01 {
		t.Errorf("score = %f, want < 0.01 for very old chunk", scores[0])
	}
}

func TestTemporalScorerUsesMaxTimestampWhenNoQueryTime(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "old content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "new content", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0, 0.0}, {0.0, 1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	halfLife := int64(604800)
	now := time.Now().Unix()
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now-halfLife, "c1"); err != nil {
		t.Fatalf("update indexed_at c1: %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now, "c2"); err != nil {
		t.Fatalf("update indexed_at c2: %v", err)
	}

	scorer := NewTemporalScorer(store.DB(), float64(halfLife))
	// Zero-value Timestamp: scorer should use max(indexed_at) as reference.
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	// c2 is at "now" (the max), so it should score 1.0.
	if math.Abs(scores[1]-1.0) > 0.001 {
		t.Errorf("scores[1] (newest) = %f, want 1.0", scores[1])
	}

	// c1 is one half-life ago, so it should score ~0.5.
	if math.Abs(scores[0]-0.5) > 0.05 {
		t.Errorf("scores[0] (one half-life old) = %f, want ~0.5", scores[0])
	}
}

func TestTemporalScorerEmptyChunks(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewTemporalScorer(store.DB(), 0)

	scores, err := scorer.ScoreBatch(context.Background(), nil, "", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if scores != nil {
		t.Errorf("scores = %v, want nil for empty chunks", scores)
	}
}

func TestTemporalScorerCustomHalfLife(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	now := time.Now().Unix()
	customHalfLife := float64(3600) // 1 hour
	// Set indexed_at to exactly 1 half-life ago.
	if _, err := store.DB().ExecContext(ctx, `UPDATE chunks SET indexed_at = ? WHERE id = ?`, now-3600, "c1"); err != nil {
		t.Fatalf("update indexed_at: %v", err)
	}

	scorer := NewTemporalScorer(store.DB(), customHalfLife)
	qCtx := QueryContext{Timestamp: time.Unix(now, 0)}
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, qCtx)
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	// Exactly 1 half-life ago: score = 2^(-1) = 0.5
	if math.Abs(scores[0]-0.5) > 0.001 {
		t.Errorf("score = %f, want 0.5 for chunk exactly 1 half-life old", scores[0])
	}
}

func TestTemporalScorerInterfaceCompliance(t *testing.T) {
	store := newScorerTestStore(t)
	var _ SignalScorer = NewTemporalScorer(store.DB(), 0)
}
