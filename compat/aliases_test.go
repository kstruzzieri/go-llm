package compat

import (
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestResolveModel_PassThroughWhenNoAlias(t *testing.T) {
	got, err := resolveModel("qwen3:8b", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Provider != "" || got.Model != "qwen3:8b" {
		t.Errorf("got %+v, want {Provider:'' Model:'qwen3:8b'}", got)
	}
}

func TestResolveModel_QualifiedInput(t *testing.T) {
	got, err := resolveModel("ollama/qwen3:8b", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Provider != "ollama" || got.Model != "qwen3:8b" {
		t.Errorf("got %+v, want ollama/qwen3:8b", got)
	}
}

func TestResolveModel_UnqualifiedAlias(t *testing.T) {
	aliases := map[string]string{"gpt-4": "qwen3-coder-next:latest"}
	got, err := resolveModel("gpt-4", aliases)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Provider != "" || got.Model != "qwen3-coder-next:latest" {
		t.Errorf("got %+v", got)
	}
}

func TestResolveModel_QualifiedAlias(t *testing.T) {
	aliases := map[string]string{"gpt-4": "ollama/qwen3-coder-next:latest"}
	got, err := resolveModel("gpt-4", aliases)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Provider != "ollama" || got.Model != "qwen3-coder-next:latest" {
		t.Errorf("got %+v", got)
	}
}

func TestResolveModel_EmptyInput(t *testing.T) {
	if _, err := resolveModel("", nil); err == nil {
		t.Fatal("empty input must error")
	}
}

func TestResolveModel_WhitespaceInput(t *testing.T) {
	// Regression guard: clients (and OpenAI SDKs) occasionally send stray
	// whitespace. Trim and reject rather than producing {Model:" "} which
	// the router has no way to match.
	for _, in := range []string{" ", "\t", "\n", "   "} {
		if _, err := resolveModel(in, nil); err == nil {
			t.Errorf("resolveModel(%q) = nil err, want error", in)
		}
	}
}

func TestResolveModel_MalformedSlash(t *testing.T) {
	// Regression guard for the parseKey edges: inputs that produce an empty
	// Model field must fail fast with a clear error, not get smuggled to the
	// router and resurface as "model not found".
	for _, in := range []string{"/", "ollama/", "  ollama/  "} {
		if _, err := resolveModel(in, nil); err == nil {
			t.Errorf("resolveModel(%q) = nil err, want malformed-model error", in)
		}
	}
}

func TestResolveModel_LeadingSlashIsUnqualified(t *testing.T) {
	// "/qwen3:8b" is a typo of "qwen3:8b" — both should route the same way
	// (empty Provider, Model preserved). Only empty Model is fatal.
	got, err := resolveModel("/qwen3:8b", nil)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Provider != "" || got.Model != "qwen3:8b" {
		t.Errorf("got %+v, want {Provider:'' Model:'qwen3:8b'}", got)
	}
}

func TestResolveModel_AliasLookupIsSingleLevel(t *testing.T) {
	// Locks in the documented contract: alias targets are NOT recursively
	// resolved. If someone later adds a loop, this test fails and forces a
	// conscious decision (plus cycle-detection) rather than silent breakage.
	aliases := map[string]string{
		"gpt-4":       "gpt-4-turbo",
		"gpt-4-turbo": "qwen3-coder-next:latest",
	}
	got, err := resolveModel("gpt-4", aliases)
	if err != nil {
		t.Fatalf("resolveModel: %v", err)
	}
	if got.Model != "gpt-4-turbo" {
		t.Errorf("got Model=%q, want %q (single-level lookup)", got.Model, "gpt-4-turbo")
	}
}

func TestRecommendedAliases_ContainsCoreMappings(t *testing.T) {
	m := RecommendedAliases()
	for _, want := range []string{"gpt-4", "gpt-4o-mini", "gpt-3.5-turbo"} {
		if _, ok := m[want]; !ok {
			t.Errorf("RecommendedAliases missing %q", want)
		}
	}
	// Must not be exported nil.
	if m == nil {
		t.Fatal("RecommendedAliases() returned nil")
	}
	// Mutation by caller must not affect future calls.
	m["gpt-4"] = "evil"
	if RecommendedAliases()["gpt-4"] == "evil" {
		t.Error("RecommendedAliases() must return a fresh map each call")
	}
}

func TestResolvedKey_String(t *testing.T) {
	k := provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	if k.String() != "ollama/qwen3:8b" {
		t.Errorf("got %q", k.String())
	}
}
