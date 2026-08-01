package rageval

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sync"
	"testing"
)

// mixedBaselinePath is the committed mixed-experiment report, generated via
// `go run ./cmd/rag-eval -experiment mixed -out
// internal/rageval/testdata/mixed-baseline.json`.
const mixedBaselinePath = "testdata/mixed-baseline.json"

// mixedEvalReport runs the experiment once per test binary and shares the
// result: the report is read-only in every test, and the experiment already
// proves determinism internally (double run + byte compare), so re-running it
// per test would only re-prove what Summary.AllDeterministic asserts.
var mixedEvalReportOnce = sync.OnceValues(func() (*MixedReport, error) {
	return RunMixedExperiment(context.Background(), MixedOptions{})
})

func mixedEvalReport(t *testing.T) *MixedReport {
	t.Helper()
	report, err := mixedEvalReportOnce()
	if err != nil {
		t.Fatalf("RunMixedExperiment: %v", err)
	}
	return report
}

// TestMixedEvalSchemaAndShape pins the report envelope: schema version
// literal, the five fixture cases in declared order, the three sweep
// fractions by literal, and the budget formula via one hand-computed
// floor-wins row.
func TestMixedEvalSchemaAndShape(t *testing.T) {
	report := mixedEvalReport(t)
	if report.SchemaVersion != "mixed-assembly-eval/v1" {
		t.Fatalf("schema = %q, want mixed-assembly-eval/v1", report.SchemaVersion)
	}
	if MixedSchemaVersion != "mixed-assembly-eval/v1" {
		t.Fatalf("MixedSchemaVersion const = %q", MixedSchemaVersion)
	}
	// The anchor byte cap must stay agent's defaultOutputCap; a drifted mirror
	// would silently change every anchor's admission ceiling.
	if mixedEvalOutputCap != 64<<10 {
		t.Fatalf("mixedEvalOutputCap = %d, want 64<<10 (agent's defaultOutputCap)", mixedEvalOutputCap)
	}
	wantCases := []struct{ name, stratum string }{
		{"conversation_only", "conversation_only"},
		{"memory_only", "memory_only"},
		{"cross_domain", "cross_domain_join"},
		{"stale_fresh", "stale_vs_fresh"},
		{"chain_retention", "chain_retention"},
	}
	if len(report.Cases) != len(wantCases) {
		t.Fatalf("cases = %d, want %d", len(report.Cases), len(wantCases))
	}
	// Fraction literals pinned here, NOT read from mixedSweepFractions: the
	// test is the independent record of the registered sweep.
	wantFractions := []float64{0.4, 0.6, 0.8}
	for i, c := range report.Cases {
		if c.Name != wantCases[i].name || c.Stratum != wantCases[i].stratum {
			t.Fatalf("case[%d] = %s/%s, want %s/%s", i, c.Name, c.Stratum, wantCases[i].name, wantCases[i].stratum)
		}
		if c.RawTokens <= 0 {
			t.Fatalf("case %s: raw_tokens = %d, want > 0", c.Name, c.RawTokens)
		}
		if len(c.Fractions) != len(wantFractions) {
			t.Fatalf("case %s: fraction rows = %d, want %d", c.Name, len(c.Fractions), len(wantFractions))
		}
		for j, row := range c.Fractions {
			if row.Fraction != wantFractions[j] {
				t.Fatalf("case %s row %d: fraction = %v, want %v", c.Name, j, row.Fraction, wantFractions[j])
			}
			if row.Budget <= 0 {
				t.Fatalf("case %s f=%v: budget = %d, want > 0", c.Name, row.Fraction, row.Budget)
			}
		}
	}
	// Budget formula, one hand-computed row where the minViable floor WINS.
	// memory_only: System is 122 bytes => est (122+3)/4 = 31; the final goal
	// message is 489 bytes => est (489+3)/4 = 123; slack 64.
	// minViable = 31 + 123 + 64 = 218. RawTokens = 327 (pinned below), so
	// round(0.4*327) = 131 < 218: the floor wins the f=0.4 budget.
	mem := report.Cases[1]
	if mem.RawTokens != 327 {
		t.Fatalf("memory_only raw_tokens = %d, want 327", mem.RawTokens)
	}
	if got := mem.Fractions[0].Budget; got != 218 {
		t.Fatalf("memory_only f=0.4 budget = %d, want floor 218 (31+123+64)", got)
	}
	// And one row where the fraction term wins: round(0.8*327) = 262 > 218.
	if got := mem.Fractions[2].Budget; got != 262 {
		t.Fatalf("memory_only f=0.8 budget = %d, want round(0.8*327) = 262", got)
	}
}

