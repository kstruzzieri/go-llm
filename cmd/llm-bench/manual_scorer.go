package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
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
		model:   normalizeModelSelector(modelSelectorWithoutDefaultBenchProvider(model)),
	}
}

// newManualScorer indexes the labels by canonical (trace, model) key. Duplicate
// labels for a key are rejected because the live replay path has no artifact
// hash to bind the score to a specific labeled output.
func newManualScorer(labels []Label) (*ManualScorer, error) {
	m := make(map[manualLabelKey]manualLabel, len(labels))
	seen := make(map[manualLabelKey]string, len(labels))
	for _, l := range labels {
		key := manualScorerKey(l.TraceID, l.CandidateModel)
		source := manualLabelSource(l)
		if prev, ok := seen[key]; ok {
			return nil, fmt.Errorf("manual scorer: ambiguous labels for (trace, model) key %q/%q: %s and %s", key.traceID, key.model, prev, source)
		}
		seen[key] = source
		m[key] = manualLabel{quality: l.ExpectedAnswerQuality, notes: l.LabelNotes}
	}
	return &ManualScorer{labels: m}, nil
}

func manualLabelSource(l Label) string {
	source := fmt.Sprintf("trace %q model %q", l.TraceID, l.CandidateModel)
	if hash := strings.TrimSpace(l.ArtifactHash); hash != "" {
		source += fmt.Sprintf(" artifact_hash %q", hash)
	}
	return source
}

// Score implements Scorer. AnswerQuality is the human label; a missing label is
// a loud error (never a silent 0) so coverage gaps in a human-judged run are
// visible rather than scored as failures.
func (s *ManualScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	label, ok := s.labels[manualScorerKey(trace.ID, actual.Model)]
	if !ok {
		return Score{}, fmt.Errorf("manual scorer: no human label for trace %q model %q", trace.ID, actual.Model)
	}

	score, schemaErr := baseMechanicalScore(trace, actual.Transcript)
	if schemaErr != nil {
		return Score{}, fmt.Errorf("trace %q: compile tool schemas: %w", trace.ID, schemaErr)
	}
	score.AnswerQuality = label.quality
	score.Notes = joinScoreNotes(score.Notes, fmt.Sprintf("manual-label: %s", label.notes))
	return score, nil
}

// manualReportCoverage records how completely the report covers the labels, so
// a citable artifact never silently under-counts: Stale labels (hash no longer
// matches an artifact) and Errored scorings are surfaced rather than dropped.
type manualReportCoverage struct {
	Scored  int
	Stale   int
	Errored int
}

