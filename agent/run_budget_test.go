package agent

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func testRunBudget(t *testing.T, ctx context.Context, req Request) (context.Context, Request, *runBudget) {
	t.Helper()
	ctx, req, b := newRunBudget(ctx, req)
	t.Cleanup(func() { b.close() })
	return ctx, req, b
}

func TestRunBudgetCapacityIntersection(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		parent, child                 Request
		input, output, predict, steps int
	}{
		{"parent defaults", Request{}, Request{Budget: Budget{InputCeiling: 16384}, MaxSteps: 32}, 8192, 0, 0, 16},
		{"smaller parent", Request{Budget: Budget{InputCeiling: 4000, OutputReserve: 128}, MaxSteps: 2}, Request{Budget: Budget{InputCeiling: 16000, OutputReserve: 1024}, MaxSteps: 6}, 4000, 128, 128, 2},
		{"smaller child", Request{Budget: Budget{InputCeiling: 16000, OutputReserve: 1024}, MaxSteps: 6}, Request{Budget: Budget{InputCeiling: 4000, OutputReserve: 128}, MaxSteps: 2}, 4000, 128, 128, 2},
		{"parent generation without reserve", Request{Options: provider.ModelOptions{NumPredict: 100}}, Request{Budget: Budget{OutputReserve: 1024}}, 8192, 100, 100, 16},
		{"uncapped child inherits unbounded parent cap", Request{Options: provider.ModelOptions{NumPredict: 100}}, Request{}, 8192, 0, 100, 16},
		{"unlimited child inherits unbounded parent cap", Request{Options: provider.ModelOptions{NumPredict: 100}}, Request{Options: provider.ModelOptions{NumPredict: -1}}, 8192, 0, 100, 16},
		{"inherited finite fallback", Request{Budget: Budget{TotalTokens: 10000}}, Request{}, 8192, 0, 2048, 16},
		{"child generation default below parent", Request{Budget: Budget{TotalTokens: 10000, OutputReserve: 4096}}, Request{}, 8192, 0, 2048, 16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, _ := testRunBudget(t, t.Context(), tc.parent)
			_, got, _ := testRunBudget(t, ctx, tc.child)
			if got.Budget.InputCeiling != tc.input || got.Budget.OutputReserve != tc.output ||
				got.Options.NumPredict != tc.predict || got.MaxSteps != tc.steps {
				t.Fatalf("effective request = %+v; want input/output/predict/steps %d/%d/%d/%d", got, tc.input, tc.output, tc.predict, tc.steps)
			}
		})
	}
}

func TestRunBudgetGenerationPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		total, reserve, predict, want int
	}{
		{"reserve wins", 10000, 128, 300, 128},
		{"explicit predict", 10000, 0, 300, 300},
		{"finite fallback", 10000, 0, 0, 2048},
		{"finite unlimited replaced", 10000, 0, -1, 2048},
		{"unbounded unchanged", 0, 0, -1, -1},
		{"unbounded zero unchanged", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := Request{Budget: Budget{TotalTokens: tc.total, OutputReserve: tc.reserve}, Options: provider.ModelOptions{NumPredict: tc.predict}}
			_, got, _ := testRunBudget(t, t.Context(), input)
			if got.Options.NumPredict != tc.want || got.Budget.OutputReserve != tc.reserve {
				t.Fatalf("predict/reserve = %d/%d, want %d/%d", got.Options.NumPredict, got.Budget.OutputReserve, tc.want, tc.reserve)
			}
		})
	}
}

func TestRunBudgetClosedAncestor(t *testing.T) {
	for _, closeRoot := range []bool{false, true} {
		ctx, _, root := testRunBudget(t, t.Context(), Request{})
		ctx, _, middle := testRunBudget(t, ctx, Request{})
		if closeRoot {
			root.close()
		} else {
			middle.close()
		}
		ctx, _, child := testRunBudget(t, context.WithoutCancel(ctx), Request{})
		r, err := child.reserve(ctx, 1)
		if r != nil || !errors.Is(err, errRunBudgetExhausted) {
			t.Fatalf("late admission = %v, %v", r, err)
		}
		if !child.stopped() {
			t.Fatal("closed ancestor did not stop child")
		}
	}
}

func TestRunBudgetCanceledAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	ctx, _, b := testRunBudget(t, ctx, Request{})
	cancel()
	r, err := b.reserve(ctx, 1)
	if r != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("admission = %v, %v", r, err)
	}
}

func TestRunBudgetConcurrentReservations(t *testing.T) {
	ctx, _, root := testRunBudget(t, t.Context(), Request{Budget: Budget{InputCeiling: 100000, TotalTokens: 8096}})
	var wg sync.WaitGroup
	// Four bounded siblings report admission while retaining their reservation.
	admitted := make(chan *tokenReservation, 4)
	for range 4 {
		childCtx, _, child := testRunBudget(t, ctx, Request{Budget: Budget{InputCeiling: 100000, OutputReserve: 1024}})
		wg.Go(func() {
			r, err := child.reserve(childCtx, 1000)
			if err != nil {
				t.Errorf("sibling admission: %v", err)
			}
			admitted <- r
		})
	}
	wg.Wait()
	close(admitted)
	if root.stopped() {
		t.Fatal("in-flight reservations permanently stopped root")
	}
	fifthCtx, _, fifth := testRunBudget(t, ctx, Request{Budget: Budget{OutputReserve: 1024}})
	if r, err := fifth.reserve(fifthCtx, 1000); r != nil || !errors.Is(err, errRunBudgetExhausted) {
		t.Fatalf("fifth admission = %v, %v", r, err)
	}
	separateCtx, _, separate := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: 8096, OutputReserve: 1024}})
	r, err := separate.reserve(separateCtx, 1000)
	if err != nil {
		t.Fatalf("independent root: %v", err)
	}
	r.settle(ModelResult{}, nil)
	count := 0
	for r := range admitted {
		if r != nil {
			count++
			r.settle(ModelResult{}, nil)
		}
	}
	if count != 4 || !root.stopped() {
		t.Fatalf("admitted=%d, exhausted=%v", count, root.stopped())
	}
}

