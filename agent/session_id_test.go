package agent

import (
	"context"
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
