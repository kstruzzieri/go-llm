package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// TestRoutePlanAppliesProfileThinkParserControls verifies buildChatRequest
// binds the selected profile's think mode/tags onto the outgoing request as
// parse controls, without mutating the caller's immutable request snapshot.
func TestRoutePlanAppliesProfileThinkParserControls(t *testing.T) {
	tags := &ThinkTags{Open: "<r>", Close: "</r>"}
	rp := &RoutePlan{
		Model: "qwen3:8b",
		Profile: &ModelProfile{
			Key:       ModelKey{Provider: "ollama", Model: "qwen3:8b"},
			ThinkMode: ThinkToggle,
			ThinkTags: tags,
		},
		Request: RoutingRequest{
			Options: ModelOptions{Think: Ptr(true)},
		},
	}

	req := rp.buildChatRequest(false)

	if req.ParseThinkMode == nil {
		t.Fatal("ParseThinkMode = nil, want profile's ThinkToggle")
	}
	if *req.ParseThinkMode != ThinkToggle {
		t.Errorf("ParseThinkMode = %v, want %v", *req.ParseThinkMode, ThinkToggle)
	}
	if req.ParseThinkTags == nil {
		t.Fatal("ParseThinkTags = nil, want profile's tags")
	}
	if *req.ParseThinkTags != *tags {
		t.Errorf("ParseThinkTags = %+v, want %+v", *req.ParseThinkTags, *tags)
	}
	if req.ParseThinkTags == tags {
		t.Error("ParseThinkTags aliases the profile's ThinkTags pointer; must be a copy")
	}

	// Toggle profile: wire think options pass through untouched.
	if req.Options.Think == nil || !*req.Options.Think {
		t.Error("Options.Think not preserved for ThinkToggle profile")
	}
	// Caller's snapshot must be unmutated.
	if rp.Request.Options.Think == nil || !*rp.Request.Options.Think {
		t.Error("caller's RoutePlan.Request.Options.Think mutated by buildChatRequest")
	}
}

// TestRoutePlanToggleProfileZeroOptionsKeepsAutoParse verifies that a
// ThinkToggle profile with no caller think intent (no -think flag, no
// effort) stamps ThinkAuto as the parse mode: the wire request is
// untouched, so the model may still emit inline think tags, and a
// toggle-INACTIVE parser would leak them into Content. Explicit intent
// (Think set either way, or an effort hint) keeps toggle semantics.
func TestRoutePlanToggleProfileZeroOptionsKeepsAutoParse(t *testing.T) {
	tests := []struct {
		name string
		opts ModelOptions
		want ThinkMode
	}{
		{"zero options fall back to auto", ModelOptions{}, ThinkAuto},
		{"explicit false keeps toggle", ModelOptions{Think: Ptr(false)}, ThinkToggle},
		{"explicit true keeps toggle", ModelOptions{Think: Ptr(true)}, ThinkToggle},
		{"effort keeps toggle", ModelOptions{ThinkEffort: "high"}, ThinkToggle},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := &RoutePlan{
				Model: "qwen3:8b",
				Profile: &ModelProfile{
					Key:       ModelKey{Provider: "ollama", Model: "qwen3:8b"},
					ThinkMode: ThinkToggle,
				},
				Request: RoutingRequest{Options: tt.opts},
			}

			req := rp.buildChatRequest(false)

			if req.ParseThinkMode == nil {
				t.Fatal("ParseThinkMode = nil, want a stamped mode")
			}
			if *req.ParseThinkMode != tt.want {
				t.Errorf("ParseThinkMode = %v, want %v", *req.ParseThinkMode, tt.want)
			}
		})
	}
}

// TestRoutedToggleProfileZeroOptionsStillExtractsThinking drives the exact
// request buildChatRequest produces for a toggle profile with zero think
// options through the ollama provider: the stamped parse mode must keep
// inline <think> extraction active (develop's ThinkAuto behavior), not pass
// raw tags into Content.
func TestRoutedToggleProfileZeroOptionsStillExtractsThinking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"<think>why</think>answer"},"done":true}`))
	}))
	defer srv.Close()

	rp := &RoutePlan{
		Model: "m",
		Profile: &ModelProfile{
			Key:       ModelKey{Provider: "ollama", Model: "m"},
			ThinkMode: ThinkToggle,
		},
		Request: RoutingRequest{
			Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		},
	}
	req := rp.buildChatRequest(false)

	p := NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Thinking != "why" {
		t.Errorf("Thinking = %q, want %q", resp.Thinking, "why")
	}
	if resp.Content != "answer" {
		t.Errorf("Content = %q, want %q (raw think tags must not leak)", resp.Content, "answer")
	}
}

// TestRoutePlanClearsWireThinkForThinkNoneProfile verifies that when the
// selected profile is ThinkNone (e.g. a routed fallback to a non-thinking
// model), the outgoing request carries no wire think controls — while the
// caller's RoutePlan.Request snapshot stays untouched.
func TestRoutePlanClearsWireThinkForThinkNoneProfile(t *testing.T) {
	rp := &RoutePlan{
		Model: "gemma4:31b",
		Profile: &ModelProfile{
			Key:       ModelKey{Provider: "ollama", Model: "gemma4:31b"},
			ThinkMode: ThinkNone,
		},
		Request: RoutingRequest{
			Options: ModelOptions{Think: Ptr(true), ThinkEffort: "high"},
		},
	}

	req := rp.buildChatRequest(false)

	if req.Options.Think != nil {
		t.Errorf("Options.Think = %v, want nil for ThinkNone profile", *req.Options.Think)
	}
	if req.Options.ThinkEffort != "" {
		t.Errorf("Options.ThinkEffort = %q, want empty for ThinkNone profile", req.Options.ThinkEffort)
	}
	if req.ParseThinkMode == nil || *req.ParseThinkMode != ThinkNone {
		t.Error("ParseThinkMode should carry the profile's ThinkNone")
	}

	// Caller's snapshot must be unmutated.
	if rp.Request.Options.Think == nil || !*rp.Request.Options.Think {
		t.Error("caller's Options.Think mutated by buildChatRequest")
	}
	if rp.Request.Options.ThinkEffort != "high" {
		t.Errorf("caller's Options.ThinkEffort = %q, want %q", rp.Request.Options.ThinkEffort, "high")
	}
}
