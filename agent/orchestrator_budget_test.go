package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
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
	tool     func(context.Context, ToolCallEvent) error
	result   func(context.Context, ToolResultEvent) error
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

func (o budgetObserver) OnToolCall(ctx context.Context, e ToolCallEvent) error {
	if o.tool != nil {
		return o.tool(ctx, e)
	}
	return nil
}

func (o budgetObserver) OnToolResult(ctx context.Context, e ToolResultEvent) error {
	if o.result != nil {
		return o.result(ctx, e)
	}
	return nil
}

// exhaustRunBudget spends a 10,000-credit allowance through a nested Run.
func exhaustRunBudget(ctx context.Context) error {
	child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
		Content: "child", Usage: provider.Usage{TotalTokens: 10000},
	}}}}, ContextManager{})
	_, err := child.Run(ctx, Request{Goal: "q"}, nil)
	return err
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
			if err != nil || res.StopReason != BudgetReached || len(limited.reqs) != 1 || len(res.Messages) != 3 {
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
			if last := res.Messages[len(res.Messages)-1]; last.Role != "assistant" || last.Content != "accepted" || len(last.ToolCalls) != 0 {
				t.Fatalf("accepted text not persisted without calls: %+v", res.Messages)
			}
			next := New(&scriptedCaller{responses: []ModelResult{finalAnswer("next")}}, ContextManager{})
			if _, err := next.Run(t.Context(), Request{Goal: "continue", History: res.Messages}, nil); err != nil {
				t.Fatalf("dangling history: %v", err)
			}
		}
		assertBudgetUsage(t, res)
	}
}

// Exhaustion during preparation must gate both serial and parallel invocation,
// including earlier parallel calls already prepared before the last callback.
func TestRunBudgetStopsToolsAfterCallback(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			first := &invokeCountingTool{echoTool: echoTool{name: "first"}}
			last := &invokeCountingTool{echoTool: echoTool{name: "last"}}
			tools := []Tool{last}
			calls := []provider.ToolCall{tc("last", "{}")}
			if parallel {
				tools = append([]Tool{first}, tools...)
				calls = append([]provider.ToolCall{tc("first", "{}")}, calls...)
			}
			child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
				Content: "child", Usage: provider.Usage{TotalTokens: 10000},
			}}}}, ContextManager{})
			obs := budgetObserver{tool: func(ctx context.Context, e ToolCallEvent) error {
				if e.Call.Function.Name == "last" {
					_, err := child.Run(ctx, Request{Goal: "q"}, nil)
					return err
				}
				return nil
			}}
			caller := &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{ToolCalls: calls}}}}
			res, err := New(caller, ContextManager{}).Run(t.Context(), Request{
				Goal: "q", Tools: tools, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, obs)
			if err != nil || res.StopReason != BudgetReached || first.invoked != 0 || last.invoked != 0 {
				t.Fatalf("stop=%v err=%v invokes=%d,%d", res.StopReason, err, first.invoked, last.invoked)
			}
			if len(res.ToolCalls) != 0 || res.DescendantUsage == nil || res.DescendantUsage.TotalTokens != 10000 {
				t.Fatalf("audit=%+v descendants=%+v", res.ToolCalls, res.DescendantUsage)
			}
			if err := validateHistory(res.Messages); err != nil {
				t.Fatalf("unexecuted call in history: %v", err)
			}
		})
	}
}

type budgetInvokeTool struct {
	echoTool
	invoke func(context.Context) (ToolResult, error)
}

func (t budgetInvokeTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	return t.invoke(ctx)
}

