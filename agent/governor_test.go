package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type erroringTool struct{}

func (erroringTool) Spec() ToolSpec { return ToolSpec{Name: "boom", Parameters: json.RawMessage(`{}`)} }
func (erroringTool) Effect() Effect { return Effect{Class: Read} }
func (erroringTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, errors.New("kaboom")
}

func loopToolCall() provider.ChatResponse {
	return provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "1", Type: "function",
		Function: provider.ToolCallFunction{Name: "boom", Arguments: json.RawMessage(`{}`)},
	}}}
}

func TestConsecutiveToolErrorsStop(t *testing.T) {
	// model keeps calling the failing tool forever.
	resps := make([]ModelResult, 10)
	for i := range resps {
		resps[i] = ModelResult{Response: loopToolCall()}
	}
	mc := &scriptedCaller{responses: resps}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{erroringTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != ToolErrorCapReached {
		t.Fatalf("stop = %v, want ToolErrorCapReached", res.StopReason)
	}
}

func TestStepCapStop(t *testing.T) {
	// non-failing tool, distinct args each step (so the repeat governor does not
	// trip); the model never answers, so MaxSteps must bound the loop.
	resps := make([]ModelResult, 10)
	for i := range resps {
		resps[i] = ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{
				Name: "echo", Arguments: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
			},
		}}}}
	}
	mc := &scriptedCaller{responses: resps}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", MaxSteps: 3, Tools: []Tool{echoTool{name: "echo"}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != StepCapReached {
		t.Fatalf("stop = %v, want StepCapReached", res.StopReason)
	}
}

func TestBudgetCapStop(t *testing.T) {
	// each tool turn reports 50 tokens; TotalTokens cap of 60 trips after step 1.
	resps := make([]ModelResult, 10)
	for i := range resps {
		resps[i] = ModelResult{Response: provider.ChatResponse{
			Usage: provider.Usage{TotalTokens: 50},
			ToolCalls: []provider.ToolCall{{
				ID: "1", Type: "function",
				Function: provider.ToolCallFunction{
					Name: "echo", Arguments: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
				},
			}},
		}}
	}
	mc := &scriptedCaller{responses: resps}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Budget: Budget{TotalTokens: 60}, Tools: []Tool{echoTool{name: "echo"}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != BudgetReached {
		t.Fatalf("stop = %v, want BudgetReached", res.StopReason)
	}
}

func TestBudgetCapStopOnFinalAnswer(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{
			Content: "final",
			Done:    true,
			Usage:   provider.Usage{TotalTokens: 75},
		}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Budget: Budget{TotalTokens: 60}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "final" {
		t.Fatalf("answer = %q, want final", res.Answer)
	}
	if res.StopReason != BudgetReached {
		t.Fatalf("stop = %v, want BudgetReached", res.StopReason)
	}
}

func TestToolInvocationBudgetCapsDistinctCallsAndResetsPerRun(t *testing.T) {
	responses := make([]ModelResult, 0, 8)
	for _, offset := range []int{0, 10} {
		for i := range 3 {
			responses = append(responses, ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
				ID: fmt.Sprintf("%d", offset+i), Type: "function",
				Function: provider.ToolCallFunction{
					Name: "count", Arguments: json.RawMessage(fmt.Sprintf(`{"i":%d}`, offset+i)),
				},
			}}}})
		}
		responses = append(responses, ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}})
	}

	tool := &countingTool{}
	o := newTestOrchestrator(&scriptedCaller{responses: responses})
	req := Request{
		Goal: "q", Tools: []Tool{tool},
		Budget: Budget{ToolInvocations: map[string]int{"count": 2}},
	}
	for run := range 2 {
		res, err := o.Run(context.Background(), req, nil)
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if res.StopReason != Completed {
			t.Fatalf("run %d stop = %v, want Completed", run, res.StopReason)
		}
		if len(res.ToolCalls) != 3 {
			t.Fatalf("run %d tool calls = %d, want 3", run, len(res.ToolCalls))
		}
		if !res.ToolCalls[0].Invoked || !res.ToolCalls[1].Invoked || res.ToolCalls[2].Invoked || !res.ToolCalls[2].IsError {
			t.Fatalf("run %d invocation records = %+v, want two invokes then one synthetic error", run, res.ToolCalls)
		}
		if tool.n != (run+1)*2 {
			t.Fatalf("after run %d invokes = %d, want %d", run, tool.n, (run+1)*2)
		}
	}
}

func TestToolInvocationBudgetHandlesOtherwiseParallelBatch(t *testing.T) {
	batch := func(step int) ModelResult {
		return ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: fmt.Sprintf("a-%d", step), Function: provider.ToolCallFunction{Name: "a", Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d}`, step))}},
			{ID: fmt.Sprintf("b-%d", step), Function: provider.ToolCallFunction{Name: "b", Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d}`, step))}},
		}}}
	}
	o := newTestOrchestrator(&scriptedCaller{responses: []ModelResult{
		batch(0), batch(1), {Response: provider.ChatResponse{Content: "done", Done: true}},
	}})
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{echoTool{name: "a"}, echoTool{name: "b"}},
		Budget: Budget{ToolInvocations: map[string]int{"a": 1}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 4 {
		t.Fatalf("tool calls = %d, want 4", len(res.ToolCalls))
	}
	if !res.ToolCalls[0].Invoked || !res.ToolCalls[1].Invoked || res.ToolCalls[2].Invoked || !res.ToolCalls[2].IsError || !res.ToolCalls[3].Invoked {
		t.Fatalf("invocation records = %+v", res.ToolCalls)
	}
}

