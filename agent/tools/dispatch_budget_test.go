package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func dispatchReadRegistry() []agent.Tool {
	var tools []agent.Tool
	for _, name := range []string{"read_file", "search", "glob", "list"} {
		tools = append(tools, dispatchNamedTool{name: name, effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}})
	}
	return tools
}

func newBudgetDispatch(t *testing.T, caller agent.ModelCaller, cm agent.ContextManager, limits DispatchLimits) *Dispatch {
	t.Helper()
	d, err := NewDispatch(caller, cm, dispatchReadRegistry(), limits)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func budgetDispatchCall(id string, tasks ...string) provider.ToolCall {
	raw, _ := json.Marshal(map[string][]string{"tasks": tasks})
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: DispatchToolName, Arguments: raw}}
}

func dispatchBudgetAnswer(usage provider.Usage) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{Content: "child answer", Done: true, Usage: usage}, RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}}
}

type dispatchCountedTool struct {
	dispatchNamedTool
	calls *atomic.Int32
}

func (t dispatchCountedTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	t.calls.Add(1)
	return agent.ToolResult{Content: "forbidden"}, nil
}

type dispatchPlanningTool struct{ dispatchNamedTool }

func (dispatchPlanningTool) Plan(context.Context, json.RawMessage) (agent.ToolPlan, error) {
	return agent.ToolPlan{}, nil
}

func TestDispatchRejectsUnsafeSelectedTools(t *testing.T) {
	for _, name := range []string{"read_file", "search", "glob", "list", "retrieve"} {
		for _, effect := range []agent.Effect{
			{Class: agent.Write}, {Class: agent.Exec}, {Class: agent.Network},
			{Class: agent.Read | agent.Write}, {Class: agent.Read | agent.Exec}, {Class: agent.Read | agent.Network},
			{Class: agent.Read, Approval: agent.ApprovalAlways},
		} {
			available := dispatchReadRegistry()
			for i, tool := range available {
				if tool.Spec().Name == name {
					available = append(available[:i], available[i+1:]...)
					break
				}
			}
			available = append(available, dispatchNamedTool{name: name, effect: effect})
			if _, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{}); err == nil {
				t.Fatalf("accepted %s with %+v", name, effect)
			}
		}
		available := dispatchReadRegistry()
		for i, tool := range available {
			if tool.Spec().Name == name {
				available = append(available[:i], available[i+1:]...)
				break
			}
		}
		available = append(available, dispatchPlanningTool{dispatchNamedTool{name: name, effect: agent.Effect{Class: agent.Read}}})
		if _, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{}); err == nil {
			t.Fatalf("accepted planning %s", name)
		}
	}
}

func TestDispatchCannotInvokeOmittedParentTools(t *testing.T) {
	var calls atomic.Int32
	available := dispatchReadRegistry()
	for _, entry := range []struct {
		name  string
		class agent.EffectClass
	}{
		{"write_file", agent.Write}, {"run_command", agent.Exec}, {"fetch", agent.Network},
		{"dispatch", agent.Read | agent.Network}, {"mcp_private", agent.Read}, {"plan", agent.Read},
	} {
		if entry.name == "plan" {
			available = append(available, dispatchPlanningTool{dispatchNamedTool{name: entry.name, effect: agent.Effect{Class: entry.class}}})
		} else {
			available = append(available, dispatchCountedTool{dispatchNamedTool{name: entry.name, effect: agent.Effect{Class: entry.class}}, &calls})
		}
	}
	for _, name := range []string{"write_file", "run_command", "fetch", "dispatch", "mcp_private", "plan"} {
		t.Run(name, func(t *testing.T) {
			n := 0
			model := dispatchModelFunc(func(_ context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
				n++
				if n == 1 {
					return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "bad", Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage("{}")}}}}}, nil
				}
				last := req.Messages[len(req.Messages)-1]
				if last.Role != "tool" || !strings.Contains(last.Content, "unknown tool: "+name) {
					t.Errorf("denial missing: %+v", last)
				}
				return dispatchBudgetAnswer(provider.Usage{}), nil
			})
			d, err := NewDispatch(model, agent.ContextManager{}, available, DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			out, err := d.Invoke(t.Context(), json.RawMessage("{\"tasks\":[\"try omitted tool\"]}"))
			if err != nil || out.IsError || n != 2 {
				t.Fatalf("invocation: %+v %v, calls=%d", out, err, n)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("omitted tools invoked %d times", calls.Load())
	}
}

