package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestraintHardCorpusBalance validates the local (gitignored) hard
// "tempting-but-unneeded" restraint probe against its design
// (docs/superpowers/specs/2026-06-17-hard-restraint-corpus-design.md).
// It SKIPS when the private manifest is absent (CI, fresh clones) OR while
// authoring is in progress (< 12 entries), so it never goes red in CI or
// mid-authoring. Once the probe is complete (>= 12) it asserts the fixed
// A5/B4/C3 archetype mix, the 5-tempting/7-adversarial difficulty split,
// manifest shape, replay-decodable tool schemas, golden-empty + non-empty
// tools, and the audit fields that anchor validity.
func TestRestraintHardCorpusBalance(t *testing.T) {
	const wantN = 12
	manifestPath := filepath.FromSlash("../../docs/llm/calibration/restraint-hard-manifest.jsonl")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Skipf("private restraint-hard manifest absent (%v) — local-only check", err)
	}
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	m, err := loadManifest(manifestPath)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if len(m.Entries) < wantN {
		t.Skipf("restraint-hard probe not complete (%d/%d) — authoring in progress", len(m.Entries), wantN)
	}
	if len(m.Entries) != wantN {
		t.Fatalf("manifest has %d entries, want exactly %d for the probe", len(m.Entries), wantN)
	}
	// difficulty must live only on golden.difficulty (single source of truth);
	// checked on the complete probe so it never trips the mid-authoring skip above.
	if strings.Contains(string(rawManifest), `"difficulty"`) {
		t.Errorf("manifest must not duplicate difficulty; golden.difficulty is the single source of truth")
	}
	traceDir := filepath.FromSlash("../../docs/llm/traces/restraint-hard-local")

	// Fixed probe shape from the spec.
	wantCategory := map[string]int{"complete-diff-review": 5, "excerpt-explain": 4, "artifact-diagnose": 3}
	wantDifficulty := map[string]int{"tempting": 5, "adversarial": 7}
	gotCategory := map[string]int{}
	gotDifficulty := map[string]int{}

	for _, e := range m.Entries {
		if e.Partition != PartitionChallenge {
			t.Errorf("trace %q partition = %q, want %q", e.TraceID, e.Partition, PartitionChallenge)
		}
		if e.Source != "restraint-hard-local" {
			t.Errorf("trace %q manifest source = %q, want restraint-hard-local", e.TraceID, e.Source)
		}
		if !e.AllowedAsModelEvidence {
			t.Errorf("trace %q must be allowed_as_model_evidence=true", e.TraceID)
		}
		if _, ok := wantCategory[e.Category]; !ok {
			t.Errorf("trace %q has unexpected category %q", e.TraceID, e.Category)
		}
		path := filepath.Join(traceDir, e.TraceID+".json")
		traces, err := loadTraces([]string{path})
		if err != nil {
			t.Errorf("trace %q: %v", e.TraceID, err)
			continue
		}
		if len(traces) != 1 {
			t.Errorf("trace %q: loadTraces returned %d traces from %s, want 1", e.TraceID, len(traces), path)
			continue
		}
		tr := traces[0]
		if tr.ID != e.TraceID {
			t.Errorf("trace file %q has id %q; manifest/filename and trace id must agree (artifacts key on the trace id)", e.TraceID, tr.ID)
		}
		if tr.Source != "restraint-hard-local" {
			t.Errorf("trace %q source = %q, want restraint-hard-local", e.TraceID, tr.Source)
		}
		if len(tr.Tools) == 0 {
			t.Errorf("trace %q has no tools (restraint metric only counts tool-exposed traces)", e.TraceID)
		} else if _, err := decodeTraceTools(tr.Tools); err != nil {
			t.Errorf("trace %q tools do not decode for replay: %v", e.TraceID, err)
		}
		if len(tr.Golden.ToolCalls) != 0 {
			t.Errorf("trace %q is not golden-empty", e.TraceID)
		}
		switch tr.Golden.Difficulty {
		case "tempting", "adversarial":
		default:
			t.Errorf("trace %q difficulty %q not in {tempting,adversarial} for A/B/C probe", e.TraceID, tr.Golden.Difficulty)
		}
		if tr.Golden.RestraintRationale == "" {
			t.Errorf("trace %q missing restraint_rationale (validity anchor)", e.TraceID)
		}
		if tr.Golden.FailureMode == "" {
			t.Errorf("trace %q missing failure_mode (temptation vector)", e.TraceID)
		}
		if tr.Golden.FinalAnswerSubstring == "" {
			t.Errorf("trace %q missing final_answer_substring (keeps it AQ-labelable later)", e.TraceID)
		}
		gotCategory[e.Category]++
		gotDifficulty[tr.Golden.Difficulty]++
	}

	for cat, want := range wantCategory {
		if gotCategory[cat] != want {
			t.Errorf("category %q: got %d traces, want %d", cat, gotCategory[cat], want)
		}
	}
	for cat, got := range gotCategory {
		if _, ok := wantCategory[cat]; !ok {
			t.Errorf("unexpected category %q has %d traces", cat, got)
		}
	}
	for diff, want := range wantDifficulty {
		if gotDifficulty[diff] != want {
			t.Errorf("difficulty %q: got %d, want %d", diff, gotDifficulty[diff], want)
		}
	}
	for diff, got := range gotDifficulty {
		if _, ok := wantDifficulty[diff]; !ok {
			t.Errorf("unexpected difficulty %q has %d traces", diff, got)
		}
	}
}
