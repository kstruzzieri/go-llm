package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

type budgetModelFunc func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (ModelResult, error)

func (f budgetModelFunc) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
	return f(ctx, req, onToken)
}

type budgetObserver struct {
	nopObserver
	pressure func(context.Context, PressureEvent) error
	step     func(context.Context, StepEvent) error
	token    func(context.Context, TokenEvent) error
}

func (o budgetObserver) OnPressure(ctx context.Context, e PressureEvent) error {
	if o.pressure != nil {
		return o.pressure(ctx, e)
	}
	return nil
}
func (o budgetObserver) OnStep(ctx context.Context, e StepEvent) error {
	if o.step != nil {
		return o.step(ctx, e)
	}
	return nil
}
func (o budgetObserver) OnToken(ctx context.Context, e TokenEvent) error {
	if o.token != nil {
		return o.token(ctx, e)
	}
	return nil
}

func assertBudgetUsage(t *testing.T, res Result) {
	t.Helper()
	var want provider.Usage
	for _, step := range res.Steps {
		want.PromptTokens += step.Response.Usage.PromptTokens
		want.CompletionTokens += step.Response.Usage.CompletionTokens
		want.TotalTokens += step.Response.Usage.TotalTokens
	}
	if res.Usage != want {
		t.Fatalf("local Usage = %+v; step sum = %+v", res.Usage, want)
	}
	wire, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "\"DescendantUsage\":") != (res.DescendantUsage != nil) {
		t.Fatalf("descendant JSON presence: %s", wire)
	}
}

func TestRunBudgetAdmissionAfterAssembly(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%v", mixed), func(t *testing.T) {
			req := Request{Goal: "q", Budget: Budget{InputCeiling: 8192, OutputReserve: 64, TotalTokens: 100000}, Tools: []Tool{structuredTool{}}}
			baseline := &toolThenAnswerCaller{}
			want := &pressureRec{}
			if _, err := New(baseline, ContextManager{Mixed: mixed}).Run(t.Context(), req, want); err != nil {
				t.Fatal(err)
			}
			if len(want.pressures) != 2 {
				t.Fatal(want.pressures)
			}

			// The first tiny allowance reaches assembly but never Chat.
			req.Budget.TotalTokens = 1
			tiny := &toolThenAnswerCaller{}
			tinyObs := &pressureRec{}
			res, err := New(tiny, ContextManager{Mixed: mixed}).Run(t.Context(), req, tinyObs)
			if err != nil || res.StopReason != BudgetReached || len(tiny.reqs) != 0 {
				t.Fatalf("tiny: %+v, %v; calls=%d", res, err, len(tiny.reqs))
			}
			if !reflect.DeepEqual(tinyObs.pressures, want.pressures[:1]) {
				t.Fatalf("pressure changed: %+v vs %+v", tinyObs.pressures, want.pressures[:1])
			}
			assertBudgetUsage(t, res)

			// Leave one credit after the first request. The second assembly has
			// a real structured anchor and must still use its static capacity.
			req.Budget.TotalTokens = want.pressures[0].Pressure.InputTokens + 64 + 1
			limited := &toolThenAnswerCaller{}
			got := &pressureRec{}
			res, err = New(limited, ContextManager{Mixed: mixed}).Run(t.Context(), req, got)
			if err != nil || res.StopReason != BudgetReached || len(limited.reqs) != 1 {
				t.Fatalf("limited: %+v, %v; calls=%d", res, err, len(limited.reqs))
			}
			if !reflect.DeepEqual(got.pressures, want.pressures) {
				t.Fatalf("assembly changed: %+v vs %+v", got.pressures, want.pressures)
			}
			assertBudgetUsage(t, res)
		})
	}
}

func TestRunBudgetKeepsContextExhaustion(t *testing.T) {
	caller := &toolThenAnswerCaller{}
	_, err := New(caller, ContextManager{}).Run(t.Context(), Request{Goal: strings.Repeat("x", 10000), Budget: Budget{InputCeiling: 100, TotalTokens: 1}}, nil)
	if !errors.Is(err, ErrContextExhausted) || len(caller.reqs) != 0 {
		t.Fatalf("err=%v calls=%d", err, len(caller.reqs))
	}
}

