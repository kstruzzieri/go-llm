package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// usageStep is one top-level step served by model ("provider/model"); an
// empty model leaves the step without a RouteOutcome.
func usageStep(model string, u provider.Usage) agent.StepRecord {
	st := agent.StepRecord{Response: provider.ChatResponse{Usage: u}}
	if model != "" {
		p, m, _ := strings.Cut(model, "/")
		st.RouteOutcome = &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: p, Model: m}}
	}
	return st
}

func usageRun(steps ...agent.StepRecord) agent.Result { return agent.Result{Steps: steps} }

func TestCostUSD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		u     modelUsage
		price config.ModelPrice
		want  float64
	}{
		{"cached input rate", modelUsage{Prompt: 2_000_000, Cached: 1_500_000, Completion: 100_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), Output: 8.0}, 2.10},
		{"nil cached rate bills at input", modelUsage{Prompt: 2_000_000, Cached: 1_500_000, Completion: 100_000}, config.ModelPrice{Input: 2.0, Output: 8.0}, 4.80},
		{"cached above prompt is clamped", modelUsage{Prompt: 2_000_000, Cached: 3_000_000, Completion: 100_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), Output: 8.0}, 1.20},
		{"all cached", modelUsage{Prompt: 2_000_000, Cached: 2_000_000, Completion: 100_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), Output: 8.0}, 1.20},
		{"cache write rate", modelUsage{Prompt: 2_000_000, Cached: 1_000_000, CacheWrite: 500_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), CacheWrite: new(2.5), Output: 8.0}, 2.45},
		{"nil write rate bills at input", modelUsage{Prompt: 2_000_000, Cached: 1_000_000, CacheWrite: 500_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), Output: 8.0}, 2.20},
		{"write clamped to prompt minus cached", modelUsage{Prompt: 1_000_000, Cached: 1_000_000, CacheWrite: 1_000_000}, config.ModelPrice{Input: 2.0, CachedInput: new(0.2), CacheWrite: new(10.0), Output: 8.0}, 0.20},
		{"negative counters count as zero", modelUsage{Prompt: -1_000_000, Cached: -5, CacheWrite: -5, Completion: -1_000_000}, config.ModelPrice{Input: 2.0, Output: 8.0}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := costUSD(tc.u, &tc.price); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("costUSD = %.10f; want %.10f", got, tc.want)
			}
		})
	}
}

func TestLedgerRenderNoReports(t *testing.T) {
	l := &usageLedger{}
	l.addRun(usageRun(
		usageStep("p/m", provider.Usage{PromptTokens: 20, CompletionTokens: 4}),
		usageStep("p/m", provider.Usage{PromptTokens: 10, CompletionTokens: 2}),
	))
	want := []string{
		"usage (this process): input 30 tok (cached n/a), output 6 tok, 2 requests",
		"last run: input 30 tok (cached n/a), output 6 tok",
		"est. cost: unavailable (no prices set)",
	}
	if got := l.render(nil); !slices.Equal(got, want) {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
	}
}

func TestLedgerRenderPartialReports(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []agent.StepRecord
		want  string
	}{
		{"two of three", []agent.StepRecord{
			usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 40, CachedPromptTokensReported: true, CompletionTokens: 10}),
			usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 20, CachedPromptTokensReported: true, CompletionTokens: 10}),
			usageStep("q/n", provider.Usage{PromptTokens: 100, CompletionTokens: 10}),
		}, "usage (this process): input 300 tok (60 cached, 20%), output 30 tok, 3 requests; cache reported on 2 of 3 requests"},
		{"all reported", []agent.StepRecord{
			usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 40, CachedPromptTokensReported: true, CompletionTokens: 10}),
			usageStep("q/n", provider.Usage{PromptTokens: 100, CachedPromptTokens: 20, CachedPromptTokensReported: true, CompletionTokens: 10}),
		}, "usage (this process): input 200 tok (60 cached, 30%), output 20 tok, 2 requests"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			l.addRun(usageRun(tc.steps...))
			if got := l.render(nil)[0]; got != tc.want {
				t.Fatalf("line 1 = %q; want %q", got, tc.want)
			}
		})
	}
}

// The display clamps per request before summing: line 1 can never show more
// cached than input, and a negative counter counts as zero.
func TestLedgerRenderClampsDisplayedTokens(t *testing.T) {
	l := &usageLedger{}
	l.addRun(usageRun(
		usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 500, CachedPromptTokensReported: true, CompletionTokens: 5}),
		usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 50, CachedPromptTokensReported: true, CompletionTokens: 5}),
		usageStep("p/m", provider.Usage{PromptTokens: -7, CachedPromptTokens: 3, CachedPromptTokensReported: true, CompletionTokens: -2}),
	))
	// Two of the three raw reports (cached 500 > prompt 100, prompt -7) are
	// unreliable, and both lines say so.
	want := []string{
		"usage (this process): input 200 tok (150 cached, 75%), output 10 tok, 3 requests; unreliable usage on 2 requests",
		"last run: input 200 tok (150 cached), output 10 tok; unreliable usage on 2 requests",
	}
	if got := l.render(nil)[:2]; !slices.Equal(got, want) {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
	}
}

