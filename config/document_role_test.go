package config

import (
	"encoding/json"
	"os"
	"strings"
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

func TestRemoveRole(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.RemoveRole("spare"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.authored.Models["spare"]; ok {
		t.Fatal("authored still holds role")
	}
	if _, ok := d.modelDrops["spare"]; !ok {
		t.Fatal("tombstone not recorded")
	}
}

func TestRemoveRoleRefusals(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	assertDiag(t, d.RemoveRole(""), CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.RemoveRole("nope"), CodeRoleNotFound, SubjectRole, "nope")
	// Routed: "agent" is defaults["agent"] — first routing use case named.
	assertDiag(t, d.RemoveRole("agent"), CodeRoleInUse, SubjectUseCase, "agent")
	// Fallback-referenced: unbind "chat" first so only the fallback blocks.
	if err := d.UnbindUseCase("chat"); err != nil {
		t.Fatal(err)
	}
	assertDiag(t, d.RemoveRole("fast"), CodeRoleInUse, SubjectRole, "agent")
	// Refusals are transactional: nothing changed.
	if len(d.authored.Models) != 3 || len(d.modelDrops) != 0 {
		t.Fatalf("refusal mutated state: %v %v", d.authored.Models, d.modelDrops)
	}
}

// Defaults precedence over fallback scan: "fast" is BOTH routed (chat)
// and fallback-referenced (agent) in the fixture; the refusal names the
// use case, proving the defaults walk runs first.
func TestRemoveRolePrecedence(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	assertDiag(t, d.RemoveRole("fast"), CodeRoleInUse, SubjectUseCase, "chat")
}

// Removing the last role is generically valid (no minimum-models rule):
// remove routes first, then every role.
func TestRemoveAllRoles(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	for _, uc := range []string{"agent", "chat"} {
		if err := d.UnbindUseCase(uc); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"agent", "fast", "spare"} {
		if err := d.RemoveRole(role); err != nil {
			t.Fatalf("remove %s: %v", role, err)
		}
	}
}

// A removed role's raw entry (unknown members included) leaves the
// rendered bytes, and the tombstone survives to the save commit.
func TestRemoveRoleRender(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.UnbindUseCase("agent"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRole("agent"); err != nil {
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
	if strings.Contains(string(out), "future_role_field") {
		t.Fatalf("removed role's raw members survived:\n%s", out)
	}
	if len(d.modelDrops) != 0 {
		t.Fatal("commitSavedLocked did not clear modelDrops")
	}
}
