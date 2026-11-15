package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// policyConfig is a config whose provider "ollama" speaks format and whose
// roles are models.
func policyConfig(format string, models map[string]ModelConfig) *Config {
	return &Config{
		Providers: map[string]ProviderConfig{"ollama": {BaseURL: "http://127.0.0.1:11434", APIFormat: format}},
		Models:    models,
	}
}

func mustPolicy(t *testing.T, c *Config, providerName, model string) ModelCapabilityPolicy {
	t.Helper()
	p, err := c.CapabilityPolicy(providerName, model)
	if err != nil {
		t.Fatalf("CapabilityPolicy(%q, %q) error = %v", providerName, model, err)
	}
	return p
}

func TestCapabilityPolicy_LaterRoleExplicitListWins(t *testing.T) {
	// The first role in sorted order declares nothing; the later one declares
	// chat and stream. Bootstrap takes the later list as the override, so the
	// policy must too.
	c := policyConfig("openai-compat", map[string]ModelConfig{
		"a-chat":  {Provider: "ollama", Name: "shared", Type: "dense"},
		"b-tools": {Provider: "ollama", Name: "shared", Type: "dense", Capabilities: []string{"chat", "stream"}},
	})
	got := mustPolicy(t, c, "ollama", "shared")
	want := ModelCapabilityPolicy{
		Authored:  true,
		Explicit:  []string{"chat", "stream"},
		Derived:   []string{"chat", "generate", "stream"},
		TypeNames: []string{"dense"},
		TypeCaps:  provider.CapChat | provider.CapGenerate | provider.CapStream,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v; want %+v", got, want)
	}
}

func TestCapabilityPolicy_EquivalentListsAgree(t *testing.T) {
	// Reordered and repeated tokens parse to the same mask: no conflict, and
	// the first list in sorted role order is the one kept.
	c := policyConfig("", map[string]ModelConfig{
		"a": {Provider: "ollama", Name: "shared", Type: "dense", Capabilities: []string{"stream", "chat"}},
		"b": {Provider: "ollama", Name: "shared", Type: "dense", Capabilities: []string{"chat", "stream", "chat"}},
	})
	got := mustPolicy(t, c, "ollama", "shared")
	if !reflect.DeepEqual(got.Explicit, []string{"stream", "chat"}) {
		t.Fatalf("Explicit = %v; want [stream chat]", got.Explicit)
	}
	if got.Derived != nil || got.TypeNames != nil || got.TypeCaps != 0 {
		t.Fatalf("roles with a list contributed to the type tier: %+v", got)
	}
}

func TestCapabilityPolicy_InvalidListIsError(t *testing.T) {
	c := policyConfig("", map[string]ModelConfig{
		"a": {Provider: "ollama", Name: "m", Type: "dense", Capabilities: []string{"completion"}},
	})
	_, parseErr := provider.ParseCapsStrict([]string{"completion"})
	if parseErr == nil {
		t.Fatal("ParseCapsStrict accepted completion; the fixture is wrong")
	}
	got, err := c.CapabilityPolicy("ollama", "m")
	if err == nil {
		t.Fatalf("CapabilityPolicy = %+v, nil; want the parse error", got)
	}
	if !strings.Contains(err.Error(), parseErr.Error()) {
		t.Fatalf("error = %q; want it to carry %q", err, parseErr)
	}
	if !reflect.DeepEqual(got, ModelCapabilityPolicy{}) {
		t.Fatalf("policy beside the error = %+v; want zero", got)
	}
}

func TestCapabilityPolicy_ConflictMessage(t *testing.T) {
	c := policyConfig("", map[string]ModelConfig{
		"chat":  {Provider: "ollama", Name: "shared", Type: "dense", Capabilities: []string{"chat"}},
		"tools": {Provider: "ollama", Name: "shared", Type: "dense", Capabilities: []string{"tool_call"}},
	})
	got, err := c.CapabilityPolicy("ollama", "shared")
	if err == nil {
		t.Fatalf("CapabilityPolicy = %+v, nil; want a conflict", got)
	}
	// The substring mcp/server_test.go and config/document_mutate_test.go match.
	if !strings.Contains(err.Error(), "conflicting capability overrides for ollama/shared") {
		t.Fatalf("error = %q; want the conflicting-overrides message", err)
	}
}

func TestCapabilityPolicy_SharedSelectorUnionsTypes(t *testing.T) {
	c := policyConfig("openai-compat", map[string]ModelConfig{
		"chat":  {Provider: "ollama", Name: "hybrid", Type: "dense"},
		"embed": {Provider: "ollama", Name: "hybrid", Type: "embedding"},
		"other": {Provider: "ollama", Name: "else", Type: "moe"},
	})
	got := mustPolicy(t, c, "ollama", "hybrid")
	want := ModelCapabilityPolicy{
		Authored:  true,
		Derived:   []string{"chat", "embed", "generate", "stream"},
		TypeNames: []string{"dense", "embedding"},
		TypeCaps:  provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapEmbed,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %+v; want %+v", got, want)
	}
}