func TestRunBudgetFixedGeneration(t *testing.T) {
	for _, predict := range []int{0, 300} {
		t.Run(fmt.Sprintf("predict=%d", predict), func(t *testing.T) {
			run := func(total int) (Result, []int, []PressureEvent) {
				t.Helper()
				var caps []int
				mc := budgetModelFunc(func(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
					caps = append(caps, req.Options.NumPredict)
					return echoCall(fmt.Sprint(len(caps)), fmt.Sprintf("{\"n\":%d}", len(caps))), nil
				})
				obs := &pressureRec{}
				res, err := New(mc, ContextManager{}).Run(t.Context(), Request{
					Goal: "q", MaxSteps: 3, Tools: []Tool{echoTool{name: "echo"}},
					Budget: Budget{TotalTokens: total}, Options: provider.ModelOptions{NumPredict: predict},
				}, obs)
				if err != nil {
					t.Fatal(err)
				}
				return res, caps, obs.pressures
			}
			_, _, baseline := run(100000)
			generation := predict
			if generation == 0 {
				generation = 2048
			}
			limit := baseline[0].Pressure.InputTokens + baseline[1].Pressure.InputTokens + 2*generation + 1
			res, caps, pressures := run(limit)
			if res.StopReason != BudgetReached || !reflect.DeepEqual(caps, []int{generation, generation}) {
				t.Fatalf("stop=%v caps=%v", res.StopReason, caps)
			}
			if !reflect.DeepEqual(pressures, baseline) {
				t.Fatalf("static pressure changed: %+v vs %+v", pressures, baseline)
			}
			for _, e := range pressures {
				if e.Pressure.InputBudget != 8192 {
					t.Fatal(e.Pressure)
				}
			}
			if res.DescendantUsage != nil {
				t.Fatal(res.DescendantUsage)
			}
			assertBudgetUsage(t, res)
		})
	}
}

func TestRunBudgetSettlesBeforeCallbacks(t *testing.T) {
	for _, phase := range []string{"token", "step", "block", "provider", "cancel", "pressure"} {
		t.Run(phase, func(t *testing.T) {
			parentCtx, _, parent := testRunBudget(t, t.Context(), Request{Budget: Budget{TotalTokens: 10000}})
			childCtx, cancel := context.WithCancel(parentCtx)
			defer cancel()
			failure := errors.New("callback failed")
			estimate, calls := 0, 0
			probe := func(charge int) {
				t.Helper()
				ctx, _, b := testRunBudget(t, parentCtx, Request{Options: provider.ModelOptions{NumPredict: 1}})
				remaining := 10000 - charge
				if r, err := b.reserve(ctx, remaining); r != nil || !errors.Is(err, errRunBudgetExhausted) {
					t.Fatalf("settlement not visible: %v, %v", r, err)
				}
				r, err := b.reserve(ctx, remaining-1)
				if err != nil {
					t.Fatal(err)
				}
				r.settle(ModelResult{}, nil)
			}
			obs := budgetObserver{pressure: func(_ context.Context, e PressureEvent) error {
				estimate = e.Pressure.InputTokens
				if phase == "pressure" {
					return failure
				}
				return nil
			}}
			if phase == "token" {
				obs.token = func(context.Context, TokenEvent) error { return failure }
			}
			if phase == "step" {
				obs.step = func(context.Context, StepEvent) error { probe(estimate + 1); return failure }
			}
			var opts []Option
			if phase == "block" {
				opts = append(opts, WithInterceptors(&stubInterceptor{name: "block", output: func(OutputInspection) []Finding {
					probe(estimate + 1)
					return []Finding{{Rule: "deny", Verdict: VerdictBlock, Risk: 100}}
				}}))
			}
			mc := budgetModelFunc(func(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
				calls++
				mr := ModelResult{Response: provider.ChatResponse{Content: "answer", Done: true, Usage: provider.Usage{PromptTokens: estimate, CompletionTokens: 1, TotalTokens: estimate + 1}}}
				switch phase {
				case "token":
					return mr, onToken(mr.Response)
				case "provider":
					return mr, failure
				case "cancel":
					cancel()
					return mr, context.Canceled
				default:
					return mr, nil
				}
			})
			res, err := New(mc, ContextManager{}, opts...).Run(childCtx, Request{Goal: "q", Budget: Budget{OutputReserve: 64}}, obs)
			if phase == "block" {
				var blocked *BlockedError
				if !errors.As(err, &blocked) {
					t.Fatalf("block: %v", err)
				}
			} else if phase == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, failure) {
				t.Fatalf("error: %v", err)
			}
			if phase == "pressure" {
				if calls != 0 {
					t.Fatal(calls)
				}
				probe(0)
			} else if phase != "step" && phase != "block" {
				probe(estimate + 64)
			}
			if phase == "token" || phase == "provider" || phase == "cancel" {
				if res.Usage != (provider.Usage{}) || len(res.Steps) != 0 {
					t.Fatalf("failed Chat recorded: %+v", res)
				}
			}
			snapshot := parent.close()
			if phase == "pressure" {
				if snapshot != nil {
					t.Fatal(snapshot)
				}
			} else if snapshot == nil || snapshot.TotalTokens != estimate+1 {
				t.Fatalf("known failed/success usage: %+v", snapshot)
			}
			assertBudgetUsage(t, res)
		})
	}
}

