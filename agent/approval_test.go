package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type writeTool struct{ planPreview string }

func (writeTool) Spec() ToolSpec { return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{}`)} }
func (writeTool) Effect() Effect {
	return Effect{Class: Write, Approval: ApprovalOnWrite}
}
func (w writeTool) Plan(context.Context, json.RawMessage) (ToolPlan, error) {
	return ToolPlan{Effect: Effect{Class: Write, Approval: ApprovalOnWrite}, Preview: w.planPreview}, nil
}
func (writeTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "wrote"}, nil
}

type defaultWriteTool struct{}

func (defaultWriteTool) Spec() ToolSpec {
	return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{}`)}
}
func (defaultWriteTool) Effect() Effect { return Effect{Class: Write} }
func (defaultWriteTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "wrote"}, nil
}

func writeCallSeq() []ModelResult {
	return []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "writer", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "ok", Done: true}},
	}
}

func TestNilApproverDeniesWriteTool(t *testing.T) {
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ToolCalls[0].Denied {
		t.Fatalf("nil approver must deny a Write tool, got %+v", res.ToolCalls[0])
	}
}

func TestNilApproverDeniesDefaultWriteTool(t *testing.T) {
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{defaultWriteTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ToolCalls[0].Denied {
		t.Fatalf("nil approver must deny a zero-policy Write tool, got %+v", res.ToolCalls[0])
	}
}

type capturingApprover struct {
	gotPreview string
	allow      bool
}

func (c *capturingApprover) Approve(_ context.Context, _ provider.ToolCall, preview string) (bool, error) {
	c.gotPreview = preview
	return c.allow, nil
}

func TestApproverReceivesPlanPreview(t *testing.T) {
	ap := &capturingApprover{allow: true}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planPreview: "DIFF: +1 -0"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.gotPreview != "DIFF: +1 -0" {
		t.Fatalf("approver preview = %q, want plan preview", ap.gotPreview)
	}
}
