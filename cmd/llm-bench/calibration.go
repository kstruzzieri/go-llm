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
	Trace             Trace     `json:"trace"`
	ActualFinalAnswer string    `json:"actual_final_answer"`
	ActualToolCalls   []string  `json:"actual_tool_calls"`
	ActualTranscript  []Turn    `json:"actual_transcript"`
	CapturedAt        time.Time `json:"captured_at"`
}

// Label is the human-supplied truth for one frozen Artifact. ArtifactHash
// links them: a Label with a hash that doesn't match any current Artifact
// is "stale" and excluded from agreement. Duplicate matched ArtifactHash
// values are rejected so each artifact contributes at most once.
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
// Order-sensitive on trace IDs, tool calls, and transcript turns; candidate
// model is normalized to lowercase to avoid spurious mismatch from case.
type artifactHashInput struct {
	TraceID           string   `json:"trace_id"`
	CandidateModel    string   `json:"candidate_model"`
	Trace             Trace    `json:"trace"`
	ActualFinalAnswer string   `json:"actual_final_answer"`
	ActualToolCalls   []string `json:"actual_tool_calls"`
	ActualTranscript  []Turn   `json:"actual_transcript"`
}

// artifactHash returns the canonical sha256 hash for an Artifact. Stable
// under JSON struct-tag ordering; sensitive to every input including
// tool-call sequence order.
func artifactHash(a Artifact) string {
	raw, _ := json.Marshal(artifactHashInput{
		TraceID:           a.TraceID,
		CandidateModel:    normalizeModelSelector(a.CandidateModel),
		Trace:             a.Trace,
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
func runCalibrateCapture(ctx context.Context, opts calibrateCaptureOptions) (retErr error) {
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
	traceByID := make(map[string]Trace, len(opts.Traces))
	for _, trace := range opts.Traces {
		if _, ok := traceByID[trace.ID]; ok {
			return fmt.Errorf("calibrate-capture: duplicate trace ID %q", trace.ID)
		}
		traceByID[trace.ID] = trace
	}
	results, err := opts.Runner.RunAll(ctx, opts.Targets, opts.Traces)
	if err != nil {
		return fmt.Errorf("calibrate-capture: run: %w", err)
	}
	now := func() time.Time { return time.Now().UTC() }
	if opts.Clock != nil {
		now = opts.Clock
	}
	var artifacts []Artifact
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			// Skip failed runs — they cannot be labeled coherently.
			// Surface the per-pair reason: a silently dropped result makes a
			// partial corpus look complete and hides timeout-vs-refusal.
			fmt.Fprintf(os.Stderr, "calibrate-capture: skipped %s/%s: %v\n", r.TraceID, r.Model, r.Err)
			failed++
			continue
		}
		trace, ok := traceByID[r.TraceID]
		if !ok {
			return fmt.Errorf("calibrate-capture: result %s/%s has no matching trace context", r.TraceID, r.Model)
		}
		artifact := Artifact{
			TraceID:           r.TraceID,
			CandidateModel:    r.Model,
			Trace:             trace,
			ActualFinalAnswer: lastAssistantContent(r.Transcript),
			ActualToolCalls:   extractToolNames(r.Transcript),
			ActualTranscript:  r.Transcript,
			CapturedAt:        now(),
		}
		artifact.ArtifactHash = artifactHash(artifact)
		artifacts = append(artifacts, artifact)
	}
	if len(artifacts) == 0 {
		return fmt.Errorf("calibrate-capture: no artifacts written; %d results returned, %d failed", len(results), failed)
	}
	if failed > 0 {
		// Partial capture is a valid best-effort outcome (the caller can gap-fill
		// the missing pairs), so this is a warning, not an error — but it must be
		// loud: a partial corpus that prints only the success line reads as complete.
		fmt.Fprintf(os.Stderr, "calibrate-capture: WARNING partial capture — wrote %d artifact(s), %d of %d runs failed\n", len(artifacts), failed, len(results))
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutputPath), 0o755); err != nil {
		return fmt.Errorf("calibrate-capture: mkdir: %w", err)
	}
	f, err := os.OpenFile(opts.OutputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("calibrate-capture: open output: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("calibrate-capture: close output: %w", closeErr)
		}
	}()
	enc := json.NewEncoder(f)
	for _, artifact := range artifacts {
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
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load labels: %w", err)
	}
	return matchLabels(labels, arts)
}

