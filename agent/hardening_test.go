package agent

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestNewToolRegistryRejectsNilTool(t *testing.T) {
	if _, err := newToolRegistry([]Tool{nil}); err == nil {
		t.Fatal("nil tool must error, not panic")
	}
}

func TestRunRejectsEmptyGoal(t *testing.T) {
	o := newTestOrchestrator(&scriptedCaller{})
	if _, err := o.Run(context.Background(), Request{Goal: ""}, nil); err == nil {
		t.Fatal("empty goal must error")
	}
}

type capturingCaller struct {
	got  provider.ChatRequest
	resp ModelResult
}

func (c *capturingCaller) Chat(_ context.Context, req provider.ChatRequest,
	_ func(provider.ChatResponse) error) (ModelResult, error) {
	c.got = req
	return c.resp, nil
}

func TestRunWiresOutputReserveToNumPredict(t *testing.T) {
	cc := &capturingCaller{resp: ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}}
	o := New(cc, ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator})
	if _, err := o.Run(context.Background(), Request{Goal: "q", Budget: Budget{OutputReserve: 256}}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cc.got.Options.NumPredict != 256 {
		t.Fatalf("NumPredict = %d, want 256 (OutputReserve must be forwarded)", cc.got.Options.NumPredict)
	}
}

type multiChunkCaller struct{}

func (multiChunkCaller) Chat(_ context.Context, _ provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {
	for _, c := range []string{"a", "b", "c"} {
		if onToken != nil {
			if err := onToken(provider.ChatResponse{Content: c}); err != nil {
				return ModelResult{}, err
			}
		}
	}
	return ModelResult{Response: provider.ChatResponse{Content: "abc", Done: true}}, nil
}

func TestRunBoundsTokenEventsPerStep(t *testing.T) {
	o := New(multiChunkCaller{}, ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator})
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	n := 0
	for _, e := range res.Events {
		if e.Kind == "token" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 token event for a single streaming step, got %d", n)
	}
}
