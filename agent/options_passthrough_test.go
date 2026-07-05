package agent

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
)

// capturingModelCaller records the ChatRequest of the most recent Chat call,
// then returns a final answer so Run completes in one step.
type capturingModelCaller struct {
	got provider.ChatRequest
}

func (c *capturingModelCaller) Chat(_ context.Context, req provider.ChatRequest,
	_ func(provider.ChatResponse) error) (ModelResult, error) {
	c.got = req
	return ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

func TestRequestOptionsReachChatRequest(t *testing.T) {
	mc := &capturingModelCaller{}
	o := newTestOrchestrator(mc)
	opts := provider.ModelOptions{
		Think:       provider.Ptr(true),
		ThinkEffort: "high",
		Temperature: provider.Ptr(0.2),
	}
	_, err := o.Run(context.Background(), Request{Goal: "g", Options: opts}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.got.Options.Think == nil || *mc.got.Options.Think != true {
		t.Fatalf("Options.Think = %v, want true", mc.got.Options.Think)
	}
	if mc.got.Options.ThinkEffort != "high" {
		t.Fatalf("Options.ThinkEffort = %q, want high", mc.got.Options.ThinkEffort)
	}
	if mc.got.Options.Temperature == nil || *mc.got.Options.Temperature != 0.2 {
		t.Fatalf("Options.Temperature = %v, want 0.2", mc.got.Options.Temperature)
	}
}

func TestOutputReserveStillWinsNumPredict(t *testing.T) {
	mc := &capturingModelCaller{}
	o := newTestOrchestrator(mc)
	req := Request{
		Goal:    "g",
		Options: provider.ModelOptions{NumPredict: 111},
		Budget:  Budget{OutputReserve: 222},
	}
	if _, err := o.Run(context.Background(), req, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.got.Options.NumPredict != 222 {
		t.Fatalf("Options.NumPredict = %d, want 222 (Budget.OutputReserve wins)", mc.got.Options.NumPredict)
	}
}

// TestSummarizePathDoesNotInheritThink pins routerSummarizer.Summarize to its
// own fixed ModelOptions (NumPredict only). The summarize path builds its
// RoutingRequest directly — it never routes through buildChatRequest or
// Request.Options — so main-run think settings structurally cannot leak in.
// This guards against a future refactor accidentally threading them through.
func TestSummarizePathDoesNotInheritThink(t *testing.T) {
	var gotReq provider.RoutingRequest
	s := &routerSummarizer{
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			gotReq = rr
			return fakePlan{}, nil
		},
	}
	if _, err := s.Summarize(context.Background(), "",
		[]conversation.Message{{Role: "user", Content: "old turn"}}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if gotReq.Options.Think != nil {
		t.Fatalf("Options.Think = %v, want nil (summarize must not inherit run think settings)", gotReq.Options.Think)
	}
	if gotReq.Options.ThinkEffort != "" {
		t.Fatalf("Options.ThinkEffort = %q, want empty", gotReq.Options.ThinkEffort)
	}
	if gotReq.Options.NumPredict != DefaultSummaryOutputReserve {
		t.Fatalf("Options.NumPredict = %d, want %d", gotReq.Options.NumPredict, DefaultSummaryOutputReserve)
	}
}
