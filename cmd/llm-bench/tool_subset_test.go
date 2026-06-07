package main

import (
	"errors"
	"strings"
	"testing"
)

func TestExpectsToolCalls(t *testing.T) {
	if !expectsToolCalls(Trace{Golden: Golden{ToolCalls: []string{"search"}}}) {
		t.Errorf("trace with golden tool calls should expect tool calls")
	}
	if expectsToolCalls(Trace{Golden: Golden{}}) {
		t.Errorf("trace with no golden tool calls should not expect tool calls")
	}
}

// The expected-tool-call subset must exclude vacuous no-call rows: a no-tool
// trace returns ToolArgsValid=1.0/computed=true by vacuous truth, but it is not
// an expected pair and must not inflate the subset.
func TestToolUseSubsetRows_ExcludesVacuousRows(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}},  // expected, computed
		{Model: "m", TraceID: "t2", Score: Score{ToolArgsValid: 0.0, ToolArgsValidComputed: false}}, // expected, made none
		{Model: "m", TraceID: "t3", Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}},  // vacuous (not expected)
	}
	expected := map[string]bool{"t1": true, "t2": true, "t3": false}
	rows := toolUseSubsetRows([]string{"m"}, results, expected)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v; want 1", rows)
	}
	r := rows[0]
	if r.ExpectedPairs != 2 {
		t.Errorf("ExpectedPairs = %d; want 2 (t1, t2 — not the vacuous t3)", r.ExpectedPairs)
	}
	if r.ComputedPairs != 1 {
		t.Errorf("ComputedPairs = %d; want 1 (only t1 computed)", r.ComputedPairs)
	}
	if r.Coverage != 0.5 {
		t.Errorf("Coverage = %v; want 0.5", r.Coverage)
	}
	if r.MeanArgsValid != 1.0 {
		t.Errorf("MeanArgsValid = %v; want 1.0 (over the one computed expected pair)", r.MeanArgsValid)
	}
	if r.ClaimSupported {
		t.Errorf("ClaimSupported should be false with only 2 expected pairs")
	}
}

// An errored replay on an expected-tool-call trace is an expected pair that is
// not computed — it must lower coverage, not vanish.
func TestToolUseSubsetRows_ErroredExpectedRowCountsAgainstCoverage(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}},
		{Model: "m", TraceID: "t2", Err: errors.New("boom")}, // expected trace, errored -> not computed
	}
	expected := map[string]bool{"t1": true, "t2": true}
	rows := toolUseSubsetRows([]string{"m"}, results, expected)
	r := rows[0]
	if r.ExpectedPairs != 2 || r.ComputedPairs != 1 {
		t.Fatalf("ExpectedPairs/ComputedPairs = %d/%d; want 2/1 (errored expected row counts as not-computed)", r.ExpectedPairs, r.ComputedPairs)
	}
}

func TestToolUseSubsetRows_ClaimGate(t *testing.T) {
	// 10 expected pairs, all computed and valid -> supported.
	var ok []Result
	expectedAll := map[string]bool{}
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		ok = append(ok, Result{Model: "m", TraceID: id, Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: true}})
		expectedAll[id] = true
	}
	if r := toolUseSubsetRows([]string{"m"}, ok, expectedAll)[0]; !r.ClaimSupported {
		t.Errorf("10 computed expected pairs should support the claim; got %+v", r)
	}

	// 10 expected pairs but only 6 computed (coverage 0.6 < 0.8) -> not supported.
	var weak []Result
	expectedWeak := map[string]bool{}
	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		computed := i < 6
		weak = append(weak, Result{Model: "m", TraceID: id, Score: Score{ToolArgsValid: 1.0, ToolArgsValidComputed: computed}})
		expectedWeak[id] = true
	}
	r := toolUseSubsetRows([]string{"m"}, weak, expectedWeak)[0]
	if r.ClaimSupported {
		t.Errorf("0.6 coverage should NOT support the claim; got %+v", r)
	}
	if !strings.Contains(r.ClaimReason, "80%") && !strings.Contains(r.ClaimReason, "coverage") {
		t.Errorf("ClaimReason should explain the coverage shortfall; got %q", r.ClaimReason)
	}
}