// Lines 1 and 2 share one qualifier suffix, each computed over its own scope
// (process vs last run), in a fixed order: partial cache reports, dispatch
// children coverage, unreliable usage.
func TestLedgerLineQualifiers(t *testing.T) {
	weighted := provider.Usage{PromptTokens: 596, CachedPromptTokens: 4992, CachedPromptTokensReported: true, CompletionTokens: 20}
	reported := provider.Usage{PromptTokens: 100, CachedPromptTokens: 40, CachedPromptTokensReported: true, CompletionTokens: 10}
	silent := provider.Usage{PromptTokens: 100, CompletionTokens: 10}
	// cached 80 > prompt 50: an unreliable aggregate, clamped to 50/50.
	children := &provider.Usage{PromptTokens: 50, CachedPromptTokens: 80, CachedPromptTokensReported: true, CompletionTokens: 5}
	for _, tc := range []struct {
		name string
		runs []agent.Result
		want []string
	}{
		{"abacus billing-weighted on line 1", []agent.Result{usageRun(usageStep("abacus/route", weighted))}, []string{
			"usage (this process): input 596 tok (596 cached, 100%), output 20 tok, 1 requests; unreliable usage on 1 requests",
			"last run: input 596 tok (596 cached), output 20 tok; unreliable usage on 1 requests",
		}},
		{"abacus non-streaming sends no prompt", []agent.Result{usageRun(usageStep("abacus/route", provider.Usage{CompletionTokens: 50}))}, []string{
			"usage (this process): input 0 tok (cached n/a), output 50 tok, 1 requests; unreliable usage on 1 requests",
			"last run: input 0 tok (cached n/a), output 50 tok; unreliable usage on 1 requests",
		}},
		{"partial report on line 2", []agent.Result{
			usageRun(usageStep("p/m", reported)),
			usageRun(usageStep("p/m", reported), usageStep("q/n", silent)),
		}, []string{
			"usage (this process): input 300 tok (80 cached, 26%), output 30 tok, 3 requests; cache reported on 2 of 3 requests",
			"last run: input 200 tok (40 cached), output 20 tok; cache reported on 1 of 2 requests",
		}},
		{"all qualifiers in order; children count once", []agent.Result{
			{Steps: []agent.StepRecord{usageStep("p/m", reported)}, DescendantUsage: children},
			{Steps: []agent.StepRecord{usageStep("p/m", silent), usageStep("abacus/route", weighted)}, DescendantUsage: children},
		}, []string{
			// 100+100+596 top-level + 2*50 children = 896 input; 40+596+2*50 = 736 cached.
			"usage (this process): input 896 tok (736 cached, 82%), output 50 tok, 3 requests; cache reported on 2 of 3 requests; dispatch children: 100 input tok, cache coverage unknown; unreliable usage on 2 requests",
			"last run: input 746 tok (646 cached), output 35 tok; cache reported on 1 of 2 requests; dispatch children: 50 input tok, cache coverage unknown; unreliable usage on 2 requests",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			for _, res := range tc.runs {
				l.addRun(res)
			}
			if got := l.render(nil)[:2]; !slices.Equal(got, tc.want) {
				t.Fatalf("lines 1-2 =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// A request is unreliable when it has no prompt count or any counter had
// to be clamped: the clamp keeps the figures sane, the flag keeps them out of
// the dollar total.
func TestLedgerUnreliableConditions(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    provider.Usage
		want bool
	}{
		{"consistent counters", provider.Usage{PromptTokens: 100, CachedPromptTokens: 60, CacheWriteTokens: 40, CompletionTokens: 5}, false},
		{"no prompt count", provider.Usage{CompletionTokens: 5}, true},
		{"negative prompt", provider.Usage{PromptTokens: -1, CompletionTokens: 5}, true},
		{"cached above prompt", provider.Usage{PromptTokens: 100, CachedPromptTokens: 101}, true},
		{"negative cached", provider.Usage{PromptTokens: 100, CachedPromptTokens: -1}, true},
		{"negative completion", provider.Usage{PromptTokens: 100, CompletionTokens: -1}, true},
		{"cache write above uncached prompt", provider.Usage{PromptTokens: 100, CachedPromptTokens: 60, CacheWriteTokens: 41}, true},
		{"negative cache write", provider.Usage{PromptTokens: 100, CacheWriteTokens: -1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m modelUsage
			m.add(tc.u, true)
			if got := m.UnreliableRequests == 1; got != tc.want {
				t.Fatalf("unreliable = %v; want %v (bucket %+v)", got, tc.want, m)
			}
		})
	}
}

// A's write has no room left after its cached tokens, which breaks the
// Usage contract (cache-write is part of prompt). Clamping it per request
// would bill $0.1100 and summing the raw write $0.5100; neither is a
// measurement, so the bucket is excluded and named instead.
func TestLedgerExcludesClampedCacheWrite(t *testing.T) {
	prices := map[string]*config.ModelPrice{"p/m": {Input: 1000, CachedInput: new(100.0), CacheWrite: new(5000.0), Output: 1000}}
	l := &usageLedger{}
	l.addRun(usageRun(
		usageStep("p/m", provider.Usage{PromptTokens: 100, CachedPromptTokens: 100, CacheWriteTokens: 100}),
		usageStep("p/m", provider.Usage{PromptTokens: 100}),
	))
	if got, want := l.render(prices)[2], "est. cost: $0.0000 (excludes unreliable usage: p/m)"; got != want {
		t.Fatalf("line 3 = %q; want %q", got, want)
	}
}

// Dispatch children arrive as one aggregate: their tokens count, they add no
// requests and never move the reported-request count, and line 1 always says
// their cache coverage is unknown so the share cannot read as complete.
func TestLedgerDispatchChildren(t *testing.T) {
	children := &provider.Usage{PromptTokens: 400, CachedPromptTokens: 100, CachedPromptTokensReported: true, CompletionTokens: 40}
	unreported := &provider.Usage{PromptTokens: 400, CompletionTokens: 40}
	reported := provider.Usage{PromptTokens: 100, CachedPromptTokens: 50, CachedPromptTokensReported: true, CompletionTokens: 10}
	silent := provider.Usage{PromptTokens: 100, CompletionTokens: 10}
	for _, tc := range []struct {
		name     string
		top      provider.Usage
		children *provider.Usage
		want     string
	}{
		{"all top-level reported", reported, children, "usage (this process): input 600 tok (200 cached, 33%), output 60 tok, 2 requests; dispatch children: 400 input tok, cache coverage unknown"},
		{"only children reported", silent, children, "usage (this process): input 600 tok (100 cached, 16%), output 60 tok, 2 requests; cache reported on 0 of 2 requests; dispatch children: 400 input tok, cache coverage unknown"},
		{"nothing reported", silent, unreported, "usage (this process): input 600 tok (cached n/a), output 60 tok, 2 requests; dispatch children: 400 input tok, cache coverage unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			res := usageRun(usageStep("p/m", tc.top), usageStep("p/m", tc.top))
			res.DescendantUsage = tc.children
			l.addRun(res)
			if got := l.render(nil)[0]; got != tc.want {
				t.Fatalf("line 1 = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestLedgerLastRunAndSideTasks(t *testing.T) {
	l := &usageLedger{}
	if got, want := l.render(nil), []string{
		"usage (this process): input 0 tok (cached n/a), output 0 tok, 0 requests",
		"last run: none",
		"est. cost: unavailable (no prices set)",
	}; !slices.Equal(got, want) {
		t.Fatalf("empty render =\n%q\nwant\n%q", got, want)
	}
	l.addRun(usageRun(usageStep("p/m", provider.Usage{PromptTokens: 100, CompletionTokens: 10})))
	l.addRun(usageRun(usageStep("q/n", provider.Usage{PromptTokens: 50, CompletionTokens: 5})))
	l.addSide("summarize", "p/m", provider.Usage{PromptTokens: 30, CompletionTokens: 3})
	l.addSide("summarize", "", provider.Usage{PromptTokens: 20, CompletionTokens: 2})
	l.addSide("embed", "e/x", provider.Usage{PromptTokens: 7})
	want := []string{
		"usage (this process): input 150 tok (cached n/a), output 15 tok, 2 requests",
		"last run: input 50 tok (cached n/a), output 5 tok",
		"est. cost: unavailable (no prices set)",
		"side task embed: input 7 tok, output 0 tok, 1 calls",
		"side task summarize: input 50 tok, output 5 tok, 2 calls",
	}
	if got := l.render(nil); !slices.Equal(got, want) {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
	}
}

func TestLedgerRenderPartialPrices(t *testing.T) {
	prices := map[string]*config.ModelPrice{"a/priced": {Input: 2, Output: 8}}
	for _, tc := range []struct {
		name  string
		steps []agent.StepRecord
		want  string
	}{
		{"one unpriced", []agent.StepRecord{
			usageStep("a/priced", provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000}),
			usageStep("b/unpriced", provider.Usage{PromptTokens: 10, CompletionTokens: 1}),
		}, "est. cost: $2.8000 (excludes unpriced: b/unpriced)"},
		{"all priced", []agent.StepRecord{
			usageStep("a/priced", provider.Usage{PromptTokens: 1_000_000, CompletionTokens: 100_000}),
		}, "est. cost: $2.8000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			l.addRun(usageRun(tc.steps...))
			if got := l.render(prices)[2]; got != tc.want {
				t.Fatalf("line 3 = %q; want %q", got, tc.want)
			}
		})
	}
}

// Line 3 prices agent runs AND side tasks, each by its own model, and names
// every unpriced bucket once: "unknown", "dispatch children" and side models,
// a side model as "model (task)".
func TestLedgerCostCoversSideTasksAndNamedBuckets(t *testing.T) {
	prices := map[string]*config.ModelPrice{"a/priced": {Input: 2, CachedInput: new(0.2), Output: 8}}
	l := &usageLedger{}
	res := usageRun(
		usageStep("a/priced", provider.Usage{PromptTokens: 1_000_000, CachedPromptTokens: 500_000, CachedPromptTokensReported: true}),
		usageStep("", provider.Usage{PromptTokens: 10, CompletionTokens: 1}),
	)
	res.DescendantUsage = &provider.Usage{PromptTokens: 10, CompletionTokens: 1}
	l.addRun(res)
	l.addSide("summarize", "a/priced", provider.Usage{PromptTokens: 1_000_000})
	l.addSide("probe", "s/m", provider.Usage{PromptTokens: 5, CompletionTokens: 1})
	l.addSide("embed", "", provider.Usage{PromptTokens: 3})
	if got, want := l.render(prices)[2], "est. cost: $3.1000 (excludes unpriced: dispatch children, s/m (probe), unknown)"; got != want {
		t.Fatalf("line 3 = %q; want %q", got, want)
	}
}

// Ruling P4-j: a request whose raw usage has cached above prompt, or no prompt
// at all, is billing-weighted or missing; its whole bucket leaves the dollar
// total and is named, so the estimate is never silently low.
func TestLedgerRenderExcludesUnreliableUsage(t *testing.T) {
	prices := map[string]*config.ModelPrice{
		"a/priced":     {Input: 1000, Output: 1000},
		"abacus/route": {Input: 1000, CachedInput: new(100.0), Output: 1000},
	}
	// good costs (1000*1000 + 100*1000) / 1e6 = $1.1000.
	good := provider.Usage{PromptTokens: 1000, CompletionTokens: 100}
	weighted := provider.Usage{PromptTokens: 596, CachedPromptTokens: 4992, CachedPromptTokensReported: true, CompletionTokens: 20}
	for _, tc := range []struct {
		name  string
		build func(*usageLedger)
		want  string
	}{
		{"cached above prompt", func(l *usageLedger) {
			// Clamped, this request would still cost (596*100 + 20*1000)/1e6 = $0.0796.
			l.addRun(usageRun(usageStep("abacus/route", weighted)))
		}, "est. cost: $0.0000 (excludes unreliable usage: abacus/route)"},
		{"cached above prompt beside a good bucket", func(l *usageLedger) {
			l.addRun(usageRun(usageStep("abacus/route", good), usageStep("abacus/route", weighted), usageStep("a/priced", good)))
		}, "est. cost: $1.1000 (excludes unreliable usage: abacus/route)"},
		{"prompt zero with completion", func(l *usageLedger) {
			l.addRun(usageRun(usageStep("abacus/route", provider.Usage{CompletionTokens: 50}), usageStep("a/priced", good)))
		}, "est. cost: $1.1000 (excludes unreliable usage: abacus/route)"},
		{"unreliable side task", func(l *usageLedger) {
			// The $1.10 is a/priced's run cost: only the probe bucket is out.
			l.addRun(usageRun(usageStep("a/priced", good)))
			l.addSide("probe", "a/priced", provider.Usage{CompletionTokens: 5})
		}, "est. cost: $1.1000 (excludes unreliable usage: a/priced (probe))"},
		{"run and side buckets of one model are named apart", func(l *usageLedger) {
			l.addRun(usageRun(usageStep("abacus/route", weighted)))
			l.addSide("grounding", "abacus/route", provider.Usage{CompletionTokens: 5})
			l.addSide("summarize", "abacus/route", good)
		}, "est. cost: $1.1000 (excludes unreliable usage: abacus/route, abacus/route (grounding))"},
		{"one unpriced model under two tasks", func(l *usageLedger) {
			l.addRun(usageRun(usageStep("a/priced", good)))
			l.addSide("probe", "b/unpriced", good)
			l.addSide("embed", "b/unpriced", good)
			l.addSide("embed", "", good)
		}, "est. cost: $1.1000 (excludes unpriced: b/unpriced (embed), b/unpriced (probe), unknown)"},
		{"zero-usage request is still named", func(l *usageLedger) {
			l.addRun(usageRun(usageStep("", provider.Usage{}), usageStep("a/priced", good)))
		}, "est. cost: $1.1000 (excludes unpriced: unknown)"},
		{"mixed: unpriced wins over unreliable", func(l *usageLedger) {
			res := usageRun(usageStep("b/unpriced", provider.Usage{CompletionTokens: 9}), usageStep("abacus/route", weighted), usageStep("a/priced", good))
			res.DescendantUsage = &provider.Usage{PromptTokens: 5, CachedPromptTokens: 50, CompletionTokens: 1}
			l.addRun(res)
		}, "est. cost: $1.1000 (excludes unpriced: b/unpriced, dispatch children; excludes unreliable usage: abacus/route)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			tc.build(l)
			if got := l.render(prices)[2]; got != tc.want {
				t.Fatalf("line 3 = %q; want %q", got, tc.want)
			}
		})
	}
}

// The ledger is shared with background goroutines (auto-index embeddings in
// Task 4.3); -race is the assertion.
func TestLedgerConcurrentUse(t *testing.T) {
	l := &usageLedger{}
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			for range 200 {
				if i%2 == 0 {
					l.addSide("embed", "e/x", provider.Usage{PromptTokens: 1})
				} else {
					l.addRun(usageRun(usageStep("p/m", provider.Usage{PromptTokens: 1})))
				}
				_ = l.render(map[string]*config.ModelPrice{"p/m": {Input: 1}})
			}
		})
	}
	wg.Wait()
	if got, want := l.render(nil)[0], "usage (this process): input 400 tok (cached n/a), output 0 tok, 400 requests"; got != want {
		t.Fatalf("line 1 = %q; want %q", got, want)
	}
}

// usageThenFailCaller serves one tool-calling step with usage, then fails.
type usageThenFailCaller struct{ calls int }

func (c *usageThenFailCaller) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.calls++
	if c.calls > 1 {
		return agent.ModelResult{}, errors.New("provider boom")
	}
	return agent.ModelResult{
		Response: provider.ChatResponse{
			ToolCalls: []provider.ToolCall{{ID: "t1", Type: "function", Function: provider.ToolCallFunction{Name: "nope", Arguments: json.RawMessage(`{}`)}}},
			Usage:     provider.Usage{PromptTokens: 50, CompletionTokens: 5},
		},
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "primary", Model: "m"}},
	}, nil
}

