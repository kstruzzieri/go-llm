package provider

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestAdjustFeedbackScoreLowScoredCountReturnsNeutral(t *testing.T) {
	agg := Aggregate{Score: 0.9, ScoredCount: 2}
	got := adjustFeedbackScore(agg)
	if got != 0.5 {
		t.Errorf("ScoredCount=%d: adjusted = %v, want neutral 0.5", agg.ScoredCount, got)
	}
}

func TestAdjustFeedbackScoreShrinksTowardNeutral(t *testing.T) {
	// ScoredCount = priorSamples should shrink halfway (confidence == 0.5).
	agg := Aggregate{Score: 1.0, ScoredCount: feedbackPriorSamples}
	got := adjustFeedbackScore(agg)
	want := 0.5 + (1.0-0.5)*0.5
	if abs(got-want) > 1e-9 {
		t.Errorf("adjusted = %v, want %v", got, want)
	}
}

func TestAdjustFeedbackScoreLargeScoredCountApproachesRaw(t *testing.T) {
	agg := Aggregate{Score: 0.9, ScoredCount: 1000}
	got := adjustFeedbackScore(agg)
	if got < 0.85 {
		t.Errorf("ScoredCount=%d should approach raw 0.9, got %v", agg.ScoredCount, got)
	}
}

// TestAdjustFeedbackScoreAtFloor documents the intentional discontinuity
// at ScoredCount == feedbackMinScoredCount: count=2 returns neutral 0.5
// regardless of raw, count=3 jumps to the confidence-shrunk value. For
// raw < 0.5 this means the third sample DROPS adjusted below 0.5 — a
// known design trade-off (one new sample is enough to express signal
// direction, but not enough to express magnitude).
func TestAdjustFeedbackScoreAtFloor(t *testing.T) {
	rawLow := 0.1
	belowFloor := adjustFeedbackScore(Aggregate{Score: rawLow, ScoredCount: feedbackMinScoredCount - 1})
	if belowFloor != 0.5 {
		t.Errorf("below floor: adjusted = %v, want 0.5", belowFloor)
	}
	atFloor := adjustFeedbackScore(Aggregate{Score: rawLow, ScoredCount: feedbackMinScoredCount})
	confidence := float64(feedbackMinScoredCount) / float64(feedbackMinScoredCount+feedbackPriorSamples)
	want := 0.5 + (rawLow-0.5)*confidence
	if abs(atFloor-want) > 1e-9 {
		t.Errorf("at floor: adjusted = %v, want %v", atFloor, want)
	}
	if atFloor >= 0.5 {
		t.Errorf("at floor with raw=%v should be < 0.5, got %v (signal-direction property lost)", rawLow, atFloor)
	}
}

// TestAdjustFeedbackScoreRejectsPathologicalAggregates guards against
// third-party RoutingFeedbackStore implementations returning NaN, Inf,
// or out-of-range scores. Without the guard, NaN propagates through
// computeWeightedScore (weightedSum += NaN * w) and poisons every
// candidate's composite, making sort.SliceStable order-dependent
// garbage. All pathological inputs fall back to neutral 0.5.
func TestAdjustFeedbackScoreRejectsPathologicalAggregates(t *testing.T) {
	cases := []struct {
		name  string
		score float64
	}{
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
		{"below_range", -0.5},
		{"above_range", 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adjustFeedbackScore(Aggregate{Score: tc.score, ScoredCount: 50})
			if got != 0.5 {
				t.Errorf("score=%v: adjusted = %v, want neutral 0.5", tc.score, got)
			}
		})
	}
}

func TestBuildFeedbackSnapshotOffModeInactive(t *testing.T) {
	router, _ := setupTestRouter(t)
	router.routingFeedback = mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router.feedbackScoringMode = FeedbackScoringOff

	snap := router.buildFeedbackSnapshot(context.Background(), []*ModelProfile{{Key: ModelKey{Provider: "test", Model: "qwen3:8b"}}}, "chat")
	if snap == nil {
		t.Fatalf("buildFeedbackSnapshot returned nil; expected non-nil inactive snapshot")
	}
	if snap.active {
		t.Errorf("Off mode snapshot.active = true, want false")
	}
}

