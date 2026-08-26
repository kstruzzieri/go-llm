package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
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

func forkFacts() ModelFacts {
	return ModelFacts{
		Key:  provider.ModelKey{Provider: "local", Model: "m7"},
		Type: "moe", Parameters: "7B", ContextWindow: 4096,
	}
}

func forkOpts(confirm ...string) ForkRoleModelOpts {
	return ForkRoleModelOpts{
		SetRoleModelOpts: SetRoleModelOpts{ConfirmUnknown: true},
		ConfirmDrops:     confirm,
	}
}

// The fork overlay: role-level intent (Description, Fallbacks, Options)
// travels; identity/capacity from facts; caps/think/slots cleared unless
// re-asserted (SetRoleModel preservation table).
func TestForkRoleModelOverlay(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	src := d.authored.Models["agent"]
	m := d.authored.Models["agent-m"]
	if m.Name != "m7" || m.Provider != "local" || m.Type != "moe" ||
		m.Parameters != "7B" || m.ContextWindow != 4096 {
		t.Fatalf("identity/capacity: %+v", m)
	}
	if m.Description != "keep me" || !reflect.DeepEqual(m.Fallbacks, src.Fallbacks) ||
		m.Options == nil || *m.Options.Temperature != 0.2 {
		t.Fatalf("role-level intent not copied: %+v", m)
	}
	if m.Capabilities != nil || m.ThinkMode != "" || m.ThinkTags != nil || m.Slots != 0 {
		t.Fatalf("model-specific fields not cleared: %+v", m)
	}
	// Deep copy: mutating the fork's Options cannot reach the source.
	*m.Options.Temperature = 0.9
	m.Fallbacks[0] = "mutated"
	if *d.authored.Models["agent"].Options.Temperature != 0.2 {
		t.Fatal("Options aliased between source and fork")
	}
	if d.authored.Models["agent"].Fallbacks[0] != "fast" {
		t.Fatal("Fallbacks aliased between source and fork")
	}
	// Source untouched.
	if src.Slots != 2 || src.ThinkTags == nil {
		t.Fatalf("source mutated: %+v", src)
	}
}

// Drop confirmation: exact sorted match or refusal BEFORE mutation, with
// the computed set extractable via DropSetOf.
func TestForkRoleModelDropConfirmation(t *testing.T) {
	cases := []struct {
		name    string
		confirm []string
	}{
		{"missing", nil},
		{"partial", []string{"slots"}},
		{"unknown-token", []string{"slots", "think_tags", "capabilities"}},
		{"duplicate", []string{"slots", "slots", "think_tags"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := loadTestDoc(t, roleFixture)
			err := d.ForkRoleModel("agent", "agent-m", forkFacts(), forkOpts(tc.confirm...))
			if err == nil {
				t.Fatal("expected refusal")
			}
			if _, ok := d.authored.Models["agent-m"]; ok {
				t.Fatal("refusal mutated the document")
			}
			switch tc.name {
			case "unknown-token", "duplicate":
				assertDiag(t, err, CodeInvalidArgument, SubjectNone, "")
			default:
				assertDiag(t, err, CodeDropConfirmationRequired, SubjectRole, "agent")
				drops, ok := DropSetOf(err)
				if !ok || !slices.Equal(drops, []string{"slots", "think_tags"}) {
					t.Fatalf("DropSetOf = %v, %v", drops, ok)
				}
			}
		})
	}
}

