package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
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
	want := []string{
		"usage (this process): input 200 tok (150 cached, 75%), output 10 tok, 3 requests",
		"last run: input 200 tok (150 cached), output 10 tok",
	}
	if got := l.render(nil)[:2]; !slices.Equal(got, want) {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
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
// every unpriced bucket once: "unknown", "dispatch children" and side models.
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
	if got, want := l.render(prices)[2], "est. cost: $3.1000 (excludes unpriced: dispatch children, s/m, unknown)"; got != want {
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
			l.addRun(usageRun(usageStep("a/priced", good)))
			l.addSide("probe", "a/priced", provider.Usage{CompletionTokens: 5})
		}, "est. cost: $1.1000 (excludes unreliable usage: a/priced)"},
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
