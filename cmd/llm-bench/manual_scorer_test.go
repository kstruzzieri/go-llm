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

func TestNewScorer_ManualRejectsAmbiguousTraceModelLabels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONL(path, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: "sha256:first", ExpectedAnswerQuality: 1.0},
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: "sha256:second", ExpectedAnswerQuality: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := newScorer(context.Background(), "manual", scorerOptions{manualLabelsPath: path})
	if err == nil {
		t.Fatalf("newScorer(manual) accepted ambiguous labels for the same trace/model; want error")
	}
	for _, want := range []string{"ambiguous", "(trace, model)", "sha256:first", "sha256:second"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err, want)
		}
	}
}

func TestFormatManualQualityReport_AggregatesPerModelAndFlagsLatency(t *testing.T) {
	results := []Result{
		{Model: "qwen3:8b", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
		{Model: "qwen3:8b", TraceID: "t2", Score: Score{AnswerQuality: 0.5}},
		{Model: "gemma4:31b", TraceID: "t1", Score: Score{AnswerQuality: 1.0}},
	}
	out := formatManualQualityReport([]string{"qwen3:8b", "gemma4:31b"}, results, manualReportCoverage{Scored: 3, Stale: 1, Errored: 0})
	for _, want := range []string{"qwen3:8b", "gemma4:31b", "human label", "Latency is NOT included", "stale", "all matched labels"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q:\n%s", want, out)
		}
	}
}

func TestRunManualReport_SurfacesStaleAndScoredCounts(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a1 := testCalibrationArtifact("t1", "answer one")
	a2 := testCalibrationArtifact("t2", "answer two")
	if err := writeJSONL(arts, []any{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{TraceID: "t2", CandidateModel: "ollama/c", ArtifactHash: a2.ArtifactHash, ExpectedAnswerQuality: 0.5},
		Label{TraceID: "t3", CandidateModel: "ollama/c", ArtifactHash: "stale-no-match", ExpectedAnswerQuality: 1.0},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := runManualReport(context.Background(), labels, arts)
	if err != nil {
		t.Fatalf("runManualReport: %v", err)
	}
	if !strings.Contains(report, "stale: 1") {
		t.Fatalf("report does not surface the 1 stale label:\n%s", report)
	}
}

func TestRunManualReport_RejectsAmbiguousTraceModel(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	// Two artifacts share (trace, model) but differ in content -> distinct
	// hashes -> both match their labels -> ambiguous for a (trace, model) key.
	a1 := testCalibrationArtifact("t1", "answer A")
	a2 := testCalibrationArtifact("t1", "answer B")
	if err := writeJSONL(arts, []any{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a2.ArtifactHash, ExpectedAnswerQuality: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runManualReport(context.Background(), labels, arts); err == nil {
		t.Fatalf("runManualReport accepted two artifacts sharing (trace, model); want a loud ambiguity error")
	}
}

func TestManualScorer_ReturnsHumanLabelAsAnswerQuality(t *testing.T) {
	s, err := newManualScorer([]Label{
		{TraceID: "conversation-fa-l02", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 0.5, LabelNotes: "partial"},
	})
	if err != nil {
		t.Fatalf("newManualScorer: %v", err)
	}
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
	s, err := newManualScorer([]Label{
		{TraceID: "t1", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 1.0},
	})
	if err != nil {
		t.Fatalf("newManualScorer: %v", err)
	}
	score, err := s.Score(context.Background(), Trace{ID: "t1"}, Result{Model: "ollama/QWEN3:8b"})
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score.AnswerQuality != 1.0 {
		t.Fatalf("AnswerQuality = %v; want 1.0 (prefix/case-insensitive match)", score.AnswerQuality)
	}
}

func TestManualScorer_MissingLabelFailsLoud(t *testing.T) {
	s, err := newManualScorer([]Label{
		{TraceID: "t1", CandidateModel: "qwen3:8b", ExpectedAnswerQuality: 1.0},
	})
	if err != nil {
		t.Fatalf("newManualScorer: %v", err)
	}
	if _, err := s.Score(context.Background(), Trace{ID: "t1"}, Result{Model: "gemma4:31b"}); err == nil {
		t.Fatalf("Score for an unlabeled (trace, model) returned nil error; want a loud miss")
	}
}