// Pin every ThinkTags present/absent × Slots set/unset combination. A
// valid-vocabulary superset is a confirmation mismatch, not invalid input.
func TestForkRoleModelDropSetAllCombinations(t *testing.T) {
	cases := []struct {
		name     string
		hasSlots bool
		hasTags  bool
		want     []string
	}{
		{"neither", false, false, nil},
		{"slots-only", true, false, []string{"slots"}},
		{"tags-only", false, true, []string{"think_tags"}},
		{"both", true, true, []string{"slots", "think_tags"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := roleFixture
			if !tc.hasSlots {
				body = strings.Replace(body, `"slots": 2,`, "", 1)
			}
			if !tc.hasTags {
				body = strings.Replace(body,
					`"think_tags": {"open": "<t>", "close": "</t>"},`, "", 1)
			}
			d := loadTestDoc(t, body)
			if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
				forkOpts(tc.want...)); err != nil {
				t.Fatal(err)
			}
		})
	}

	body := strings.Replace(roleFixture,
		`"think_tags": {"open": "<t>", "close": "</t>"},`, "", 1)
	d := loadTestDoc(t, body) // slots-only
	err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags"))
	assertDiag(t, err, CodeDropConfirmationRequired, SubjectRole, "agent")
	drops, ok := DropSetOf(err)
	if !ok || !slices.Equal(drops, []string{"slots"}) {
		t.Fatalf("DropSetOf = %v, %v", drops, ok)
	}

	// Wrong token, same length: slots-only source confirmed with think_tags.
	d2 := loadTestDoc(t, body)
	err = d2.ForkRoleModel("agent", "agent-m", forkFacts(), forkOpts("think_tags"))
	assertDiag(t, err, CodeDropConfirmationRequired, SubjectRole, "agent")
	if drops, ok := DropSetOf(err); !ok || !slices.Equal(drops, []string{"slots"}) {
		t.Fatalf("DropSetOf = %v, %v", drops, ok)
	}
}

// A re-asserted ThinkTags is not a drop; a source without think_tags/slots
// requires an empty confirmation; unordered confirmation is accepted
// (compared sorted).
func TestForkRoleModelDropSetVariants(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	opts := forkOpts("slots")
	opts.ThinkTags = &ThinkTagsConfig{Open: "<n>", Close: "</n>"}
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(), opts); err != nil {
		t.Fatal(err)
	}
	if d.authored.Models["agent-m"].ThinkTags.Open != "<n>" {
		t.Fatal("re-asserted think tags missing")
	}
	// fast: no think_tags, no slots -> empty drop set, empty confirm.
	if err := d.ForkRoleModel("fast", "fast-m", forkFacts(), forkOpts()); err != nil {
		t.Fatal(err)
	}
	// Unordered confirmation.
	if err := d.ForkRoleModel("agent", "agent-m2", forkFacts(),
		forkOpts("think_tags", "slots")); err != nil {
		t.Fatal(err)
	}
}

func TestForkRoleModelRefusals(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	assertDiag(t, d.ForkRoleModel("", "x", forkFacts(), forkOpts()),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.ForkRoleModel("agent", "", forkFacts(), forkOpts()),
		CodeInvalidArgument, SubjectNone, "")
	missingModel := forkFacts()
	missingModel.Key.Model = ""
	assertDiag(t, d.ForkRoleModel("agent", "x", missingModel, forkOpts()),
		CodeInvalidArgument, SubjectNone, "")
	badType := forkFacts()
	badType.Type = "quantum"
	assertDiag(t, d.ForkRoleModel("agent", "x", badType, forkOpts()),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.ForkRoleModel("ghost", "x", forkFacts(), forkOpts()),
		CodeRoleNotFound, SubjectRole, "ghost")
	assertDiag(t, d.ForkRoleModel("agent", "fast", forkFacts(), forkOpts("slots", "think_tags")),
		CodeRoleExists, SubjectRole, "fast")
	bad := forkFacts()
	bad.Key.Provider = "ghost"
	assertDiag(t, d.ForkRoleModel("agent", "x", bad, forkOpts("slots", "think_tags")),
		CodeProviderNotFound, SubjectProvider, "ghost")
}

// Guard precedence is contract-visible. Malformed confirmation is checked
// only after facts, source, destination, and provider guards.
func TestForkRoleModelRefusalPrecedence(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	badDrops := forkOpts("bogus")
	assertDiag(t, d.ForkRoleModel("ghost", "x", forkFacts(), badDrops),
		CodeRoleNotFound, SubjectRole, "ghost")
	assertDiag(t, d.ForkRoleModel("agent", "fast", forkFacts(), badDrops),
		CodeRoleExists, SubjectRole, "fast")
	badProvider := forkFacts()
	badProvider.Key.Provider = "ghost"
	assertDiag(t, d.ForkRoleModel("agent", "x", badProvider, badDrops),
		CodeProviderNotFound, SubjectProvider, "ghost")
}

