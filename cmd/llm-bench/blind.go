package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// blindFillMarker delimits the human-fill region inside a worksheet block. The
// ingest parser only reads score:/notes: lines that appear AFTER this marker,
// so candidate output text that happens to contain "score:" cannot be
// misparsed.
const blindFillMarker = "--- fill below (score: 0 | 0.5 | 1) ---"

// blindEndMarker terminates a worksheet block; the human-fill region runs
// from blindFillMarker to this line. Renderer and ingest parser must agree on
// this literal, so it lives in one place.
const blindEndMarker = "=== END ==="

// ingestBlindWorksheet parses a filled blind worksheet into Labels, rejoining
// trace_id + candidate_model from arts on artifact_hash (R-D3 — the untouched
// artifacts file is the join key, so no separate restore map). Blocks with a
// blank score are skipped (partial labeling allowed) and counted. A score
// outside {0, 0.5, 1.0}, duplicate worksheet block, or hash absent from arts is
// a loud error. labeler and labeled_at are stamped on every emitted label.
//
// Note: the block parser splits on "=== ARTIFACT ". A candidate-output line
// that itself begins "=== ARTIFACT " could trigger a spurious block boundary.
// The unknown-artifact_hash loud error (hash not in arts → error) is the
// designed backstop for this edge case.
func ingestBlindWorksheet(worksheet string, arts []Artifact, labeler string) (labels []Label, skipped int, err error) {
	artByHash := make(map[string]Artifact, len(arts))
	for _, a := range arts {
		artByHash[a.ArtifactHash] = a
	}

	var hash string
	var score, notes string
	afterMarker := false
	seenWorksheetHash := map[string]struct{}{}
	flush := func() error {
		if hash == "" {
			return nil
		}
		defer func() {
			hash, score, notes, afterMarker = "", "", "", false
		}()
		if _, dup := seenWorksheetHash[hash]; dup {
			return fmt.Errorf("worksheet contains duplicate artifact_hash %q", hash)
		}
		seenWorksheetHash[hash] = struct{}{}
		if strings.TrimSpace(score) == "" {
			skipped++
			return nil
		}
		q, perr := parseBlindScore(score)
		if perr != nil {
			return fmt.Errorf("artifact %s: %w", hash, perr)
		}
		a, ok := artByHash[hash]
		if !ok {
			return fmt.Errorf("worksheet references unknown artifact_hash %q (not in -artifacts)", hash)
		}
		labels = append(labels, Label{
			TraceID:               a.TraceID,
			CandidateModel:        a.CandidateModel,
			ArtifactHash:          hash,
			ExpectedAnswerQuality: q,
			LabelNotes:            strings.TrimSpace(notes),
			LabeledAt:             time.Now().UTC(),
			Labeler:               labeler,
		})
		return nil
	}

	for _, line := range strings.Split(worksheet, "\n") {
		switch {
		case strings.HasPrefix(line, "=== ARTIFACT "):
			if ferr := flush(); ferr != nil {
				return nil, 0, ferr
			}
			hash = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "=== ARTIFACT "), " ==="))
			score, notes, afterMarker = "", "", false
		case strings.HasPrefix(line, blindFillMarker):
			afterMarker = true
		case strings.HasPrefix(line, blindEndMarker):
			// The fill region ends at the block terminator: a stray score:/notes:
			// line between "=== END ===" and the next block must not attach to
			// the previous block (it would be reported as unscored instead).
			afterMarker = false
		case afterMarker && strings.HasPrefix(line, "score:"):
			score = strings.TrimSpace(strings.TrimPrefix(line, "score:"))
		case afterMarker && strings.HasPrefix(line, "notes:"):
			notes = strings.TrimSpace(strings.TrimPrefix(line, "notes:"))
		}
	}
	if ferr := flush(); ferr != nil {
		return nil, 0, ferr
	}
	return labels, skipped, nil
}

