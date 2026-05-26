package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner implements calibrationRunner with canned Results so we don't
// need a live Ollama for the capture test.
type fakeRunner struct {
	results []Result
}

func (f *fakeRunner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	return f.results, nil
}

func TestRunCalibrateCapture_WritesOneArtifactPerResult(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "artifacts.jsonl")
	runner := &fakeRunner{results: []Result{
		{Model: "ollama/cand", TraceID: "t1", Transcript: []Turn{
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
			{Role: "assistant", Content: "answer one"},
		}},
		{Model: "ollama/cand", TraceID: "t2", Transcript: []Turn{
			{Role: "assistant", Content: "answer two"},
		}},
	}}
	traces := []Trace{
		{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "u1"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
		{ID: "t2", System: "s", Turns: []Turn{{Role: "user", Content: "u2"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "ollama/cand", Provider: "ollama", Model: "cand"}}

	if err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
	}); err != nil {
		t.Fatalf("runCalibrateCapture: %v", err)
	}

	// Read back artifacts.jsonl: expect 2 records, each with non-empty
	// ArtifactHash, no ExpectedAnswerQuality field set (labels are
	// separate), and ActualFinalAnswer matching the transcript.
	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var artifacts []Artifact
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var a Artifact
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil {
			t.Fatalf("decode: %v", err)
		}
		artifacts = append(artifacts, a)
	}
	if len(artifacts) != 2 {
		t.Fatalf("got %d artifacts; want 2", len(artifacts))
	}
	for i, a := range artifacts {
		if a.ArtifactHash == "" {
			t.Fatalf("artifact %d missing hash", i)
		}
		if a.CandidateModel != "ollama/cand" {
			t.Fatalf("artifact %d candidate = %q", i, a.CandidateModel)
		}
	}
	if artifacts[0].ActualFinalAnswer != "answer one" {
		t.Fatalf("artifact[0].ActualFinalAnswer = %q", artifacts[0].ActualFinalAnswer)
	}
}

func TestArtifactHash_DeterministicAcrossInvocations(t *testing.T) {
	a := Artifact{
		TraceID: "t1", CandidateModel: "ollama/cand",
		ActualFinalAnswer: "the answer is 4",
		ActualToolCalls:   []string{"read_file"},
		ActualTranscript:  []Turn{{Role: "assistant", Content: "the answer is 4"}},
	}
	b := a
	if artifactHash(a) != artifactHash(b) {
		t.Fatalf("artifactHash not deterministic for identical Artifact")
	}
}

func TestArtifactHash_SensitiveToFinalAnswer(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualFinalAnswer = "v1"
	h1 := artifactHash(base)
	base.ActualFinalAnswer = "v2"
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash not sensitive to ActualFinalAnswer")
	}
}

func TestArtifactHash_SensitiveToToolCallSequence(t *testing.T) {
	base := Artifact{TraceID: "t", CandidateModel: "c"}
	base.ActualToolCalls = []string{"a", "b"}
	h1 := artifactHash(base)
	base.ActualToolCalls = []string{"b", "a"}
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash should be order-sensitive on ActualToolCalls")
	}
}

func TestArtifactHash_PrefixIsSha256(t *testing.T) {
	h := artifactHash(Artifact{TraceID: "t", CandidateModel: "c"})
	if !strings.HasPrefix(h, "sha256:") {
		t.Fatalf("artifactHash %q does not start with sha256:", h)
	}
	// sha256: + 64 hex chars = 71 total
	if len(h) != 71 {
		t.Fatalf("artifactHash length = %d; want 71", len(h))
	}
}

func TestLoadLabelsAndArtifacts_MatchAndStale(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	arts := filepath.Join(dir, "artifacts.jsonl")
	art := Artifact{TraceID: "t1", CandidateModel: "ollama/c", ActualFinalAnswer: "x"}
	art.ArtifactHash = artifactHash(art)
	if err := writeJSONL(arts, []any{art}); err != nil {
		t.Fatalf("seed arts: %v", err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: art.ArtifactHash, ExpectedAnswerQuality: 1.0, Labeler: "manual"},
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: "sha256:stale", ExpectedAnswerQuality: 0.5, Labeler: "manual"},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	matched, stale, err := loadLabelsMatchedAgainst(labels, arts)
	if err != nil {
		t.Fatalf("loadLabelsMatchedAgainst: %v", err)
	}
	if len(matched) != 1 || matched[0].Label.ArtifactHash != art.ArtifactHash {
		t.Fatalf("matched = %+v; want 1 entry with art.ArtifactHash", matched)
	}
	if len(stale) != 1 || stale[0].ArtifactHash != "sha256:stale" {
		t.Fatalf("stale = %+v; want 1 stale label", stale)
	}
}

// fakeScorer returns canned answer-quality values per (trace, candidate).
type fakeScorer struct {
	judgeByTrace map[string]float64
	called       int
}

func (s *fakeScorer) Score(ctx context.Context, t Trace, r Result) (Score, error) {
	s.called++
	return Score{AnswerQuality: s.judgeByTrace[t.ID]}, nil
}