func TestCapabilityPolicy_EmptyProviderNeverMatches(t *testing.T) {
	// The builders skip a role with an empty provider or name; config.Load
	// fills an empty provider, so only a programmatic config has one.
	c := policyConfig("openai-compat", map[string]ModelConfig{
		"noprov": {Name: "m", Type: "dense", Capabilities: []string{"chat"}},
		"noname": {Provider: "ollama", Type: "dense", Capabilities: []string{"chat"}},
	})
	for _, sel := range [][2]string{{"", "m"}, {"ollama", "m"}, {"ollama", ""}} {
		if got := mustPolicy(t, c, sel[0], sel[1]); !reflect.DeepEqual(got, ModelCapabilityPolicy{}) {
			t.Errorf("CapabilityPolicy(%q, %q) = %+v; want unauthored", sel[0], sel[1], got)
		}
	}
}

func TestCapabilityPolicy_OllamaHasNoTypeCaps(t *testing.T) {
	for _, format := range []string{"", "ollama"} {
		c := policyConfig(format, map[string]ModelConfig{
			"chat": {Provider: "ollama", Name: "m", Type: "dense"},
		})
		got := mustPolicy(t, c, "ollama", "m")
		if got.TypeCaps != 0 {
			t.Errorf("format %q: TypeCaps = %v; want 0", format, got.TypeCaps)
		}
		if !reflect.DeepEqual(got.Derived, []string{"chat", "generate", "stream"}) || !reflect.DeepEqual(got.TypeNames, []string{"dense"}) {
			t.Errorf("format %q: Derived = %v, TypeNames = %v; want the dense set", format, got.Derived, got.TypeNames)
		}
	}
}

func TestCapabilityPolicy_NoAliasFallback(t *testing.T) {
	// Config-derived lookups are exact-spelling, as routing is.
	c := policyConfig("", map[string]ModelConfig{
		"chat": {Provider: "ollama", Name: "qwen3", Type: "dense", Capabilities: []string{"chat"}},
	})
	if got := mustPolicy(t, c, "ollama", "qwen3:latest"); !reflect.DeepEqual(got, ModelCapabilityPolicy{}) {
		t.Fatalf("policy for qwen3:latest = %+v; want unauthored", got)
	}
	if got := mustPolicy(t, c, "ollama", "qwen3"); !got.Authored || len(got.Explicit) != 1 {
		t.Fatalf("policy for qwen3 = %+v; want the configured list", got)
	}
}

func TestCapabilityPolicy_ReturnsCopies(t *testing.T) {
	c := policyConfig("openai-compat", map[string]ModelConfig{
		"a": {Provider: "ollama", Name: "m", Type: "dense", Capabilities: []string{"chat", "stream"}},
		"b": {Provider: "ollama", Name: "d", Type: "dense"},
	})
	explicit := mustPolicy(t, c, "ollama", "m")
	derived := mustPolicy(t, c, "ollama", "d")
	explicit.Explicit[0] = "mutated"
	derived.Derived[0] = "mutated"
	derived.TypeNames[0] = "mutated"
	if got := c.Models["a"].Capabilities; !reflect.DeepEqual(got, []string{"chat", "stream"}) {
		t.Fatalf("config list after mutating Explicit = %v", got)
	}
	if got := mustPolicy(t, c, "ollama", "m").Explicit; !reflect.DeepEqual(got, []string{"chat", "stream"}) {
		t.Fatalf("Explicit on a second call = %v", got)
	}
	again := mustPolicy(t, c, "ollama", "d")
	if !reflect.DeepEqual(again.Derived, []string{"chat", "generate", "stream"}) || !reflect.DeepEqual(again.TypeNames, []string{"dense"}) {
		t.Fatalf("second call = %+v; want fresh slices", again)
	}
	// The derived table itself must not have been written through.
	if got := (ModelConfig{Type: "dense"}).ResolvedCapabilities(); !reflect.DeepEqual(got, []string{"chat", "generate", "stream"}) {
		t.Fatalf("dense derived set = %v", got)
	}
}

func TestCapabilityPolicy_NilConfig(t *testing.T) {
	var c *Config
	if got := mustPolicy(t, c, "ollama", "m"); !reflect.DeepEqual(got, ModelCapabilityPolicy{}) {
		t.Fatalf("nil config policy = %+v; want zero", got)
	}
}