// parseBlindScore accepts only the three legal label values.
func parseBlindScore(s string) (float64, error) {
	switch strings.TrimSpace(s) {
	case "0", "0.0":
		return 0, nil
	case "0.5":
		return 0.5, nil
	case "1", "1.0":
		return 1, nil
	default:
		return 0, fmt.Errorf("invalid score %q (want 0, 0.5, or 1)", s)
	}
}

// renderBlindWorksheet emits one fill-in block per artifact for blind labeling
// (R-D3): the trace prompt, committed rubric, and candidate final answer are
// shown, but the model identity is withheld. Blocks are ordered by
// (trace_id, artifact_hash) so all candidates for one prompt sit together while
// their order is model-independent. The labeler fills score:/notes:;
// -blind-ingest then rejoins the true model on artifact_hash from the untouched
// artifacts file.
//
// Worksheet block contract (shared with ingestBlindWorksheet): "=== ARTIFACT
// <hash> ===" opens a block and "=== END ===" closes it; the human-fill region
// is gated by blindFillMarker. score:/notes: are read only after the marker, so
// those tokens inside candidate output are safely ignored. The renderer cannot
// stop a model answer from itself containing an "=== ARTIFACT " line, so the
// ingest parser is responsible for not mis-splitting on a sentinel embedded in
// candidate output; its unknown-artifact_hash check is the loud backstop.
func renderBlindWorksheet(arts []Artifact) string {
	ordered := make([]Artifact, len(arts))
	copy(ordered, arts)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TraceID != ordered[j].TraceID {
			return ordered[i].TraceID < ordered[j].TraceID
		}
		return ordered[i].ArtifactHash < ordered[j].ArtifactHash
	})

	var b strings.Builder
	fmt.Fprintln(&b, "# llm-bench — blind labeling worksheet")
	fmt.Fprintln(&b, "#")
	fmt.Fprintln(&b, "# Score each candidate output from the text alone. Model identity is hidden.")
	fmt.Fprintln(&b, "# For each block, fill the score: line with 0, 0.5, or 1, and notes: optionally.")
	fmt.Fprintln(&b, "# Leave score: blank to skip a block (it will be reported as unscored, not labeled).")
	fmt.Fprintln(&b, "# Then run: llm-bench -blind-ingest -worksheet <this file> -artifacts <artifacts.jsonl> -labels-out <labels.jsonl>")
	fmt.Fprintln(&b)
	for _, a := range ordered {
		fmt.Fprintf(&b, "=== ARTIFACT %s ===\n", a.ArtifactHash)
		fmt.Fprintf(&b, "trace: %s\n\n", a.TraceID)
		fmt.Fprintln(&b, "[prompt]")
		fmt.Fprintln(&b, strings.TrimSpace(a.Trace.System))
		for _, turn := range a.Trace.Turns {
			content := strings.TrimSpace(turn.Content)
			if content == "" {
				continue
			}
			fmt.Fprintf(&b, "\n<%s>\n%s\n", turn.Role, content)
		}
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[rubric]")
		fmt.Fprintln(&b, strings.TrimSpace(a.Trace.Golden.FinalAnswerCriteria))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[candidate output]")
		fmt.Fprintln(&b, strings.TrimSpace(a.ActualFinalAnswer))
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, blindFillMarker)
		fmt.Fprintln(&b, "score: ")
		fmt.Fprintln(&b, "notes: ")
		fmt.Fprintln(&b, blindEndMarker)
		fmt.Fprintln(&b)
	}
	return redactPaths(b.String())
}

// writeLabelsJSONL writes labels as JSONL (one Label per line). LabeledAt
// stamping is the caller's responsibility (ingestBlindWorksheet already stamps
// it). Mirrors the artifacts/manifest writers.
func writeLabelsJSONL(path string, labels []Label) (retErr error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("labels: mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("labels: open output: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); retErr == nil && closeErr != nil {
			retErr = fmt.Errorf("labels: close output: %w", closeErr)
		}
	}()
	enc := json.NewEncoder(f)
	for _, l := range labels {
		if err := enc.Encode(l); err != nil {
			return fmt.Errorf("labels: encode %q: %w", l.ArtifactHash, err)
		}
	}
	return nil
}
