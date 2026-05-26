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