// An ordinary REPL session has observability OFF; the ledger must still record
// every turn, completed or failed.
func TestContextPrintsUsageLedger(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		caller := &scriptCaller{responses: []agent.ModelResult{{
			Response:     provider.ChatResponse{Content: "answer", Usage: provider.Usage{PromptTokens: 120, CompletionTokens: 7, CachedPromptTokens: 30, CachedPromptTokensReported: true}},
			RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "primary", Model: "m"}},
		}}}
		sess := newTestSession(t, caller, t.TempDir())
		sess.usage = &usageLedger{}
		sess.prices = map[string]*config.ModelPrice{"primary/m": {Input: 1000, Output: 2000}}
		if sess.obs != nil {
			t.Fatal("fixture must run with observability off")
		}
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "goal", nil); err != nil {
			t.Fatal(err)
		}
		got := slash(t, sess, "/context")
		// (120*1000 + 7*2000) / 1e6 = $0.1340; cached bills at Input (no cached rate).
		want := "usage (this process): input 120 tok (30 cached, 25%), output 7 tok, 1 requests\n" +
			"last run: input 120 tok (30 cached), output 7 tok\n" +
			"est. cost: $0.1340\n"
		if !strings.HasSuffix(got, want) {
			t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
		}
	})
	t.Run("failed", func(t *testing.T) {
		sess := newTestSession(t, &usageThenFailCaller{}, t.TempDir())
		sess.usage = &usageLedger{}
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "goal", nil); err == nil {
			t.Fatal("runOnce succeeded; want the provider failure")
		}
		got := slash(t, sess, "/context")
		if want := "usage (this process): input 50 tok (cached n/a), output 5 tok, 1 requests\nlast run: input 50 tok (cached n/a), output 5 tok\n"; !strings.Contains(got, want) {
			t.Fatalf("/context =\n%s\nwant\n%s", got, want)
		}
	})
}

