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

	"github.com/kstruzzieri/go-llm/ollama"
)

// fakeRunner implements calibrationRunner with canned Results so we don't
// need a live Ollama for the capture test.
type fakeRunner struct {
	results []Result
	calls   int
}

func (f *fakeRunner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	f.calls++
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
	defer func() { _ = f.Close() }()
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
		if a.Trace.ID != a.TraceID {
			t.Fatalf("artifact %d trace context ID = %q; want %q", i, a.Trace.ID, a.TraceID)
		}
	}
	if artifacts[0].ActualFinalAnswer != "answer one" {
		t.Fatalf("artifact[0].ActualFinalAnswer = %q", artifacts[0].ActualFinalAnswer)
	}
}

func TestRunCalibrateCapture_FailsWhenNoArtifactsWritten(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "artifacts.jsonl")
	runner := &fakeRunner{results: []Result{
		{Model: "ollama/cand", TraceID: "t1", Err: fmt.Errorf("model unavailable")},
	}}
	traces := []Trace{
		{ID: "t1", System: "s", Turns: []Turn{{Role: "user", Content: "u1"}}, Golden: Golden{FinalAnswerCriteria: "c"}},
	}
	targets := []ModelTarget{{Display: "ollama/cand", Provider: "ollama", Model: "cand"}}

	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
	})
	if err == nil || !strings.Contains(err.Error(), "no artifacts written") {
		t.Fatalf("runCalibrateCapture err = %v; want no artifacts written", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("output stat err = %v; want file not created", statErr)
	}
}

