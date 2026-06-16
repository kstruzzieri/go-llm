package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRealWorkflowCorpusBalance validates the local (gitignored) grown restraint
// corpus against the growth design (see docs/llm/real-workflow-testing.md).
// It skips when the private manifest is absent (CI, fresh clones) OR when the
// corpus has not yet been grown to target (authoring in progress), so it never
// goes red in CI or mid-authoring. Once the corpus claims to be grown (>= minN
// entries) it asserts golden-empty, audit fields, difficulty balance, and
// archetype coverage.
func TestRealWorkflowCorpusBalance(t *testing.T) {
	const minN = 45
	manifestPath := filepath.FromSlash("../../docs/llm/calibration/real-workflow-manifest.jsonl")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Skipf("private real-workflow manifest absent (%v) — local-only check", err)
	}
	m, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(m.Entries) < minN {
		t.Skipf("real-workflow corpus not yet grown to target (%d/%d entries) — authoring in progress", len(m.Entries), minN)
	}
	traceDir := filepath.FromSlash("../../docs/llm/traces/real-workflow-local")

	diffCounts := map[string]int{}
	archetypes := map[string]struct{}{}
	for _, e := range m.Entries {
		archetypes[e.Category] = struct{}{}
		path := filepath.Join(traceDir, e.TraceID+".json")
		traces, err := loadTraces([]string{path})
		if err != nil {
			t.Errorf("trace %q: %v", e.TraceID, err)
			continue
		}
		tr := traces[0]
		if len(tr.Golden.ToolCalls) != 0 {
			t.Errorf("trace %q is not golden-empty (restraint requires empty tool_calls)", e.TraceID)
		}
		if tr.Golden.Difficulty == "" {
			t.Errorf("trace %q missing difficulty", e.TraceID)
		}
		if tr.Golden.RestraintRationale == "" {
			t.Errorf("trace %q missing restraint_rationale (validity anchor)", e.TraceID)
		}
		diffCounts[tr.Golden.Difficulty]++
	}
	for _, tier := range []string{"obvious", "tempting", "adversarial"} {
		if diffCounts[tier] == 0 {
			t.Errorf("no traces in difficulty tier %q", tier)
		}
	}
	// Discriminative power lives in the hard tiers; warn if obvious dominates.
	if diffCounts["tempting"]+diffCounts["adversarial"] < diffCounts["obvious"] {
		t.Errorf("hard tiers (%d) should outnumber obvious (%d) for power",
			diffCounts["tempting"]+diffCounts["adversarial"], diffCounts["obvious"])
	}
	if len(archetypes) < 6 {
		t.Errorf("only %d archetypes present, want >= 6", len(archetypes))
	}
}