func TestBuildFeedbackSnapshotShadowReadsStore(t *testing.T) {
	router, _ := setupTestRouter(t)
	rf := mustNewRoutingFeedback(t, MemoryStoreConfig{})
	router.routingFeedback = rf
	router.feedbackScoringMode = FeedbackScoringShadow

	k := FeedbackKey{Provider: "test", Model: "qwen3:8b", UseCase: "chat"}
	// Seed enough Success signals to exceed feedbackMinScoredCount.
	for i := 0; i < feedbackMinScoredCount+1; i++ {
		if err := rf.Record(context.Background(), k, FeedbackSignal{Kind: RoutingSignalSuccess, At: time.Now()}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	snap := router.buildFeedbackSnapshot(context.Background(),
		[]*ModelProfile{{Key: ModelKey{Provider: "test", Model: "qwen3:8b"}}}, "chat")
	if snap == nil || !snap.active {
		t.Fatalf("Shadow mode snapshot inactive or nil: %+v", snap)
	}
	cf := snap.lookup(FeedbackKey{Provider: "test", Model: "qwen3:8b", UseCase: "chat"})
	if cf == nil {
		t.Fatalf("snapshot has no entry for seeded key")
	}
	if cf.raw.Score <= 0.5 {
		t.Errorf("raw score for seeded successes = %v, want > 0.5", cf.raw.Score)
	}
}

func TestBuildFeedbackSnapshotFailOpenOnStoreError(t *testing.T) {
	router, _ := setupTestRouter(t)
	router.routingFeedback = NewRoutingFeedback(&flakyStore{err: errors.New("disk full")})
	router.feedbackScoringMode = FeedbackScoringEnforce

	cap := &capturingLogger{}
	router.feedbackLogger = cap

	snap := router.buildFeedbackSnapshot(context.Background(),
		[]*ModelProfile{{Key: ModelKey{Provider: "test", Model: "qwen3:8b"}}}, "chat")
	if snap == nil {
		t.Fatalf("nil snapshot on read error; want non-nil inactive snapshot")
	}
	if snap.active {
		t.Errorf("snapshot.active = true after fail-open; want false")
	}
	// Tighten the latch invariant: every fail-open state mutation must be present
	// so a later chain step's readCandidates / lookup short-circuits correctly.
	if !snap.failed {
		t.Errorf("snapshot.failed = false after fail-open; want true (so further readCandidates is a no-op)")
	}
	if snap.status != feedbackSnapshotStatusReadError {
		t.Errorf("snapshot.status = %q after fail-open; want %q", snap.status, feedbackSnapshotStatusReadError)
	}
	if snap.byKey != nil {
		t.Errorf("snapshot.byKey = %+v after fail-open; want nil (cleared so stale entries cannot leak)", snap.byKey)
	}
	if len(cap.snapshot()) != 1 {
		t.Errorf("warnFeedbackReadOnce fired %d times, want 1", len(cap.snapshot()))
	}
}

// TestSnapshotReadCandidatesAfterLatchIsNoOp covers the second
// fail-open reachable path: buildFeedbackSnapshot succeeded with an
// empty initial set, then a follow-up readCandidates extension fails.
// The latch must hold for subsequent extensions, no second warning, no
// stale entries. This is the chain-routing invariant Task 8 will
// depend on — landing the assertion now prevents a regression during
// the wiring change.
func TestSnapshotReadCandidatesAfterLatchIsNoOp(t *testing.T) {
	router, _ := setupTestRouter(t)
	router.routingFeedback = NewRoutingFeedback(&flakyStore{err: errors.New("disk full")})
	router.feedbackScoringMode = FeedbackScoringEnforce
	cap := &capturingLogger{}
	router.feedbackLogger = cap

	snap := router.buildFeedbackSnapshot(context.Background(), nil, "chat")
	if !snap.active {
		t.Fatalf("nil-initial-set snapshot inactive after construction; want active with empty byKey")
	}

	// First extension triggers fail-open + warning.
	first := []*ModelProfile{{Key: ModelKey{Provider: "test", Model: "qwen3:8b"}}}
	snap.readCandidates(context.Background(), router, first, "chat")
	if snap.active || !snap.failed || snap.status != feedbackSnapshotStatusReadError {
		t.Fatalf("first readCandidates did not latch: active=%v failed=%v status=%q",
			snap.active, snap.failed, snap.status)
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("first extension: warning count = %d, want 1", got)
	}

	// Second extension with a DIFFERENT key must be a no-op.
	second := []*ModelProfile{{Key: ModelKey{Provider: "fallback", Model: "qwen3:8b"}}}
	snap.readCandidates(context.Background(), router, second, "chat")
	if snap.active {
		t.Errorf("second extension re-activated snapshot; want stays inactive")
	}
	if got := len(cap.snapshot()); got != 1 {
		t.Errorf("second extension fired %d warnings, want still 1 (latch must silence)", got)
	}
	if snap.lookup(FeedbackKey{Provider: "fallback", Model: "qwen3:8b", UseCase: "chat"}) != nil {
		t.Errorf("inactive snapshot returned non-nil lookup; want nil")
	}
}

// mustNewRoutingFeedback returns a RoutingFeedback wrapping a fresh
// MemoryStore. Test helper to keep the setup terse.
func mustNewRoutingFeedback(t *testing.T, cfg MemoryStoreConfig) *RoutingFeedback {
	t.Helper()
	store, err := NewMemoryStore(cfg)
	if err != nil {
		t.Fatalf("NewMemoryStore: %v", err)
	}
	return NewRoutingFeedback(store)
}

// flakyStore is a RoutingFeedbackStore whose Get always returns the
// configured error. Record/RecordBatch succeed so test setup can still
// seed signals through other channels.
type flakyStore struct{ err error }

func (f *flakyStore) Get(_ context.Context, _ FeedbackKey) (Aggregate, error) {
	return Aggregate{}, f.err
}
func (f *flakyStore) Record(_ context.Context, _ FeedbackKey, _ FeedbackSignal) error {
	return nil
}
func (f *flakyStore) RecordBatch(_ context.Context, _ []FeedbackItem) error { return nil }

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