func TestRunCalibrateCapture_DuplicateTraceIDRejectedBeforeReplay(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "artifacts.jsonl")
	runner := &fakeRunner{results: []Result{
		{Model: "ollama/cand", TraceID: "dup", Transcript: []Turn{{Role: "assistant", Content: "answer"}}},
	}}
	traces := []Trace{
		{ID: "dup", System: "s1", Turns: []Turn{{Role: "user", Content: "u1"}}, Golden: Golden{FinalAnswerCriteria: "c1"}},
		{ID: "dup", System: "s2", Turns: []Turn{{Role: "user", Content: "u2"}}, Golden: Golden{FinalAnswerCriteria: "c2"}},
	}
	targets := []ModelTarget{{Display: "ollama/cand", Provider: "ollama", Model: "cand"}}

	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner: runner, Targets: targets, Traces: traces, OutputPath: out,
	})
	if err == nil || !strings.Contains(err.Error(), `duplicate trace ID "dup"`) {
		t.Fatalf("runCalibrateCapture err = %v; want duplicate trace ID", err)
	}
	if runner.calls != 0 {
		t.Fatalf("RunAll calls = %d; want 0", runner.calls)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("output stat err = %v; want file not created", statErr)
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

func TestArtifactHash_SensitiveToTraceIDCase(t *testing.T) {
	base := Artifact{TraceID: "Foo", CandidateModel: "c"}
	h1 := artifactHash(base)
	base.TraceID = "foo"
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash should preserve TraceID case")
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

func TestArtifactHash_SensitiveToGoldenRubric(t *testing.T) {
	base := testCalibrationArtifact("t", "answer")
	h1 := artifactHash(base)
	base.Trace.Golden.FinalAnswerCriteria = "different rubric"
	h2 := artifactHash(base)
	if h1 == h2 {
		t.Fatalf("artifactHash not sensitive to original trace rubric")
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

func TestCalibrationTraceFromArtifactRequiresOriginalRubric(t *testing.T) {
	artifact := Artifact{TraceID: "t", CandidateModel: "ollama/c"}
	_, err := calibrationTraceFromArtifact(artifact)
	if err == nil || !strings.Contains(err.Error(), "missing original trace context") {
		t.Fatalf("calibrationTraceFromArtifact err = %v; want missing trace context", err)
	}
}

func TestLoadLabelsAndArtifacts_MatchAndStale(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	arts := filepath.Join(dir, "artifacts.jsonl")
	art := testCalibrationArtifact("t1", "x")
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

func TestLoadLabelsAndArtifacts_DuplicateMatchedLabelRejected(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	arts := filepath.Join(dir, "artifacts.jsonl")
	art := testCalibrationArtifact("t1", "x")
	if err := writeJSONL(arts, []any{art}); err != nil {
		t.Fatalf("seed arts: %v", err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: art.ArtifactHash, ExpectedAnswerQuality: 1.0, Labeler: "manual"},
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: art.ArtifactHash, ExpectedAnswerQuality: 0.5, Labeler: "manual"},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	_, _, err := loadLabelsMatchedAgainst(labels, arts)
	if err == nil || !strings.Contains(err.Error(), "duplicate label for artifact_hash") {
		t.Fatalf("loadLabelsMatchedAgainst err = %v; want duplicate label error", err)
	}
}

func TestLoadLabels_InvalidExpectedAnswerQualityRejected(t *testing.T) {
	dir := t.TempDir()
	labels := filepath.Join(dir, "labels.jsonl")
	if err := writeJSONL(labels, []any{
		Label{TraceID: "t1", CandidateModel: "ollama/c", ArtifactHash: "sha256:h", ExpectedAnswerQuality: 2.0, Labeler: "manual"},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	_, err := loadLabels(labels)
	if err == nil || !strings.Contains(err.Error(), "invalid expected_answer_quality") {
		t.Fatalf("loadLabels err = %v; want invalid expected_answer_quality", err)
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

func testCalibrationArtifact(traceID, answer string) Artifact {
	trace := Trace{
		ID:     traceID,
		System: "system for " + traceID,
		Turns:  []Turn{{Role: "user", Content: "question for " + traceID}},
		Golden: Golden{FinalAnswerCriteria: "rubric for " + traceID},
	}
	artifact := Artifact{
		TraceID:           traceID,
		CandidateModel:    "ollama/c",
		Trace:             trace,
		ActualFinalAnswer: answer,
		ActualTranscript:  []Turn{{Role: "assistant", Content: answer}},
	}
	artifact.ArtifactHash = artifactHash(artifact)
	return artifact
}

type recordingScorer struct {
	trace Trace
}

func (s *recordingScorer) Score(ctx context.Context, t Trace, r Result) (Score, error) {
	s.trace = t
	return Score{AnswerQuality: 1.0}, nil
}

func TestRunCalibrate_UsesArtifactTraceRubric(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	artifact := testCalibrationArtifact("trace-rubric", "answer")
	artifact.Trace.Golden.FinalAnswerCriteria = "trace-specific rubric"
	artifact.ArtifactHash = artifactHash(artifact)
	if err := writeJSONL(arts, []any{artifact}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{Label{
		TraceID: artifact.TraceID, CandidateModel: artifact.CandidateModel, ArtifactHash: artifact.ArtifactHash,
		ExpectedAnswerQuality: 1.0, Labeler: "manual",
	}}); err != nil {
		t.Fatal(err)
	}
	scorer := &recordingScorer{}
	_, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath: labels, ArtifactsPath: arts, Scorer: scorer,
		JudgeModel: "ollama/judge", MinLabels: 1,
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if scorer.trace.Golden.FinalAnswerCriteria != "trace-specific rubric" {
		t.Fatalf("scored rubric = %q; want original trace rubric", scorer.trace.Golden.FinalAnswerCriteria)
	}
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
		a := testCalibrationArtifact(traceID, traceID)
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

func TestRunCalibrate_SkipsSelfJudgedArtifacts(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	reportDir := filepath.Join(dir, "reports")

	self := testCalibrationArtifact("self", "self answer")
	self.CandidateModel = "ollama/judge"
	self.ArtifactHash = artifactHash(self)
	other := testCalibrationArtifact("other", "other answer")
	if err := writeJSONL(arts, []any{self, other}); err != nil {
		t.Fatalf("seed arts: %v", err)
	}
	if err := writeJSONL(labels, []any{
		Label{TraceID: "self", CandidateModel: self.CandidateModel, ArtifactHash: self.ArtifactHash, ExpectedAnswerQuality: 1.0, Labeler: "manual"},
		Label{TraceID: "other", CandidateModel: other.CandidateModel, ArtifactHash: other.ArtifactHash, ExpectedAnswerQuality: 1.0, Labeler: "manual"},
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}

	fs := &fakeScorer{judgeByTrace: map[string]float64{"other": 1.0}}
	res, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath:    labels,
		ArtifactsPath: arts,
		Scorer:        fs,
		JudgeModel:    "judge",
		ReportDir:     reportDir,
		MinLabels:     1,
		Clock:         func() time.Time { return time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if res.MatchedCount != 1 || res.SelfJudgedSkipCount != 1 || res.AgreeCount != 1 {
		t.Fatalf("MatchedCount=%d SelfJudgedSkipCount=%d AgreeCount=%d; want 1 / 1 / 1",
			res.MatchedCount, res.SelfJudgedSkipCount, res.AgreeCount)
	}
	if fs.called != 1 {
		t.Fatalf("Score calls=%d; want 1", fs.called)
	}
	if len(res.PerLabel) != 1 || res.PerLabel[0].TraceID != "other" {
		t.Fatalf("PerLabel=%+v; want only non-self artifact", res.PerLabel)
	}
	report, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(report), "Self-judged labels skipped: 1") {
		t.Fatalf("report missing self-judged skip count:\n%s", report)
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
		a := testCalibrationArtifact(traceID, traceID)
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

func TestCalibrationExitCodeFailsClosedOnInsufficientLabels(t *testing.T) {
	if got := calibrationExitCode("PASS"); got != 0 {
		t.Fatalf("PASS exit code = %d; want 0", got)
	}
	if got := calibrationExitCode("FAIL"); got == 0 {
		t.Fatalf("FAIL exit code = %d; want non-zero", got)
	}
	if got := calibrationExitCode("INSUFFICIENT_LABELS"); got == 0 {
		t.Fatalf("INSUFFICIENT_LABELS exit code = %d; want non-zero", got)
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
	a := testCalibrationArtifact("t", "x")
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
		Clock:      func() time.Time { return time.Date(2026, 5, 25, 13, 14, 15, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if !strings.HasSuffix(res.ReportPath, "2026-05-25T131415Z-ollama_gemma4_31b.md") {
		t.Fatalf("ReportPath=%q; want suffix 2026-05-25T131415Z-ollama_gemma4_31b.md", res.ReportPath)
	}
}

func TestRunCalibrate_ReportFilenameDoesNotOverwriteSameTimestamp(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	reportDir := filepath.Join(dir, "reports")
	a := testCalibrationArtifact("t", "x")
	if err := writeJSONL(arts, []any{a}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{Label{
		TraceID: "t", CandidateModel: "ollama/c", ArtifactHash: a.ArtifactHash,
		ExpectedAnswerQuality: 1.0, Labeler: "manual",
	}}); err != nil {
		t.Fatal(err)
	}
	run := func() CalibrationResult {
		t.Helper()
		res, err := runCalibrate(context.Background(), calibrateOptions{
			LabelsPath: labels, ArtifactsPath: arts,
			Scorer:     &fakeScorer{judgeByTrace: map[string]float64{"t": 1.0}},
			JudgeModel: "ollama/gemma4:31b",
			MinLabels:  1,
			ReportDir:  reportDir,
			Clock:      func() time.Time { return time.Date(2026, 5, 25, 13, 14, 15, 0, time.UTC) },
		})
		if err != nil {
			t.Fatalf("runCalibrate: %v", err)
		}
		return res
	}

	first := run()
	if err := os.WriteFile(first.ReportPath, []byte("sentinel"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	second := run()
	if first.ReportPath == second.ReportPath {
		t.Fatalf("ReportPath reused %q; want unique path", first.ReportPath)
	}
	if !strings.HasSuffix(second.ReportPath, "2026-05-25T131415Z-ollama_gemma4_31b-2.md") {
		t.Fatalf("second ReportPath=%q; want -2 suffix", second.ReportPath)
	}
	got, err := os.ReadFile(first.ReportPath)
	if err != nil {
		t.Fatalf("read first report: %v", err)
	}
	if string(got) != "sentinel" {
		t.Fatalf("first report was overwritten: %q", got)
	}
}

// fakeStabilityScorer returns a sequence of scores per Score() call.
type fakeStabilityScorer struct {
	callCount int
	sequence  []float64
}

func (s *fakeStabilityScorer) Score(ctx context.Context, t Trace, r Result) (Score, error) {
	if s.callCount >= len(s.sequence) {
		s.callCount++
		return Score{AnswerQuality: s.sequence[len(s.sequence)-1]}, nil
	}
	v := s.sequence[s.callCount]
	s.callCount++
	return Score{AnswerQuality: v}, nil
}

func TestRunCalibrate_StabilityRunsReportSpread(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a := testCalibrationArtifact("t", "x")
	if err := writeJSONL(arts, []any{a}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONL(labels, []any{Label{
		TraceID: "t", CandidateModel: "ollama/c", ArtifactHash: a.ArtifactHash,
		ExpectedAnswerQuality: 1.0, Labeler: "manual",
	}}); err != nil {
		t.Fatal(err)
	}
	fs := &fakeStabilityScorer{sequence: []float64{0.8, 1.0, 0.6}} // primary=0.8, spread across 3 runs = 0.4
	res, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath: labels, ArtifactsPath: arts, Scorer: fs,
		JudgeModel: "ollama/judge", MinLabels: 1, StabilityRuns: 3,
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if len(res.PerLabel) != 1 {
		t.Fatalf("PerLabel=%d; want 1", len(res.PerLabel))
	}
	if res.PerLabel[0].StabilitySpread != 0.4 {
		t.Fatalf("StabilitySpread=%v; want 0.4", res.PerLabel[0].StabilitySpread)
	}
	if fs.callCount != 3 {
		t.Fatalf("Score calls=%d; want 3 total stability samples", fs.callCount)
	}
	// Stability spread does NOT affect agreement (primary sample is counted).
	if res.AgreeCount != 1 {
		t.Fatalf("AgreeCount=%d; want 1 (stability runs are diagnostic only)", res.AgreeCount)
	}
}

func TestRunCalibrate_StabilityRunsDoNotPersistJudgeCache(t *testing.T) {
	dir := t.TempDir()
	arts := filepath.Join(dir, "artifacts.jsonl")
	labels := filepath.Join(dir, "labels.jsonl")
	a := testCalibrationArtifact("t", "x")
	a.ActualTranscript = []Turn{{Role: "user", Content: "ask"}, {Role: "assistant", Content: "x"}}
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
	c, _ := newTestCache(t)
	judge := &fakeJudgeClient{resp: &ollama.ChatResponse{Message: ollama.ChatMessage{
		Content: `{"answer_quality":1.0,"justification":"j"}`,
	}}}
	scorer := &LLMJudgeScorer{Client: judge, JudgeModel: "ollama/judge", Cache: c, BypassCache: false}
	_, err := runCalibrate(context.Background(), calibrateOptions{
		LabelsPath: labels, ArtifactsPath: arts, Scorer: scorer,
		JudgeModel: "ollama/judge", MinLabels: 1, StabilityRuns: 3,
	})
	if err != nil {
		t.Fatalf("runCalibrate: %v", err)
	}
	if judge.called != 3 {
		t.Fatalf("judge called %d times; want 3 total bypassed stability samples", judge.called)
	}
	// Stability mode bypasses cache for all samples and must NOT write rows.
	var count int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM judge_cache`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cache row count = %d; want 0 (stability runs must bypass write)", count)
	}
}

// writeJSONL is a tiny test helper local to calibration_test.go.
func writeJSONL(path string, records []any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
