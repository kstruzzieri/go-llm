package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// scriptedCaller returns queued responses in order, one per Chat call.
type scriptedCaller struct {
	responses []ModelResult
	calls     int
}

func (s *scriptedCaller) Chat(_ context.Context, _ provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {
	r := s.responses[s.calls]
	s.calls++
	if onToken != nil {
		if err := onToken(provider.ChatResponse{Content: r.Response.Content}); err != nil {
			return ModelResult{}, err
		}
	}
	return r, nil
}

func newTestOrchestrator(mc ModelCaller) *Orchestrator {
	return New(mc, ContextManager{
		Compactor: RecencyCompactor{Estimate: runeEstimator},
		Estimate:  runeEstimator,
	})
}

func TestRunSingleStepFinalAnswer(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "the answer", Done: true}, RouteOutcome: &provider.RouteOutcome{}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "the answer" || res.StopReason != Completed {
		t.Fatalf("got answer=%q stop=%v", res.Answer, res.StopReason)
	}
	if len(res.Steps) != 1 || res.Steps[0].RouteOutcome == nil {
		t.Fatalf("expected one step with RouteOutcome, got %+v", res.Steps)
	}
}

func TestRunWithZeroValueContextManagerUsesDefaults(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "ok", Done: true}},
	}}
	o := New(mc, ContextManager{})
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatalf("Run with zero-value ContextManager: %v", err)
	}
	if res.Answer != "ok" {
		t.Fatalf("answer = %q, want ok", res.Answer)
	}
}

func TestRunReturnsCurrentTurnTranscriptWithToolObservation(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{
				Name:      "echo",
				Arguments: json.RawMessage(`{"x":1}`),
			},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal:  "use echo",
		Tools: []Tool{echoTool{name: "echo"}},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Messages) != 4 {
		t.Fatalf("Messages = %+v, want user/tool-call/tool/final transcript", res.Messages)
	}
	if res.Messages[0].Role != "user" || res.Messages[0].Content != "use echo" {
		t.Fatalf("message 0 = %+v, want current user goal", res.Messages[0])
	}
	if res.Messages[1].Role != "assistant" || len(res.Messages[1].ToolCalls) != 1 {
		t.Fatalf("message 1 = %+v, want assistant tool call", res.Messages[1])
	}
	if res.Messages[2].Role != "tool" || res.Messages[2].ToolName != "echo" ||
		res.Messages[2].ToolCallID != "c1" || res.Messages[2].Content != `tool-said:{"x":1}` {
		t.Fatalf("message 2 = %+v, want echo tool observation", res.Messages[2])
	}
	if res.Messages[3].Role != "assistant" || res.Messages[3].Content != "done" {
		t.Fatalf("message 3 = %+v, want final answer", res.Messages[3])
	}
}
