package feedback

import (
	"context"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteSignalStore {
	t.Helper()
	db := openTestDB(t)
	store, err := NewSignalStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSignalStore: %v", err)
	}
	return store
}

func TestInsertRetrievalAndSignal(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "test query", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}

	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 1 {
		t.Errorf("SignalCount = %d, want 1", count)
	}
}

func TestSignalCountEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SignalCount = %d, want 0", count)
	}
}

func TestGetAggregate(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Non-existent chunk returns zero aggregate.
	agg, err := store.GetAggregate(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	if agg.WeightedScore != 0 || agg.RetrievalCount != 0 {
		t.Errorf("expected zero aggregate, got %+v", agg)
	}

	// Insert a signal to create an aggregate row.
	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	agg, err = store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	// Signal creates an aggregate with last_signal_at set, but weighted_score
	// is only updated by RecomputeAggregates. retrieval_count is 0 until
	// IncrementRetrievalCount is called.
	if agg.RetrievalCount != 0 {
		t.Errorf("retrieval_count = %d, want 0", agg.RetrievalCount)
	}
}

func TestGetAggregatesBatch(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Empty batch returns empty map.
	result, err := store.GetAggregatesBatch(ctx, nil)
	if err != nil {
		t.Fatalf("GetAggregatesBatch empty: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}

	// Set up some data.
	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1", "chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-2", SignalCodeUndone, -0.7); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	result, err = store.GetAggregatesBatch(ctx, []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("GetAggregatesBatch: %v", err)
	}

	// chunk-1 and chunk-2 exist, chunk-3 does not.
	if len(result) != 2 {
		t.Errorf("expected 2 results, got %d", len(result))
	}
	if _, ok := result["chunk-1"]; !ok {
		t.Error("chunk-1 not in results")
	}
	if _, ok := result["chunk-2"]; !ok {
		t.Error("chunk-2 not in results")
	}
}

func TestIncrementRetrievalCount(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	keys := []string{"chunk-1", "chunk-2"}
	if err := store.IncrementRetrievalCount(ctx, keys); err != nil {
		t.Fatalf("IncrementRetrievalCount: %v", err)
	}
	if err := store.IncrementRetrievalCount(ctx, keys); err != nil {
		t.Fatalf("IncrementRetrievalCount (2nd): %v", err)
	}

	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	if agg.RetrievalCount != 2 {
		t.Errorf("retrieval_count = %d, want 2", agg.RetrievalCount)
	}

	// Empty slice is a no-op.
	if err := store.IncrementRetrievalCount(ctx, nil); err != nil {
		t.Fatalf("IncrementRetrievalCount nil: %v", err)
	}
}

func TestRecomputeAggregates(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6); err != nil {
		t.Fatalf("InsertSignal (2nd): %v", err)
	}

	if err := store.RecomputeAggregates(ctx, 0.1); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	agg, err := store.GetAggregate(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("GetAggregate: %v", err)
	}
	// Both signals are very recent so decay is negligible.
	// Score should be approximately 0.8 + 0.6 = 1.4.
	if agg.WeightedScore < 1.3 || agg.WeightedScore > 1.5 {
		t.Errorf("weighted_score = %f, expected ~1.4", agg.WeightedScore)
	}
}

func TestRecomputeAggregatesEmpty(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Recompute with no signals should be a no-op.
	if err := store.RecomputeAggregates(ctx, 0.1); err != nil {
		t.Fatalf("RecomputeAggregates (empty): %v", err)
	}
}

func TestPruneSignals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	// Prune with a future cutoff should delete the signal.
	cutoff := time.Now().Add(time.Hour)
	n, err := store.PruneSignals(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneSignals: %v", err)
	}
	if n != 1 {
		t.Errorf("PruneSignals deleted %d, want 1", n)
	}

	count, err := store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 0 {
		t.Errorf("SignalCount = %d, want 0 after prune", count)
	}
}

func TestPruneSignalsNone(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	if err := store.InsertRetrieval(ctx, "r1", "q", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	// Prune with a past cutoff should delete nothing.
	cutoff := time.Now().Add(-time.Hour)
	n, err := store.PruneSignals(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneSignals: %v", err)
	}
	if n != 0 {
		t.Errorf("PruneSignals deleted %d, want 0", n)
	}
}

func TestPruneRetrievals(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	// Insert two retrievals, only one with signals.
	if err := store.InsertRetrieval(ctx, "r1", "q1", []string{"chunk-1"}); err != nil {
		t.Fatalf("InsertRetrieval r1: %v", err)
	}
	if err := store.InsertRetrieval(ctx, "r2", "q2", []string{"chunk-2"}); err != nil {
		t.Fatalf("InsertRetrieval r2: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8); err != nil {
		t.Fatalf("InsertSignal: %v", err)
	}

	n, err := store.PruneRetrievals(ctx)
	if err != nil {
		t.Fatalf("PruneRetrievals: %v", err)
	}
	if n != 1 {
		t.Errorf("PruneRetrievals deleted %d, want 1", n)
	}
}
