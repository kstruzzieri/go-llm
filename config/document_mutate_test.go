package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// setRoleFixture carries distinct nonzero old values on EVERY field the
// preservation table touches, so clearing and replacement are each provable.
const setRoleFixture = `{
  "providers": {"local": {"base_url": "http://localhost:1"}},
  "models": {
    "agent": {"name": "m1", "provider": "local", "type": "moe",
      "description": "keep me", "parameters": "30B", "context_window": 32768,
      "capabilities": ["chat", "stream"], "fallbacks": ["fast"],
      "options": {"temperature": 0.2}, "slots": 2,
      "think_mode": "always", "think_tags": {"open": "<t>", "close": "</t>"}},
    "fast": {"name": "m2", "provider": "local", "type": "dense"}
  },
  "defaults": {"agent": "agent"}
}`

func agentFacts() ModelFacts {
	return ModelFacts{
		Key:           provider.ModelKey{Provider: "local", Model: "m9"},
		Type:          "dense",
		Parameters:    "9B",
		ContextWindow: 8192,
		Dimensions:    0,
	}
}

func eligibleOpts() SetRoleModelOpts {
	return SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		Caps:         provider.CapChat | provider.CapStream,
		KnownMask:    provider.CapChat | provider.CapStream,
	}
}

// (a) Preservation table. REPLACED from facts: Name, Provider, Type,
// Parameters, ContextWindow, Dimensions (replace includes replacing WITH
// zero when facts say so — never stale). PRESERVED: Fallbacks, Description,
// Options. CLEARED unless re-asserted: Capabilities, ThinkMode, ThinkTags,
// Slots.
func TestSetRoleModelPreservationTable(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	before := d.authored.Models["agent"]
	if _, err := d.SetRoleModel("agent", agentFacts(), eligibleOpts()); err != nil {
		t.Fatal(err)
	}
	m := d.authored.Models["agent"]
	if m.Name != "m9" || m.Provider != "local" || m.Type != "dense" ||
		m.Parameters != "9B" || m.ContextWindow != 8192 || m.Dimensions != 0 {
		t.Fatalf("identity/capacity not replaced from facts: %+v", m)
	}
	if !reflect.DeepEqual(m.Fallbacks, before.Fallbacks) ||
		m.Description != "keep me" ||
		!reflect.DeepEqual(m.Options, before.Options) {
		t.Fatalf("role-level intent not preserved: %+v", m)
	}
	if m.Capabilities != nil || m.ThinkMode != "" || m.ThinkTags != nil || m.Slots != 0 {
		t.Fatalf("caps/think/slots not cleared: %+v", m)
	}
}

// Dimensions is REPLACE semantics, not clear: nonzero facts persist.
func TestSetRoleModelReplacesDimensions(t *testing.T) {
	body := `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{"emb":{"name":"e1","provider":"local","type":"embedding","dimensions":768}},
	  "defaults":{"embedding":"emb"}}`
	d := loadTestDoc(t, body)
	f := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "e2"}, Type: "embedding", Dimensions: 1024}
	o := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"embedding": provider.CapEmbed},
		Caps:         provider.CapEmbed, KnownMask: provider.CapEmbed,
	}
	if _, err := d.SetRoleModel("emb", f, o); err != nil {
		t.Fatal(err)
	}
	if got := d.authored.Models["emb"].Dimensions; got != 1024 {
		t.Fatalf("dimensions = %d, want 1024 (replaced from facts)", got)
	}
}

// (b) Type is REQUIRED (spec amendment 1).
func TestSetRoleModelRequiresType(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	f := agentFacts()
	f.Type = ""
	if _, err := d.SetRoleModel("agent", f, eligibleOpts()); err == nil ||
		!strings.Contains(err.Error(), "type") {
		t.Fatalf("err = %v, want type-required", err)
	}
}

