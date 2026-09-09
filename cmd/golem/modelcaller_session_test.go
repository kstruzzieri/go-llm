package main

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestChainModelCallerForwardsSessionID covers the caller golem actually uses
// when a model chain is configured. It converts ChatRequest to RoutingRequest
// independently of agent.routerModelCaller, so it needs its own guard: a drop
// here silently disables the x-opencode-session header for chain runs only.
func TestChainModelCallerForwardsSessionID(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
	}{
		{name: "id is forwarded", sessionID: "thread-1"},
		{name: "empty stays empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured provider.RoutingRequest
			mc := &chainModelCaller{
				chain: []string{"ollama/m1"},
				route: func(_ context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
					captured = rr
					return fakePlan{}, nil
				},
			}

			_, err := mc.Chat(context.Background(), provider.ChatRequest{
				Messages:  []provider.ChatMessage{{Role: "user", Content: "hello"}},
				SessionID: tt.sessionID,
			}, nil)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if captured.SessionID != tt.sessionID {
				t.Errorf("RoutingRequest.SessionID = %q, want %q", captured.SessionID, tt.sessionID)
			}
		})
	}
}
