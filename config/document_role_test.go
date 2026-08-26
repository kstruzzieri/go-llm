package config

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
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

func eligibleAddOpts() SetRoleModelOpts {
	return SetRoleModelOpts{
		Requirements:   map[string]provider.Capability{"agent": provider.CapChat},
		Caps:           provider.CapChat | provider.CapStream,
		KnownMask:      provider.CapChat | provider.CapStream,
		ConfirmUnknown: true, // new roles are unrouted: unknown/no_requirements
	}
}

func TestAddRoleModel(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{
		Key:  provider.ModelKey{Provider: "local", Model: "m9"},
		Type: "dense", Parameters: "9B", ContextWindow: 8192,
	}
	if err := d.AddRoleModel("fresh", facts, eligibleAddOpts()); err != nil {
		t.Fatal(err)
	}
	m := d.authored.Models["fresh"]
	if m.Name != "m9" || m.Provider != "local" || m.Type != "dense" ||
		m.Parameters != "9B" || m.ContextWindow != 8192 {
		t.Fatalf("identity/capacity: %+v", m)
	}
	if m.Description != "" || m.Fallbacks != nil || m.Options != nil ||
		m.Capabilities != nil || m.ThinkMode != "" || m.ThinkTags != nil || m.Slots != 0 {
		t.Fatalf("new role not born minimal: %+v", m)
	}
}

func TestAddRoleModelRefusals(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	assertDiag(t, d.AddRoleModel("", facts, eligibleAddOpts()),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.AddRoleModel("x", ModelFacts{Type: "dense"}, eligibleAddOpts()),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.AddRoleModel("x", ModelFacts{Key: facts.Key, Type: "huge"}, eligibleAddOpts()),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.AddRoleModel("agent", facts, eligibleAddOpts()),
		CodeRoleExists, SubjectRole, "agent")
	assertDiag(t, d.AddRoleModel("x",
		ModelFacts{Key: provider.ModelKey{Provider: "ghost", Model: "m"}, Type: "dense"},
		eligibleAddOpts()), CodeProviderNotFound, SubjectProvider, "ghost")
}

// Unrouted new role with no override: unknown/no_requirements requires
// ConfirmUnknown (amendment-5 consistency — spec checkbox 2).
func TestAddRoleModelUnroutedRequiresConfirm(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	opts := eligibleAddOpts()
	opts.ConfirmUnknown = false
	assertDiag(t, d.AddRoleModel("fresh", facts, opts),
		CodeEligibilityUnknown, SubjectRole, "fresh")
}

// A capabilities override joining a LIVE shared selector changes
// selector-wide truth: the gate evaluates existing selector roles. "agent"
// is routed and requires chat; an override without chat is ineligible.
func TestAddRoleModelOverrideGatesSelectorRoles(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m1"}, Type: "moe"}
	opts := SetRoleModelOpts{
		Requirements:   map[string]provider.Capability{"agent": provider.CapChat},
		Capabilities:   []string{"embed"}, // valid vocabulary, lacks chat
		ConfirmUnknown: true,
	}
	assertDiag(t, d.AddRoleModel("joiner", facts, opts),
		CodeEligibilityIneligible, SubjectRole, "joiner")
}

// Re-assertions install deep copies.
func TestAddRoleModelReassertions(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	caps := []string{"chat", "stream"}
	tags := &ThinkTagsConfig{Open: "<r>", Close: "</r>"}
	opts := eligibleAddOpts()
	opts.Capabilities, opts.ThinkMode, opts.ThinkTags = caps, "toggle", tags
	if err := d.AddRoleModel("fresh", facts, opts); err != nil {
		t.Fatal(err)
	}
	caps[0] = "mutated"
	tags.Open = "<mutated>"
	m := d.authored.Models["fresh"]
	if m.Capabilities[0] != "chat" || m.ThinkTags.Open != "<r>" || m.ThinkMode != "toggle" {
		t.Fatalf("aliased inputs: %+v", m)
	}
}

// Resurrection pin (the tombstone acceptance behavior): remove a role
// whose raw entry has unknown members, re-add the same name, save — the
// stale members must NOT come back.
func TestAddRoleModelNoResurrection(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.UnbindUseCase("agent"); err != nil {
		t.Fatal(err)
	}
	if err := d.RemoveRole("agent"); err != nil {
		t.Fatal(err)
	}
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	if err := d.AddRoleModel("agent", facts, eligibleAddOpts()); err != nil {
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
		t.Fatalf("stale raw members resurrected:\n%s", out)
	}
	if !strings.Contains(string(out), `"m9"`) {
		t.Fatalf("re-added role missing:\n%s", out)
	}
}

// The baseline-reset variant: removal is saved first, then the same role
// name is added and saved again. The old unknown subtree is gone from the
// new raw baseline and cannot return.
func TestAddRoleModelNoResurrectionAfterSavedRemoval(t *testing.T) {
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
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	if err := d.AddRoleModel("agent", facts, eligibleAddOpts()); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "future_role_field") {
		t.Fatalf("saved stale raw members resurrected:\n%s", out)
	}
}

// Transactionality through a finalize-stage failure: an invalid re-asserted
// think mode passes the closure and dies at finalize; nothing changes.
func TestAddRoleModelFinalizeFailureTransactional(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	beforeEffective := d.Config()
	beforeRender := renderToString(t, d)
	beforeDrops, beforeSeeds := len(d.modelDrops), len(d.modelRawSeeds)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	opts := eligibleAddOpts()
	opts.ThinkMode = "sometimes"
	assertDiag(t, d.AddRoleModel("fresh", facts, opts), CodeThinkInvalid, SubjectRole, "fresh")
	if _, ok := d.authored.Models["fresh"]; ok {
		t.Fatal("failed mutation left the role behind")
	}
	if !reflect.DeepEqual(beforeEffective, d.Config()) || beforeRender != renderToString(t, d) {
		t.Fatal("effective view changed on failure")
	}
	if len(d.modelDrops) != beforeDrops || len(d.modelRawSeeds) != beforeSeeds {
		t.Fatal("raw bookkeeping changed on failure")
	}
}