// (c) P0 CARVE-DOWN: when opts.Capabilities is supplied, the gate evaluates
// THAT final set (REPLACE semantics, omissions definitive) — discovered
// facts cannot launder an override that omits a required capability.
func TestSetRoleModelCarveDownGate(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	o := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat | provider.CapToolCall},
		Caps:         provider.CapChat | provider.CapStream | provider.CapToolCall,
		KnownMask:    provider.CapChat | provider.CapStream | provider.CapToolCall,
		Capabilities: []string{"chat"}, // override carves tool_call OUT
	}
	_, err := d.SetRoleModel("agent", agentFacts(), o)
	if err == nil || !strings.Contains(err.Error(), "missing_capability:tool_call") {
		t.Fatalf("err = %v, want ineligible from the FINAL override set", err)
	}
	// Inverse: an override that ADDS the requirement is eligible even when
	// facts were unknown.
	o2 := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat | provider.CapToolCall},
		Capabilities: []string{"chat", "stream", "tool_call"},
	}
	if _, err := d.SetRoleModel("agent", agentFacts(), o2); err != nil {
		t.Fatalf("explicit override declaring the requirement must be eligible: %v", err)
	}
}

// (d) Aggregation: per-use-case verdicts over fallback-chain membership; a
// MISSING requirement for an affected use case counts as unknown and cannot
// be masked by another known-satisfied requirement.
func TestSetRoleModelAggregationMissingRequirementIsUnknown(t *testing.T) {
	body := `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{
	    "general":{"name":"g1","provider":"local","type":"dense","fallbacks":["agent"]},
	    "agent":{"name":"m1","provider":"local","type":"dense"}},
	  "defaults":{"chat":"general","agent":"agent"}}`
	d := loadTestDoc(t, body)
	o := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		Caps:         provider.CapChat, KnownMask: provider.CapChat,
	}
	if _, err := d.SetRoleModel("agent", agentFacts(), o); err == nil {
		t.Fatal("missing requirement for affected use case must force unknown/confirm")
	}
	o.ConfirmUnknown = true
	if _, err := d.SetRoleModel("agent", agentFacts(), o); err != nil {
		t.Fatalf("ConfirmUnknown must clear the missing-requirement unknown: %v", err)
	}
}

// Known-ineligible on ANY affected use case rejects even with ConfirmUnknown.
func TestSetRoleModelAggregationIneligibleWins(t *testing.T) {
	body := `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{
	    "general":{"name":"g1","provider":"local","type":"dense","fallbacks":["agent"]},
	    "agent":{"name":"m1","provider":"local","type":"dense"}},
	  "defaults":{"chat":"general","agent":"agent"}}`
	d := loadTestDoc(t, body)
	o := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{
			"chat":  provider.CapChat | provider.CapThinking,
			"agent": provider.CapChat,
		},
		Caps:           provider.CapChat,
		KnownMask:      provider.CapChat | provider.CapThinking,
		ConfirmUnknown: true,
	}
	_, err := d.SetRoleModel("agent", agentFacts(), o)
	if err == nil || !strings.Contains(err.Error(), "missing_capability:thinking") {
		t.Fatalf("err = %v, want chain-aggregated ineligibility", err)
	}
}

// (amendment 5) A role no default/fallback chain references aggregates to
// unknown — never vacuously eligible.
func TestSetRoleModelZeroAffectedUseCasesIsUnknown(t *testing.T) {
	body := `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{"orphan":{"name":"m1","provider":"local","type":"dense"},
	            "agent":{"name":"m2","provider":"local","type":"dense"}},
	  "defaults":{"agent":"agent"}}`
	d := loadTestDoc(t, body)
	o := SetRoleModelOpts{
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
		Caps:         provider.CapChat, KnownMask: provider.CapChat,
	}
	f := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m9"}, Type: "dense"}
	if _, err := d.SetRoleModel("orphan", f, o); err == nil {
		t.Fatal("zero affected use cases must be unknown (needs ConfirmUnknown)")
	}
	o.ConfirmUnknown = true
	if _, err := d.SetRoleModel("orphan", f, o); err != nil {
		t.Fatalf("ConfirmUnknown must clear it: %v", err)
	}
}

// (e) Conflicts are checked on the FINALIZED representation and reject
// without mutating the document.
func TestSetRoleModelSelectorConflictsFinalized(t *testing.T) {
	body := `{"providers":{"local":{"base_url":"http://localhost:1"}},
	  "models":{
	    "agent":{"name":"m1","provider":"local","type":"dense","options":{"temperature":0.2}},
	    "fast":{"name":"m2","provider":"local","type":"dense","options":{"temperature":0.9}}},
	  "defaults":{"agent":"agent"}}`
	d := loadTestDoc(t, body)
	before := d.authored.clone()
	_, err := d.SetRoleModel("agent",
		ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m2"}, Type: "dense"},
		SetRoleModelOpts{ConfirmUnknown: true})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v, want conflict", err)
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("rejected retarget mutated the document")
	}
}