// A supplied capability override joining a live selector gates the existing
// selector role before the fork can mutate anything.
func TestForkRoleModelOverrideGatesSelectorRoles(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m1"}, Type: "moe"}
	opts := forkOpts("slots", "think_tags")
	opts.Capabilities = []string{"embed"}
	opts.Requirements = map[string]provider.Capability{"agent": provider.CapChat}
	assertDiag(t, d.ForkRoleModel("agent", "agent-m", facts, opts),
		CodeEligibilityIneligible, SubjectRole, "agent-m")
}

// Copied role-level intent still passes the normal finalize authority. A
// changed type that makes a copied fallback incompatible is refused.
func TestForkRoleModelCopiedFallbackMustRemainCompatible(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	before := renderToString(t, d)
	facts := forkFacts()
	facts.Type = "embedding"
	err := d.ForkRoleModel("agent", "agent-m", facts,
		forkOpts("slots", "think_tags"))
	assertDiag(t, err, CodeModelInvalid, SubjectRole, "agent-m")
	if renderToString(t, d) != before || len(d.modelRawSeeds) != 0 {
		t.Fatal("incompatible copied fallback mutated the document")
	}
}

// Fork transactionality through a finalize failure: re-asserted
// capabilities that conflict with the source selector's authored override
// die at validate's selector-conflict check when the fork keeps the same
// selector; the document is unchanged and no seed is registered.
func TestForkRoleModelFinalizeFailureTransactional(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	beforeEffective := d.Config()
	beforeRender := renderToString(t, d)
	beforeDrops, beforeSeeds := len(d.modelDrops), len(d.modelRawSeeds)
	sameSelector := ModelFacts{
		Key:  provider.ModelKey{Provider: "local", Model: "m1"},
		Type: "moe",
	}
	opts := forkOpts("slots", "think_tags")
	opts.Capabilities = []string{"chat"} // agent has ["chat","stream"]: conflict
	opts.Requirements = map[string]provider.Capability{"agent": provider.CapChat}
	err := d.ForkRoleModel("agent", "agent-m", sameSelector, opts)
	assertDiag(t, err, CodeSelectorConflict, SubjectRole, "agent")
	if _, ok := d.authored.Models["agent-m"]; ok {
		t.Fatal("failed fork left the role")
	}
	if !reflect.DeepEqual(beforeEffective, d.Config()) || beforeRender != renderToString(t, d) ||
		len(d.modelDrops) != beforeDrops || len(d.modelRawSeeds) != beforeSeeds {
		t.Fatal("failed fork changed authored/effective/raw bookkeeping state")
	}
}

func renderedModelEntries(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Models
}