func TestToolInvocationBudgetRejectsInvalidConfiguration(t *testing.T) {
	for name, limits := range map[string]map[string]int{
		"empty name":   {"": 1},
		"zero limit":   {"count": 0},
		"unknown tool": {"missing": 1},
	} {
		t.Run(name, func(t *testing.T) {
			o := newTestOrchestrator(&scriptedCaller{})
			_, err := o.Run(context.Background(), Request{
				Goal: "q", Tools: []Tool{&countingTool{}}, Budget: Budget{ToolInvocations: limits},
			}, nil)
			if err == nil {
				t.Fatal("Run succeeded with invalid tool invocation budget")
			}
		})
	}
}

func TestObserverStepErrorAborts(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "x", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{Goal: "q"}, stepAbortObserver{})
	if err == nil {
		t.Fatal("OnStep error must abort Run")
	}
}

func TestObserverTokenErrorAborts(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "streamed", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{Goal: "q"}, tokenAbortObserver{})
	if err == nil {
		t.Fatal("OnToken error must abort Run")
	}
}

func TestObserverToolCallErrorAborts(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{}`)},
		}}}},
	}}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, toolCallAbortObserver{})
	if err == nil {
		t.Fatal("OnToolCall error must abort Run")
	}
}

type stepAbortObserver struct{}

func (stepAbortObserver) OnStep(context.Context, StepEvent) error         { return errors.New("stop") }
func (stepAbortObserver) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (stepAbortObserver) OnToken(context.Context, TokenEvent) error       { return nil }

type tokenAbortObserver struct{}

func (tokenAbortObserver) OnStep(context.Context, StepEvent) error         { return nil }
func (tokenAbortObserver) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (tokenAbortObserver) OnToken(context.Context, TokenEvent) error       { return errors.New("stop") }

type toolCallAbortObserver struct{}

func (toolCallAbortObserver) OnStep(context.Context, StepEvent) error { return nil }
func (toolCallAbortObserver) OnToolCall(context.Context, ToolCallEvent) error {
	return errors.New("stop")
}
func (toolCallAbortObserver) OnToken(context.Context, TokenEvent) error { return nil }

type countingTool struct{ n int }

func (*countingTool) Spec() ToolSpec {
	return ToolSpec{Name: "count", Parameters: json.RawMessage(`{}`)}
}
func (*countingTool) Effect() Effect { return Effect{Class: Read} }
func (c *countingTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	c.n++
	return ToolResult{Content: "same-result"}, nil
}

func TestGovernorStopsDispatchingWithinToolCallBatch(t *testing.T) {
	calls := make([]provider.ToolCall, defaultToolErrorCap+2)
	for i := range calls {
		calls[i] = provider.ToolCall{
			ID:   fmt.Sprintf("%d", i),
			Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "count",
				Arguments: json.RawMessage(`{}`),
			},
		}
	}
	tool := &countingTool{}
	o := newTestOrchestrator(&scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
	}})
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{tool}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if tool.n != defaultToolErrorCap {
		t.Fatalf("invocations = %d, want %d", tool.n, defaultToolErrorCap)
	}
	if len(res.ToolCalls) != defaultToolErrorCap {
		t.Fatalf("tool call records = %d, want %d", len(res.ToolCalls), defaultToolErrorCap)
	}
	if res.StopReason != RepeatLimitReached {
		t.Fatalf("stop = %v, want RepeatLimitReached", res.StopReason)
	}
}

// pollingTool returns a DIFFERENT result on each call for identical args,
// simulating a status/poll that makes progress.
type pollingTool struct{ n int }

func (*pollingTool) Spec() ToolSpec { return ToolSpec{Name: "poll", Parameters: json.RawMessage(`{}`)} }
func (*pollingTool) Effect() Effect { return Effect{Class: Read} }
func (p *pollingTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	p.n++
	return ToolResult{Content: fmt.Sprintf("status-%d", p.n)}, nil
}

func TestGovernorAllowsProgressingRepeatedCalls(t *testing.T) {
	// Same tool + identical args called 5x, but each result differs (progress).
	// Must NOT trip the governor; the run completes when the model answers.
	resps := make([]ModelResult, 6)
	for i := range 5 {
		resps[i] = ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "poll", Arguments: json.RawMessage(`{}`)},
		}}}}
	}
	resps[5] = ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}
	o := newTestOrchestrator(&scriptedCaller{responses: resps})
	res, err := o.Run(context.Background(), Request{Goal: "q", MaxSteps: 10, Tools: []Tool{&pollingTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != Completed {
		t.Fatalf("progressing repeats must not trip the governor, got %v", res.StopReason)
	}
}

func TestGovernorStopsStuckIdenticalCalls(t *testing.T) {
	// echoTool returns an identical result for identical args -> no progress ->
	// the no-progress repeat guard trips with RepeatLimitReached (not ToolErrorCap).
	resps := make([]ModelResult, 10)
	for i := range resps {
		resps[i] = ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{}`)},
		}}}}
	}
	o := newTestOrchestrator(&scriptedCaller{responses: resps})
	res, err := o.Run(context.Background(), Request{Goal: "q", MaxSteps: 10, Tools: []Tool{echoTool{name: "echo"}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != RepeatLimitReached {
		t.Fatalf("stuck identical calls must trip RepeatLimitReached, got %v", res.StopReason)
	}
}
