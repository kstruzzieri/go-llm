package provider

import (
	"context"
	"errors"
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
	if len(cap.snapshot()) != 1 {
		t.Errorf("warnFeedbackReadOnce fired %d times, want 1", len(cap.snapshot()))
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