// ACCEPTANCE GATE (firn-ide#263 Slice B Task 0): unknown/future authored
// JSON members survive ForkRoleModel into the published bytes; the
// dropped think_tags/slots do not; the source entry equals its canonical
// pre-fork baseline. The unknown subtree's duplicate keys stay duplicated.
func TestForkRoleModelLosslessUnknownMembers(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	beforeEntries := renderedModelEntries(t, []byte(renderToString(t, d)))
	sourceBefore := append(json.RawMessage(nil), beforeEntries["agent"]...)
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
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
	entries := renderedModelEntries(t, out)
	if !bytes.Equal(entries["agent"], sourceBefore) {
		t.Fatalf("source changed:\nbefore=%s\nafter=%s", sourceBefore, entries["agent"])
	}
	var source, fork map[string]json.RawMessage
	if err := json.Unmarshal(entries["agent"], &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(entries["agent-m"], &fork); err != nil {
		t.Fatal(err)
	}
	if fork == nil {
		t.Fatalf("fork missing from published bytes:\n%s", out)
	}
	if !bytes.Equal(fork["future_role_field"], source["future_role_field"]) ||
		bytes.Count(fork["future_role_field"], []byte(`"dup"`)) != 2 {
		t.Fatalf("unknown duplicate subtree changed: source=%s fork=%s",
			source["future_role_field"], fork["future_role_field"])
	}
	if _, ok := fork["think_tags"]; ok {
		t.Fatal("dropped think_tags survived in fork")
	}
	if _, ok := fork["slots"]; ok {
		t.Fatal("dropped slots survived in fork")
	}
	if string(fork["name"]) != `"m7"` {
		t.Fatalf("fork selector wrong: %s", fork["name"])
	}
	// Source keeps everything.
	if _, ok := source["think_tags"]; !ok {
		t.Fatal("source lost think_tags")
	}
	if string(source["future_role_field"]) == "" {
		t.Fatal("source lost unknown member")
	}
}

// Chained fork in one draft: A -> B, then fork B -> C carries A's unknown members.
func TestForkRoleModelChained(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.ForkRoleModel("agent", "b", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	if err := d.ForkRoleModel("b", "c", forkFacts(), forkOpts()); err != nil {
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
	var doc struct {
		Models map[string]map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if string(doc.Models["c"]["future_role_field"]) == "" {
		t.Fatal("chained fork lost the unknown member")
	}
}

// A source born in-draft has no raw continuity: fork succeeds with no seed.
func TestForkRoleModelFromInDraftRole(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	if err := d.AddRoleModel("fresh", facts, eligibleAddOpts()); err != nil {
		t.Fatal(err)
	}
	if err := d.ForkRoleModel("fresh", "fresh-m", forkFacts(), forkOpts()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.modelRawSeeds["fresh-m"]; ok {
		t.Fatal("seed registered for a source with no raw entry")
	}
}

// A source re-created after in-draft removal has no raw continuity either:
// the tombstone blocks the stale raw entry from seeding the fork.
func TestForkRoleModelTombstonedSource(t *testing.T) {
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
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(), forkOpts()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.modelRawSeeds["agent-m"]; ok {
		t.Fatal("tombstoned source seeded stale raw members")
	}
}

// Removing a forked role deletes its pending seed (mutate diff rule).
func TestRemoveForkedRoleDeletesSeed(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.modelRawSeeds["agent-m"]; !ok {
		t.Fatal("seed not registered")
	}
	if err := d.RemoveRole("agent-m"); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.modelRawSeeds["agent-m"]; ok {
		t.Fatal("seed survived removal")
	}
}

// A removed target name may be reused by a fork in the same draft. The
// tombstone deletes the target's stale raw subtree, then the source seed wins.
func TestForkRoleModelReusedTargetUsesSourceSeed(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.RemoveRole("spare"); err != nil {
		t.Fatal(err)
	}
	if err := d.ForkRoleModel("agent", "spare", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.modelDrops["spare"]; !ok {
		t.Fatal("reused target lost its tombstone")
	}
	if _, ok := d.modelRawSeeds["spare"]; !ok {
		t.Fatal("reused target did not receive the source seed")
	}
	path := d.Origin().Path
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(renderedModelEntries(t, out)["spare"], &fields); err != nil {
		t.Fatal(err)
	}
	if fields["future_role_field"] == nil || fields["stale_spare_field"] != nil {
		t.Fatalf("wrong raw subtree won: %v", fields)
	}
}

// Ordinary pre-publication failure retains a fork seed and the old revision;
// retry still publishes the lossless fork.
func TestForkRoleModelFailedSaveRetry(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	path, revision := d.Origin().Path, d.Revision()
	orig := publishReplaceFn
	t.Cleanup(func() { publishReplaceFn = orig })
	publishReplaceFn = func(string, []byte, string) error {
		return errors.New("injected pre-publication failure")
	}
	err := d.SaveReplace(path, revision)
	publishReplaceFn = orig
	if err == nil || d.Revision() != revision || d.modelRawSeeds["agent-m"] == nil {
		t.Fatalf("failed save consumed pending state: err=%v", err)
	}
	if err := d.SaveReplace(path, revision); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(renderedModelEntries(t, out)["agent-m"], &fields); err != nil {
		t.Fatal(err)
	}
	if fields["future_role_field"] == nil {
		t.Fatal("retry lost fork seed")
	}
}

// The same retry invariant holds for a tombstoned remove/re-add: stale raw
// members remain suppressed after the failed publication and retry.
func TestAddRoleModelFailedSaveRetryDoesNotResurrect(t *testing.T) {
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
	path, revision := d.Origin().Path, d.Revision()
	orig := publishReplaceFn
	t.Cleanup(func() { publishReplaceFn = orig })
	publishReplaceFn = func(string, []byte, string) error {
		return errors.New("injected pre-publication failure")
	}
	err := d.SaveReplace(path, revision)
	publishReplaceFn = orig
	if err == nil || d.Revision() != revision {
		t.Fatalf("failed save changed revision: err=%v", err)
	}
	if _, ok := d.modelDrops["agent"]; !ok {
		t.Fatal("failed save consumed tombstone")
	}
	if err := d.SaveReplace(path, revision); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "future_role_field") {
		t.Fatal("retry resurrected stale raw members")
	}
}

// Byte-stability: fork -> save publishes once; an immediate second save
// publishes nothing (seeds cleared at commit; published bytes canonical).
func TestForkRoleModelByteStability(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	if err := d.ForkRoleModel("agent", "agent-m", forkFacts(),
		forkOpts("slots", "think_tags")); err != nil {
		t.Fatal(err)
	}
	path := d.Origin().Path
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	if len(d.modelRawSeeds) != 0 || len(d.modelDrops) != 0 {
		t.Fatal("commit did not clear model bookkeeping")
	}
	published := 0
	orig := publishReplaceFn
	publishReplaceFn = func(p string, data []byte, rev string) error {
		published++
		return orig(p, data, rev)
	}
	defer func() { publishReplaceFn = orig }()
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	if published != 0 {
		t.Fatalf("no-op save published %d times", published)
	}
}

// ACCEPTANCE GATE (firn-ide#263 Slice B Task 0): omitted authored fields
// — ThinkTags, Slots, Options, Fallbacks, Description, unknown members —
// survive SetRoleOverrides; only capabilities/think_mode change.
func TestSetRoleOverridesLossless(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	var beforeFields map[string]json.RawMessage
	beforeEntry := renderedModelEntries(t, []byte(renderToString(t, d)))["agent"]
	if err := json.Unmarshal(beforeEntry, &beforeFields); err != nil {
		t.Fatal(err)
	}
	sel := provider.ModelKey{Provider: "local", Model: "m1"}
	ov := RoleOverrides{Capabilities: []string{"chat"}, ThinkMode: "toggle"}
	opts := SetRoleOverridesOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
	}
	if err := d.SetRoleOverrides(sel, ov, opts); err != nil {
		t.Fatal(err)
	}
	m := d.authored.Models["agent"]
	if !slices.Equal(m.Capabilities, []string{"chat"}) || m.ThinkMode != "toggle" {
		t.Fatalf("overrides not applied: %+v", m)
	}
	if m.ThinkTags == nil || m.Slots != 2 || m.Options == nil ||
		m.Description != "keep me" || len(m.Fallbacks) != 1 {
		t.Fatalf("omitted fields lost: %+v", m)
	}
	path := d.Origin().Path
	if err := d.SaveReplace(path, d.Revision()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entry map[string]json.RawMessage
	if err := json.Unmarshal(renderedModelEntries(t, out)["agent"], &entry); err != nil {
		t.Fatal(err)
	}
	for key, want := range beforeFields {
		if key == "capabilities" || key == "think_mode" {
			continue
		}
		if !bytes.Equal(entry[key], want) {
			t.Fatalf("published bytes changed omitted field %q: before=%s after=%s",
				key, want, entry[key])
		}
	}
	for key := range entry {
		if key == "capabilities" || key == "think_mode" {
			continue
		}
		if _, existed := beforeFields[key]; !existed {
			t.Fatalf("published bytes added unrelated field %q", key)
		}
	}
	if string(entry["think_mode"]) != `"toggle"` {
		t.Fatalf("think_mode not published: %s", entry["think_mode"])
	}
}

func TestSetRoleOverridesNoOpByteStable(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	before := renderToString(t, d)
	err := d.SetRoleOverrides(
		provider.ModelKey{Provider: "local", Model: "m1"},
		RoleOverrides{Capabilities: []string{"chat", "stream"}, ThinkMode: "always"},
		SetRoleOverridesOpts{
			Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if after := renderToString(t, d); after != before {
		t.Fatalf("no-op override changed canonical bytes:\nbefore=%s\nafter=%s", before, after)
	}
}

// Selector-wide: every matching role gets identical values; clearing uses
// nil capabilities / empty think mode.
func TestSetRoleOverridesSelectorWideAndClear(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	// Put "spare" on agent's selector so two roles match.
	facts := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m1"}, Type: "moe"}
	if _, err := d.SetRoleModel("spare", facts, SetRoleModelOpts{ConfirmUnknown: true}); err != nil {
		t.Fatal(err)
	}
	sel := provider.ModelKey{Provider: "local", Model: "m1"}
	ov := RoleOverrides{Capabilities: []string{"chat", "stream"}, ThinkMode: "auto"}
	opts := SetRoleOverridesOpts{ConfirmUnknown: true}
	if err := d.SetRoleOverrides(sel, ov, opts); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"agent", "spare"} {
		m := d.authored.Models[role]
		if !slices.Equal(m.Capabilities, []string{"chat", "stream"}) || m.ThinkMode != "auto" {
			t.Fatalf("%s: not uniform: %+v", role, m)
		}
	}
	// Clear both.
	if err := d.SetRoleOverrides(sel, RoleOverrides{}, opts); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"agent", "spare"} {
		m := d.authored.Models[role]
		if m.Capabilities != nil || m.ThinkMode != "" {
			t.Fatalf("%s: not cleared: %+v", role, m)
		}
		if role == "agent" && (m.ThinkTags == nil || m.Slots != 2) {
			t.Fatalf("clear touched omitted fields: %+v", m)
		}
	}
}

func TestSetRoleOverridesRefusals(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	opts := SetRoleOverridesOpts{ConfirmUnknown: true}
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{}, RoleOverrides{}, opts),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "local", Model: "m1"},
		RoleOverrides{Capabilities: []string{}}, opts),
		CodeInvalidArgument, SubjectNone, "")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "local", Model: "nope"},
		RoleOverrides{}, opts), CodeRoleNotFound, SubjectNone, "")
	assertDiag(t, d.SetRoleOverrides(provider.ModelKey{Provider: "local", Model: "m1"},
		RoleOverrides{Capabilities: []string{"bogus"}}, opts),
		CodeModelInvalid, SubjectRole, "agent")
}