func TestRunBudgetSettlement(t *testing.T) {
	for _, tc := range []struct {
		name            string
		usage           provider.Usage
		err             error
		outcome         *provider.RouteOutcome
		responseOutcome bool
		charge          int
		telemetry       bool
		continues       bool // charged beyond the reservation without a generation overrun
	}{
		{name: "prompt floor", usage: provider.Usage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100}, charge: 120, telemetry: true},
		{name: "reported larger prompt", usage: provider.Usage{PromptTokens: 120, CompletionTokens: 20, TotalTokens: 140}, charge: 140, telemetry: true},
		{name: "prompt undercount beyond reservation", usage: provider.Usage{PromptTokens: 200, CompletionTokens: 20, TotalTokens: 220}, charge: 220, telemetry: true, continues: true},
		{name: "derive logical total", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20}, charge: 120, telemetry: true},
		{name: "reasoning subset", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, ReasoningTokens: 10, ReasoningTokensReported: true}, charge: 120, telemetry: true},
		{name: "total only", usage: provider.Usage{TotalTokens: 100}, charge: 150, telemetry: true},
		{name: "missing", charge: 150},
		{name: "failed with usage", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, err: errors.New("provider failed"), charge: 150, telemetry: true},
		{name: "callback failed", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, err: errors.New("callback failed"), charge: 150, telemetry: true},
		{name: "router sentinel without outcome", err: provider.ErrRouterClosed, charge: 150},
		{name: "multiple attempts", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, outcome: &provider.RouteOutcome{Attempts: make([]provider.RouteAttempt, 2)}, charge: 150, telemetry: true},
		{name: "response multiple attempts", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, outcome: &provider.RouteOutcome{Attempts: make([]provider.RouteAttempt, 2)}, responseOutcome: true, charge: 150, telemetry: true},
		{name: "fallback without attempts", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, outcome: &provider.RouteOutcome{FallbacksUsed: 1}, charge: 150, telemetry: true},
		{name: "response fallback without attempts", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}, outcome: &provider.RouteOutcome{FallbacksUsed: 1}, responseOutcome: true, charge: 150, telemetry: true},
		{name: "overrun", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 80, TotalTokens: 180}, charge: 180, telemetry: true},
		{name: "contradiction", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 119}, charge: 150},
		{name: "negative prompt", usage: provider.Usage{PromptTokens: -1}, charge: 150},
		{name: "negative completion", usage: provider.Usage{CompletionTokens: -1}, charge: 150},
		{name: "negative total", usage: provider.Usage{TotalTokens: -1}, charge: 150},
		{name: "negative reasoning", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120, ReasoningTokens: -1}, charge: 150},
		{name: "larger contradictory total", usage: provider.Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 500}, charge: 500},
		{name: "overflow", usage: provider.Usage{PromptTokens: math.MaxInt, CompletionTokens: 1}, charge: math.MaxInt},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, _, root := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: 300}})
			childCtx, _, child := testRunBudget(t, ctx, Request{Budget: Budget{OutputReserve: 50}})
			r, err := child.reserve(childCtx, 100)
			if err != nil {
				t.Fatal(err)
			}
			mr := ModelResult{Response: provider.ChatResponse{Usage: tc.usage}, RouteOutcome: tc.outcome}
			if tc.responseOutcome {
				mr.Response.RouteOutcome, mr.RouteOutcome = tc.outcome, nil
			}
			r.settle(mr, tc.err)
			r.settle(mr, tc.err) // Must not release/charge/aggregate twice.
			if child.stopped() != (tc.charge > 150 && !tc.continues) {
				t.Fatalf("overrun stop = %v", child.stopped())
			}

			probeCtx, _, probe := testRunBudget(t, ctx, Request{Options: provider.ModelOptions{NumPredict: 1}})
			remaining := max(0, 300-tc.charge)
			if got, err := probe.reserve(probeCtx, remaining); got != nil || !errors.Is(err, errRunBudgetExhausted) {
				t.Fatalf("one above remaining: %v, %v", got, err)
			}
			if remaining > 0 {
				got, err := probe.reserve(probeCtx, remaining-1)
				if err != nil {
					t.Fatalf("exact remaining: %v", err)
				}
				got.settle(ModelResult{}, nil)
			}
			snapshot := root.close()
			if !tc.telemetry {
				if snapshot != nil {
					t.Fatalf("fabricated telemetry: %+v", snapshot)
				}
			} else {
				want := tc.usage
				want.ReasoningTokens, want.ReasoningTokensReported = 0, false
				if snapshot == nil || *snapshot != want {
					t.Fatalf("telemetry = %+v, want %+v", snapshot, want)
				}
			}
		})
	}
}

func TestRunBudgetInvalidEstimate(t *testing.T) {
	ctx, _, b := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: math.MaxInt, OutputReserve: 1}})
	for _, estimate := range []int{-1, math.MaxInt} {
		if r, err := b.reserve(ctx, estimate); r != nil || !errors.Is(err, errRunBudgetExhausted) {
			t.Fatalf("estimate %d: %v, %v", estimate, r, err)
		}
	}
	r, err := b.reserve(ctx, math.MaxInt-1)
	if err != nil {
		t.Fatal(err)
	}
	r.settle(ModelResult{}, nil)
	if !b.stopped() {
		t.Fatal("maximum charge wrapped")
	}
}

