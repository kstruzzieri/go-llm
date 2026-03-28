package feedback

import (
	"context"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestCollector creates a Collector backed by an in-memory SQLite store
// with the given config. The collector is automatically closed on test
// cleanup.
func newTestCollector(t *testing.T, cfg CollectorConfig) *Collector {
	t.Helper()
	store := newTestStore(t)
	c := NewCollector(store, cfg)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestRegisterRetrievalReturnsID(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "test query", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty retrieval ID")
	}
	if len(id) != 32 { // 16 bytes = 32 hex chars
		t.Errorf("id length = %d, want 32", len(id))
	}
}

func TestRegisterRetrievalUniqueIDs(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id1, err := c.RegisterRetrieval(ctx, "q1", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval 1: %v", err)
	}
	id2, err := c.RegisterRetrieval(ctx, "q2", []string{"chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval 2: %v", err)
	}
	if id1 == id2 {
		t.Error("expected unique retrieval IDs")
	}
}

func TestRecordAgainstOpenWindow(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	err = c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
		ChunkKeys:   []string{"chunk-1"},
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestRecordUnknownWindow(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	err := c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: "nonexistent",
		ChunkKeys:   []string{"chunk-1"},
	})
	if err == nil {
		t.Error("expected error for unknown retrieval ID")
	}
}

func TestRecordMissingRetrievalID(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	err := c.Record(ctx, Signal{
		Kind:      SignalCompletionAccepted,
		ChunkKeys: []string{"chunk-1"},
	})
	if err == nil {
		t.Error("expected error for empty retrieval ID")
	}
}

func TestRecordBroadcastsToAllChunks(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	// Record with empty ChunkKeys should broadcast to all chunks.
	err = c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Verify signal count: 3 signals (one per chunk).
	count, err := c.store.SignalCount(ctx)
	if err != nil {
		t.Fatalf("SignalCount: %v", err)
	}
	if count != 3 {
		t.Errorf("SignalCount = %d, want 3", count)
	}
}

func TestWeightsColdStart(t *testing.T) {
	// WarmupSignals=100 means we need at least 100 signals before weights
	// are non-zero.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 100, MinRetrievals: 1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights during warmup = %f, want 0", w)
	}
}

func TestWeightsBelowMinRetrievals(t *testing.T) {
	// Set warmup to 0 so cold-start does not interfere, but require 5
	// retrievals.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 5})
	ctx := context.Background()

	// Register only 1 retrieval (below MinRetrievals=5).
	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Force recompute so weighted_score is populated.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights below MinRetrievals = %f, want 0", w)
	}
}

func TestWeightsAboveThresholds(t *testing.T) {
	// Warmup=0 and MinRetrievals=1 so a single retrieval+signal suffices.
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Force recompute.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// Single recent signal of strength 0.8, decay negligible.
	if w < 0.7 || w > 0.9 {
		t.Errorf("Weights = %f, expected ~0.8", w)
	}
}

func TestWeightsBatch(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}); err != nil {
		t.Fatalf("Record chunk-1: %v", err)
	}
	if err := c.Record(ctx, Signal{Kind: SignalCompletionRejected, RetrievalID: id, ChunkKeys: []string{"chunk-2"}}); err != nil {
		t.Fatalf("Record chunk-2: %v", err)
	}

	// Force recompute.
	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	weights, err := c.WeightsBatch(ctx, []string{"chunk-1", "chunk-2", "chunk-3"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}

	if len(weights) != 3 {
		t.Fatalf("WeightsBatch returned %d entries, want 3", len(weights))
	}

	if weights["chunk-1"] < 0.7 || weights["chunk-1"] > 0.9 {
		t.Errorf("chunk-1 weight = %f, expected ~0.8", weights["chunk-1"])
	}
	if weights["chunk-2"] > -0.7 || weights["chunk-2"] < -0.9 {
		t.Errorf("chunk-2 weight = %f, expected ~-0.8", weights["chunk-2"])
	}
	if weights["chunk-3"] != 0 {
		t.Errorf("chunk-3 weight = %f, expected 0", weights["chunk-3"])
	}
}

func TestWeightsBatchColdStart(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 100, MinRetrievals: 1})
	ctx := context.Background()

	weights, err := c.WeightsBatch(ctx, []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("WeightsBatch: %v", err)
	}
	for k, w := range weights {
		if w != 0 {
			t.Errorf("chunk %q weight = %f during warmup, want 0", k, w)
		}
	}
}

func TestCollectorClose(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{})
	// Close should not panic or hang.
	c.Close()
}

func TestRecordCustomStrength(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.1})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	// Use a custom strength instead of the default.
	if err := c.Record(ctx, Signal{
		Kind:        SignalCompletionAccepted,
		RetrievalID: id,
		ChunkKeys:   []string{"chunk-1"},
		Strength:    0.5,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if err := c.store.RecomputeAggregates(ctx, c.config.DecayLambda); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// Custom strength of 0.5 should be used.
	if w < 0.4 || w > 0.6 {
		t.Errorf("Weights = %f, expected ~0.5", w)
	}
}

func TestMultipleSignalsSameChunk(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1, DecayLambda: 0.0})
	ctx := context.Background()

	id, err := c.RegisterRetrieval(ctx, "q", []string{"chunk-1"})
	if err != nil {
		t.Fatalf("RegisterRetrieval: %v", err)
	}

	signals := []Signal{
		{Kind: SignalCompletionAccepted, RetrievalID: id, ChunkKeys: []string{"chunk-1"}}, // +0.8
		{Kind: SignalCodeKept, RetrievalID: id, ChunkKeys: []string{"chunk-1"}},           // +0.6
		{Kind: SignalFileOpened, RetrievalID: id, ChunkKeys: []string{"chunk-1"}},         // +0.3
	}
	for _, sig := range signals {
		if err := c.Record(ctx, sig); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	if err := c.store.RecomputeAggregates(ctx, 0.0); err != nil {
		t.Fatalf("RecomputeAggregates: %v", err)
	}

	w, err := c.Weights(ctx, "chunk-1")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	// With decay=0, score = 0.8 + 0.6 + 0.3 = 1.7 exactly.
	if w < 1.6 || w > 1.8 {
		t.Errorf("Weights = %f, expected ~1.7", w)
	}
}

func TestWeightsNonExistentChunk(t *testing.T) {
	c := newTestCollector(t, CollectorConfig{WarmupSignals: 0, MinRetrievals: 1})
	ctx := context.Background()

	w, err := c.Weights(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("Weights: %v", err)
	}
	if w != 0 {
		t.Errorf("Weights = %f, want 0 for nonexistent chunk", w)
	}
}