// matchLabels partitions already-loaded labels into (matched, stale) against
// the artifact set. A label is "matched" when its ArtifactHash is present in
// arts and "stale" otherwise; stale labels are reported, not errors. Duplicate
// matched ArtifactHash values are rejected so each artifact contributes at most
// once. Kept pure (no I/O) so the paired-report path can load artifacts once
// and retain the full set for lineup/gap detection.
func matchLabels(labels []Label, arts []Artifact) ([]matchedLabel, []Label, error) {
	byHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		byHash[a.ArtifactHash] = a
	}
	var matched []matchedLabel
	var stale []Label
	seenMatched := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		a, ok := byHash[l.ArtifactHash]
		if !ok {
			stale = append(stale, l)
			continue
		}
		if _, ok := seenMatched[l.ArtifactHash]; ok {
			return nil, nil, fmt.Errorf("duplicate label for artifact_hash %q", l.ArtifactHash)
		}
		seenMatched[l.ArtifactHash] = struct{}{}
		matched = append(matched, matchedLabel{Label: l, Artifact: a})
	}
	return matched, stale, nil
}

func loadArtifacts(path string) ([]Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
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
	defer func() { _ = f.Close() }()
	var out []Label
	dec := json.NewDecoder(f)
	for dec.More() {
		var l Label
		if err := dec.Decode(&l); err != nil {
			return nil, fmt.Errorf("decode label: %w", err)
		}
		// This is the single domain guard for every downstream mode (manual,
		// paired, calibration, discrimination): an out-of-domain value aborts the
		// load. KNOWN GAP: an omitted expected_answer_quality field JSON-decodes
		// to 0.0 — a valid score — so a forgotten label is indistinguishable from
		// a deliberate 0.0. Closing it needs a presence signal (*float64) on
		// Label; deferred because it touches the shared label schema.
		if !validExpectedAnswerQuality(l.ExpectedAnswerQuality) {
			return nil, fmt.Errorf("invalid expected_answer_quality for label %s/%s: %.2f (want one of 0.0, 0.5, 1.0)",
				l.TraceID, l.CandidateModel, l.ExpectedAnswerQuality)
		}
		out = append(out, l)
	}
	return out, nil
}

func validExpectedAnswerQuality(v float64) bool {
	return v == 0 || v == 0.5 || v == 1
}

const (
	calibrationAgreementDelta          = 0.25
	calibrationPassThreshold           = 0.85
	calibrationBorderlinePassThreshold = 0.80
	calibrationDefaultMinLabels        = 50
)

type calibrationAgreementMode string

const (
	calibrationAgreementExact     calibrationAgreementMode = "exact"
	calibrationAgreementTolerance calibrationAgreementMode = "tolerance"
)

func normalizeCalibrationAgreementMode(mode calibrationAgreementMode) (calibrationAgreementMode, error) {
	if mode == "" {
		return calibrationAgreementExact, nil
	}
	switch mode {
	case calibrationAgreementExact, calibrationAgreementTolerance:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid calibrate agreement mode %q (want exact or tolerance)", mode)
	}
}

type calibrationSubsetSummary struct {
	Count         int
	AgreeCount    int
	AgreementRate float64
}

type knownFixtureOutcome struct {
	TraceID  string
	Expected float64
	Judge    float64
	Present  bool
	Pass     bool
}

var knownSubtleBugFixtureIDs = []string{"fa-f03", "fa-c05", "fa-g04"}

// normalizeFixtureTraceID maps a captured trace ID to its bare conversation
// id so known-bug fixtures match regardless of the conversationTraceIDPrefix
// that capture prepends (see conversationToTrace).
func normalizeFixtureTraceID(traceID string) string {
	return strings.TrimPrefix(traceID, conversationTraceIDPrefix)
}

