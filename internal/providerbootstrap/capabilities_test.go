package providerbootstrap

import (
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestCapabilitiesForKey_ExplicitCaps(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{
		"chat": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
	}}
	got := capabilitiesForKey(cfg, provider.ModelKey{Provider: "lc", Model: "qwen"})
	if !reflect.DeepEqual(got, []string{"chat", "tool_call"}) {
		t.Fatalf("caps = %v", got)
	}
}

func TestCapabilitiesForKey_UnknownKeyNil(t *testing.T) {
	cfg := &config.Config{Models: map[string]config.ModelConfig{}}
	if got := capabilitiesForKey(cfg, provider.ModelKey{Provider: "x", Model: "y"}); got != nil {
		t.Fatalf("caps = %v, want nil", got)
	}
}

func TestBuildCapabilityOverrides_OpenAICompatModel(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat"}},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
		},
	}
	got, err := buildCapabilityOverrides(cfg)
	if err != nil {
		t.Fatalf("buildCapabilityOverrides: %v", err)
	}
	caps := got[provider.ModelKey{Provider: "lc", Model: "qwen"}]
	if !reflect.DeepEqual(caps, []string{"chat", "tool_call"}) {
		t.Fatalf("override caps = %v", caps)
	}
}

func TestBuildCapabilityOverrides_ConflictErrors(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat"}},
		Models: map[string]config.ModelConfig{
			"chat":   {Provider: "lc", Name: "qwen", Capabilities: []string{"chat"}},
			"review": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
		},
	}
	if _, err := buildCapabilityOverrides(cfg); err == nil {
		t.Fatalf("expected conflict error for divergent caps on same key")
	}
}

func TestBuildCapabilityFloors_DerivedOpenAICompatOnly(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			"ollama":   {BaseURL: "http://localhost:11434"},
			"ollama2":  {APIFormat: "ollama"},
		},
		Models: map[string]config.ModelConfig{
			"agent":     {Provider: "llamacpp", Name: "byo-model", Type: "dense"},                                                       // undeclared => floor
			"chat":      {Provider: "llamacpp", Name: "declared", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}}, // explicit => override, NOT floor
			"embed":     {Provider: "ollama", Name: "qwen3-embedding:8b", Type: "embedding"},                                            // default-empty format => ollama => neither
			"summarize": {Provider: "ollama2", Name: "small", Type: "dense"},                                                            // explicit ollama format => no floor
			"orphan":    {Provider: "ghost", Name: "m", Type: "dense"},                                                                  // provider missing from cfg.Providers => no floor
			"notype":    {Provider: "llamacpp", Name: "untyped", Type: ""},                                                              // empty derived set => no floor
		},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	if got := floors[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; len(got) == 0 {
		t.Fatalf("undeclared openai-compat model missing floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; ok {
		t.Fatalf("explicitly declared model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ollama", Model: "qwen3-embedding:8b"}]; ok {
		t.Fatalf("ollama model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ollama2", Model: "small"}]; ok {
		t.Fatalf("explicit ollama-format model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ghost", Model: "m"}]; ok {
		t.Fatalf("model on a provider missing from cfg.Providers must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "llamacpp", Model: "untyped"}]; ok {
		t.Fatalf("openai-compat model with empty derived set must not get a floor")
	}

	overrides, err := buildCapabilityOverrides(cfg)
	if err != nil {
		t.Fatalf("overrides: %v", err)
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; ok {
		t.Fatalf("undeclared openai-compat model must no longer get a REPLACE override")
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; !ok {
		t.Fatalf("explicit declaration must remain an override")
	}
}

func TestBuildCapabilityFloors_SharedKeyUnionsDerived(t *testing.T) {
	// Two roles, same key, different types => floors union (OR semantics
	// make conflicts impossible; no error path for floors).
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "llamacpp", Name: "m", Type: "dense"},
			"embed": {Provider: "llamacpp", Name: "m", Type: "embedding"},
		},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	got := floors[provider.ModelKey{Provider: "llamacpp", Model: "m"}]
	// dense derives chat,generate,stream; embedding derives embed => union of all four.
	want := map[string]bool{"chat": true, "generate": true, "stream": true, "embed": true}
	if len(got) != len(want) {
		t.Fatalf("floor union = %v, want keys %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Fatalf("unexpected floor token %q in %v", tok, got)
		}
	}
}

func TestInstallCapabilityFloors_NilSafe(t *testing.T) {
	if err := installCapabilityFloors(nil, nil); err != nil {
		t.Fatalf("installCapabilityFloors(nil,nil) = %v, want nil", err)
	}
}

func TestInstallCapabilityOverrides_NilSafe(t *testing.T) {
	if err := installCapabilityOverrides(nil, nil); err != nil {
		t.Fatalf("installCapabilityOverrides(nil,nil) = %v, want nil", err)
	}
}
