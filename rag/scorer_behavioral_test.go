package rag

import (
	"context"
	"errors"
	"testing"
)

type fakeWeighter struct {
	weights map[string]float64
	err     error
	gotKeys []string
}

func (f *fakeWeighter) WeightsBatch(ctx context.Context, keys []string) (map[string]float64, error) {
	f.gotKeys = keys
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]float64, len(keys))
	for _, k := range keys {
		out[k] = f.weights[k]
	}
	return out, nil
}

func TestBehavioralScorerUsesStableKey(t *testing.T) {
	fw := &fakeWeighter{weights: map[string]float64{"sk1": 0.7}}
	s := NewBehavioralScorer(fw)
	chunks := []Chunk{
		{ID: "c1", StableKey: "sk1"},
		{ID: "c2", StableKey: ""}, // empty => neutral, never queried
	}
	scores, err := s.ScoreBatch(context.Background(), chunks, "q", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if scores[0] != 0.7 {
		t.Errorf("scores[0] = %v, want 0.7", scores[0])
	}
	if scores[1] != 0 {
		t.Errorf("scores[1] = %v, want 0 (empty StableKey)", scores[1])
	}
	for _, k := range fw.gotKeys {
		if k == "" {
			t.Fatal("empty string was passed as a weighter key")
		}
	}
}

func TestBehavioralScorerFailsOpen(t *testing.T) {
	fw := &fakeWeighter{err: errors.New("store unavailable")}
	s := NewBehavioralScorer(fw)
	chunks := []Chunk{{ID: "c1", StableKey: "sk1"}}
	scores, err := s.ScoreBatch(context.Background(), chunks, "q", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch returned error, want nil (fail-open): %v", err)
	}
	if len(scores) != 1 || scores[0] != 0 {
		t.Errorf("scores = %v, want [0] on fail-open", scores)
	}
}

func TestBehavioralScorerPreservesCancellation(t *testing.T) {
	fw := &fakeWeighter{err: context.Canceled}
	s := NewBehavioralScorer(fw)
	chunks := []Chunk{{ID: "c1", StableKey: "sk1"}}
	_, err := s.ScoreBatch(context.Background(), chunks, "q", nil, QueryContext{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ScoreBatch error = %v, want context.Canceled", err)
	}
}

func TestBehavioralScorerDeduplicatesKeys(t *testing.T) {
	fw := &fakeWeighter{weights: map[string]float64{"sk1": 0.5}}
	s := NewBehavioralScorer(fw)
	chunks := []Chunk{
		{ID: "c1", StableKey: "sk1"},
		{ID: "c2", StableKey: "sk1"},
	}
	scores, err := s.ScoreBatch(context.Background(), chunks, "q", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if len(fw.gotKeys) != 1 {
		t.Errorf("weighter got %d keys, want 1 (deduped): %v", len(fw.gotKeys), fw.gotKeys)
	}
	if scores[0] != 0.5 || scores[1] != 0.5 {
		t.Errorf("scores = %v, want both 0.5", scores)
	}
}

func TestBehavioralScorerName(t *testing.T) {
	if NewBehavioralScorer(nil).Name() != "behavioral" {
		t.Errorf("Name() = %q, want \"behavioral\"", NewBehavioralScorer(nil).Name())
	}
}