func TestRunBudgetStopsQueuedParallelTool(t *testing.T) {
	for _, idCase := range []string{"unique", "", "duplicate"} {
		t.Run(fmt.Sprintf("ids=%q", idCase), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started := make(chan struct{}, parallelToolCallLimit)
			release := make(chan struct{})
			var late atomic.Int32
			child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
				Content: "child", Usage: provider.Usage{TotalTokens: 10000},
			}}}}, ContextManager{})
			var tools []Tool
			var calls []provider.ToolCall
			for i := 0; i <= parallelToolCallLimit; i++ {
				name := fmt.Sprint("read", i)
				call := tc(name, "{}")
				if idCase != "unique" {
					call.ID = idCase
				}
				calls = append(calls, call)
				tools = append(tools, budgetInvokeTool{echoTool{name: name}, func(ctx context.Context) (ToolResult, error) {
					if i == parallelToolCallLimit {
						late.Add(1)
						return ToolResult{}, nil
					}
					started <- struct{}{}
					if i == 0 {
						// Hold every worker until all eight are admitted. Exhaust the
						// allowance before freeing any slot for the ninth invocation.
						for range parallelToolCallLimit {
							select {
							case <-started:
							case <-ctx.Done():
								return ToolResult{}, ctx.Err()
							}
						}
						_, err := child.Run(ctx, Request{Goal: "q"}, nil)
						close(release)
						return ToolResult{Content: "nested"}, err
					}
					select {
					case <-release:
						return ToolResult{Content: "read"}, nil
					case <-ctx.Done():
						return ToolResult{}, ctx.Err()
					}
				}})
			}
			caller := &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{ToolCalls: calls}}}}
			res, err := New(caller, ContextManager{}).Run(ctx, Request{
				Goal: "q", Tools: tools, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, nil)
			if err != nil || res.StopReason != BudgetReached || late.Load() != 0 || len(res.ToolCalls) != parallelToolCallLimit {
				t.Fatalf("stop=%v err=%v late=%d records=%+v", res.StopReason, err, late.Load(), res.ToolCalls)
			}
			if !reflect.DeepEqual(res.Messages[1].ToolCalls, calls[:parallelToolCallLimit]) || len(toolMessages(res.Messages)) != parallelToolCallLimit {
				t.Fatalf("incomplete transcript: %+v", res.Messages)
			}
		})
	}
}

func TestRunBudgetStopsSerialToolWithDuplicateIDs(t *testing.T) {
	for _, id := range []string{"", "duplicate"} {
		t.Run(fmt.Sprintf("id=%q", id), func(t *testing.T) {
			child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
				Content: "child", Usage: provider.Usage{TotalTokens: 10000},
			}}}}, ContextManager{})
			first := budgetInvokeTool{echoTool{name: "dispatch"}, func(ctx context.Context) (ToolResult, error) {
				_, err := child.Run(ctx, Request{Goal: "q"}, nil)
				return ToolResult{Content: "child finished"}, err
			}}
			calls := []provider.ToolCall{toolCall(id, "dispatch", "{}"), toolCall(id, "write_file", "{}")}
			caller := batchThenAnswer(calls...)
			res, err := New(caller, ContextManager{}).Run(t.Context(), Request{
				Goal: "q", Tools: []Tool{first, fakeWriteTool{name: "write_file", approval: ApprovalNever}},
				Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, nil)
			if err != nil || res.StopReason != BudgetReached || len(res.ToolCalls) != 1 {
				t.Fatalf("stop=%v err=%v records=%+v; want budget stop and one completed call", res.StopReason, err, res.ToolCalls)
			}
			if !reflect.DeepEqual(res.Messages[1].ToolCalls, calls[:1]) || len(toolMessages(res.Messages)) != 1 {
				t.Fatalf("incomplete transcript: %+v; want only dispatch", res.Messages)
			}
		})
	}
}

func TestRunBudgetVerificationPreservesCancellation(t *testing.T) {
	for _, exhaust := range []bool{false, true} {
		t.Run(fmt.Sprintf("exhaust=%v", exhaust), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var runCtx context.Context
			child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
				Content: "child", Usage: provider.Usage{TotalTokens: 10000},
			}}}}, ContextManager{})
			ic := &stubInterceptor{name: "verification", input: func(in InputInspection) []Finding {
				for _, msg := range in.Messages {
					if msg.Content == "verifier-budget-cancel" {
						if exhaust {
							if _, err := child.Run(runCtx, Request{Goal: "q"}, nil); err != nil {
								t.Fatal(err)
							}
						}
						cancel()
					}
				}
				return nil
			}}
			obs := budgetObserver{step: func(ctx context.Context, _ StepEvent) error { runCtx = ctx; return nil }}
			calls := 0
			caller := budgetModelFunc(func(ctx context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
				calls++
				if err := ctx.Err(); err != nil {
					return ModelResult{}, err
				}
				if calls == 1 {
					return ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{tc("write_file", "{}")}}}, nil
				}
				return finalAnswer("done"), nil
			})
			parent := New(caller, ContextManager{}, WithInterceptors(ic), WithVerifier(stubVerifier{out: "verifier-budget-cancel"}))
			res, err := parent.Run(ctx, Request{
				Goal: "q", MaxSteps: 3, Tools: []Tool{fakeWriteTool{name: "write_file", approval: ApprovalNever}},
				Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, obs)
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("stop=%v err=%v model calls=%d; want cancellation after one call", res.StopReason, err, calls)
			}
		})
	}
}

func TestRunBudgetOverrunPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tool := &invokeCountingTool{echoTool: echoTool{name: "echo"}}
	mr := echoCall("one", "{}")
	mr.Response.Usage.TotalTokens = 20000
	obs := budgetObserver{step: func(context.Context, StepEvent) error { cancel(); return nil }}
	res, err := New(&scriptedCaller{responses: []ModelResult{mr}}, ContextManager{}).Run(ctx, Request{
		Goal: "q", Tools: []Tool{tool}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, obs)
	if !errors.Is(err, context.Canceled) || tool.invoked != 0 || len(res.Messages) != 1 {
		t.Fatalf("stop=%v err=%v invokes=%d messages=%+v", res.StopReason, err, tool.invoked, res.Messages)
	}
	assertBudgetUsage(t, res)
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
	if retained.Err() == nil {
		t.Fatal("returned run left its context live")
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

func TestRunBudgetOverrunMeansGenerationBeyondCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		usage   provider.Usage
		stop    StopReason
		invoked int
	}{
		// A real tokenizer and chat template routinely count more prompt tokens
		// than len/4. The excess is charged, but it is not a provider overrun.
		{name: "prompt undercount", usage: provider.Usage{PromptTokens: 5000, CompletionTokens: 10, TotalTokens: 5010}, stop: Completed, invoked: 1},
		// One token past the fixed cap shows the provider ignored it.
		{name: "generation beyond cap", usage: provider.Usage{PromptTokens: 1, CompletionTokens: 65, TotalTokens: 66}, stop: BudgetReached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &invokeCountingTool{echoTool: echoTool{name: "echo"}}
			first := echoCall("one", "{}")
			first.Response.Usage = tc.usage
			caller := &scriptedCaller{responses: []ModelResult{first, finalAnswer("done")}}
			res, err := New(caller, ContextManager{}).Run(t.Context(), Request{
				Goal: "q", Tools: []Tool{tool}, Budget: Budget{OutputReserve: 64, TotalTokens: 100000},
			}, nil)
			if err != nil || res.StopReason != tc.stop || tool.invoked != tc.invoked {
				t.Fatalf("stop=%v err=%v invoked=%d; want %v and %d", res.StopReason, err, tool.invoked, tc.stop, tc.invoked)
			}
			assertBudgetUsage(t, res)
		})
	}
}

// Budget checks must not change cancellation or governor outcomes for runs
// that never exhaust an allowance, including unbounded ones.
func TestRunBudgetCancellationKeepsPriorOutcomes(t *testing.T) {
	t.Run("cancel before next chat keeps transcript", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		caller := batchThenAnswer(toolCall("c1", "echo", "{}"))
		obs := budgetObserver{pressure: func(_ context.Context, e PressureEvent) error {
			if e.Step == 1 {
				cancel()
			}
			return nil
		}}
		res, err := New(caller, ContextManager{}).Run(ctx, Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, obs)
		if !errors.Is(err, context.Canceled) || len(res.Messages) != 3 || caller.calls != 1 {
			t.Fatalf("err=%v messages=%+v calls=%d; want cancellation with step 0 transcript", err, res.Messages, caller.calls)
		}
	})
	t.Run("governor stop survives later cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := make([]provider.ToolCall, defaultToolErrorCap)
		for i := range calls {
			calls[i] = toolCall(fmt.Sprint("u", i), fmt.Sprint("unknown", i), "{}")
		}
		seen := 0
		obs := budgetObserver{result: func(context.Context, ToolResultEvent) error {
			if seen++; seen == defaultToolErrorCap {
				cancel()
			}
			return nil
		}}
		res, err := New(batchThenAnswer(calls...), ContextManager{}).Run(ctx, Request{Goal: "q"}, obs)
		if err != nil || res.StopReason != ToolErrorCapReached {
			t.Fatalf("stop=%v err=%v; want governor stop", res.StopReason, err)
		}
	})
}

