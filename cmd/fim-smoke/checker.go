package main

import (
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/completion"
)

// CheckResult is the outcome of running a fixture's invariants against a
// completion response. Failures is nil when all assertions passed.
type CheckResult struct {
	Fixture  string
	Failures []string
}

// OK reports whether all invariants passed.
func (r CheckResult) OK() bool { return len(r.Failures) == 0 }

// Check runs the expectations in a fixture against the observed response.
// A nil Expect (no sidecar) returns an OK result with no failures.
func Check(f *Fixture, resp *completion.FIMResponse, stopTokens []string) CheckResult {
	res := CheckResult{Fixture: f.Path}
	if f.Expect == nil {
		return res
	}
	e := f.Expect

	if e.Context != "" && resp.CursorContext.String() != e.Context {
		res.Failures = append(res.Failures,
			fmt.Sprintf("context=%s want %s", resp.CursorContext, e.Context))
	}

	if len(e.Shape) > 0 {
		got := resp.CompletionShape.String()
		matched := false
		for _, want := range e.Shape {
			if got == want {
				matched = true
				break
			}
		}
		if !matched {
			res.Failures = append(res.Failures,
				fmt.Sprintf("shape=%s want one of %v", got, e.Shape))
		}
	}

	if resp.BudgetTrace != nil {
		pct := resp.BudgetTrace.PrefixBudgetPct
		if e.MinPrefixPct > 0 && pct < e.MinPrefixPct {
			res.Failures = append(res.Failures,
				fmt.Sprintf("prefix_budget_pct=%d below min %d", pct, e.MinPrefixPct))
		}
		if e.MaxPrefixPct > 0 && pct > e.MaxPrefixPct {
			res.Failures = append(res.Failures,
				fmt.Sprintf("prefix_budget_pct=%d above max %d", pct, e.MaxPrefixPct))
		}
	}

	if e.MinTokens > 0 && resp.Tokens < e.MinTokens {
		res.Failures = append(res.Failures,
			fmt.Sprintf("tokens=%d below min %d", resp.Tokens, e.MinTokens))
	}

	if e.NoStopLeak {
		if leaked := findStopLeak(resp.Completion, stopTokens); leaked != "" {
			res.Failures = append(res.Failures,
				fmt.Sprintf("stop token leaked: %q", leaked))
		}
	}

	return res
}

// findStopLeak returns the first stop token found anywhere in the output,
// or "" when none is present. An empty completion or an empty stop list
// short-circuits to "".
func findStopLeak(output string, stopTokens []string) string {
	if output == "" {
		return ""
	}
	for _, tok := range stopTokens {
		if tok == "" {
			continue
		}
		if strings.Contains(output, tok) {
			return tok
		}
	}
	return ""
}
