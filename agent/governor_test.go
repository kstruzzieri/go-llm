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
