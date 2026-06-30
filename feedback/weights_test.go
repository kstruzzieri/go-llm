package feedback

import (
	"context"
	"testing"
	"time"
)

// warmedStore returns a store where chunk-1 has 2 positive signals (clears a
// WarmupSignals=2 gate) and retrieval_count=1, with aggregates recomputed.
func warmedStore(t *testing.T) *SQLiteSignalStore {
	t.Helper()
	ctx := context.Background()
	store := newTestStore(t)
	if err := store.IncrementRetrievalCount(ctx, []string{"chunk-1"}); err != nil {
		t.Fatalf("IncrementRetrievalCount: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCompletionAccepted, 0.8, time.Time{}); err != nil {
		t.Fatalf("InsertSignal 1: %v", err)
	}
	if err := store.InsertSignal(ctx, "r1", "chunk-1", SignalCodeKept, 0.6, time.Time{}); err != nil {
		t.Fatalf("InsertSignal 2: %v", err)
	}
	if err := store.RecomputeAggregates(ctx, 0); err != nil { // lambda 0 => no decay
		t.Fatalf("RecomputeAggregates: %v", err)
	}
	return store
}

func TestWeightReaderWarmed(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t)
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 2, MinRetrievals: 1})

	w, err := r.WeightsBatch(ctx, []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if got := w["chunk-1"]; got < 1.39 || got > 1.41 {
		t.Errorf("chunk-1 weight = %v, want ~1.4", got)
	}
	if w["chunk-2"] != 0 {
		t.Errorf("chunk-2 weight = %v, want 0 (unknown key)", w["chunk-2"])
	}
}

func TestWeightReaderColdStart(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t) // 2 signals total
	// WarmupSignals=5 not met by 2 signals => all zero.
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 5, MinRetrievals: 1})
	w, err := r.WeightsBatch(ctx, []string{"chunk-1"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if w["chunk-1"] != 0 {
		t.Errorf("cold-start weight = %v, want 0", w["chunk-1"])
	}
}

func TestWeightReaderMinRetrievalsGate(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t) // retrieval_count for chunk-1 == 1
	r := NewWeightReader(store, CollectorConfig{WarmupSignals: 2, MinRetrievals: 5})
	w, err := r.WeightsBatch(ctx, []string{"chunk-1"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if w["chunk-1"] != 0 {
		t.Errorf("below-MinRetrievals weight = %v, want 0", w["chunk-1"])
	}
}

func TestWeightReaderParityWithCollector(t *testing.T) {
	ctx := context.Background()
	store := warmedStore(t)
	cfg := CollectorConfig{WarmupSignals: 2, MinRetrievals: 1}

	reader := NewWeightReader(store, cfg)
	collector := NewCollector(store, cfg)
	defer collector.Close()

	keys := []string{"chunk-1", "chunk-2"}
	rw, err := reader.WeightsBatch(ctx, keys)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	cw, err := collector.WeightsBatch(ctx, keys)
	if err != nil {
		t.Fatalf("collector: %v", err)
	}
	for _, k := range keys {
		if rw[k] != cw[k] {
			t.Errorf("parity mismatch for %q: reader=%v collector=%v", k, rw[k], cw[k])
		}
	}
}
