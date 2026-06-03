package main

import (
	"context"
	"fmt"
	"sort"
)

// ManualScorer scores AnswerQuality from human labels rather than an LLM judge
// or substring match. The quality is the expected_answer_quality the labeler
// recorded for the (trace, candidate-model) pair — the gold-standard,
// deterministic, on-box quality signal: no judge, no model call, nothing leaves
// the box. Tool metrics are computed from the actual transcript exactly as the
// other scorers do.
//
// It is the trustworthy basis for a first accepted run: the labels ARE the
// human judgment the LLM judge was only ever trying to approximate.
type ManualScorer struct {
	labels map[manualLabelKey]manualLabel
}

type manualLabelKey struct {
	traceID string
	model   string
}

type manualLabel struct {
	quality float64
	notes   string
}

// manualScorerKey canonicalizes a (traceID, model) pair so labels stored bare
// (e.g. "qwen3:8b") match a run's actual.Model regardless of bench-provider
// prefix or casing.
func manualScorerKey(traceID, model string) manualLabelKey {
	return manualLabelKey{
		traceID: normalizeModelSelector(traceID),
		model:   normalizeModelSelector(modelSelectorWithoutBenchProvider(model)),
	}
}

// newManualScorer indexes the labels by canonical (trace, model) key. A later
// label for the same key wins, matching last-write-wins JSONL semantics.
func newManualScorer(labels []Label) *ManualScorer {
	m := make(map[manualLabelKey]manualLabel, len(labels))
	for _, l := range labels {
		m[manualScorerKey(l.TraceID, l.CandidateModel)] = manualLabel{quality: l.ExpectedAnswerQuality, notes: l.LabelNotes}
	}
	return &ManualScorer{labels: m}
}

// Score implements Scorer. AnswerQuality is the human label; a missing label is
// a loud error (never a silent 0) so coverage gaps in a human-judged run are
// visible rather than scored as failures.
func (s *ManualScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	label, ok := s.labels[manualScorerKey(trace.ID, actual.Model)]
	if !ok {
		return Score{}, fmt.Errorf("manual scorer: no human label for trace %q model %q", trace.ID, actual.Model)
	}

	toolArgsScore, toolArgsComputed, toolArgsNotes, schemaErr := scoreToolArguments(trace, actual.Transcript)
	if schemaErr != nil {
		return Score{}, fmt.Errorf("trace %q: compile tool schemas: %w", trace.ID, schemaErr)
	}

	return Score{
		ToolSequenceMatch:     toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		ToolArgsValid:         toolArgsScore,
		ToolArgsValidComputed: toolArgsComputed,
		AnswerQuality:         label.quality,
		Notes:                 joinScoreNotes(toolArgsNotes, fmt.Sprintf("manual-label: %s", label.notes)),
	}, nil
}

// runManualReport scores every frozen labeled artifact with the human labels
// and returns a per-model quality baseline report. It reads the same
// (labels, artifacts) pair the calibration flow uses, so the scored outputs
// are exactly the ones the human judged — no fresh replay, no judge.
func runManualReport(ctx context.Context, labelsPath, artifactsPath string) (string, error) {
	matched, _, err := loadLabelsMatchedAgainst(labelsPath, artifactsPath)
	if err != nil {
		return "", err
	}
	if len(matched) == 0 {
		return "", fmt.Errorf("no labels matched artifacts in %q / %q", labelsPath, artifactsPath)
	}
	labels, err := loadLabels(labelsPath)
	if err != nil {
		return "", err
	}
	scorer := newManualScorer(labels)

	results := make([]Result, 0, len(matched))
	modelSeen := map[string]bool{}
	models := make([]string, 0)
	for _, m := range matched {
		trace, traceErr := calibrationTraceFromArtifact(m.Artifact)
		if traceErr != nil {
			return "", traceErr
		}
		actual := Result{Model: m.Artifact.CandidateModel, TraceID: m.Artifact.TraceID, Transcript: m.Artifact.ActualTranscript}
		score, scoreErr := scorer.Score(ctx, trace, actual)
		results = append(results, Result{Model: m.Artifact.CandidateModel, TraceID: m.Artifact.TraceID, Score: score, Err: scoreErr})
		if !modelSeen[m.Artifact.CandidateModel] {
			modelSeen[m.Artifact.CandidateModel] = true
			models = append(models, m.Artifact.CandidateModel)
		}
	}
	sort.Strings(models)
	return formatManualQualityReport(models, results), nil
}