func TestRunBudgetUnboundedIgnoresReportedSpend(t *testing.T) {
	ctx, _, b := testRunBudget(t, t.Context(), Request{Options: provider.ModelOptions{NumPredict: 1}})
	// Without a finite allowance an unusable estimate has nothing to protect.
	for _, estimate := range []int{-1, math.MaxInt} {
		r, err := b.reserve(ctx, estimate)
		if err != nil {
			t.Fatalf("unbounded estimate %d: %v", estimate, err)
		}
		r.settle(ModelResult{}, nil)
	}
	for range 2 {
		r, err := b.reserve(ctx, 100)
		if err != nil {
			t.Fatalf("unbounded admission: %v", err)
		}
		r.settle(ModelResult{Response: provider.ChatResponse{Usage: provider.Usage{TotalTokens: math.MaxInt}}}, nil)
		if b.stopped() {
			t.Fatal("unbounded run stopped on reported spend")
		}
	}
}

func TestRunBudgetSnapshotAfterLateSettlement(t *testing.T) {
	ctx, _, root := testRunBudget(t, t.Context(), Request{})
	childCtx, _, child := testRunBudget(t, ctx, Request{})
	first, err := child.reserve(childCtx, 10)
	if err != nil {
		t.Fatal(err)
	}
	first.settle(ModelResult{Response: provider.ChatResponse{Usage: provider.Usage{TotalTokens: 10}}}, nil)
	second, err := child.reserve(childCtx, 10)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := root.close()
	want := &provider.Usage{TotalTokens: 10}
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("snapshot: %+v", snapshot)
	}
	second.settle(ModelResult{Response: provider.ChatResponse{Usage: provider.Usage{TotalTokens: 20}}}, nil)
	second.settle(ModelResult{}, nil)
	if !reflect.DeepEqual(snapshot, want) {
		t.Fatalf("mutated snapshot: %+v", snapshot)
	}
	if !child.stopped() {
		t.Fatal("child not sealed")
	}
}

func TestRunBudgetOverrunBlocksLaterAdmission(t *testing.T) {
	ctx, _, b := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: 1000, OutputReserve: 10}})
	r, err := b.reserve(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Completion 20 exceeds the fixed cap of 10 while charged stays far below 1000.
	r.settle(ModelResult{Response: provider.ChatResponse{Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}}}, nil)
	if r, err := b.reserve(ctx, 1); r != nil || !errors.Is(err, errRunBudgetExhausted) {
		t.Fatalf("overrun run re-admitted: %v %v", r, err)
	}
	childCtx, _, child := testRunBudget(t, ctx, Request{})
	if r, err := child.reserve(childCtx, 1); r != nil || !errors.Is(err, errRunBudgetExhausted) {
		t.Fatalf("child of overrun run admitted: %v %v", r, err)
	}
}

func TestRunBudgetUsedOverflowFailsClosed(t *testing.T) {
	ctx, _, _ := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: math.MaxInt}})
	heldCtx, _, holder := testRunBudget(t, ctx, Request{Options: provider.ModelOptions{NumPredict: 1}})
	held, err := holder.reserve(heldCtx, math.MaxInt/2)
	if err != nil {
		t.Fatal(err)
	}
	defer held.settle(ModelResult{}, nil)
	spendCtx, _, spender := testRunBudget(t, ctx, Request{Options: provider.ModelOptions{NumPredict: 1}})
	r, err := spender.reserve(spendCtx, 1)
	if err != nil {
		t.Fatal(err)
	}
	r.settle(ModelResult{Response: provider.ChatResponse{Usage: provider.Usage{TotalTokens: math.MaxInt - 2}}}, nil)
	// charged + reserved now overflows int; admission must fail rather than wrap.
	probeCtx, _, probe := testRunBudget(t, ctx, Request{Options: provider.ModelOptions{NumPredict: 1}})
	if got, err := probe.reserve(probeCtx, 0); got != nil || !errors.Is(err, errRunBudgetExhausted) {
		t.Fatalf("wrapped usage admitted: %v %v", got, err)
	}
}
