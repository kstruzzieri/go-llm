package main

import (
	"fmt"
	"sort"
	"strings"
)

// blindFillMarker delimits the human-fill region inside a worksheet block. The
// ingest parser only reads score:/notes: lines that appear AFTER this marker,
// so candidate output text that happens to contain "score:" cannot be
// misparsed.
const blindFillMarker = "--- fill below (score: 0 | 0.5 | 1) ---"

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
		fmt.Fprintln(&b, "=== END ===")
		fmt.Fprintln(&b)
	}
	return redactPaths(b.String())
}