// TestMixedEvalDeterminism exercises both halves of the determinism contract:
// the double-run identity RunMixedExperiment enforces internally, and the
// comparison helper's error path on genuinely differing reports.
func TestMixedEvalDeterminism(t *testing.T) {
	report := mixedEvalReport(t)
	if !report.Summary.AllDeterministic {
		t.Fatal("summary.all_deterministic = false after a successful run")
	}
	a := &MixedReport{SchemaVersion: MixedSchemaVersion}
	b := &MixedReport{SchemaVersion: MixedSchemaVersion + "-drifted"}
	if err := mixedReportsIdentical(a, b); err == nil {
		t.Fatal("mixedReportsIdentical accepted two differing reports")
	}
	if err := mixedReportsIdentical(a, a); err != nil {
		t.Fatalf("mixedReportsIdentical rejected identical reports: %v", err)
	}
}

// TestMixedEvalShedsUnderSweep pins that the fixture produces REAL pressure:
// a sweep where nothing is ever shed would measure only budget slack. The
// named cases are hand-built, so which of them shed at f=0.4 is a fixture
// fact, pinned from the committed baseline read-through.
func TestMixedEvalShedsUnderSweep(t *testing.T) {
	report := mixedEvalReport(t)
	byName := make(map[string]MixedCaseReport, len(report.Cases))
	for _, c := range report.Cases {
		byName[c.Name] = c
	}
	// At f=0.4 every fixture case sheds messages in BOTH arms.
	for _, name := range []string{"conversation_only", "memory_only", "cross_domain", "stale_fresh", "chain_retention"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("case %q missing from report", name)
		}
		row := c.Fractions[0]
		if row.Fraction != 0.4 {
			t.Fatalf("%s: row 0 fraction = %v, want 0.4", name, row.Fraction)
		}
		if row.Legacy.ShedMessages <= 0 {
			t.Fatalf("%s f=0.4: legacy shed_messages = %d, want > 0", name, row.Legacy.ShedMessages)
		}
		if row.Mixed.ShedMessages <= 0 {
			t.Fatalf("%s f=0.4: mixed shed_messages = %d, want > 0", name, row.Mixed.ShedMessages)
		}
	}
	// Monotonicity spot-check on conversation_only: the f=0.8 assembly retains
	// more messages than the f=0.4 one. Exact values pinned from the baseline:
	// legacy retains 3 of 11 messages at f=0.4 and 9 of 11 at f=0.8.
	conv := byName["conversation_only"]
	if got := conv.Fractions[0].Legacy.Messages; got != 3 {
		t.Fatalf("conversation_only f=0.4 legacy messages = %d, want 3", got)
	}
	if got := conv.Fractions[2].Legacy.Messages; got != 9 {
		t.Fatalf("conversation_only f=0.8 legacy messages = %d, want 9", got)
	}
}