// runManualReport scores every frozen labeled artifact with the human labels
// and returns a per-model quality baseline report. It reads the same
// (labels, artifacts) pair the calibration flow uses, so the scored outputs
// are exactly the ones the human judged — no fresh replay, no judge. Stale
// labels and scoring errors are surfaced in the coverage line; a duplicate
// (trace, model) is a hard error because the manual scorer keys on it.
//
// When filter is non-nil the manifest is applied to drop judge-validation and
// non-model-evidence rows before scoring (R-C1). The artifact-hash rejoin
// (R-D3) backfills TraceID and CandidateModel from the matched Artifact onto
// every label before building the scorer, so blind labels (artifact_hash only,
// no candidate_model) score correctly.
func runManualReport(ctx context.Context, labelsPath, artifactsPath string, filter *corpusFilter) (string, error) {
	matched, stale, err := loadLabelsMatchedAgainst(labelsPath, artifactsPath)
	if err != nil {
		return "", err
	}
	if len(matched) == 0 {
		return "", fmt.Errorf("no labels matched artifacts in %q / %q", labelsPath, artifactsPath)
	}

	// R-C1: drop judge-validation and non-model-evidence rows using the same
	// machinery as the live-replay path. R-D3: the manifest is keyed on trace
	// IDs, which the matched artifacts carry, so no separate restore map.
	// The exclusions return is intentionally discarded: with the manifest as the
	// authoritative evidence set, a matched artifact whose trace is absent from
	// the manifest (excl.Unclassified) is dropped on purpose. The inverse — a
	// manifest-selected *evidence* trace absent from the artifacts — is the loud
	// failure below, which Round 2A's contract test also guards; a missing
	// non-evidence trace (the canary) is excluded and noted in the report.
	var corpusNote string
	if filter != nil {
		keep, data, _ := corpusEvidenceFilter(filter.Manifest, filter.Selection, tracesFromArtifacts(matchedArtifacts(matched)))
		if data != nil {
			evidence, nonEvidence := splitMissingByEvidence(filter.Manifest, data.MissingSelected)
			if len(evidence) > 0 {
				return "", fmt.Errorf("corpus selection missing %d selected evidence trace(s) from matched artifacts: %v", len(evidence), evidence)
			}
			corpusNote = missingCorpusNote(nonEvidence)
		}
		matched = filterMatchedByTrace(matched, keep)
		// Filter stale to the same selection so the coverage "stale" count
		// reflects the reported partition, matching runPairedReport (otherwise a
		// partition-scoped manual report overstates stale labels from other
		// partitions).
		stale = filterLabelsByTrace(stale, keep)
		if len(matched) == 0 {
			return "", fmt.Errorf("corpus selection matched no labeled artifacts (partitions=%v categories=%v sources=%v only-evidence=%v) — check for a typo in the selection",
				filter.Selection.Partitions, filter.Selection.Categories, filter.Selection.Sources, filter.Selection.OnlyModelEvidence)
		}
	}

	// R-D3 rejoin: the label may be blind (no candidate_model / trace_id); the
	// matched Artifact is the authoritative identity. Backfill both before
	// keying the scorer, so blind labels score and prefixed/cased labels stay
	// consistent. Guard against two artifacts colliding on (trace, model).
	seen := make(map[manualLabelKey]string, len(matched))
	labels := make([]Label, 0, len(matched))
	for _, m := range matched {
		lbl := m.Label
		lbl.TraceID = m.Artifact.TraceID
		lbl.CandidateModel = m.Artifact.CandidateModel
		key := manualScorerKey(lbl.TraceID, lbl.CandidateModel)
		id := lbl.TraceID + "/" + lbl.CandidateModel
		if prev, ok := seen[key]; ok {
			return "", fmt.Errorf("manual report: %s and %s map to the same (trace, model); one artifact per (trace, model) is required", prev, id)
		}
		seen[key] = id
		labels = append(labels, lbl)
	}
	scorer, err := newManualScorer(labels)
	if err != nil {
		return "", err
	}

	results := make([]Result, 0, len(matched))
	modelSeen := map[string]bool{}
	models := make([]string, 0)
	errored := 0
	for _, m := range matched {
		trace, traceErr := calibrationTraceFromArtifact(m.Artifact)
		if traceErr != nil {
			return "", traceErr
		}
		actual := Result{Model: m.Artifact.CandidateModel, TraceID: m.Artifact.TraceID, Transcript: m.Artifact.ActualTranscript}
		score, scoreErr := scorer.Score(ctx, trace, actual)
		if scoreErr != nil {
			errored++
		}
		results = append(results, Result{Model: m.Artifact.CandidateModel, TraceID: m.Artifact.TraceID, Score: score, Err: scoreErr})
		if !modelSeen[m.Artifact.CandidateModel] {
			modelSeen[m.Artifact.CandidateModel] = true
			models = append(models, m.Artifact.CandidateModel)
		}
	}
	sort.Strings(models)
	cov := manualReportCoverage{Scored: len(matched) - errored, Stale: len(stale), Errored: errored}
	return formatManualQualityReport(models, results, cov) + corpusNote, nil
}

// matchedArtifacts extracts the Artifact from each matched label.
func matchedArtifacts(ms []matchedLabel) []Artifact {
	out := make([]Artifact, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Artifact)
	}
	return out
}

// filterMatchedByTrace keeps only matched labels whose artifact trace ID is in
// keep (the corpus evidence selection).
func filterMatchedByTrace(ms []matchedLabel, keep map[string]struct{}) []matchedLabel {
	out := make([]matchedLabel, 0, len(ms))
	for _, m := range ms {
		if _, ok := keep[m.Artifact.TraceID]; ok {
			out = append(out, m)
		}
	}
	return out
}