// calibrateOptions configures runCalibrate.
//
// MinLabels defaults to calibrationDefaultMinLabels (50) when zero so a
// sufficient sample backs the agreement verdict. StabilityRuns, when >1,
// runs the judge exactly N times per artifact and reports the max-min agreement
// spread as a diagnostic; it does NOT gate the PASS/FAIL verdict. Clock is
// an optional time source for deterministic report filenames.
type calibrateOptions struct {
	LabelsPath    string
	ArtifactsPath string
	Scorer        Scorer
	JudgeModel    string
	JudgeProvider string // provider instance identity for report provenance (e.g. "ollama", "openai-compat:<endpoint-id>")
	ReportDir     string
	MinLabels     int // defaults to calibrationDefaultMinLabels when zero
	StabilityRuns int // when >1, runs the judge exactly N times per artifact and reports max-min spread as a diagnostic (does NOT gate the PASS/FAIL verdict); uses bypass-cache (Task 23)
	AgreementMode calibrationAgreementMode
	Clock         func() time.Time
}

// CalibrationResult is the in-memory summary returned by runCalibrate. The
// markdown report contains the same data plus per-label rows.
type CalibrationResult struct {
	JudgeModel               string
	JudgeProvider            string
	MatchedCount             int
	StaleCount               int
	SelfJudgedSkipCount      int
	AgreeCount               int
	AgreementRate            float64
	AgreementMode            calibrationAgreementMode
	ToleranceAgreeCount      int
	ToleranceAgreementRate   float64
	Overall                  calibrationSubsetSummary
	R1Anchor                 calibrationSubsetSummary
	Borderline               calibrationSubsetSummary
	ClearOne                 calibrationSubsetSummary
	HarshDisagreementCount   int
	LenientDisagreementCount int
	KnownFixtures            map[string]knownFixtureOutcome
	StratifiedGateFailures   []string
	MinLabels                int
	StabilityRuns            int
	Verdict                  string // PASS / FAIL / INSUFFICIENT_LABELS
	ReportPath               string
	PerLabel                 []perLabelOutcome
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
	ToleranceAgree  bool
	LabelNotes      string
	StabilitySpread float64
}

