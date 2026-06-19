package agent

import (
	"context"
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