// TestMixedEvalDecisionHistogram pins the mixed arm's per-subject decision
// histogram for the cross_domain case at f=0.6: at least one rendered bucket
// AND at least one omitted bucket, with the exact buckets pinned from the
// committed baseline read-through.
func TestMixedEvalDecisionHistogram(t *testing.T) {
	report := mixedEvalReport(t)
	var cross MixedCaseReport
	found := false
	for _, c := range report.Cases {
		if c.Name == "cross_domain" {
			cross, found = c, true
		}
	}
	if !found {
		t.Fatal("cross_domain case missing from report")
	}
	row := cross.Fractions[1]
	if row.Fraction != 0.6 {
		t.Fatalf("cross_domain row 1 fraction = %v, want 0.6", row.Fraction)
	}
	hist := row.Mixed.DecisionHistogram
	if hist == nil {
		t.Fatal("cross_domain f=0.6: decision histogram is nil")
	}
	// Baseline read: 11 subjects — 6 base admissions, 4 upgrades, and 1
	// omission (the older history exchange yields to the anchors).
	want := map[string]int{"base": 6, "upgrade": 4, "omitted": 1}
	if len(hist) != len(want) {
		t.Fatalf("cross_domain f=0.6: histogram = %v, want %v", hist, want)
	}
	for k, v := range want {
		if hist[k] != v {
			t.Fatalf("cross_domain f=0.6: histogram[%q] = %d, want %d (full: %v)", k, hist[k], v, hist)
		}
	}
	if row.Mixed.OmittedSubjects != 1 {
		t.Fatalf("cross_domain f=0.6: omitted_subjects = %d, want 1", row.Mixed.OmittedSubjects)
	}
}

// TestMixedEvalNoCrossArmDiffKeys walks every object key in the marshaled
// report against a pinned allowlist and a cross-arm-diff pattern ban. The
// experiment records both arms' shapes; it must never ship a field that
// DIFFS decisions or causes across arms (#331 slice 3c: Pressure.Cause is
// documented as not comparable between the legacy and mixed arms).
func TestMixedEvalNoCrossArmDiffKeys(t *testing.T) {
	report := mixedEvalReport(t)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var tree any
	if err := json.Unmarshal(raw, &tree); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	allowed := map[string]bool{
		// envelope
		"schema_version": true, "cases": true, "summary": true,
		// case
		"name": true, "stratum": true, "raw_tokens": true, "fractions": true,
		// fraction row
		"fraction": true, "budget": true, "legacy": true, "mixed": true,
		// arm stats
		"context_tokens": true, "context_bytes": true, "messages": true,
		"shed_messages": true, "shed_bytes": true, "pressure_level": true,
		// mixed-arm extras
		"omitted_subjects": true, "anchor_omissions": true, "decision_histogram": true,
		// decision buckets (agent's fixed vocabulary)
		"base": true, "floor": true, "upgrade": true, "omitted": true,
		// summary
		"all_deterministic": true,
	}
	banned := regexp.MustCompile(`(?i)(diff|delta|cause_compare)`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if !allowed[k] {
					t.Errorf("unexpected report key %q at %s", k, path)
				}
				if banned.MatchString(k) {
					t.Errorf("cross-arm diff key %q at %s", k, path)
				}
				walk(path+"."+k, child)
			}
		case []any:
			for _, child := range x {
				walk(path+"[]", child)
			}
		}
	}
	walk("$", tree)
}

// TestMixedEvalBaselineUpToDate is THE regeneration gate for this fixture:
// the live report, marshaled exactly as writeJSONReport does, must equal the
// committed baseline byte-for-byte. Regenerate after intentional changes via
// `go run ./cmd/rag-eval -experiment mixed -out
// internal/rageval/testdata/mixed-baseline.json`.
func TestMixedEvalBaselineUpToDate(t *testing.T) {
	report := mixedEvalReport(t)
	got, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	got = append(got, '\n') // writeJSONReport appends a trailing newline.
	want, err := os.ReadFile(mixedBaselinePath)
	if err != nil {
		t.Fatalf("read committed baseline: %v", err)
	}
	if string(got) != string(want) {
		t.Fatal("live mixed report differs from committed testdata/mixed-baseline.json; regenerate via `go run ./cmd/rag-eval -experiment mixed -out internal/rageval/testdata/mixed-baseline.json`")
	}
}
