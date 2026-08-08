package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func newTestOrchestrator(mc ModelCaller, opts ...Option) *Orchestrator {
	return New(mc, ContextManager{
		Compactor: RecencyCompactor{Estimate: runeEstimator},
		Estimate:  runeEstimator,
	}, opts...)
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

// pressureRec is a local observer capturing callback order and pressure events
// without importing agenttest (avoids any test import cycle).
type pressureRec struct {
	kinds      []string
	pressures  []PressureEvent
	onPressErr error
}

func (r *pressureRec) OnStep(_ context.Context, _ StepEvent) error {
	r.kinds = append(r.kinds, "step")
	return nil
}
func (r *pressureRec) OnToolCall(_ context.Context, _ ToolCallEvent) error {
	r.kinds = append(r.kinds, "tool_call")
	return nil
}
func (r *pressureRec) OnToken(_ context.Context, _ TokenEvent) error {
	r.kinds = append(r.kinds, "token")
	return nil
}
func (r *pressureRec) OnPressure(_ context.Context, e PressureEvent) error {
	r.kinds = append(r.kinds, "pressure")
	r.pressures = append(r.pressures, e)
	return r.onPressErr
}

func TestOnPressureFiresBeforeFirstStep(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	rec := &pressureRec{}
	if _, err := o.Run(context.Background(), Request{Goal: "q"}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.kinds) == 0 || rec.kinds[0] != "pressure" {
		t.Fatalf("first callback should be pressure, got %v", rec.kinds)
	}
	if len(rec.pressures) == 0 {
		t.Fatal("expected a pressure event")
	}
}

func TestOnPressureFiresOnExhaustionWithoutModelCall(t *testing.T) {
	mc := &scriptedCaller{responses: nil}
	o := newTestOrchestrator(mc)
	rec := &pressureRec{}
	req := Request{Goal: "0123456789012345678901234567890123456789", Budget: Budget{InputCeiling: 2}}
	_, err := o.Run(context.Background(), req, rec)
	if !errors.Is(err, ErrContextExhausted) {
		t.Fatalf("want ErrContextExhausted, got %v", err)
	}
	if mc.calls != 0 {
		t.Fatalf("model should not be called on exhaustion, calls=%d", mc.calls)
	}
	if len(rec.pressures) != 1 || rec.pressures[0].Pressure.Mitigation != MitigationHalt {
		t.Fatalf("expected one halt pressure event, got %+v", rec.pressures)
	}
}

func TestOnPressureErrorAbortsBeforeModelCall(t *testing.T) {
	mc := &scriptedCaller{responses: nil}
	o := newTestOrchestrator(mc)
	sentinel := fmt.Errorf("observer abort")
	rec := &pressureRec{onPressErr: sentinel}
	_, err := o.Run(context.Background(), Request{Goal: "q"}, rec)
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
	if mc.calls != 0 {
		t.Fatalf("model should not be called when OnPressure errors, calls=%d", mc.calls)
	}
}
