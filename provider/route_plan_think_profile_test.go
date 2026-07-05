package provider

import "testing"

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