func TestRunBudgetKeepsGovernorStopInSameBatch(t *testing.T) {
	child := New(&scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
		Content: "child", Usage: provider.Usage{TotalTokens: 10000},
	}}}}, ContextManager{})
	// The third consecutive error trips the governor after a nested run has
	// also exhausted the shared allowance.
	spend := budgetInvokeTool{echoTool{name: "spend"}, func(ctx context.Context) (ToolResult, error) {
		if _, err := child.Run(ctx, Request{Goal: "q"}, nil); err != nil {
			return ToolResult{}, err
		}
		return ToolResult{Content: "spent", IsError: true}, nil
	}}
	calls := []provider.ToolCall{toolCall("a", "missing_a", "{}"), toolCall("b", "missing_b", "{}"), toolCall("c", "spend", "{}")}
	res, err := New(batchThenAnswer(calls...), ContextManager{}).Run(t.Context(), Request{
		Goal: "q", Tools: []Tool{spend}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, nil)
	if err != nil || res.StopReason != ToolErrorCapReached || len(toolMessages(res.Messages)) != len(calls) {
		t.Fatalf("stop=%v err=%v messages=%+v; want governor stop with all observations", res.StopReason, err, res.Messages)
	}
	if !reflect.DeepEqual(res.Messages[1].ToolCalls, calls) {
		t.Fatalf("completed calls dropped: %+v", res.Messages[1].ToolCalls)
	}
}

func TestRunBudgetOverrunParentStopsNestedChild(t *testing.T) {
	childCalls := 0
	child := New(budgetModelFunc(func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (ModelResult, error) {
		childCalls++
		return finalAnswer("c"), nil
	}), ContextManager{})
	var childRes Result
	obs := budgetObserver{step: func(ctx context.Context, _ StepEvent) error {
		var err error
		childRes, err = child.Run(ctx, Request{Goal: "q"}, nil)
		return err
	}}
	// Total-only usage above the reservation is an overrun while charged
	// remains below the 10,000 allowance.
	mr := finalAnswer("parent")
	mr.Response.Usage = provider.Usage{TotalTokens: 5000}
	res, err := New(&scriptedCaller{responses: []ModelResult{mr}}, ContextManager{}).Run(t.Context(), Request{
		Goal: "q", Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, obs)
	if err != nil || res.StopReason != BudgetReached {
		t.Fatalf("parent: stop=%v err=%v", res.StopReason, err)
	}
	if childCalls != 0 || childRes.StopReason != BudgetReached {
		t.Fatalf("child admitted under overrun parent: calls=%d stop=%v", childCalls, childRes.StopReason)
	}
}

func TestRunBudgetBatchEndPrecedence(t *testing.T) {
	spend := budgetInvokeTool{echoTool{name: "dispatch"}, func(ctx context.Context) (ToolResult, error) {
		return ToolResult{Content: "ok"}, exhaustRunBudget(ctx)
	}}
	boom := errors.New("observer failed")
	for _, tc := range []struct {
		name   string
		result func(cancel context.CancelFunc) error
		want   error
	}{
		{name: "cancellation beats budget stop", result: func(cancel context.CancelFunc) error { cancel(); return nil }, want: context.Canceled},
		{name: "observer error beats budget stop", result: func(context.CancelFunc) error { return boom }, want: boom},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			obs := budgetObserver{result: func(context.Context, ToolResultEvent) error { return tc.result(cancel) }}
			res, err := New(batchThenAnswer(toolCall("a", "dispatch", "{}")), ContextManager{}).Run(ctx, Request{
				Goal: "q", Tools: []Tool{spend}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, obs)
			if !errors.Is(err, tc.want) {
				t.Fatalf("stop=%v err=%v; want %v", res.StopReason, err, tc.want)
			}
		})
	}
}

