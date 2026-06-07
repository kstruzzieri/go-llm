package main

import (
	"fmt"
	"math"
	"strings"
)

const (
	// toolUseClaimMinExpectedPairs and toolUseClaimMinCoverage are the gate for
	// citing a tool-use claim (spec §57-64): at least this many expected-tool-
	// call (model, trace) pairs, and ToolArgsValid computed=true for at least
	// this fraction of them. Vacuous no-call rows never count toward the subset.
	toolUseClaimMinExpectedPairs = 10
	toolUseClaimMinCoverage      = 0.80
)

// expectsToolCalls reports whether a trace's golden rubric expects any tool
// calls. The expected-tool-call subset is defined by the trace, not the result,
// so an errored or no-call replay on an expecting trace still counts as an
// expected pair.
func expectsToolCalls(t Trace) bool {
	return len(t.Golden.ToolCalls) > 0
}

// toolUseSubsetRow is one model's expected-tool-call subset summary: how many
// (model, trace) pairs expected tool calls, how many produced a computed
// ToolArgsValid, the coverage, the mean ToolArgsValid over computed expected
// pairs, and whether the tool-use claim is supportable.
type toolUseSubsetRow struct {
	Model          string
	ExpectedPairs  int
	ComputedPairs  int
	Coverage       float64
	MeanArgsValid  float64
	ClaimSupported bool
	ClaimReason    string
}

// toolUseSubsetRows computes the expected-tool-call subset per model. expected
// maps trace ID → whether that trace expects tool calls (built from the trace
// goldens). Coverage = ComputedPairs / ExpectedPairs; MeanArgsValid averages
// only the computed expected pairs (so vacuous no-call rows can't inflate it).
func toolUseSubsetRows(models []string, results []Result, expected map[string]bool) []toolUseSubsetRow {
	type acc struct {
		expectedPairs int
		computedPairs int
		argsSum       float64
	}
	byModel := make(map[string]*acc, len(models))
	for _, m := range models {
		byModel[m] = &acc{}
	}
	for _, r := range results {
		if !expected[r.TraceID] {
			continue
		}
		a, ok := byModel[r.Model]
		if !ok {
			continue
		}
		a.expectedPairs++
		if r.Err == nil && r.Score.ToolArgsValidComputed {
			a.computedPairs++
			a.argsSum += r.Score.ToolArgsValid
		}
	}

	rows := make([]toolUseSubsetRow, 0, len(models))
	for _, m := range models {
		a := byModel[m]
		row := toolUseSubsetRow{Model: m, ExpectedPairs: a.expectedPairs, ComputedPairs: a.computedPairs}
		if a.expectedPairs > 0 {
			row.Coverage = float64(a.computedPairs) / float64(a.expectedPairs)
		}
		if a.computedPairs > 0 {
			row.MeanArgsValid = a.argsSum / float64(a.computedPairs)
		}
		row.ClaimSupported, row.ClaimReason = toolUseClaimVerdict(a.expectedPairs, row.Coverage)
		rows = append(rows, row)
	}
	return rows
}

// formatToolUseSubset renders the expected-tool-call subset table: the honest
// ToolArgsValid view that excludes vacuous no-call rows, with the tool-use-claim
// verdict per model.
func formatToolUseSubset(rows []toolUseSubsetRow) string {
	var b strings.Builder
	fmt.Fprintln(&b, "## Tool-use (expected-tool-call subset)")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "> ToolArgsValid overall (the Results column above) includes no-tool traces that pass by vacuous truth. This subset counts only traces whose golden expects tool calls. A tool-use claim needs >=10 expected-tool-call pairs with ToolArgsValid computed=true for >=80% of them.")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Model | expected-tool-call pairs | computed coverage | ToolArgsValid (subset) | tool-use claim |")
	fmt.Fprintln(&b, "|---|---|---|---|---|")
	for _, r := range rows {
		coverageCell := "n/a"
		subsetCell := "n/a"
		if r.ExpectedPairs > 0 {
			coverageCell = fmt.Sprintf("%s (%d/%d)", formatCoveragePercent(r.Coverage), r.ComputedPairs, r.ExpectedPairs)
		}
		if r.ComputedPairs > 0 {
			subsetCell = fmt.Sprintf("%.2f", r.MeanArgsValid)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n",
			markdownCell(r.Model), r.ExpectedPairs, coverageCell, subsetCell, markdownCell(r.ClaimReason))
	}
	fmt.Fprintln(&b)
	return b.String()
}

// toolUseClaimVerdict applies the tool-use-claim gate and returns a verdict plus
// a human-readable reason naming the limiting factor.
func toolUseClaimVerdict(expectedPairs int, coverage float64) (bool, string) {
	if expectedPairs < toolUseClaimMinExpectedPairs {
		return false, fmt.Sprintf("insufficient: %d expected-tool-call pairs (need >=%d)", expectedPairs, toolUseClaimMinExpectedPairs)
	}
	if coverage < toolUseClaimMinCoverage {
		return false, fmt.Sprintf("insufficient: %s computed coverage (need >=%.0f%%)", formatCoveragePercent(coverage), toolUseClaimMinCoverage*100)
	}
	return true, fmt.Sprintf("supported: %d pairs, %s computed", expectedPairs, formatCoveragePercent(coverage))
}

func formatCoveragePercent(coverage float64) string {
	tenths := math.Floor(coverage*1000+1e-9) / 10
	s := fmt.Sprintf("%.1f", tenths)
	s = strings.TrimSuffix(s, ".0")
	return s + "%"
}
