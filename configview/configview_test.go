package configview

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func testConfig() *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"local": {BaseURL: "http://user:pass@localhost:8080/v1?key=zzz", APIFormat: "openai-compat", APIKey: "expanded-secret"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Name: "m1", Provider: "local", Type: "dense", Fallbacks: []string{"fast"}},
			"fast":  {Name: "m2", Provider: "local", Type: "dense"},
			// explicit caps: omissions are DEFINITIVE (no tool_call → known-absent)
			"declared": {Name: "m3", Provider: "local", Type: "dense", Capabilities: []string{"chat", "stream"}},
		},
		Defaults: map[string]string{"chat": "agent", "agent": "agent"},
	}
}

func testInput() BuildInput {
	return BuildInput{
		Doc: DocSnapshot{Config: testConfig(),
			Origin: config.Origin{Source: config.OriginExplicit, Path: "/tmp/x/models.json"}, Revision: "r1"},
		Inventory: Inventory{
			Models: []InventoryModel{{
				Key:  provider.ModelKey{Provider: "local", Model: "m2"},
				Caps: provider.CapChat | provider.CapStream, KnownMask: provider.CapChat | provider.CapStream,
				ProfileSource: "static",
			}},
			Providers: []InventoryProvider{{Name: "local", Reachable: true}},
		},
		Requirements: map[string]provider.Capability{"agent": provider.CapChat | provider.CapStream | provider.CapToolCall},
		Profile:      ProfileState{ActiveID: "user/x", BaselineID: "user/x", Revision: "r1"},
	}
}

func bindingFor(t *testing.T, s Snapshot, uc string) RoleBinding {
	t.Helper()
	for _, b := range s.Bindings {
		if b.UseCase == uc {
			return b
		}
	}
	t.Fatalf("no binding for %q", uc)
	return RoleBinding{}
}

// Resolution walks the chain against inventory: m1 absent, m2 present →
// resolves to the FALLBACK entry.
func TestBuildResolutionWalk(t *testing.T) {
	s := Build(testInput())
	b := bindingFor(t, s, "agent")
	if b.ResolutionState != "resolved" || b.Resolved != "local/m2" || !b.IsFallback {
		t.Fatalf("agent binding = %+v", b)
	}
}

// Zero inventory → resolution unknown, never a false chain[0] claim.
func TestBuildResolutionUnknownWithoutInventory(t *testing.T) {
	in := testInput()
	in.Inventory = Inventory{}
	s := Build(in)
	b := bindingFor(t, s, "agent")
	if b.ResolutionState != "unknown" || b.Resolved != "" || b.IsFallback {
		t.Fatalf("binding = %+v, want unknown/empty", b)
	}
	if want := []string{"local/m1", "local/m2"}; !reflect.DeepEqual(b.Chain, want) {
		t.Fatalf("chain = %v, want %v", b.Chain, want)
	}
}

// A chain whose inventoried entries are all known-unreachable → none-eligible.
func TestBuildResolutionNoneEligible(t *testing.T) {
	in := testInput()
	in.Inventory.Providers = []InventoryProvider{{Name: "local", Reachable: false}}
	s := Build(in)
	b := bindingFor(t, s, "agent")
	if b.ResolutionState != "none-eligible" || b.Resolved != "" {
		t.Fatalf("binding = %+v, want none-eligible", b)
	}
}

// Explicit config capabilities are authoritative: tool_call omitted →
// KNOWN-absent → ineligible. Type-derived stays present-only → unknown.
func TestBuildCapsAuthority(t *testing.T) {
	s := Build(testInput())
	b := bindingFor(t, s, "agent")
	got := map[string]Candidate{}
	for _, c := range b.Candidates {
		got[c.Selector] = c
	}
	if c := got["local/m3"]; c.Eligibility != EligibilityIneligible ||
		!reflect.DeepEqual(c.Reasons, []string{"missing_capability:tool_call"}) {
		t.Fatalf("explicit-caps candidate = %+v, want ineligible/known-absent", c)
	}
	if c := got["local/m1"]; c.Eligibility != EligibilityUnknown ||
		!reflect.DeepEqual(c.Reasons, []string{"capability_unknown:tool_call"}) {
		t.Fatalf("type-derived candidate = %+v, want unknown", c)
	}
}