func TestRunBudgetStopsPreparationAfterExhaustion(t *testing.T) {
	t.Run("serial", func(t *testing.T) {
		spend := budgetInvokeTool{echoTool{name: "dispatch"}, func(ctx context.Context) (ToolResult, error) {
			return ToolResult{Content: "ok"}, exhaustRunBudget(ctx)
		}}
		var prepared []string
		obs := budgetObserver{tool: func(_ context.Context, e ToolCallEvent) error {
			prepared = append(prepared, e.Call.Function.Name)
			return nil
		}}
		calls := []provider.ToolCall{toolCall("a", "dispatch", "{}"), toolCall("b", "write_file", "{}")}
		res, err := New(batchThenAnswer(calls...), ContextManager{}).Run(t.Context(), Request{
			Goal: "q", Tools: []Tool{spend, fakeWriteTool{name: "write_file", approval: ApprovalNever}},
			Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
		}, obs)
		if err != nil || res.StopReason != BudgetReached || !reflect.DeepEqual(prepared, []string{"dispatch"}) {
			t.Fatalf("stop=%v err=%v prepared=%v; want only dispatch prepared", res.StopReason, err, prepared)
		}
		if got := kinds(res.Events); got != "step,tool_call,tool_result,stop" {
			t.Fatalf("events=%s; refused serial call must have no event", got)
		}
	})
	t.Run("parallel", func(t *testing.T) {
		a := &invokeCountingTool{echoTool: echoTool{name: "a"}}
		b := &invokeCountingTool{echoTool: echoTool{name: "b"}}
		var prepared []string
		obs := budgetObserver{tool: func(ctx context.Context, e ToolCallEvent) error {
			prepared = append(prepared, e.Call.Function.Name)
			if e.Call.Function.Name == "a" {
				return exhaustRunBudget(ctx)
			}
			return nil
		}}
		caller := &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{tc("a", "{}"), tc("b", "{}")}}}}}
		res, err := New(caller, ContextManager{}).Run(t.Context(), Request{
			Goal: "q", Tools: []Tool{a, b}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
		}, obs)
		if err != nil || res.StopReason != BudgetReached || a.invoked+b.invoked != 0 || len(prepared) != 1 {
			t.Fatalf("stop=%v err=%v invokes=%d/%d prepared=%v", res.StopReason, err, a.invoked, b.invoked, prepared)
		}
		if got := kinds(res.Events); got != "step,stop" {
			t.Fatalf("events=%s; uninvoked prepared calls must have no events", got)
		}
	})
}

func TestRunBudgetKeepsAssistantTextWhenNoCallRan(t *testing.T) {
	tool := &invokeCountingTool{echoTool: echoTool{name: "last"}}
	obs := budgetObserver{tool: func(ctx context.Context, _ ToolCallEvent) error { return exhaustRunBudget(ctx) }}
	caller := &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{
		Content: "let me check", ToolCalls: []provider.ToolCall{tc("last", "{}")},
	}}}}
	res, err := New(caller, ContextManager{}).Run(t.Context(), Request{
		Goal: "q", Tools: []Tool{tool}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, obs)
	if err != nil || res.StopReason != BudgetReached || tool.invoked != 0 {
		t.Fatalf("stop=%v err=%v invoked=%d", res.StopReason, err, tool.invoked)
	}
	if len(res.Messages) != 2 || res.Messages[1].Content != "let me check" || len(res.Messages[1].ToolCalls) != 0 {
		t.Fatalf("assistant text lost or call left dangling: %+v", res.Messages)
	}
	if got := kinds(res.Events); got != "token,step,stop" {
		t.Fatalf("events=%s; call refused after preparation must have no event", got)
	}
}