func TestRunCalibrate_AgreementMatchesByHand(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	reportDir := filepath.Join(dir, "reports")

	// 4 artifacts, 4 labels:
	// - label 0: expected 1.0, judge 1.0 → agree (Δ=0)
	// - label 1: expected 1.0, judge 0.6 → disagree (Δ=0.4 > 0.25)
	// - label 2: expected 0.5, judge 0.7 → agree (Δ=0.2 ≤ 0.25)
	// - label 3: expected 0.0, judge 0.25 → agree (Δ=0.25 == 0.25)
	var artifacts []any
	var labelRecs []any
	judge := map[string]float64{}
	expected := []float64{1.0, 1.0, 0.5, 0.0}
	judged := []float64{1.0, 0.6, 0.7, 0.25}
	for i := 0; i < 4; i++ {
		traceID := fmt.Sprintf("t%d", i)
		a := Artifact{TraceID: traceID, CandidateModel: "ollama/c", ActualFinalAnswer: traceID}
		a.ArtifactHash = artifactHash(a)
		artifacts = append(artifacts, a)
		labelRecs = append(labelRecs, Label{
			TraceID: traceID, CandidateModel: "ollama/c", ArtifactHash: a.ArtifactHash,
			ExpectedAnswerQuality: expected[i], Labeler: "manual",
		})
		judge[traceID] = judged[i]
	}
	if err := writeJSONL(arts, artifacts); err != nil {
		t.Fatalf("seed arts: %v", err)
	}
	if err := writeJSONL(labels, labelRecs); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	fs := &fakeScorer{judgeByTrace: judge}
	res, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath:    labels,
		ArtifactsPath: arts,
		Scorer:        fs,
		JudgeModel:    "ollama/judge",
		ReportDir:     reportDir,
		MinLabels:     2, // lower threshold for the test
		Clock:         func() time.Time { return time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if res.MatchedCount != 4 || res.AgreeCount != 3 {
		t.Fatalf("MatchedCount=%d AgreeCount=%d; want 4 / 3", res.MatchedCount, res.AgreeCount)
	}
	// 3 of 4 agreed = 75% < 85% threshold → FAIL.
	if res.Verdict != "FAIL" {
		t.Fatalf("Verdict=%q; want FAIL (3/4 = 75%% below 85%% threshold)", res.Verdict)
	}
}

func TestRunCalibrate_InsufficientLabelsNeverPasses(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")

	// Seed only 3 matched artifacts/labels, all perfect agreement.
	var artifacts, labelRecs []any
	for i := 0; i < 3; i++ {
		traceID := fmt.Sprintf("t%d", i)
		a := Artifact{TraceID: traceID, CandidateModel: "ollama/c", ActualFinalAnswer: traceID}
		a.ArtifactHash = artifactHash(a)
		artifacts = append(artifacts, a)
		labelRecs = append(labelRecs, Label{
			TraceID: traceID, CandidateModel: "ollama/c", ArtifactHash: a.ArtifactHash,
			ExpectedAnswerQuality: 1.0, Labeler: "manual",
		})
	}
	if err := writeJSONL(arts, artifacts); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, labelRecs); err != nil {
		t.Fatal(err)
	}

	fs := &fakeScorer{judgeByTrace: map[string]float64{"t0": 1, "t1": 1, "t2": 1}}
	res, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath: labels, ArtifactsPath: arts, Scorer: fs,
		JudgeModel: "ollama/judge",
		// Use default MinLabels=50; we only have 3.
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if res.Verdict != "INSUFFICIENT_LABELS" {
		t.Fatalf("Verdict=%q; want INSUFFICIENT_LABELS even at 100%% agreement on 3 matched", res.Verdict)
	}
}

func TestSlugifyModel_ReplacesSlashAndColon(t *testing.T) {
	cases := map[string]string{
		"ollama/gemma4:31b":              "ollama_gemma4_31b",
		"ollama/qwen3-coder-next:latest": "ollama_qwen3-coder-next_latest",
		"bare-name":                      "bare-name",
	}
	for in, want := range cases {
		if got := slugifyModel(in); got != want {
			t.Errorf("slugifyModel(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestRunCalibrate_ReportFilenameIsSlugified(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	reportDir := filepath.Join(dir, "reports")
	a := Artifact{TraceID: "t", CandidateModel: "ollama/c", ActualFinalAnswer: "x"}
	a.ArtifactHash = artifactHash(a)
	if err := writeJSONL(arts, []any{a}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{Label{
		TraceID: "t", CandidateModel: "ollama/c", ArtifactHash: a.ArtifactHash,
		ExpectedAnswerQuality: 1.0, Labeler: "manual",
	}}); err != nil {
		t.Fatal(err)
	}
	res, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath: labels, ArtifactsPath: arts,
		Scorer:     &fakeScorer{judgeByTrace: map[string]float64{"t": 1.0}},
		JudgeModel: "ollama/gemma4:31b",
		MinLabels:  1,
		ReportDir:  reportDir,
		Clock:      func() time.Time { return time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if !strings.HasSuffix(res.ReportPath, "2026-05-25-ollama_gemma4_31b.md") {
		t.Fatalf("ReportPath=%q; want suffix 2026-05-25-ollama_gemma4_31b.md", res.ReportPath)
	}
}

// writeJSONL is a tiny test helper local to calibration_test.go.
func writeJSONL(path string, records []any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