// Eligibility: setting an override that removes a required capability from
// a routed selector refuses; clearing falls back to opts facts.
func TestSetRoleOverridesEligibility(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	sel := provider.ModelKey{Provider: "local", Model: "m1"}
	opts := SetRoleOverridesOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapToolCall},
	}
	assertDiag(t, d.SetRoleOverrides(sel, RoleOverrides{Capabilities: []string{"chat"}}, opts),
		CodeEligibilityIneligible, SubjectRole, "agent")
	// Clearing with unknown facts: requires ConfirmUnknown.
	clearOpts := SetRoleOverridesOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
	}
	assertDiag(t, d.SetRoleOverrides(sel, RoleOverrides{}, clearOpts),
		CodeEligibilityUnknown, SubjectRole, "agent")
	clearOpts.Caps = provider.CapChat | provider.CapStream
	clearOpts.KnownMask = provider.CapChat | provider.CapStream
	if err := d.SetRoleOverrides(sel, RoleOverrides{}, clearOpts); err != nil {
		t.Fatal(err)
	}
}

func TestSetRoleOverridesFinalizeFailureTransactional(t *testing.T) {
	d := loadTestDoc(t, roleFixture)
	beforeEffective := d.Config()
	beforeRender := renderToString(t, d)
	err := d.SetRoleOverrides(
		provider.ModelKey{Provider: "local", Model: "m1"},
		RoleOverrides{Capabilities: []string{"chat", "stream"}, ThinkMode: "sometimes"},
		SetRoleOverridesOpts{
			Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		},
	)
	assertDiag(t, err, CodeThinkInvalid, SubjectRole, "agent")
	if !reflect.DeepEqual(beforeEffective, d.Config()) || beforeRender != renderToString(t, d) {
		t.Fatal("finalize refusal changed the document")
	}
}
