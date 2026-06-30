package feedback

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var errTestBatchTooLarge = errors.New("test batch too large")

type batchingStore struct {
	SignalStore
	signalCount int
	maxBatch    int
	calls       [][]string
	aggs        map[string]Aggregate
}

func (s *batchingStore) SignalCount(ctx context.Context) (int, error) {
	return s.signalCount, nil
}

func (s *batchingStore) GetAggregatesBatch(ctx context.Context, chunkKeys []string) (map[string]Aggregate, error) {
	if len(chunkKeys) > s.maxBatch {
		return nil, errTestBatchTooLarge
	}
	s.calls = append(s.calls, append([]string(nil), chunkKeys...))
	out := make(map[string]Aggregate, len(chunkKeys))
	for _, k := range chunkKeys {
		if agg, ok := s.aggs[k]; ok {
			out[k] = agg
		}
	}
	return out, nil
}

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

func TestWeightReaderBatchesAggregateLookups(t *testing.T) {
	const keyCount = 1001
	keys := make([]string, keyCount)
	aggs := make(map[string]Aggregate, keyCount)
	for i := range keys {
		keys[i] = fmt.Sprintf("chunk-%04d", i)
		aggs[keys[i]] = Aggregate{WeightedScore: float64(i), RetrievalCount: 1}
	}
	store := &batchingStore{signalCount: 1, maxBatch: 900, aggs: aggs}
	reader := NewWeightReader(store, CollectorConfig{WarmupSignals: 1, MinRetrievals: 1})

	got, err := reader.WeightsBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	if len(store.calls) != 2 {
		t.Fatalf("GetAggregatesBatch calls = %d, want 2", len(store.calls))
	}
	for i, call := range store.calls {
		if len(call) > store.maxBatch {
			t.Fatalf("call %d len = %d, want <= %d", i, len(call), store.maxBatch)
		}
	}
	if got["chunk-1000"] != 1000 {
		t.Fatalf("chunk-1000 weight = %v, want 1000", got["chunk-1000"])
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