func TestRunBudgetNestedCallbacks(t *testing.T) {
	for _, phase := range []string{"pressure", "token", "step", "output"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			childCalls := 0
			child := New(budgetModelFunc(func(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
				childCalls++
				if req.Options.NumPredict != 64 {
					t.Fatalf("child cap = %d", req.Options.NumPredict)
				}
				return ModelResult{Response: provider.ChatResponse{Content: "child", Usage: provider.Usage{TotalTokens: 7}}}, nil
			}), ContextManager{})
			estimate := 0
			var parentCtx context.Context
			nest := func(ctx context.Context) {
				t.Helper()
				res, err := child.Run(ctx, Request{Goal: "q", Budget: Budget{OutputReserve: 1024}}, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantStop := BudgetReached
				if phase == "pressure" {
					wantStop = Completed
				}
				if res.StopReason != wantStop {
					t.Fatalf("child: %+v", res)
				}
			}
			obs := budgetObserver{pressure: func(ctx context.Context, e PressureEvent) error {
				parentCtx = ctx
				estimate = e.Pressure.InputTokens
				if phase == "pressure" {
					nest(ctx)
				}
				return nil
			}}
			if phase == "token" {
				obs.token = func(ctx context.Context, _ TokenEvent) error { nest(ctx); return nil }
			}
			if phase == "step" {
				obs.step = func(ctx context.Context, _ StepEvent) error { nest(ctx); return nil }
			}
			var opts []Option
			if phase == "output" {
				opts = append(opts, WithInterceptors(&stubInterceptor{name: "nested", output: func(OutputInspection) []Finding { nest(parentCtx); return nil }}))
			}
			mc := budgetModelFunc(func(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
				mr := ModelResult{Response: provider.ChatResponse{Content: "parent", Usage: provider.Usage{PromptTokens: estimate, CompletionTokens: 1, TotalTokens: estimate + 1}}}
				if phase == "token" {
					return mr, onToken(mr.Response)
				}
				return mr, nil
			})
			// Measure static E, then leave enough for one whole request plus
			// the prompt floor. A token callback cannot borrow its reservation.
			rec := &pressureRec{}
			_, err := New(&scriptedCaller{responses: []ModelResult{finalAnswer("ok")}}, ContextManager{}).Run(ctx, Request{Goal: "q", Budget: Budget{OutputReserve: 64}}, rec)
			if err != nil {
				t.Fatal(err)
			}
			e := rec.pressures[0].Pressure.InputTokens
			total := 2*e + 65
			res, err := New(mc, ContextManager{}, opts...).Run(ctx, Request{Goal: "q", Budget: Budget{TotalTokens: total, OutputReserve: 64}}, obs)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "token" {
				if childCalls != 0 || res.DescendantUsage != nil {
					t.Fatalf("borrowed in-flight credit: calls=%d, %+v", childCalls, res.DescendantUsage)
				}
			} else {
				if childCalls != 1 || res.DescendantUsage == nil || res.DescendantUsage.TotalTokens != 7 {
					t.Fatalf("nested accounting: calls=%d, %+v", childCalls, res.DescendantUsage)
				}
			}
			assertBudgetUsage(t, res)
		})
	}
}