// Exact sorted sequences — not just determinism.
func TestBuildExactOrdering(t *testing.T) {
	s := Build(testInput())
	var ucs []string
	for _, b := range s.Bindings {
		ucs = append(ucs, b.UseCase)
	}
	if want := []string{"agent", "chat"}; !reflect.DeepEqual(ucs, want) {
		t.Fatalf("binding order = %v, want %v", ucs, want)
	}
	b := bindingFor(t, s, "agent")
	var sels []string
	for _, c := range b.Candidates {
		sels = append(sels, c.Selector)
	}
	if want := []string{"local/m1", "local/m2", "local/m3"}; !reflect.DeepEqual(sels, want) {
		t.Fatalf("candidate order = %v, want %v", sels, want)
	}
	var models []string
	for _, m := range s.Models {
		models = append(models, m.Selector)
	}
	if want := []string{"local/m1", "local/m2", "local/m3"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("model order = %v, want %v", models, want)
	}
}

// Two roles sharing a selector with different non-empty types → Type "" and
// a selector_type_conflict diagnostic.
func TestBuildSelectorTypeConflict(t *testing.T) {
	in := testInput()
	cfg := in.Doc.Config
	m := cfg.Models["fast"]
	m.Name = "m1" // now agent and fast both point at local/m1...
	m.Type = "moe" // ...with disagreeing types
	cfg.Models["fast"] = m
	s := Build(in)
	found := false
	for _, d := range s.Diagnostics {
		if d.Code == "selector_type_conflict" && d.Subject == "local/m1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no selector_type_conflict diagnostic: %+v", s.Diagnostics)
	}
	for _, ms := range s.Models {
		if ms.Selector == "local/m1" && ms.Type != "" {
			t.Fatalf("conflicted selector kept type %q", ms.Type)
		}
	}
}

// Projection hygiene: never echo secrets, credentials, or paths.
// Failure messages deliberately do NOT include the serialized snapshot.
func TestBuildProjectionHygiene(t *testing.T) {
	s := Build(testInput())
	if got := s.Providers[0].Endpoint; got != "http://localhost:8080" {
		t.Fatalf("endpoint = %q", got)
	}
	if !s.Providers[0].HasKey {
		t.Fatal("HasKey should be true")
	}
	if s.Origin.Source != "explicit_path" {
		t.Fatalf("origin wire = %q", s.Origin.Source)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"expanded-secret", "user:pass", "key=zzz", "/tmp/x"} {
		if bytes.Contains(raw, []byte(banned)) {
			t.Fatalf("snapshot JSON leaks a banned token (offset %d)", bytes.Index(raw, []byte(banned)))
		}
	}
}

func TestBuildDeterministic(t *testing.T) {
	a, b := Build(testInput()), Build(testInput())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Build not deterministic")
	}
}

// Golden v1 wire contract, shared by CLI and MCP consumers: field names,
// empty/null behavior, ordering. Any diff = deliberate contract change → v2.
func TestSnapshotGoldenContract(t *testing.T) {
	in := BuildInput{
		Doc: DocSnapshot{Config: &config.Config{
			Providers: map[string]config.ProviderConfig{"p": {BaseURL: "http://h:1", APIFormat: "openai-compat"}},
			Models:    map[string]config.ModelConfig{"agent": {Name: "m", Provider: "p", Type: "dense"}},
			Defaults:  map[string]string{"agent": "agent"},
		}, Origin: config.Origin{Source: config.OriginProgrammatic}},
		Requirements: map[string]provider.Capability{"agent": provider.CapChat},
	}
	got, err := json.MarshalIndent(Build(in), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != snapshotGoldenV1 {
		t.Fatalf("v1 wire contract drifted:\n--- got ---\n%s\n--- want ---\n%s", got, snapshotGoldenV1)
	}
}

// snapshotGoldenV1 was captured from the first green run, hand-reviewed for
// field names, omitted empties, and null semantics, then FROZEN.
const snapshotGoldenV1 = `{
  "ready": true,
  "origin": {
    "source": "programmatic"
  },
  "bindings": [
    {
      "use_case": "agent",
      "role": "agent",
      "resolution_state": "unknown",
      "is_fallback": false,
      "chain": [
        "p/m"
      ],
      "candidates": [
        {
          "selector": "p/m",
          "eligibility": "eligible"
        }
      ],
      "source": "config-default"
    }
  ],
  "models": [
    {
      "selector": "p/m",
      "provider": "p",
      "type": "dense",
      "caps": [
        "chat",
        "generate",
        "stream"
      ],
      "profile_source": "config"
    }
  ],
  "providers": [
    {
      "name": "p",
      "endpoint": "http://h:1",
      "api_format": "openai-compat",
      "has_key": false,
      "slot_discovery": false
    }
  ],
  "profile": {
    "dirty": false
  }
}`
