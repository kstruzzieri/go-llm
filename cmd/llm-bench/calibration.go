package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Artifact is one frozen candidate output for a (trace, candidate model)
// pair. Written by -calibrate-capture; consumed by -calibrate and by the
// labeling workflow (humans add expected_answer_quality to corresponding
// Label rows).
type Artifact struct {
	TraceID           string    `json:"trace_id"`
	CandidateModel    string    `json:"candidate_model"`
	ArtifactHash      string    `json:"artifact_hash"`
	ActualFinalAnswer string    `json:"actual_final_answer"`
	ActualToolCalls   []string  `json:"actual_tool_calls"`
	ActualTranscript  []Turn    `json:"actual_transcript"`
	CapturedAt        time.Time `json:"captured_at"`
}

// Label is the human-supplied truth for one frozen Artifact. ArtifactHash
// links them: a Label with a hash that doesn't match any current Artifact
// is "stale" and excluded from agreement.
type Label struct {
	TraceID               string    `json:"trace_id"`
	CandidateModel        string    `json:"candidate_model"`
	ArtifactHash          string    `json:"artifact_hash"`
	ActualFinalAnswer     string    `json:"actual_final_answer,omitempty"`
	ActualToolCalls       []string  `json:"actual_tool_calls,omitempty"`
	ActualTranscript      []Turn    `json:"actual_transcript,omitempty"`
	ExpectedAnswerQuality float64   `json:"expected_answer_quality"`
	LabelNotes            string    `json:"label_notes,omitempty"`
	LabeledAt             time.Time `json:"labeled_at"`
	Labeler               string    `json:"labeler"`
}

// artifactHashInput is the canonical struct hashed to produce ArtifactHash.
// Order-sensitive on tool calls and transcript turns; ID and candidate
// model normalized to lowercase to avoid spurious mismatch from case.
type artifactHashInput struct {
	TraceID           string   `json:"trace_id"`
	CandidateModel    string   `json:"candidate_model"`
	ActualFinalAnswer string   `json:"actual_final_answer"`
	ActualToolCalls   []string `json:"actual_tool_calls"`
	ActualTranscript  []Turn   `json:"actual_transcript"`
}

// artifactHash returns the canonical sha256 hash for an Artifact. Stable
// under JSON struct-tag ordering; sensitive to every input including
// tool-call sequence order.
func artifactHash(a Artifact) string {
	raw, _ := json.Marshal(artifactHashInput{
		TraceID:           normalizeModelSelector(a.TraceID),
		CandidateModel:    normalizeModelSelector(a.CandidateModel),
		ActualFinalAnswer: a.ActualFinalAnswer,
		ActualToolCalls:   a.ActualToolCalls,
		ActualTranscript:  a.ActualTranscript,
	})
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("sha256:%s", hex.EncodeToString(sum[:]))
}

// calibrationRunner abstracts Runner.RunAll so calibration tests can
// inject canned Results without needing a live Ollama.
type calibrationRunner interface {
	RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error)
}

// calibrateCaptureOptions configures runCalibrateCapture. Runner is the
// (typically *Runner) replayer; Targets and Traces are forwarded to it;
// OutputPath receives one JSONL Artifact per non-failed Result. Clock is
// an optional time source for deterministic tests; defaults to time.Now
// in UTC.
type calibrateCaptureOptions struct {
	Runner     calibrationRunner
	Targets    []ModelTarget
	Traces     []Trace
	OutputPath string
	Clock      func() time.Time
}