func TestFormatToolUseSubset_ShowsOverallVsSubsetDistinction(t *testing.T) {
	rows := []toolUseSubsetRow{
		{Model: "m", ExpectedPairs: 2, ComputedPairs: 1, Coverage: 0.5, MeanArgsValid: 1.0,
			ClaimSupported: false, ClaimReason: "insufficient: 2 expected-tool-call pairs (need >=10)"},
	}
	out := formatToolUseSubset(rows)
	for _, want := range []string{
		"## Tool-use (expected-tool-call subset)",
		"vacuous",
		"expected-tool-call pairs",
		"| m | 2 | 50% (1/2) | 1.00 | insufficient: 2 expected-tool-call pairs (need >=10) |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tool-use subset missing %q:\n%s", want, out)
		}
	}
}

func TestFormatToolUseSubset_NAWhenNoExpectedPairs(t *testing.T) {
	rows := []toolUseSubsetRow{{Model: "m", ExpectedPairs: 0, ClaimReason: "insufficient: 0 expected-tool-call pairs (need >=10)"}}
	out := formatToolUseSubset(rows)
	if !strings.Contains(out, "| m | 0 | n/a | n/a |") {
		t.Errorf("zero expected pairs should render n/a coverage and subset:\n%s", out)
	}
}

func TestFormatToolUseSubset_BorderlineCoverageDoesNotRoundUpToGate(t *testing.T) {
	coverage := float64(35) / float64(44)
	_, reason := toolUseClaimVerdict(44, coverage)
	out := formatToolUseSubset([]toolUseSubsetRow{{
		Model:          "m",
		ExpectedPairs:  44,
		ComputedPairs:  35,
		Coverage:       coverage,
		MeanArgsValid:  1.0,
		ClaimSupported: false,
		ClaimReason:    reason,
	}})
	if !strings.Contains(out, "| m | 44 | 79.5% (35/44) | 1.00 | insufficient: 79.5% computed coverage (need >=80%) |") {
		t.Fatalf("borderline coverage should not round up to the 80%% gate:\n%s", out)
	}
	if strings.Contains(out, "80% (35/44)") || strings.Contains(out, "insufficient: 80% computed coverage") {
		t.Fatalf("borderline coverage rounded up to the gate:\n%s", out)
	}
}

func TestFormatReport_EmitsToolUseSubsetOnlyWhenExpectedMapSet(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "t1", Score: Score{AnswerQuality: 1.0, ToolArgsValid: 1.0, ToolArgsValidComputed: true}},
	}
	// Without the map: no subset section (back-compat).
	plain := formatReport([]string{"m"}, results, reportOptions{Scorer: "exact-match"})
	if strings.Contains(plain, "expected-tool-call subset") {
		t.Errorf("subset section must not appear without ToolCallExpected:\n%s", plain)
	}
	// With the map: section present.
	withMap := formatReport([]string{"m"}, results, reportOptions{
		Scorer:           "exact-match",
		ToolCallExpected: map[string]bool{"t1": true},
	})
	if !strings.Contains(withMap, "## Tool-use (expected-tool-call subset)") {
		t.Errorf("subset section must appear when ToolCallExpected is set:\n%s", withMap)
	}
}

func TestFormatReport_ToolUseSubsetExcludesJudgeValidation(t *testing.T) {
	results := []Result{
		{Model: "m", TraceID: "natural-tool", Score: Score{AnswerQuality: 1.0, ToolArgsValid: 1.0, ToolArgsValidComputed: true}},
		{Model: "m", TraceID: "judge-tool", Score: Score{AnswerQuality: 1.0, ToolArgsValid: 1.0, ToolArgsValidComputed: true}},
	}
	out := formatReport([]string{"m"}, results, reportOptions{
		Scorer: "exact-match",
		Corpus: &corpusReportData{
			Counts: corpusCounts{
				ByPartition: map[CorpusPartition]int{PartitionNatural: 1, PartitionJudgeValidation: 1},
				ByCategory:  map[string]int{"tool-use": 2},
				Total:       2,
			},
			TraceToPartition: map[string]CorpusPartition{
				"natural-tool": PartitionNatural,
				"judge-tool":   PartitionJudgeValidation,
			},
		},
		ToolCallExpected: map[string]bool{
			"natural-tool": true,
			"judge-tool":   true,
		},
	})
	if !strings.Contains(out, "| m | 1 | 100% (1/1) | 1.00 |") {
		t.Fatalf("tool-use subset should count only model-evidence expected-tool rows:\n%s", out)
	}
	if strings.Contains(out, "| m | 2 | 100% (2/2) |") {
		t.Fatalf("judge-validation row leaked into the tool-use subset:\n%s", out)
	}
}