// /model set does not reset the ledger: both models stay in the totals. Runs
// through run(), so it also covers main's ledger and price wiring.
func TestUsageLedgerSurvivesModelSwitch(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	raw, err := os.ReadFile(fx.configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Price only the startup "agent" role (the first 32768 window).
	priced := strings.Replace(string(raw), `"context_window": 32768,`, `"context_window": 32768, "price": {"input": 1000000, "output": 2000000},`, 1)
	if priced == string(raw) {
		t.Fatal("fixture config shape changed")
	}
	if err := os.WriteFile(fx.configPath, []byte(priced), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		if sess.obs != nil {
			t.Fatal("fixture must run with observability off")
		}
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "first", nil); err != nil {
			t.Fatal(err)
		}
		if got := slash(t, sess, "/model set swap"); !strings.HasPrefix(got, "model: swap\n") {
			t.Fatalf("/model set swap = %q", got)
		}
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "second", nil); err != nil {
			t.Fatal(err)
		}
		// Each backend reports prompt 1, completion 1. Only primary/agent-model
		// is priced: 1*1 + 1*2 = $3.0000.
		want := "usage (this process): input 2 tok (cached n/a), output 2 tok, 2 requests\n" +
			"last run: input 1 tok (cached n/a), output 1 tok\n" +
			"est. cost: $3.0000 (excludes unpriced: alt/alt-model)\n"
		if got := slash(t, sess, "/context"); !strings.HasSuffix(got, want) {
			t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
		}
	})
}