// runCalibrateCapture replays each (trace, candidate) and writes one
// Artifact per non-failed Result to OutputPath as JSONL. Artifacts have
// no ExpectedAnswerQuality — labels are a separate file the operator
// hand-edits.
func runCalibrateCapture(ctx context.Context, opts calibrateCaptureOptions) error {
	if opts.Runner == nil {
		return fmt.Errorf("calibrate-capture: nil runner")
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return fmt.Errorf("calibrate-capture: empty output path")
	}
	if len(opts.Traces) == 0 {
		return errors.New("calibrate-capture: no traces")
	}
	if len(opts.Targets) == 0 {
		return errors.New("calibrate-capture: no targets")
	}
	results, err := opts.Runner.RunAll(ctx, opts.Targets, opts.Traces)
	if err != nil {
		return fmt.Errorf("calibrate-capture: run: %w", err)
	}
	now := func() time.Time { return time.Now().UTC() }
	if opts.Clock != nil {
		now = opts.Clock
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return fmt.Errorf("calibrate-capture: mkdir: %w", err)
	}
	f, err := os.OpenFile(opts.OutputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("calibrate-capture: open output: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range results {
		if r.Err != nil {
			// Skip failed runs — they cannot be labeled coherently.
			continue
		}
		artifact := Artifact{
			TraceID:           r.TraceID,
			CandidateModel:    r.Model,
			ActualFinalAnswer: lastAssistantContent(r.Transcript),
			ActualToolCalls:   extractToolNames(r.Transcript),
			ActualTranscript:  r.Transcript,
			CapturedAt:        now(),
		}
		artifact.ArtifactHash = artifactHash(artifact)
		if err := enc.Encode(artifact); err != nil {
			return fmt.Errorf("calibrate-capture: encode artifact %s/%s: %w", artifact.TraceID, artifact.CandidateModel, err)
		}
	}
	return nil
}

// matchedLabel pairs a Label with its current Artifact so the calibration
// loop can re-score the frozen output without re-running the candidate.
type matchedLabel struct {
	Label    Label
	Artifact Artifact
}

// loadLabelsMatchedAgainst loads both files and partitions labels into
// (matched, stale). A label is "stale" iff its ArtifactHash is not present
// in artifactsPath. Stale labels are not errors — they're reported and
// excluded from agreement.
func loadLabelsMatchedAgainst(labelsPath, artifactsPath string) ([]matchedLabel, []Label, error) {
	arts, err := loadArtifacts(artifactsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load artifacts: %w", err)
	}
	byHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		byHash[a.ArtifactHash] = a
	}
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load labels: %w", err)
	}
	var matched []matchedLabel
	var stale []Label
	for _, l := range labels {
		a, ok := byHash[l.ArtifactHash]
		if !ok {
			stale = append(stale, l)
			continue
		}
		matched = append(matched, matchedLabel{Label: l, Artifact: a})
	}
	return matched, stale, nil
}

func loadArtifacts(path string) ([]Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	var out []Artifact
	dec := json.NewDecoder(f)
	for dec.More() {
		var a Artifact
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("decode artifact: %w", err)
		}
		out = append(out, a)
	}
	return out, nil
}

func loadLabels(path string) ([]Label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	var out []Label
	dec := json.NewDecoder(f)
	for dec.More() {
		var l Label
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("decode label: %w", err)
		}
		out = append(out, l)
	}
	return out, nil
}

const (
	calibrationAgreementDelta   = 0.25
	calibrationPassThreshold    = 0.85
	calibrationDefaultMinLabels = 50
)

// calibrateOptions configures runCalibrate.
//
// MinLabels defaults to calibrationDefaultMinLabels (50) when zero so a
// sufficient sample backs the agreement verdict. StabilityRuns is reserved
// for Task 20 (judge stability spread) and currently ignored. Clock is an
// optional time source for deterministic report filenames.
type calibrateOptions struct {
	LabelsPath    string
	ArtifactsPath string
	Scorer        Scorer
	JudgeModel    string
	ReportDir     string
	MinLabels     int // defaults to calibrationDefaultMinLabels when zero
	StabilityRuns int // when >1, runs judge that many times with bypass-cache (Task 20)
	Clock         func() time.Time
}

// CalibrationResult is the in-memory summary returned by runCalibrate. The
// markdown report contains the same data plus per-label rows.
type CalibrationResult struct {
	JudgeModel    string
	MatchedCount  int
	StaleCount    int
	AgreeCount    int
	AgreementRate float64
	MinLabels     int
	Verdict       string // PASS / FAIL / INSUFFICIENT_LABELS
	ReportPath    string
	PerLabel      []perLabelOutcome
}

// perLabelOutcome is one row of the calibration report: a label paired with
// the judge's verdict on the same frozen artifact. StabilitySpread is
// reserved for Task 20 (multi-run judge spread) and zero otherwise.
type perLabelOutcome struct {
	TraceID         string
	CandidateModel  string
	Expected        float64
	Judge           float64
	Delta           float64
	Agree           bool
	StabilitySpread float64
}

