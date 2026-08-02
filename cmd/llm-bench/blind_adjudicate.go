package main

// Grounding adjudication pass (#331 slice 3c): labels flagged grounding-check
// during the promptless primary pass get a second, prompt-visible look.
// -adjudicate-render shows the FULL prompt for each flagged label with the
// primary score pre-filled; -adjudicate-ingest re-emits the complete label set
// with every flagged label adjudicated (score kept or corrected, notes
// rewritten with the required reason). Unflagged labels pass through untouched.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// groundingCheckFlag is the only legal non-blank flag: value on a promptless
// worksheet block; groundingCheckPrefix is what it becomes on the emitted
// label's notes, marking the label as pending adjudication.
const (
	groundingCheckFlag   = "grounding-check"
	groundingCheckPrefix = groundingCheckFlag + ";"
)

// flaggedForAdjudication reports whether a primary-pass label was flagged
// grounding-check (the adjudication selector).
func flaggedForAdjudication(l Label) bool {
	return strings.HasPrefix(l.LabelNotes, groundingCheckPrefix)
}

// formatBlindScore renders a {0, 0.5, 1} score the way the worksheets spell
// it: "0", "0.5", "1".
func formatBlindScore(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// renderAdjudicationWorksheet emits one block per grounding-check-flagged
// label, sorted by artifact hash: the FULL prompt (same body as the
// non-promptless blind worksheet — the trace line stays omitted because
// assembly trace IDs encode the arm), the question/rubric/candidate output,
// the score: line pre-filled with the primary score, and a REQUIRED reason:
// line. No flagged labels, a duplicate flagged hash, or a flagged label
// without a matching artifact is a loud error.
func renderAdjudicationWorksheet(arts []Artifact, labels []Label) (string, error) {
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}
	var flagged []Label
	seen := map[string]struct{}{}
	for _, l := range labels {
		if !flaggedForAdjudication(l) {
			continue
		}
		if _, dup := seen[l.ArtifactHash]; dup {
			return "", fmt.Errorf("adjudication: duplicate flagged label for artifact_hash %q", l.ArtifactHash)
		}
		seen[l.ArtifactHash] = struct{}{}
		flagged = append(flagged, l)
	}
	if len(flagged) == 0 {
		return "", fmt.Errorf("adjudication: no labels carry the %q notes prefix; nothing to adjudicate", groundingCheckPrefix)
	}
	sort.Slice(flagged, func(i, j int) bool { return flagged[i].ArtifactHash < flagged[j].ArtifactHash })

	var b strings.Builder
	fmt.Fprintln(&b, "# llm-bench — grounding adjudication worksheet")
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# Every block below was flagged grounding-check in the promptless primary pass.")
	fmt.Fprintln(&b, "# The full prompt is now shown. Keep or correct the pre-filled score: line and")
	fmt.Fprintln(&b, "# fill the reason: line — a reason is REQUIRED on every block.")
	fmt.Fprintln(&b, "# Then run: llm-bench -adjudicate-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -labels <labels.jsonl> -labels-out <adjudicated.jsonl>")
	fmt.Fprintln(&b)
	for _, l := range flagged {
		a, ok := artByHash[l.ArtifactHash]
		if !ok {
			return "", fmt.Errorf("adjudication: flagged label %q has no matching artifact in -artifacts", l.ArtifactHash)
		}
		fmt.Fprintf(&b, "=== ARTIFACT %s ===\n", a.ArtifactHash)
		writeBlindPromptBody(&b, a.Trace)
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[question]")
		fmt.Fprintln(&b, strings.TrimSpace(blindQuestion(a.Trace)))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[rubric]")
		fmt.Fprintln(&b, strings.TrimSpace(a.Trace.Golden.FinalAnswerCriteria))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[candidate output]")
		fmt.Fprintln(&b, strings.TrimSpace(a.ActualFinalAnswer))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, blindFillMarker)
		fmt.Fprintf(&b, "score: %s\n", formatBlindScore(l.ExpectedAnswerQuality))
		fmt.Fprintln(&b, "reason: ")
		fmt.Fprintln(&b, blindEndMarker)
		fmt.Fprintln(&b)
	}
	return redactPaths(b.String()), nil
}