// Ruling P4-l: a side-task line names its unreliable calls with the same test
// as the run lines (no prompt, or a counter that needed a clamp). Abacus non-streaming
// side calls send no prompt_tokens and would otherwise read "input 0 tok".
func TestLedgerSideTaskUnreliableQualifier(t *testing.T) {
	l := &usageLedger{}
	l.addSide("grounding", "abacus/route", provider.Usage{CompletionTokens: 9})
	l.addSide("grounding", "abacus/route", provider.Usage{PromptTokens: 80, CompletionTokens: 9})
	l.addSide("probe", "abacus/route", provider.Usage{PromptTokens: 40, CachedPromptTokens: 400, CompletionTokens: 6})
	l.addSide("summarize", "p/m", provider.Usage{PromptTokens: 300, CompletionTokens: 50})
	want := []string{
		"side task grounding: input 80 tok, output 18 tok, 2 calls; unreliable usage on 1 calls",
		"side task probe: input 40 tok, output 6 tok, 1 calls; unreliable usage on 1 calls",
		"side task summarize: input 300 tok, output 50 tok, 1 calls",
	}
	if got := l.render(nil)[3:]; !slices.Equal(got, want) {
		t.Fatalf("side task lines =\n%q\nwant\n%q", got, want)
	}
}

// Ruling P4-e/P4-k: the chain embedder records each successful embed under
// "embed" by the routed model, and a failed call leaves the ledger unchanged.
func TestChainEmbedderRecordsSideTaskUsage(t *testing.T) {
	ok := &provider.EmbedResponse{
		Model: "x", Provider: "e", Embeddings: [][]float64{{1}},
		Usage:        provider.Usage{PromptTokens: 7, TotalTokens: 7},
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "e", Model: "x"}},
	}
	// Priced by the routed provider/model, not the bare model name.
	prices := map[string]*config.ModelPrice{"e/x": {Input: 1_000_000}}
	for _, tc := range []struct {
		name     string
		plan     fakeEmbedPlan
		want     []string
		wantCost string
	}{
		{"success", fakeEmbedPlan{resp: ok}, []string{"side task embed: input 7 tok, output 0 tok, 1 calls"}, "est. cost: $7.0000"},
		{"failed call", fakeEmbedPlan{resp: ok, err: errors.New("backend down")}, []string{}, "est. cost: $0.0000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &usageLedger{}
			emb := newChainEmbedder(func(context.Context, provider.RoutingRequest) (embedExecutor, error) { return tc.plan, nil }, []string{"e/x"}, l)
			_, _ = emb.Embed(t.Context(), "e/x", []string{"q"})
			if got := l.render(nil)[3:]; !slices.Equal(got, tc.want) {
				t.Fatalf("side task lines = %q; want %q", got, tc.want)
			}
			if got := l.render(prices)[2]; got != tc.wantCost {
				t.Fatalf("line 3 = %q; want %q", got, tc.wantCost)
			}
		})
	}
}