// runCalibrate loads (labels, artifacts), invokes the scorer once per
// matched (label, artifact) pair, and computes the agreement rate against
// human ExpectedAnswerQuality values. The verdict is INSUFFICIENT_LABELS
// when MatchedCount < MinLabels, otherwise PASS only when overall agreement
// reaches calibrationPassThreshold (0.85) and all stratified gates pass.
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
	agreementMode, err := normalizeCalibrationAgreementMode(opts.AgreementMode)
	if err != nil {
		return CalibrationResult{}, err
	}
	if opts.MinLabels == 0 {
		opts.MinLabels = calibrationDefaultMinLabels
	}
	matched, stale, err := loadLabelsMatchedAgainst(opts.LabelsPath, opts.ArtifactsPath)
	if err != nil {
		return CalibrationResult{}, err
	}
	res := CalibrationResult{
		JudgeModel:    opts.JudgeModel,
		JudgeProvider: opts.JudgeProvider,
		StaleCount:    len(stale),
		MinLabels:     opts.MinLabels,
		StabilityRuns: opts.StabilityRuns,
		AgreementMode: agreementMode,
		KnownFixtures: make(map[string]knownFixtureOutcome, len(knownSubtleBugFixtureIDs)),
	}
	for _, id := range knownSubtleBugFixtureIDs {
		res.KnownFixtures[id] = knownFixtureOutcome{TraceID: id, Pass: true}
	}
	for _, m := range matched {
		if sameModelSelector(opts.JudgeModel, m.Artifact.CandidateModel) {
			res.SelfJudgedSkipCount++
			continue
		}
		trace, traceErr := calibrationTraceFromArtifact(m.Artifact)
		if traceErr != nil {
			return CalibrationResult{}, traceErr
		}
		actual := Result{
			Model:      m.Artifact.CandidateModel,
			TraceID:    m.Artifact.TraceID,
			Transcript: m.Artifact.ActualTranscript,
		}
		scorer := calibrationScorer(opts.Scorer, opts.StabilityRuns)
		score, scoreErr := scorer.Score(ctx, trace, actual)
		if scoreErr != nil {
			return CalibrationResult{}, fmt.Errorf("calibrate: scorer for %s/%s: %w",
				m.Artifact.TraceID, m.Artifact.CandidateModel, scoreErr)
		}
		delta := score.AnswerQuality - m.Label.ExpectedAnswerQuality
		if delta < 0 {
			delta = -delta
		}
		exactAgree := score.AnswerQuality == m.Label.ExpectedAnswerQuality
		toleranceAgree := delta <= calibrationAgreementDelta
		agree := exactAgree
		if agreementMode == calibrationAgreementTolerance {
			agree = toleranceAgree
		}
		outcome := perLabelOutcome{
			TraceID:        m.Artifact.TraceID,
			CandidateModel: m.Artifact.CandidateModel,
			Expected:       m.Label.ExpectedAnswerQuality,
			Judge:          score.AnswerQuality,
			Delta:          delta,
			Agree:          agree,
			ToleranceAgree: toleranceAgree,
			LabelNotes:     m.Label.LabelNotes,
		}
		if opts.StabilityRuns > 1 {
			scores := []float64{score.AnswerQuality}
			for i := 1; i < opts.StabilityRuns; i++ {
				extra, sErr := scorer.Score(ctx, trace, actual)
				if sErr != nil {
					return CalibrationResult{}, fmt.Errorf("calibrate: stability run %d for %s/%s: %w",
						i+1, m.Artifact.TraceID, m.Artifact.CandidateModel, sErr)
				}
				scores = append(scores, extra.AnswerQuality)
			}
			outcome.StabilitySpread = maxFloat(scores) - minFloat(scores)
		}
		res.PerLabel = append(res.PerLabel, outcome)
		if outcome.Agree {
			res.AgreeCount++
		}
		if outcome.ToleranceAgree {
			res.ToleranceAgreeCount++
		}
		addSubsetOutcome(&res.Overall, outcome.Agree)
		if labelHasToken(outcome.LabelNotes, "r1-anchor") {
			addSubsetOutcome(&res.R1Anchor, outcome.Agree)
		}
		if outcome.Expected == 0 || outcome.Expected == 0.5 {
			addSubsetOutcome(&res.Borderline, outcome.Agree)
		}
		if outcome.Expected == 1 {
			addSubsetOutcome(&res.ClearOne, outcome.Agree)
		}
		if !outcome.Agree {
			if outcome.Judge < outcome.Expected {
				res.HarshDisagreementCount++
			} else if outcome.Judge > outcome.Expected {
				res.LenientDisagreementCount++
			}
		}
		// Known subtle-bug fixtures gate only the bug instances (expected < 1.0)
		// and match on the normalized (prefix-stripped) trace id. A fixture fails
		// if ANY of its bug artifacts is judged a perfect 1.0; record the most
		// concerning (highest-judge) one for the report.
		if outcome.Expected < 1 {
			normID := normalizeFixtureTraceID(outcome.TraceID)
			if fixture, ok := res.KnownFixtures[normID]; ok {
				if !fixture.Present || outcome.Judge > fixture.Judge {
					fixture.Expected = outcome.Expected
					fixture.Judge = outcome.Judge
				}
				fixture.Present = true
				if outcome.Judge == 1 {
					fixture.Pass = false
				}
				res.KnownFixtures[normID] = fixture
			}
		}
		res.MatchedCount++
	}
	if res.MatchedCount > 0 {
		res.AgreementRate = float64(res.AgreeCount) / float64(res.MatchedCount)
		res.ToleranceAgreementRate = float64(res.ToleranceAgreeCount) / float64(res.MatchedCount)
	}
	finalizeSubset(&res.Overall)
	finalizeSubset(&res.R1Anchor)
	finalizeSubset(&res.Borderline)
	finalizeSubset(&res.ClearOne)
	res.StratifiedGateFailures = calibrationStratifiedGateFailures(res)
	switch {
	case res.MatchedCount < opts.MinLabels:
		res.Verdict = "INSUFFICIENT_LABELS"
	case res.AgreementRate < calibrationPassThreshold:
		res.Verdict = "FAIL"
	case len(res.StratifiedGateFailures) > 0:
		res.Verdict = "FAIL"
	default:
		res.Verdict = "PASS"
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

func labelHasToken(notes, token string) bool {
	for _, field := range strings.FieldsFunc(strings.ToLower(notes), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if field == token {
			return true
		}
	}
	return false
}

func addSubsetOutcome(s *calibrationSubsetSummary, agree bool) {
	s.Count++
	if agree {
		s.AgreeCount++
	}
}

func finalizeSubset(s *calibrationSubsetSummary) {
	if s.Count > 0 {
		s.AgreementRate = float64(s.AgreeCount) / float64(s.Count)
	}
}

func calibrationStratifiedGateFailures(res CalibrationResult) []string {
	var failures []string
	if res.Borderline.Count > 0 && res.Borderline.AgreementRate < calibrationBorderlinePassThreshold {
		failures = append(failures, fmt.Sprintf("borderline/fail agreement %.0f%% below %.0f%% threshold (%d/%d)",
			res.Borderline.AgreementRate*100, calibrationBorderlinePassThreshold*100, res.Borderline.AgreeCount, res.Borderline.Count))
	}
	for _, id := range knownSubtleBugFixtureIDs {
		fixture := res.KnownFixtures[id]
		if fixture.Present && !fixture.Pass {
			failures = append(failures, fmt.Sprintf("known subtle-bug fixture %s judged 1.0", id))
		}
	}
	return failures
}

func calibrationScorer(s Scorer, stabilityRuns int) Scorer {
	if stabilityRuns <= 1 {
		return s
	}
	if cs, ok := s.(*LLMJudgeScorer); ok {
		clone := *cs
		clone.BypassCache = true
		return &clone
	}
	return s
}

func calibrationTraceFromArtifact(a Artifact) (Trace, error) {
	trace := a.Trace
	if strings.TrimSpace(trace.ID) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace context", a.TraceID, a.CandidateModel)
	}
	if trace.ID != a.TraceID {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s trace ID mismatch: %q", a.TraceID, a.CandidateModel, trace.ID)
	}
	if strings.TrimSpace(trace.System) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace system prompt", a.TraceID, a.CandidateModel)
	}
	if len(trace.Turns) == 0 {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace turns", a.TraceID, a.CandidateModel)
	}
	if strings.TrimSpace(trace.Golden.FinalAnswerCriteria) == "" && strings.TrimSpace(trace.Golden.FinalAnswerSubstring) == "" {
		return Trace{}, fmt.Errorf("calibrate: artifact %s/%s missing original trace golden rubric", a.TraceID, a.CandidateModel)
	}
	return trace, nil
}