func TestDispatchInheritsParentCapacities(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		parentInput, parentOutput, parentSteps int
		childInput, childOutput, childSteps    int
		task                                   string
		wantCalls, wantOutput                  int
	}{
		{"smaller parent", 8192, 128, 2, 16384, 1024, 6, "q", 2, 128},
		{"smaller child", 16384, 1024, 6, 8192, 128, 2, "q", 2, 128},
		{"unset parent input", 0, 128, 2, 16384, 1024, 6, strings.Repeat("x", 7800), 0, 128},
		{"smaller child input", 16384, 128, 2, 4000, 128, 6, strings.Repeat("x", 5000), 0, 128},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var caps []int
			child := dispatchModelFunc(func(_ context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
				caps = append(caps, req.Options.NumPredict)
				mr := dispatchBudgetAnswer(provider.Usage{})
				mr.Response.Content = ""
				mr.Response.ToolCalls = []provider.ToolCall{{ID: fmt.Sprint(len(caps)), Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage("{}")}}}
				return mr, nil
			})
			d := newBudgetDispatch(t, child, agent.ContextManager{Estimate: func(s string) int { return len(s) }}, DispatchLimits{MaxSteps: tc.childSteps, Budget: agent.Budget{InputCeiling: tc.childInput, OutputReserve: tc.childOutput}})
			turn := 0
			parent := agent.New(dispatchModelFunc(func(_ context.Context, _ provider.ChatRequest) (agent.ModelResult, error) {
				turn++
				if turn == 1 {
					return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{budgetDispatchCall("d", tc.task)}}}, nil
				}
				return agent.ModelResult{Response: provider.ChatResponse{Content: "done"}}, nil
			}), agent.ContextManager{})
			res, err := parent.Run(t.Context(), agent.Request{Goal: "inspect", Tools: []agent.Tool{d}, MaxSteps: tc.parentSteps, Budget: agent.Budget{InputCeiling: tc.parentInput, OutputReserve: tc.parentOutput}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(caps) != tc.wantCalls {
				t.Fatalf("child calls=%d, want %d; result=%+v", len(caps), tc.wantCalls, res)
			}
			for _, cap := range caps {
				if cap != tc.wantOutput {
					t.Fatalf("cap=%d, want %d", cap, tc.wantOutput)
				}
			}
			if tc.wantCalls == 0 && (len(res.ToolCalls) != 1 || !res.ToolCalls[0].IsError) {
				t.Fatalf("input overflow not reported: %+v", res.ToolCalls)
			}
		})
	}
}

// One token per nonempty component makes the initial prompt exactly three:
// system, tool schemas and goal. A completed tool exchange adds eight.
func dispatchCreditContext() agent.ContextManager {
	return agent.ContextManager{Estimate: func(s string) int {
		if s == "" {
			return 0
		}
		return 1
	}}
}

