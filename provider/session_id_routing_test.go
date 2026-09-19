package provider

import (
	"context"
	"testing"
)

// TestRoutePlanCarriesSessionIDOntoChatRequest pins one half of the #533
// plumbing: the routed plan rebuilds a ChatRequest from its immutable
// RoutingRequest snapshot, so the session id has to survive that rebuild.
func TestRoutePlanCarriesSessionIDOntoChatRequest(t *testing.T) {
	tests := []struct {
		name      string
		sessionID string
	}{
		{name: "id survives the rebuild", sessionID: "thread-1"},
		{name: "empty stays empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := &RoutePlan{
				Model:   "qwen3:8b",
				Request: RoutingRequest{SessionID: tt.sessionID},
			}
			if got := rp.buildChatRequest(false); got.SessionID != tt.sessionID {
				t.Errorf("ChatRequest.SessionID = %q, want %q", got.SessionID, tt.sessionID)
			}
		})
	}
}

// TestRouterChatCarriesSessionIDToProvider is the end-to-end guard: routing
// converts ChatRequest to RoutingRequest and back, and every field not copied
// on both hops is silently dropped. Asserting at the provider — the last stop
// before the wire — is what catches a drop anywhere in between.
func TestRouterChatCarriesSessionIDToProvider(t *testing.T) {
	router, prov := setupTestRouter(t)

	_, err := router.Chat(context.Background(), ChatRequest{
		Model:     "qwen3:8b",
		Messages:  []ChatMessage{{Role: "user", Content: "hi"}},
		SessionID: "thread-direct",
	})
	if err != nil {
		t.Fatalf("Router.Chat: %v", err)
	}

	prov.mu.Lock()
	defer prov.mu.Unlock()
	if got := prov.lastChatReq.SessionID; got != "thread-direct" {
		t.Errorf("provider received SessionID = %q, want %q", got, "thread-direct")
	}
}
