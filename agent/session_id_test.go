package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestRequestSessionIDReachesChatRequest pins the plumbing that lets a caller
// give every provider call for one conversation a stable identity: the id set
// on the agent Request must arrive on the ChatRequest the model caller sees.
func TestRequestSessionIDReachesChatRequest(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{name: "set id is forwarded", id: "thread-1", want: "thread-1"},
		{name: "empty id stays empty", id: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mc := &capturingModelCaller{}
			o := newTestOrchestrator(mc)
			if _, err := o.Run(context.Background(), Request{Goal: "g", SessionID: tt.id}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := mc.got.SessionID; got != tt.want {
				t.Errorf("ChatRequest.SessionID = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSessionIDSurvivesContextAssembly(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "recency eviction"
		if mixed {
			name = "mixed structured assembly"
		}
		t.Run(name, func(t *testing.T) {
			mc := &toolThenAnswerCaller{}
			rec := &asmRec{}
			o := New(mc, ContextManager{Mixed: mixed, Estimate: runeEstimator})
			res, err := o.Run(context.Background(), Request{
				Goal:      "q",
				SessionID: "thread-assembly",
				History:   []provider.ChatMessage{{Role: "user", Content: strings.Repeat("old", 4096)}},
				Budget:    Budget{InputCeiling: 4096},
				Tools:     []Tool{structuredTool{}},
			}, rec)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			evicted := false
			for _, event := range res.Events {
				evicted = evicted || event.Kind == "compaction"
			}
			if !evicted {
				t.Fatal("history must exceed the budget and trigger eviction")
			}
			if mixed && len(rec.events) != 1 {
				t.Fatalf("mixed assemblies = %d, want 1 after the structured tool result", len(rec.events))
			}
			if len(mc.reqs) != 2 {
				t.Fatalf("model calls = %d, want 2", len(mc.reqs))
			}
			for i, req := range mc.reqs {
				if req.SessionID != "thread-assembly" {
					t.Errorf("model call %d SessionID = %q, want thread-assembly", i, req.SessionID)
				}
			}
		})
	}
}

// TestRouterModelCallerForwardsSessionID pins the hop that actually runs in
// production: the orchestrator's ChatRequest is converted to a
// provider.RoutingRequest here, and a dropped field means the session header
// is never sent no matter what the provider does with it.
func TestRouterModelCallerForwardsSessionID(t *testing.T) {
	var got provider.RoutingRequest
	mc := &routerModelCaller{
		route: func(_ context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			got = rr
			return fakePlan{}, nil
		},
	}
	if _, err := mc.Chat(context.Background(), provider.ChatRequest{SessionID: "thread-1"}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.SessionID != "thread-1" {
		t.Errorf("RoutingRequest.SessionID = %q, want %q", got.SessionID, "thread-1")
	}
}