func TestDispatchSharesParentAllowance(t *testing.T) {
	for _, mode := range []string{"siblings", "same response", "repeated turns"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var calls atomic.Int32
			started := make(chan struct{}, 4) // bounded by this invocation's four children
			release := make(chan struct{})
			if mode != "siblings" {
				close(release)
			}
			d := newBudgetDispatch(t, dispatchModelFunc(func(ctx context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
				calls.Add(1)
				if req.Options.NumPredict != 64 {
					t.Errorf("generation=%d", req.Options.NumPredict)
				}
				select {
				case started <- struct{}{}:
				case <-ctx.Done():
					return agent.ModelResult{}, ctx.Err()
				}
				select {
				case <-release:
				case <-ctx.Done():
					return agent.ModelResult{}, ctx.Err()
				}
				return dispatchBudgetAnswer(provider.Usage{TotalTokens: 7}), nil
			}), dispatchCreditContext(), DispatchLimits{MaxConcurrent: 4, Budget: agent.Budget{InputCeiling: 100000}})
			turn := 0
			parent := agent.New(dispatchModelFunc(func(_ context.Context, _ provider.ChatRequest) (agent.ModelResult, error) {
				turn++
				var batch []provider.ToolCall
				switch mode {
				case "siblings":
					batch = []provider.ToolCall{budgetDispatchCall("one", "a", "b", "c", "d")}
				case "same response":
					batch = []provider.ToolCall{budgetDispatchCall("one", "a", "b"), budgetDispatchCall("two", "c", "d")}
				default:
					batch = []provider.ToolCall{budgetDispatchCall(fmt.Sprint(turn), "a", "b")}
				}
				return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: batch}}, nil
			}), dispatchCreditContext())
			total, wantTurns := 335, 1 // 67 parent + 4*67 children
			if mode == "repeated turns" {
				total, wantTurns = 410, 2
			} // second parent request = 75
			type outcome struct {
				res agent.Result
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, err := parent.Run(ctx, agent.Request{Goal: "q", Tools: []agent.Tool{d}, Budget: agent.Budget{InputCeiling: 100000, OutputReserve: 64, TotalTokens: total}}, nil)
				done <- outcome{res, err}
			}()
			if mode == "siblings" {
				for range 4 {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("siblings could not reserve actual request costs")
					}
				}
				close(release)
			}
			got := <-done
			if got.err != nil || got.res.StopReason != agent.BudgetReached || calls.Load() != 4 || turn != wantTurns {
				t.Fatalf("calls=%d turns=%d stop=%v err=%v", calls.Load(), turn, got.res.StopReason, got.err)
			}
			if got.res.Usage != (provider.Usage{}) || got.res.DescendantUsage == nil || *got.res.DescendantUsage != (provider.Usage{TotalTokens: 28}) {
				t.Fatalf("usage=%+v descendants=%+v", got.res.Usage, got.res.DescendantUsage)
			}
		})
	}
}

func TestDispatchIndependentParentAllowances(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{}, 2) // exactly two simultaneous parent runs
	release := make(chan struct{})
	d := newBudgetDispatch(t, dispatchModelFunc(func(ctx context.Context, _ provider.ChatRequest) (agent.ModelResult, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return agent.ModelResult{}, ctx.Err()
		}
		return dispatchBudgetAnswer(provider.Usage{TotalTokens: 7}), nil
	}), dispatchCreditContext(), DispatchLimits{MaxConcurrent: 2})
	parent := agent.New(dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{budgetDispatchCall("d", "q")}}}, nil
	}), dispatchCreditContext())
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			res, err := parent.Run(ctx, agent.Request{Goal: "q", Tools: []agent.Tool{d}, Budget: agent.Budget{OutputReserve: 64, TotalTokens: 134}}, nil)
			if err != nil || res.StopReason != agent.BudgetReached || res.DescendantUsage == nil || res.DescendantUsage.TotalTokens != 7 {
				t.Errorf("independent result=%+v err=%v", res, err)
			}
		})
	}
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent run lost its allowance")
		}
	}
	close(release)
	wg.Wait()
}

func TestDispatchFailedChildDoesNotRefundParent(t *testing.T) {
	var calls atomic.Int32
	d := newBudgetDispatch(t, dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		calls.Add(1)
		return agent.ModelResult{}, provider.ErrRouterClosed
	}), dispatchCreditContext(), DispatchLimits{MaxConcurrent: 1})
	parent := agent.New(dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{budgetDispatchCall("d", "one", "two")}}}, nil
	}), dispatchCreditContext())
	res, err := parent.Run(t.Context(), agent.Request{Goal: "q", Tools: []agent.Tool{d}, Budget: agent.Budget{OutputReserve: 64, TotalTokens: 134}}, nil)
	if err != nil || res.StopReason != agent.BudgetReached || calls.Load() != 1 || res.DescendantUsage != nil {
		t.Fatalf("failed child refunded: %+v %v calls=%d", res, err, calls.Load())
	}
}