// slugifyModel makes a path-safe slug from a model selector by replacing
// '/' and ':' with '_'. Example: "ollama/gemma4:31b" -> "ollama_gemma4_31b".
func slugifyModel(s string) string {
	out := strings.ReplaceAll(s, "/", "_")
	return strings.ReplaceAll(out, ":", "_")
}

// writeCalibrationReport emits a markdown calibration report to dir with a
// slugified, timestamped filename and returns the report path. Clock is the
// time source used to stamp the filename and report header; defaults to
// time.Now().UTC() when nil.
func writeCalibrationReport(dir, judgeModel string, res CalibrationResult, clock func() time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", dir, err)
	}
	now := time.Now().UTC()
	if clock != nil {
		now = clock().UTC()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Judge calibration — %s — %s\n\n", judgeModel, now.Format("2006-01-02"))
	if provider := strings.TrimSpace(res.JudgeProvider); provider != "" {
		fmt.Fprintf(&b, "Judge provider: %s\n", provider)
	}
	fmt.Fprintf(&b, "Matched labels: %d / %d required → %s\n", res.MatchedCount, res.MinLabels, sufficiencyLabel(res))
	fmt.Fprintf(&b, "Stale labels: %d\n", res.StaleCount)
	fmt.Fprintf(&b, "Self-judged labels skipped: %d\n", res.SelfJudgedSkipCount)
	fmt.Fprintf(&b, "Agreement: %d / %d (%.0f%%) → **%s**\n\n",
		res.AgreeCount, res.MatchedCount, res.AgreementRate*100, res.Verdict)
	fmt.Fprintf(&b, "Agreement mode: %s\n", res.AgreementMode)
	fmt.Fprintf(&b, "Old tolerance diagnostic: %d / %d (%.0f%%) would have agreed under |judge - expected| <= %.2f\n\n",
		res.ToleranceAgreeCount, res.MatchedCount, res.ToleranceAgreementRate*100, calibrationAgreementDelta)
	fmt.Fprintln(&b, "| subset | agreement |")
	fmt.Fprintln(&b, "|---|---|")
	fmt.Fprintf(&b, "| overall | %s |\n", formatSubsetAgreement(res.Overall))
	fmt.Fprintf(&b, "| R1-60 anchor | %s |\n", formatSubsetAgreement(res.R1Anchor))
	fmt.Fprintf(&b, "| Borderline/fail subset | %s |\n", formatSubsetAgreement(res.Borderline))
	fmt.Fprintf(&b, "| clear-1.0 | %s |\n", formatSubsetAgreement(res.ClearOne))
	fmt.Fprintf(&b, "\nHarsh disagreements: %d\n", res.HarshDisagreementCount)
	fmt.Fprintf(&b, "Lenient disagreements: %d\n\n", res.LenientDisagreementCount)
	if len(res.StratifiedGateFailures) == 0 {
		fmt.Fprintln(&b, "Stratified gate failures: none")
		fmt.Fprintln(&b)
	} else {
		fmt.Fprintln(&b, "Stratified gate failures:")
		for _, failure := range res.StratifiedGateFailures {
			fmt.Fprintf(&b, "- %s\n", failure)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintln(&b, "## Known subtle-bug fixtures")
	fmt.Fprintln(&b, "| trace_id | expected | judge | roll-call |")
	fmt.Fprintln(&b, "|---|---|---|---|")
	for _, id := range knownSubtleBugFixtureIDs {
		f := res.KnownFixtures[id]
		status := "MISSING"
		if f.Present && f.Pass {
			status = "PASS"
		} else if f.Present {
			status = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %.2f | %.2f | %s |\n", id, f.Expected, f.Judge, status)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| trace_id | candidate | expected | judge | Δ | agree | stability |")
	fmt.Fprintln(&b, "|---|---|---|---|---|---|---|")
	for _, p := range res.PerLabel {
		agree := "✗"
		if p.Agree {
			agree = "✓"
		}
		stability := "n/a"
		if res.StabilityRuns > 1 {
			stability = fmt.Sprintf("spread=%.2f", p.StabilitySpread)
		}
		fmt.Fprintf(&b, "| %s | %s | %.2f | %.2f | %.2f | %s | %s |\n",
			p.TraceID, p.CandidateModel, p.Expected, p.Judge, p.Delta, agree, stability)
	}
	f, path, err := createCalibrationReportFile(dir, judgeModel, now)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close %q: %w", path, err)
	}
	return path, nil
}

func formatSubsetAgreement(s calibrationSubsetSummary) string {
	if s.Count == 0 {
		return "0 / 0 (n/a)"
	}
	return fmt.Sprintf("%d / %d (%.0f%%)", s.AgreeCount, s.Count, s.AgreementRate*100)
}

func createCalibrationReportFile(dir, judgeModel string, now time.Time) (*os.File, string, error) {
	for attempt := 0; ; attempt++ {
		path := calibrationReportPath(dir, judgeModel, now, attempt)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return f, path, nil
		}
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, "", fmt.Errorf("create %q: %w", path, err)
	}
}

func calibrationReportPath(dir, judgeModel string, now time.Time, attempt int) string {
	stem := fmt.Sprintf("%s-%s", now.Format("2006-01-02T150405Z"), slugifyModel(judgeModel))
	if attempt > 0 {
		stem = fmt.Sprintf("%s-%d", stem, attempt+1)
	}
	return filepath.Join(dir, stem+".md")
}

// sufficiencyLabel renders the matched-vs-required label-count state as a
// short string for the report header.
func sufficiencyLabel(res CalibrationResult) string {
	if res.MatchedCount >= res.MinLabels {
		return "SUFFICIENT"
	}
	return "INSUFFICIENT"
}

func maxFloat(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func minFloat(vs []float64) float64 {
	m := vs[0]
	for _, v := range vs[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
