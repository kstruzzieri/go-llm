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
	report, err := runManualReport(context.Background(), labels, arts, nil)
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
	if _, err := runManualReport(context.Background(), labels, arts, nil); err == nil {
		t.Fatalf("runManualReport accepted two artifacts sharing (trace, model); want a loud ambiguity error")
	}
}

func TestRunManualReport_RejoinsModelFromArtifactForBlindLabels(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	// Two models for the same trace, distinct content -> distinct hashes.
	a1 := testCalibrationArtifact("t1", "answer from model one")
	a1.CandidateModel = "ollama/gemma4:31b"
	a1.ArtifactHash = artifactHash(a1)
	a2 := testCalibrationArtifact("t1", "answer from model two")
	a2.CandidateModel = "ollama/qwen3:8b"
	a2.ArtifactHash = artifactHash(a2)
	if err := writeJSONL(arts, []any{a1, a2}); err != nil {
		t.Fatal(err)
	}
	// Blind labels: candidate_model omitted, keyed only on artifact_hash.
	if err := writeJSONL(labels, []any{
		Label{ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{ArtifactHash: a2.ArtifactHash, ExpectedAnswerQuality: 0.5},
	}); err != nil {
		t.Fatal(err)
	}
	report, err := runManualReport(context.Background(), labels, arts, nil)
	if err != nil {
		t.Fatalf("runManualReport with blind labels: %v", err)
	}
	for _, want := range []string{"gemma4:31b", "qwen3:8b"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing rejoined model %q (model was not backfilled from the artifact):\n%s", want, report)
		}
	}
}

func TestRunManualReport_FilterDropsNonEvidence(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a1 := testCalibrationArtifact("t1", "evidence answer")   // natural, evidence
	a2 := testCalibrationArtifact("canary", "canary answer") // judge-validation, non-evidence
	if err := writeJSONL(arts, []any{a1, a2}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
		Label{TraceID: "canary", CandidateModel: "ollama/c", ArtifactHash: a2.ArtifactHash, ExpectedAnswerQuality: 0.0},
	}); err != nil {
		t.Fatal(err)
	}
	filter := &corpusFilter{
		Manifest: Manifest{Entries: []ManifestEntry{
			{TraceID: "t1", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
			{TraceID: "canary", Partition: PartitionJudgeValidation, Category: "tool-canary", AllowedAsModelEvidence: false},
		}},
		Selection: corpusSelection{},
	}
	report, err := runManualReport(context.Background(), labels, arts, filter)
	if err != nil {
		t.Fatalf("runManualReport with filter: %v", err)
	}
	if strings.Contains(report, "canary") {
		t.Fatalf("canary (judge-validation, non-evidence) leaked into the manual report:\n%s", report)
	}
}

// TestRunManualReport_MissingCanaryDoesNotBlock: the committed Round-2A
// manifest lists the tool canary (judge-validation, non-evidence) which is
// never captured by the offline flow; its absence from the artifacts must not
// fail an unscoped -corpus-manifest report.
func TestRunManualReport_MissingCanaryDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a1 := testCalibrationArtifact("t1", "evidence answer")
	if err := writeJSONL(arts, []any{a1}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
	}); err != nil {
		t.Fatal(err)
	}
	filter := &corpusFilter{
		Manifest: Manifest{Entries: []ManifestEntry{
			{TraceID: "t1", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
			{TraceID: "tool-canary-01", Partition: PartitionJudgeValidation, Category: "tool-canary", AllowedAsModelEvidence: false},
		}},
		Selection: corpusSelection{},
	}
	report, err := runManualReport(context.Background(), labels, arts, filter)
	if err != nil {
		t.Fatalf("runManualReport failed on a missing non-evidence canary: %v", err)
	}
	if !strings.Contains(report, "ollama/c") {
		t.Fatalf("report missing the evidence model row:\n%s", report)
	}
	if !strings.Contains(report, "tool-canary-01") {
		t.Fatalf("report does not note the excluded missing canary (audit trail):\n%s", report)
	}
}

// TestRunManualReport_MissingEvidenceTraceErrors: a manifest-selected
// *evidence* trace absent from the matched artifacts must still fail loud.
func TestRunManualReport_MissingEvidenceTraceErrors(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a1 := testCalibrationArtifact("t1", "evidence answer")
	if err := writeJSONL(arts, []any{a1}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: a1.ArtifactHash, ExpectedAnswerQuality: 1.0},
	}); err != nil {
		t.Fatal(err)
	}
	filter := &corpusFilter{
		Manifest: Manifest{Entries: []ManifestEntry{
			{TraceID: "t1", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
			{TraceID: "t-absent", Partition: PartitionNatural, Category: "chat", AllowedAsModelEvidence: true},
		}},
		Selection: corpusSelection{},
	}
	_, err := runManualReport(context.Background(), labels, arts, filter)
	if err == nil || !strings.Contains(err.Error(), "t-absent") {
		t.Fatalf("err = %v; want loud missing-evidence error naming t-absent", err)
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

func TestManualScorer_KeepsExplicitProviderDistinct(t *testing.T) {
	s, err := newManualScorer([]Label{
		{TraceID: "t1", CandidateModel: "ollama / m", ExpectedAnswerQuality: 1.0},
		{TraceID: "t1", CandidateModel: "openai-compat / m", ExpectedAnswerQuality: 0.5},
	})
	if err != nil {
		t.Fatalf("newManualScorer: %v", err)
	}
	for model, want := range map[string]float64{"m": 1, "openai-compat/M": 0.5} {
		score, err := s.Score(context.Background(), Trace{ID: "t1"}, Result{Model: model})
		if err != nil {
			t.Fatalf("Score(%q): %v", model, err)
		}
		if score.AnswerQuality != want {
			t.Errorf("Score(%q) = %v; want %v", model, score.AnswerQuality, want)
		}
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