func TestDispatchStandaloneLocalBudget(t *testing.T) {
	var caps []int
	d := newBudgetDispatch(t, dispatchModelFunc(func(_ context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
		caps = append(caps, req.Options.NumPredict)
		return dispatchBudgetAnswer(provider.Usage{}), nil
	}), agent.ContextManager{Estimate: func(s string) int {
		if s == "" {
			return 0
		}
		return 10000
	}}, DispatchLimits{Budget: agent.Budget{InputCeiling: 100000}})
	// Each prompt+generation is 31024. Both children fit their default 32768,
	// and a later standalone invocation has no inherited cross-call pool.
	for range 2 {
		out, err := d.Invoke(t.Context(), json.RawMessage("{\"tasks\":[\"one\",\"two\"]}"))
		if err != nil || out.IsError {
			t.Fatalf("standalone: %+v %v", out, err)
		}
	}
	if !reflect.DeepEqual(caps, []int{1024, 1024, 1024, 1024}) {
		t.Fatal(caps)
	}
}

func TestDispatchReportsAdmittedOverrunAndModel(t *testing.T) {
	d := newBudgetDispatch(t, dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		return dispatchBudgetAnswer(provider.Usage{TotalTokens: 20000}), nil
	}), agent.ContextManager{}, DispatchLimits{Budget: agent.Budget{OutputReserve: 123, TotalTokens: 10000}})
	out, err := d.Invoke(t.Context(), json.RawMessage("{\"tasks\":[\"one\"]}"))
	if err != nil || out.IsError {
		t.Fatalf("%+v %v", out, err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Results) != 1 {
		t.Fatal(envelope)
	}
	got := envelope.Results[0]
	if got.Model != "local/fast" || got.Summary != "child answer" || got.StopReason != agent.BudgetReached.String() {
		t.Fatal(got)
	}
}

func TestDispatchExhaustionStopsFollowingMutation(t *testing.T) {
	var writes atomic.Int32
	writer := dispatchCountedTool{dispatchNamedTool{name: "write_file", effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}}, &writes}
	d := newBudgetDispatch(t, dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		return dispatchBudgetAnswer(provider.Usage{TotalTokens: 7}), nil
	}), dispatchCreditContext(), DispatchLimits{})
	caller := dispatchModelFunc(func(context.Context, provider.ChatRequest) (agent.ModelResult, error) {
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
			budgetDispatchCall("child", "q"),
			{ID: "write", Type: "function", Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage("{}")}},
		}}}, nil
	})
	res, err := agent.New(caller, dispatchCreditContext()).Run(t.Context(), agent.Request{
		Goal: "q", Tools: []agent.Tool{d, writer}, Budget: agent.Budget{TotalTokens: 134, OutputReserve: 64},
	}, nil)
	if err != nil || res.StopReason != agent.BudgetReached || writes.Load() != 0 {
		t.Fatalf("stop=%v err=%v writes=%d", res.StopReason, err, writes.Load())
	}
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].Invoked || res.DescendantUsage == nil || res.DescendantUsage.TotalTokens != 7 {
		t.Fatalf("audit=%+v descendants=%+v", res.ToolCalls, res.DescendantUsage)
	}
	// History accepts only plain chat today. The returned tool transcript
	// must nonetheless pair every retained call with its completed result.
	if len(res.Messages) != 3 || len(res.Messages[1].ToolCalls) != 1 ||
		res.Messages[1].ToolCalls[0].ID != "child" || res.Messages[2].ToolCallID != "child" {
		t.Fatalf("dangling call in transcript: %+v", res.Messages)
	}
}