// ingestAdjudicationWorksheet applies a filled adjudication worksheet to the
// primary label set and returns the FULL updated set: flagged labels get their
// score replaced when changed and their notes rewritten to
// "adjudicated(<old>-><new>): <reason>" (or "adjudicated(kept): <reason>");
// unflagged labels pass through untouched. Loud errors: a worksheet block for
// a hash that is not a flagged label (or not an artifact), a missing or
// invalid score, a missing reason, a duplicate block, or a flagged label
// absent from the worksheet — adjudication must be complete.
//
// Adjudicated rows deliberately keep the primary pass's Labeler and
// LabeledAt: this slice's protocol has ONE person running both passes, so a
// separate identity/stamp would duplicate the same value. Before a second
// adjudicator can be supported, Label needs distinct adjudication provenance
// fields (adjudicator, adjudicated_at) — do not overload Labeler for that.
func ingestAdjudicationWorksheet(worksheet string, arts []Artifact, labels []Label) ([]Label, error) {
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}
	flaggedByHash := map[string]struct{}{}
	for _, l := range labels {
		if !flaggedForAdjudication(l) {
			continue
		}
		if _, dup := flaggedByHash[l.ArtifactHash]; dup {
			return nil, fmt.Errorf("adjudication: duplicate flagged label for artifact_hash %q", l.ArtifactHash)
		}
		flaggedByHash[l.ArtifactHash] = struct{}{}
	}

	type verdict struct {
		score  float64
		reason string
	}
	verdicts := map[string]verdict{}
	var hash, score, reason string
	flush := func() error {
		if hash == "" {
			return nil
		}
		defer func() { hash, score, reason = "", "", "" }()
		if _, dup := verdicts[hash]; dup {
			return fmt.Errorf("adjudication worksheet: duplicate block for artifact_hash %q", hash)
		}
		if _, ok := artByHash[hash]; !ok {
			return fmt.Errorf("adjudication worksheet: unknown artifact_hash %q (not in -artifacts)", hash)
		}
		if _, ok := flaggedByHash[hash]; !ok {
			return fmt.Errorf("adjudication worksheet: artifact_hash %q is not a grounding-check-flagged label", hash)
		}
		q, err := parseBlindScore(score)
		if err != nil {
			return fmt.Errorf("adjudication artifact %s: %w", hash, err)
		}
		r := strings.TrimSpace(reason)
		if r == "" {
			return fmt.Errorf("adjudication artifact %s: reason: is required on every adjudicated block", hash)
		}
		verdicts[hash] = verdict{score: q, reason: r}
		return nil
	}

	// Adjudication blocks stay hash-addressed by design: this pass reveals
	// the full prompt anyway, so an opaque id would hide nothing.
	grammar := worksheetGrammar{headerPrefixes: []string{blindArtifactHeaderPrefix}, fillMarker: blindFillMarker, fields: []string{"score", "reason"}}
	err := scanWorksheetBlocks(worksheet, grammar,
		func(_, body string) error {
			hash, score, reason = body, "", ""
			return nil
		},
		func(field, value string) {
			if field == "score" {
				score = value
			} else {
				reason = value
			}
		},
		flush)
	if err != nil {
		return nil, err
	}

	out := make([]Label, 0, len(labels))
	for _, l := range labels {
		if !flaggedForAdjudication(l) {
			out = append(out, l)
			continue
		}
		v, ok := verdicts[l.ArtifactHash]
		if !ok {
			return nil, fmt.Errorf("adjudication: flagged label %q is missing from the worksheet (adjudication must be complete)", l.ArtifactHash)
		}
		updated := l
		if v.score == l.ExpectedAnswerQuality {
			updated.LabelNotes = fmt.Sprintf("adjudicated(kept): %s", v.reason)
		} else {
			updated.LabelNotes = fmt.Sprintf("adjudicated(%s->%s): %s",
				formatBlindScore(l.ExpectedAnswerQuality), formatBlindScore(v.score), v.reason)
			updated.ExpectedAnswerQuality = v.score
		}
		out = append(out, updated)
	}
	return out, nil
}