// runCalibrate loads (labels, artifacts), invokes the scorer once per
// matched (label, artifact) pair, and computes the agreement rate against
// human ExpectedAnswerQuality values. The verdict is INSUFFICIENT_LABELS
// when MatchedCount < MinLabels, otherwise PASS when AgreementRate is at
// least calibrationPassThreshold (0.85), else FAIL.
//
// The synthesized Trace inside the loop uses a placeholder System and a
// single empty user turn so Scorer.Score's signature is satisfied. The
// frozen ActualTranscript from the Artifact is what the scorer actually
// reads from; live LLMJudgeScorer would need a transcript with an
// assistant final answer, which the captured Artifact provides.
//
// When ReportDir is non-empty the markdown report is written and its path
// stored on the returned result; otherwise no report is emitted.
func runCalibrate(ctx context.Context, opts calibrateOptions) (CalibrationResult, error) {
	if opts.Scorer == nil {
		return CalibrationResult{}, errors.New("calibrate: nil scorer")
	}
	if opts.MinLabels == 0 {
		opts.MinLabels = calibrationDefaultMinLabels
	}
	matched, stale, err := loadLabelsMatchedAgainst(opts.LabelsPath, opts.ArtifactsPath)
	if err != nil {
		return CalibrationResult{}, err
	}
	res := CalibrationResult{
		JudgeModel:   opts.JudgeModel,
		MatchedCount: len(matched),
		StaleCount:   len(stale),
		MinLabels:    opts.MinLabels,
	}
	for _, m := range matched {
		// Construct a synthetic Trace and Result from the Label+Artifact
		// so we can call Scorer.Score against the frozen output.
		trace := Trace{
			ID:     m.Artifact.TraceID,
			System: "calibration",
			Turns:  []Turn{{Role: "user", Content: ""}},
			Golden: Golden{FinalAnswerCriteria: "frozen calibration artifact"},
		}
		actual := Result{
			Model:      m.Artifact.CandidateModel,
			TraceID:    m.Artifact.TraceID,
			Transcript: m.Artifact.ActualTranscript,
		}
		score, scoreErr := opts.Scorer.Score(ctx, trace, actual)
		if scoreErr != nil {
			return CalibrationResult{}, fmt.Errorf("calibrate: scorer for %s/%s: %w",
				m.Artifact.TraceID, m.Artifact.CandidateModel, scoreErr)
		}
		delta := score.AnswerQuality - m.Label.ExpectedAnswerQuality
		if delta < 0 {
			delta = -delta
		}
		outcome := perLabelOutcome{
			TraceID:        m.Artifact.TraceID,
			CandidateModel: m.Artifact.CandidateModel,
			Expected:       m.Label.ExpectedAnswerQuality,
			Judge:          score.AnswerQuality,
			Delta:          delta,
			Agree:          delta <= calibrationAgreementDelta,
		}
		res.PerLabel = append(res.PerLabel, outcome)
		if outcome.Agree {
			res.AgreeCount++
		}
	}
	if res.MatchedCount > 0 {
		res.AgreementRate = float64(res.AgreeCount) / float64(res.MatchedCount)
	}
	switch {
	case res.MatchedCount < opts.MinLabels:
		res.Verdict = "INSUFFICIENT_LABELS"
	case res.AgreementRate >= calibrationPassThreshold:
		res.Verdict = "PASS"
	default:
		res.Verdict = "FAIL"
	}
	if opts.ReportDir != "" {
		path, writeErr := writeCalibrationReport(opts.ReportDir, opts.JudgeModel, res, opts.Clock)
		if writeErr != nil {
			return res, fmt.Errorf("calibrate: write report: %w", writeErr)
		}
		res.ReportPath = path
	}
	return res, nil
}

// slugifyModel makes a path-safe slug from a model selector by replacing
// '/' and ':' with '_'. Example: "ollama/gemma4:31b" -> "ollama_gemma4_31b".
func slugifyModel(s string) string {
	out := strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(out, ":", "_")
}

// writeCalibrationReport emits a markdown calibration report to dir with a
// slugified, dated filename and returns the absolute path. Clock is the
// time source used to stamp the filename and report header; defaults to
// time.Now().UTC() when nil.
func writeCalibrationReport(dir, judgeModel string, res CalibrationResult, clock func() time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", dir, err)
	}
	now := time.Now().UTC()
	if clock != nil {
		now = clock()
	}
	path := fmt.Sprintf("%s/%s-%s.md", dir, now.Format("2006-01-02"), slugifyModel(judgeModel))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	fmt.Fprintf(f, "# Judge calibration — %s — %s\n\n", judgeModel, now.Format("2006-01-02"))
	fmt.Fprintf(f, "Matched labels: %d / %d required → %s\n", res.MatchedCount, res.MinLabels, sufficiencyLabel(res))
	fmt.Fprintf(f, "Stale labels: %d\n", res.StaleCount)
	fmt.Fprintf(f, "Agreement: %d / %d (%.0f%%) → **%s**\n\n",
		res.AgreeCount, res.MatchedCount, res.AgreementRate*100, res.Verdict)
	fmt.Fprintln(f, "| trace_id | candidate | expected | judge | Δ | agree | stability |")
	fmt.Fprintln(f, "|---|---|---|---|---|---|---|")
	for _, p := range res.PerLabel {
		agree := "✗"
		if p.Agree {
			agree = "✓"
		}
		stability := "n/a"
		if p.StabilitySpread > 0 {
			stability = fmt.Sprintf("spread=%.2f", p.StabilitySpread)
		}
		fmt.Fprintf(f, "| %s | %s | %.2f | %.2f | %.2f | %s | %s |\n",
			p.TraceID, p.CandidateModel, p.Expected, p.Judge, p.Delta, agree, stability)
	}
	return path, nil
}

// sufficiencyLabel renders the matched-vs-required label-count state as a
// short string for the report header.
func sufficiencyLabel(res CalibrationResult) string {
	if res.MatchedCount >= res.MinLabels {
		return "SUFFICIENT"
	}
	return "INSUFFICIENT"
}
