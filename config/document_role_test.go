package config

import (
	"encoding/json"
	"os"
	"testing"
)

// roleFixture: one provider, three roles, two routes. "spare" is
// unrouted and unreferenced (removable); "fast" is agent's fallback;
// "agent" is routed. The unknown members pin raw preservation.
const roleFixture = `{
  "providers": {
    "local": {"base_url": "http://localhost:1", "api_format": "openai-compat",
      "slot_discovery": true, "future_provider_field": true}
  },
  "models": {
    "agent": {"name": "m1", "provider": "local", "type": "moe",
      "description": "keep me", "parameters": "30B", "context_window": 32768,
      "capabilities": ["chat", "stream"], "fallbacks": ["fast"],
      "options": {"temperature": 0.2}, "slots": 2,
      "think_mode": "always", "think_tags": {"open": "<t>", "close": "</t>"},
      "future_role_field": {"nested": [1, 2], "dup": 1, "dup": 2}},
    "fast": {"name": "m2", "provider": "local", "type": "dense"},
    "spare": {"name": "m3", "provider": "local", "type": "dense",
      "stale_spare_field": true}
  },
  "defaults": {"agent": "agent", "chat": "fast"}
}`

func TestUnbindUseCase(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.UnbindUseCase("chat"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.authored.Defaults["chat"]; ok {
		t.Fatal("authored still bound")
	}
	if _, ok := d.Config().Defaults["chat"]; ok {
		t.Fatal("effective still bound")
	}
}

// Unbinding "agent" is generically valid: defaults["agent"] is a
// NewDocument birth guarantee, not a standing invariant (spec ground truth).
func TestUnbindUseCaseAgentAllowed(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.UnbindUseCase("agent"); err != nil {
		t.Fatal(err)
	}
}

func TestUnbindUseCaseRefusals(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	assertDiag(t, d.UnbindUseCase(""), CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.UnbindUseCase("missing"), CodeUseCaseNotFound, SubjectUseCase, "missing")
}

// The rendered defaults map loses only the requested use case. Decode the
// map instead of scanning for "chat", which also appears as a capability.
func TestUnbindUseCaseRender(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.UnbindUseCase("chat"); err != nil {
		t.Fatal(err)
	}
	path := d.Origin().Path
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Defaults map[string]string `json:"defaults"`
	}
	if err := json.Unmarshal(out, &rendered); err != nil {
		t.Fatal(err)
	}
	if _, ok := rendered.Defaults["chat"]; ok {
		t.Fatalf("published defaults still carry chat: %v", rendered.Defaults)
	}
	if rendered.Defaults["agent"] != "agent" {
		t.Fatalf("unrelated default changed: %v", rendered.Defaults)
	}
}