// Implicit-provider defaulting: the sibling's provider is materialized to
// "ollama" only at finalize — an authored-level check would miss this
// collision entirely.
func TestSetRoleModelConflictSeesImplicitProvider(t *testing.T) {
	body := `{"providers":{"ollama":{"base_url":"http://localhost:11434"}},
	  "models":{
	    "agent":{"name":"m1","provider":"ollama","type":"dense","options":{"temperature":0.2}},
	    "fast":{"name":"m2","type":"dense","options":{"temperature":0.9}}},
	  "defaults":{"agent":"agent"}}`
	d := loadTestDoc(t, body)
	_, err := d.SetRoleModel("agent",
		ModelFacts{Key: provider.ModelKey{Provider: "ollama", Model: "m2"}, Type: "dense"},
		SetRoleModelOpts{ConfirmUnknown: true})
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err = %v, want conflict against implicit-ollama sibling", err)
	}
}

// Behavioral conflict coverage per invariant, plus equal-value passes.
func TestSetRoleModelConflictMatrix(t *testing.T) {
	mk := func(extraA, extraB string) string {
		return `{"providers":{"local":{"base_url":"http://localhost:1"}},
		  "models":{
		    "agent":{"name":"m1","provider":"local","type":"dense"` + extraA + `},
		    "fast":{"name":"m2","provider":"local","type":"dense"` + extraB + `}},
		  "defaults":{"agent":"agent"}}`
	}
	cases := []struct {
		name           string
		extraA, extraB string
		wantConflict   bool
	}{
		{"context_window differs", `,"context_window":4096`, `,"context_window":8192`, true},
		{"context_window equal", `,"context_window":4096`, `,"context_window":4096`, false},
		{"capabilities differ", `,"capabilities":["chat"]`, `,"capabilities":["chat","stream"]`, true},
		{"capabilities equal as sets", `,"capabilities":["stream","chat"]`, `,"capabilities":["chat","stream"]`, false},
		{"think_mode differs", `,"think_mode":"always"`, `,"think_mode":"none"`, true},
		{"think_tags differ", `,"think_tags":{"open":"<a>","close":"</a>"}`, `,"think_tags":{"open":"<b>","close":"</b>"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := loadTestDoc(t, mk(tc.extraA, tc.extraB))
			opts := SetRoleModelOpts{ConfirmUnknown: true}
			f := ModelFacts{Key: provider.ModelKey{Provider: "local", Model: "m2"}, Type: "dense"}
			if strings.Contains(tc.extraA, "context_window") {
				f.ContextWindow = 4096
			}
			if strings.Contains(tc.extraA, "capabilities") {
				opts.Capabilities = []string{"chat"}
				if !tc.wantConflict {
					opts.Capabilities = []string{"stream", "chat"}
				}
			}
			if strings.Contains(tc.extraA, "think_mode") {
				opts.ThinkMode = "always"
			}
			if strings.Contains(tc.extraA, "think_tags") {
				opts.ThinkTags = &ThinkTagsConfig{Open: "<a>", Close: "</a>"}
			}
			_, err := d.SetRoleModel("agent", f, opts)
			if tc.wantConflict && (err == nil || !strings.Contains(err.Error(), "conflicting")) {
				t.Fatalf("err = %v, want conflict", err)
			}
			if !tc.wantConflict && err != nil {
				t.Fatalf("equal values must not conflict: %v", err)
			}
		})
	}
}

// (f) Result populated ONLY on success; per-field think drops.
func TestSetRoleModelResultOnlyOnSuccess(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	res, err := d.SetRoleModel("agent", agentFacts(), SetRoleModelOpts{}) // unknown, unconfirmed
	if err == nil {
		t.Fatal("want rejection")
	}
	if res != (SetRoleModelResult{}) {
		t.Fatalf("rejected call reported outcomes: %+v", res)
	}
	res, err = d.SetRoleModel("agent", agentFacts(), SetRoleModelOpts{ConfirmUnknown: true, ThinkMode: "always"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DroppedCapabilityOverride || res.DroppedThinkMode || !res.DroppedThinkTags || !res.DroppedSlots {
		t.Fatalf("per-field drop reporting wrong: %+v", res)
	}
}

// (g) Neither opts.ThinkTags nor opts.Capabilities may alias caller memory.
func TestSetRoleModelClonesCallerInputs(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	tags := &ThinkTagsConfig{Open: "<x>", Close: "</x>"}
	caps := []string{"chat", "stream"}
	o := SetRoleModelOpts{ConfirmUnknown: true, ThinkTags: tags, Capabilities: caps}
	if _, err := d.SetRoleModel("agent", agentFacts(), o); err != nil {
		t.Fatal(err)
	}
	tags.Open = "hacked"
	caps[0] = "hacked"
	m := d.authored.Models["agent"]
	if m.ThinkTags.Open != "<x>" || m.Capabilities[0] != "chat" {
		t.Fatal("document aliases caller memory")
	}
}

// (h) Facts failing shared validation (negative window) roll back cleanly.
func TestSetRoleModelNegativeWindowRollsBack(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	before := d.authored.clone()
	f := agentFacts()
	f.ContextWindow = -1
	if _, err := d.SetRoleModel("agent", f, eligibleOpts()); err == nil {
		t.Fatal("negative context_window must fail")
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("failed mutation leaked")
	}
}

// (i) Concurrent BindUseCase racing SetRoleModel is race-free; the gate
// always evaluates against in-lock state (run under -race).
func TestSetRoleModelConcurrentWithBind(t *testing.T) {
	d := loadTestDoc(t, setRoleFixture)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = d.BindUseCase("chat", "fast")
			_ = d.BindUseCase("chat", "agent")
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = d.SetRoleModel("agent", agentFacts(), SetRoleModelOpts{ConfirmUnknown: true})
	}
	<-done
}

var errTestVeto = errors.New("test veto")

func canonicalOf(t *testing.T, d *Document) []byte {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Failure at the FINALIZE stage (not argument validation) must roll back
// everything: authored, effective, revision, origin.
func TestMutateRollsBackOnFinalizeFailure(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	beforeAuthored := d.authored.clone()
	beforeEffective := d.effective.clone()
	beforeRev, beforeOrigin := d.Revision(), d.Origin()

	err := d.mutate(func(a *Config) error {
		a.Defaults["chat"] = "ghost"
		return nil
	}, nil)
	if err == nil {
		t.Fatal("want finalize failure")
	}
	if !reflect.DeepEqual(d.authored, beforeAuthored) || !reflect.DeepEqual(d.effective, beforeEffective) {
		t.Fatal("finalize failure leaked state")
	}
	if d.Revision() != beforeRev || d.Origin() != beforeOrigin {
		t.Fatal("finalize failure changed revision/origin")
	}
}

// The post-finalize hook sees the FINALIZED effective candidate and can veto.
func TestMutatePostHookVetoRollsBack(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	before := d.authored.clone()
	err := d.mutate(
		func(a *Config) error { a.Defaults["agent"] = "fast"; return nil },
		func(effective *Config) error { return errTestVeto },
	)
	if err == nil || !strings.Contains(err.Error(), "veto") {
		t.Fatalf("err = %v, want veto", err)
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("vetoed mutation leaked")
	}
}

func TestBindUseCaseRederivesEffective(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	if err := d.BindUseCase("agent", "fast"); err != nil {
		t.Fatal(err)
	}
	if got := d.Config().Defaults["agent"]; got != "fast" {
		t.Fatalf("effective defaults[agent] = %q, want fast", got)
	}
	if !strings.Contains(string(canonicalOf(t, d)), `"agent": "fast"`) {
		t.Fatal("canonical bytes missing the bound role")
	}
}

func TestBindUseCaseMissingRole(t *testing.T) {
	d := loadTestDoc(t, docNestedConfig)
	before := d.authored.clone()
	if err := d.BindUseCase("agent", "ghost"); err == nil {
		t.Fatal("want error")
	}
	if !reflect.DeepEqual(d.authored, before) {
		t.Fatal("failed bind mutated authored")
	}
}
