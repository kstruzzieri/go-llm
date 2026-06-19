package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type echoTool struct{ name string }

func (e echoTool) Spec() ToolSpec {
	return ToolSpec{Name: e.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (echoTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (echoTool) Invoke(_ context.Context, args json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "tool-said:" + string(args)}, nil
}

func TestRunDispatchesToolThenAnswers(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"x":1}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "done" || res.StopReason != Completed {
		t.Fatalf("got answer=%q stop=%v", res.Answer, res.StopReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "echo" || res.ToolCalls[0].IsError {
		t.Fatalf("expected one successful echo call, got %+v", res.ToolCalls)
	}
	if len(res.Events) < 4 || res.Events[1].Kind != "tool_call" || res.Events[2].Kind != "tool_result" {
		t.Fatalf("expected ordered tool_call/tool_result events, got %+v", res.Events)
	}
}

func TestUnknownToolBecomesObservation(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "nope", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "recovered", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "recovered" || !res.ToolCalls[0].IsError {
		t.Fatalf("unknown tool must yield IsError observation, got %+v", res.ToolCalls)
	}
}
