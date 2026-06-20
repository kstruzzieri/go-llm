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

func TestInstallCapabilityOverrides_NilSafe(t *testing.T) {
	if err := installCapabilityOverrides(nil, nil); err != nil {
		t.Fatalf("installCapabilityOverrides(nil,nil) = %v, want nil", err)
	}
}
