package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewScorer_ManualBuildsFromLabelsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONL(path, []any{
		Label{TraceID: "t1", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	sc, err := newScorer(context.Background(), "manual", scorerOptions{manualLabelsPath: path})
	if err != nil {
		t.Fatalf("newScorer(manual): %v", err)
	}
	if _, ok := sc.(*ManualScorer); !ok {
		t.Fatalf("scorer type %T; want *ManualScorer", sc)
	}
}

func TestNewScorer_ManualRequiresLabels(t *testing.T) {
	if _, err := newScorer(context.Background(), "manual", scorerOptions{manualLabelsPath: ""}); err == nil {
		t.Fatalf("newScorer(manual) with no labels path returned nil error; want error")
	}
}

func TestFormatManualQualityReport_AggregatesPerModelAndFlagsLatency(t *testing.T) {
	results := []Result{
		{Model: "qwen3:8b", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
		{Model: "qwen3:8b", TraceID: "t2", Score: Score{AnswerQuality: 0.5}},
		{Model: "gemma4:31b", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
	}
	out := formatManualQualityReport([]string{"qwen3:8b", "gemma4:31b"}, results)
	for _, want := range []string{"qwen3:8b", "gemma4:31b", "human label", "Latency is NOT included"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

func TestManualScorer_ReturnsHumanLabelAsAnswerQuality(t *testing.T) {
	s := newManualScorer([]Label{
		{TraceID: "conversation-fa-l02", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 0.5, LabelNotes: "partial"},
	})
	trace := Trace{ID: "conversation-fa-l02"}
	actual := Result{Model: "qwen3:8b", Transcript: []Turn{{Role: "assistant", Content: "ans"}}}
	score, err := s.Score(context.Background(), trace, actual)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 0.5 {
		t.Fatalf("AnswerQuality = %v; want 0.5 (the human label)", score.AnswerQuality)
	}
}

func TestManualScorer_MatchesProviderPrefixedAndCasedModel(t *testing.T) {
	// Labels are stored bare ("qwen3:8b"); a run's actual.Model may carry the
	// bench provider prefix ("ollama/qwen3:8b") or different casing.
	s := newManualScorer([]Label{
		{TraceID: "t1", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 1.0},
	})
	score, err := s.Score(context.Background(), Trace{ID: "t1"}, Result{Model: "ollama/QWEN3:8b"})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want 1.0 (prefix/case-insensitive match)", score.AnswerQuality)
	}
}

func TestManualScorer_MissingLabelFailsLoud(t *testing.T) {
	s := newManualScorer([]Label{
		{TraceID: "t1", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 1.0},
	})
	if _, err := s.Score(context.Background(), Trace{ID: "t1"}, Result{Model: "gemma4:31b"}); err == nil {
		t.Fatalf("Score for an unlabeled (trace, model) returned nil error; want a loud miss")
	}
}