// pricedAgentRole is the fixture price on primary/agent-model: $1 per input
// token and $2 per output token, so every est. cost below is exact.
const pricedAgentRole = `"price": {"input": 1000000, "output": 2000000}`

// A forced /compact runs main's summarizer once; its usage lands under
// "summarize", priced by the routed provider/model. Runs through run(), so it
// covers main's NewRouterSummarizerWithUsage wiring.
func TestLedgerRecordsSummarizeSideTask(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	raw, err := os.ReadFile(fx.configPath)
	if err != nil {
		t.Fatal(err)
	}
	priced := strings.Replace(string(raw), `"context_window": 32768,`, `"context_window": 32768, `+pricedAgentRole+`,`, 1)
	if priced == string(raw) {
		t.Fatal("fixture config shape changed")
	}
	if err := os.WriteFile(fx.configPath, []byte(priced), 0o600); err != nil {
		t.Fatal(err)
	}
	fx.primary.chatResponse = func(w http.ResponseWriter, _ *http.Request, body string) bool {
		if !strings.Contains(body, "SUMMARY_INPUT") {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{"content":"SUM"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":300,"completion_tokens":50,"total_tokens":350}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return true
	}
	fx.withSessionOpts(t, false, nil, func(t *testing.T, sess *replSession) {
		current := conversation.Conversation{ID: sess.session.id, Title: "t"}
		for i := range 5 {
			current.Messages = append(current.Messages,
				conversation.Message{Role: "user", Content: strings.Repeat(string(rune('a'+i)), 40)},
				conversation.Message{Role: "assistant", Content: strings.Repeat(string(rune('A'+i)), 40)})
		}
		if _, err := sess.session.store.Save(t.Context(), current); err != nil {
			t.Fatal(err)
		}
		if _, err := sess.session.switchTo(t.Context(), current.ID); err != nil {
			t.Fatal(err)
		}
		if got := slash(t, sess, "/compact"); !strings.HasSuffix(got, "(changed)\n") {
			t.Fatalf("/compact = %q; want a changed compaction", got)
		}
		// 300*1 + 50*2 = $400.0000.
		want := "usage (this process): input 0 tok (cached n/a), output 0 tok, 0 requests\n" +
			"last run: none\n" +
			"est. cost: $400.0000\n" +
			"side task summarize: input 300 tok, output 50 tok, 1 calls\n"
		if got := slash(t, sess, "/context"); !strings.HasSuffix(got, want) {
			t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
		}
	})
}

// Ruling P4-n: delegate_code's model call lands under "delegate", priced by
// the routed provider/model; a failed call adds nothing (P4-k). Runs through
// run() and the real delegate_code tool.
func TestLedgerRecordsDelegateSideTask(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	raw, err := os.ReadFile(fx.configPath)
	if err != nil {
		t.Fatal(err)
	}
	priced := strings.Replace(string(raw), `"think_mode": "none",`, `"think_mode": "none", `+pricedAgentRole+`,`, 1)
	if priced == string(raw) {
		t.Fatal("fixture config shape changed")
	}
	if err := os.WriteFile(fx.configPath, []byte(priced), 0o600); err != nil {
		t.Fatal(err)
	}
	var chats atomic.Int32
	fx.alt.chatResponse = func(w http.ResponseWriter, _ *http.Request, _ string) bool {
		if chats.Add(1) > 1 {
			http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
			return true
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"model":"alt-model","choices":[{"delta":{"content":"func A() {}"}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"model":"alt-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		return true
	}
	fx.withSession(t, []string{"-delegate", "-delegate-role", "swap"}, func(t *testing.T, sess *replSession) {
		var tool agent.Tool
		for _, tl := range sess.tools {
			if tl.Spec().Name == "delegate_code" {
				tool = tl
			}
		}
		if tool == nil {
			t.Fatal("delegate_code not mounted")
		}
		args := json.RawMessage(`{"prompt":"write func A"}`)
		if out, err := tool.Invoke(t.Context(), args); err != nil || out.IsError {
			t.Fatalf("first delegate = %+v, %v; want success", out, err)
		}
		if out, err := tool.Invoke(t.Context(), args); err != nil || !out.IsError {
			t.Fatalf("second delegate = %+v, %v; want the backend failure", out, err)
		}
		// One successful call: 120*1 + 30*2 = $180.0000.
		want := "est. cost: $180.0000\n" +
			"side task delegate: input 120 tok, output 30 tok, 1 calls\n"
		if got := slash(t, sess, "/context"); !strings.HasSuffix(got, want) {
			t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
		}
	})
	if got := chats.Load(); got != 2 {
		t.Fatalf("delegate backend chats = %d, want 2", got)
	}
}

// The startup tool-capability probe of an undeclared model lands under
// "probe" keyed by the full provider/model selector: the price on
// primary/agent-model applies, where the bare "agent-model" would read
// "excludes unpriced". Runs through run() and the real bootstrap Options.
func TestLedgerRecordsProbeSideTask(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	backend := newModelBackend(t, "primary", "agent-model")
	backend.chatResponse = func(w http.ResponseWriter, _ *http.Request, body string) bool {
		if !strings.Contains(body, "get_time") {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}],"usage":{"prompt_tokens":40,"completion_tokens":6,"total_tokens":46}}`)
		return true
	}
	configPath := filepath.Join(t.TempDir(), "models.json")
	// No declared capabilities: tool_call is unknown, so preflight probes.
	configJSON := fmt.Sprintf(`{
  "providers": {"primary": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "primary", "type": "dense", "context_window": 32768, %s}},
  "defaults": {"agent": "agent"}
}`, backend.url, pricedAgentRole)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after session ready")
	var got string
	err := run([]string{"-config", configPath, "-root", t.TempDir(), "-no-probe", "-no-session", "-no-memory", "-no-rag",
		"-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor"}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			got = slash(t, sess, "/context")
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	// 40*1 + 6*2 = $52.0000.
	want := "usage (this process): input 0 tok (cached n/a), output 0 tok, 0 requests\n" +
		"last run: none\n" +
		"est. cost: $52.0000\n" +
		"side task probe: input 40 tok, output 6 tok, 1 calls\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
	}
}

// writeSideTaskRunConfig serves one openai-compat provider "test" with priced
// agent-model and qwen3-embedding:8b. Every non-streaming chat (grounding
// stages, source summaries) reports usage 80/9 and answers with a valid
// source summary; every embed call reports 7 input tokens.
func writeSideTaskRunConfig(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"},{"id":"qwen3-embedding:8b"}]}`)
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			data := make([]any, len(req.Input))
			for i := range data {
				data[i] = map[string]any{"embedding": realisticTestVector(), "index": i}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": "qwen3-embedding:8b",
				"usage": map[string]int{"prompt_tokens": 7, "total_tokens": 7}})
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"model":"agent-model","choices":[{"message":{"role":"assistant","content":"{\"abstract\":\"Defines A.\",\"overview\":\"Package a defines A.\"}"}}],"usage":{"prompt_tokens":80,"completion_tokens":9,"total_tokens":89}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"test": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "stream", "tool_call"], %s},
    "embedding": {"name": "qwen3-embedding:8b", "provider": "test", "type": "embedding", "dimensions": 768,
      "capabilities": ["embed"], "price": {"input": 1000000, "output": 0}}
  },
  "defaults": {"agent": "agent", "embedding": "embedding", "chat": "agent", "summarize": "agent"}
}`, server.URL, pricedAgentRole)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root
}

// readyRetrieveOf returns the session's retrieve wrapper.
func readyRetrieveOf(t *testing.T, tools []agent.Tool) *readyRetrieve {
	t.Helper()
	for _, tool := range tools {
		if ready, ok := tool.(*readyRetrieve); ok {
			return ready
		}
	}
	t.Fatal("no retrieve tool mounted")
	return nil
}

// One retrieve query lands under "embed" and one grounding judge (extract +
// verify) under "grounding", each priced by its routed provider/model, on
// both startup retrieve construction sites in run().
func TestLedgerRecordsRetrievalSideTasks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seedAuto bool
	}{
		{name: "explicit rag-db"},
		{name: "discovered auto index", seedAuto: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeSideTaskRunConfig(t)
			args := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
				"-no-session", "-no-memory", "-no-project-context", "-no-git-context", "-no-editor", "-grounding"}
			if tc.seedAuto {
				canonical, err := filepath.EvalSymlinks(root)
				if err != nil {
					t.Fatal(err)
				}
				dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, canonical)
				if err != nil {
					t.Fatal(err)
				}
				seedIndex(t, dbPath, workspaceID, "test/qwen3-embedding:8b")
			} else {
				dbPath := filepath.Join(t.TempDir(), "explicit.db")
				seedIndex(t, dbPath, "workspace:ignored", "test/qwen3-embedding:8b")
				args = append(args, "-no-auto-index", "-rag-db", dbPath)
			}
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop after side tasks")
			var got string
			err := run(args, stdin, stdout, stderr, runHooks{
				startAutoIndex: func() func() { return func() {} },
				afterSessionReady: func(sess *replSession) error {
					out, err := readyRetrieveOf(t, sess.tools).Invoke(t.Context(), json.RawMessage(`{"query":"find A"}`))
					if err != nil || out.IsError {
						t.Fatalf("retrieve = %+v, %v", out, err)
					}
					if sess.grounding == nil {
						t.Fatal("grounding service not built")
					}
					judge, _, err := sess.grounding.newJudge()
					if err != nil {
						t.Fatal(err)
					}
					// The fixture's verify reply is malformed; both calls still succeeded.
					_, _ = judge.Judge(t.Context(), "A is defined.", []rag.SearchResult{{Chunk: rag.Chunk{ID: "c1", Source: "a.go", Content: "func A() {}"}}}, groundingEvidenceMaxBytes)
					got = slash(t, sess, "/context")
					return errStop
				},
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			// grounding 2*(80*1 + 9*2) = 196, embed 7*1 = 7.
			want := "est. cost: $203.0000\n" +
				"side task embed: input 7 tok, output 0 tok, 1 calls\n" +
				"side task grounding: input 160 tok, output 18 tok, 2 calls\n"
			if !strings.HasSuffix(got, want) {
				t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
			}
		})
	}
}

// The real background auto-index records its startup embed probe, its index
// embeds and, under -progressive, one source summary per source; a query on
// the published generation then records through the refreshed reader.
func TestLedgerRecordsAutoIndexSideTasks(t *testing.T) {
	configPath, root := writeSideTaskRunConfig(t)
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after auto-index")
	var sess *replSession
	var got string
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session", "-no-memory",
		"-no-project-context", "-no-git-context", "-no-editor", "-progressive"}, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(s *replSession) error { sess = s; return nil },
		afterAutoIndexStart: func(_ lineSourceMode, retrieve agent.Tool, _ *feedbackService) error {
			ready, ok := retrieve.(*readyRetrieve)
			if !ok {
				t.Fatalf("retrieve = %T", retrieve)
			}
			for deadline := time.Now().Add(30 * time.Second); retrieveStateOf(ready) != retrieveReady; {
				if retrieveStateOf(ready) == retrieveFailed || time.Now().After(deadline) {
					t.Fatalf("auto-index did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			out, err := ready.Invoke(t.Context(), json.RawMessage(`{"query":"find A"}`))
			if err != nil || out.IsError {
				t.Fatalf("retrieve = %+v, %v", out, err)
			}
			got = slash(t, sess, "/context")
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	// embed: probe + 1 index batch + 1 query = 3 calls * 7 = 21 (= $21);
	// source summary: 80*1 + 9*2 = 98.
	want := "est. cost: $119.0000\n" +
		"side task embed: input 21 tok, output 0 tok, 3 calls\n" +
		"side task source-summary: input 80 tok, output 9 tok, 1 calls\n"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("/context =\n%s\nwant suffix\n%s", got, want)
	}
}