func TestRunBudgetOverrunStopsBeforeTools(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		tool := &invokeCountingTool{echoTool: echoTool{name: "echo"}}
		mr := echoCall("one", "{}")
		mr.Response.Content = "accepted"
		mr.Response.Usage.TotalTokens = 20000
		caller := &scriptedCaller{responses: []ModelResult{mr}}
		var opts []Option
		if blocked {
			opts = append(opts, WithInterceptors(&stubInterceptor{name: "block", output: func(OutputInspection) []Finding { return []Finding{{Rule: "deny", Verdict: VerdictBlock, Risk: 100}} }}))
		}
		res, err := New(caller, ContextManager{}, opts...).Run(t.Context(), Request{Goal: "q", Tools: []Tool{tool}, Budget: Budget{OutputReserve: 64, TotalTokens: 10000}}, nil)
		if tool.invoked != 0 {
			t.Fatal("overrun executed tools")
		}
		if blocked {
			var block *BlockedError
			if !errors.As(err, &block) || res.Answer != "" || res.Steps[0].Response.Content != "" {
				t.Fatalf("block lost: %+v %v", res, err)
			}
		} else {
			if err != nil || res.StopReason != BudgetReached || res.Answer != "accepted" || len(res.Steps[0].Response.ToolCalls) != 1 {
				t.Fatalf("overrun: %+v %v", res, err)
			}
			next := New(&scriptedCaller{responses: []ModelResult{finalAnswer("next")}}, ContextManager{})
			if _, err := next.Run(t.Context(), Request{Goal: "continue", History: res.Messages}, nil); err != nil {
				t.Fatalf("dangling history: %v", err)
			}
		}
		assertBudgetUsage(t, res)
	}
}

func TestRunBudgetReturnedResultIsSnapshot(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var retained context.Context
	child := New(budgetModelFunc(func(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
		close(entered)
		<-release
		return ModelResult{Response: provider.ChatResponse{Content: "late", Usage: provider.Usage{TotalTokens: 30}}}, nil
	}), ContextManager{})
	parent := New(budgetModelFunc(func(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
		retained = ctx
		first := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{Content: "first", Usage: provider.Usage{TotalTokens: 20}}}}}, ContextManager{})
		if _, err := first.Run(ctx, Request{Goal: "q"}, nil); err != nil {
			t.Fatal(err)
		}
		go func() { defer close(finished); _, _ = child.Run(ctx, Request{Goal: "q"}, nil) }()
		<-entered
		return ModelResult{Response: provider.ChatResponse{Content: "parent", Usage: provider.Usage{TotalTokens: 10}}}, nil
	}), ContextManager{})
	res, err := parent.Run(t.Context(), Request{Goal: "q"}, nil)
	close(release)
	<-finished
	if err != nil {
		t.Fatal(err)
	}
	if res.DescendantUsage == nil || *res.DescendantUsage != (provider.Usage{TotalTokens: 20}) {
		t.Fatalf("snapshot changed: %+v", res.DescendantUsage)
	}
	late := &scriptedCaller{responses: []ModelResult{finalAnswer("unexpected")}}
	lateRes, err := New(late, ContextManager{}).Run(context.WithoutCancel(retained), Request{Goal: "q"}, nil)
	if err != nil || lateRes.StopReason != BudgetReached || late.calls != 0 {
		t.Fatalf("reopened returned run: %+v %v calls=%d", lateRes, err, late.calls)
	}
	assertBudgetUsage(t, res)
}

func TestRunBudgetGrandchildUsageCountedOnce(t *testing.T) {
	grandchild := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
		Content: "grandchild", Usage: provider.Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}}}}, ContextManager{})
	var childResult Result
	child := New(budgetModelFunc(func(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
		res, err := grandchild.Run(ctx, Request{Goal: "q"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertBudgetUsage(t, res)
		return ModelResult{Response: provider.ChatResponse{Content: "child", Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 6, TotalTokens: 11}}}, nil
	}), ContextManager{})
	parent := New(budgetModelFunc(func(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
		var err error
		childResult, err = child.Run(ctx, Request{Goal: "q"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ModelResult{Response: provider.ChatResponse{Content: "parent", Usage: provider.Usage{TotalTokens: 13}}}, nil
	}), ContextManager{})
	res, err := parent.Run(t.Context(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.DescendantUsage == nil || *res.DescendantUsage != (provider.Usage{PromptTokens: 8, CompletionTokens: 10, TotalTokens: 18}) {
		t.Fatalf("ancestor double-counted usage: %+v", res.DescendantUsage)
	}
	if childResult.DescendantUsage == nil || childResult.DescendantUsage.TotalTokens != 7 {
		t.Fatalf("child descendants: %+v", childResult.DescendantUsage)
	}
	assertBudgetUsage(t, res)
	assertBudgetUsage(t, childResult)
}