// A queued parallel call refused by the budget was never invoked and must not
// be audited as such, including on the trailing-record path.
func TestRunBudgetRefusedParallelCallNotAudited(t *testing.T) {
	for _, observerError := range []bool{false, true} {
		t.Run(map[bool]string{false: "budget stop", true: "observer error"}[observerError], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			started := make(chan struct{}, parallelToolCallLimit)
			release := make(chan struct{})
			var late atomic.Int32
			var tools []Tool
			var calls []provider.ToolCall
			for i := 0; i <= parallelToolCallLimit; i++ {
				name := fmt.Sprint("read", i)
				calls = append(calls, tc(name, "{}"))
				tools = append(tools, budgetInvokeTool{echoTool{name: name}, func(ctx context.Context) (ToolResult, error) {
					if i == parallelToolCallLimit {
						late.Add(1)
						return ToolResult{}, nil
					}
					select {
					case started <- struct{}{}:
					case <-ctx.Done():
						return ToolResult{}, ctx.Err()
					}
					if i == 0 {
						for range parallelToolCallLimit {
							select {
							case <-started:
							case <-ctx.Done():
								return ToolResult{}, ctx.Err()
							}
						}
						err := exhaustRunBudget(ctx)
						close(release)
						return ToolResult{Content: "nested"}, err
					}
					select {
					case <-release:
						return ToolResult{Content: "read"}, nil
					case <-ctx.Done():
						return ToolResult{}, ctx.Err()
					}
				}})
			}
			var stop error
			if observerError {
				stop = errors.New("observer stop")
			}
			obs := budgetObserver{result: func(context.Context, ToolResultEvent) error { return stop }}
			caller := &scriptedCaller{responses: []ModelResult{{Response: provider.ChatResponse{ToolCalls: calls}}}}
			res, err := New(caller, ContextManager{}).Run(ctx, Request{
				Goal: "q", Tools: tools, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
			}, obs)
			if !errors.Is(err, stop) || late.Load() != 0 {
				t.Fatalf("err=%v late=%d", err, late.Load())
			}
			for _, rec := range res.ToolCalls {
				if rec.Name == fmt.Sprint("read", parallelToolCallLimit) {
					t.Fatalf("refused call audited: %+v", rec)
				}
			}
			if stop == nil && res.StopReason != BudgetReached {
				t.Fatalf("stop=%v; want budget stop", res.StopReason)
			}
			want := []string{"step"}
			for range parallelToolCallLimit {
				want = append(want, "tool_call")
			}
			for range parallelToolCallLimit {
				want = append(want, "tool_result")
			}
			if stop == nil {
				want = append(want, "stop")
			}
			if got := kinds(res.Events); got != strings.Join(want, ",") {
				t.Fatalf("events=%s; want %s", got, strings.Join(want, ","))
			}
		})
	}
}

func TestRunBudgetPreparationStopKeepsSyntheticEvents(t *testing.T) {
	a := &invokeCountingTool{echoTool: echoTool{name: "a"}}
	b := &invokeCountingTool{echoTool: echoTool{name: "b"}}
	c := &invokeCountingTool{echoTool: echoTool{name: "c"}}
	block := &stubInterceptor{name: "guard", toolCall: func(call ToolCallInspection) []Finding {
		if call.Call.Function.Name == "a" {
			return []Finding{{Rule: "deny", Verdict: VerdictBlock, Risk: 100}}
		}
		return nil
	}}
	obs := budgetObserver{tool: func(ctx context.Context, e ToolCallEvent) error {
		if e.Call.Function.Name == "b" {
			return exhaustRunBudget(ctx)
		}
		return nil
	}}
	caller := batchThenAnswer(tc("a", "{}"), tc("b", "{}"), tc("c", "{}"))
	res, err := New(caller, ContextManager{}, WithInterceptors(block)).Run(t.Context(), Request{
		Goal: "q", Tools: []Tool{a, b, c}, Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, obs)
	if err != nil || res.StopReason != BudgetReached || a.invoked+b.invoked+c.invoked != 0 {
		t.Fatalf("stop=%v err=%v invoked=%d/%d/%d", res.StopReason, err, a.invoked, b.invoked, c.invoked)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "a" || !res.ToolCalls[0].Blocked || res.ToolCalls[0].Invoked {
		t.Fatalf("synthetic outcome lost: %+v", res.ToolCalls)
	}
	if got := kinds(res.Events); got != "step,tool_call,tool_result,stop" {
		t.Fatalf("events=%s; want only synthetic call and result", got)
	}
}

type exhaustingVerifier struct{}

func (exhaustingVerifier) Verify(ctx context.Context, _ Approver) (string, error) {
	return "", exhaustRunBudget(ctx)
}

func TestRunBudgetVerifierExhaustionOnLastStep(t *testing.T) {
	res, err := New(batchThenAnswer(toolCall("w", "write_file", "{}")), ContextManager{}, WithVerifier(exhaustingVerifier{})).Run(t.Context(), Request{
		Goal: "q", MaxSteps: 1, Tools: []Tool{fakeWriteTool{name: "write_file", approval: ApprovalNever}},
		Budget: Budget{TotalTokens: 10000, OutputReserve: 64},
	}, nil)
	if err != nil || res.StopReason != BudgetReached || len(res.Messages) != 3 {
		t.Fatalf("stop=%v err=%v messages=%+v; want BudgetReached with the step transcript", res.StopReason, err, res.Messages)
	}
}
