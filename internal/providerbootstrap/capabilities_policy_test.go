package providerbootstrap

import (
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestCapabilityPolicy_MatchesBootstrapBuilders proves config.CapabilityPolicy
// reads models.json the way the unchanged bootstrap builders do (#736): for
// every selector a table names, buildCapabilityOverrides and the policy's
// Explicit parse to the same mask or are both absent, buildCapabilityFloors
// parses to the policy's TypeCaps, and a table the builders reject is one
// whose policy errors for some selector.
func TestCapabilityPolicy_MatchesBootstrapBuilders(t *testing.T) {
	providers := map[string]config.ProviderConfig{
		"ol": {BaseURL: "http://127.0.0.1:11434"},
		"om": {BaseURL: "http://127.0.0.1:11435", APIFormat: "ollama"},
		"lc": {BaseURL: "http://127.0.0.1:8080", APIFormat: "openai-compat"},
	}
	tests := []struct {
		name    string
		models  map[string]config.ModelConfig
		wantErr bool
	}{
		{name: "single role", models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "m", Type: "dense"},
		}},
		{name: "single explicit role", models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat", "tool_call"}},
		}},
		{name: "shared roles", models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "m", Type: "dense"},
			"chat":  {Provider: "lc", Name: "m", Type: "moe"},
		}},
		{name: "later explicit list", models: map[string]config.ModelConfig{
			"a": {Provider: "lc", Name: "m", Type: "dense"},
			"b": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat", "stream"}},
		}},
		{name: "dense plus embedding", models: map[string]config.ModelConfig{
			"chat":  {Provider: "lc", Name: "hybrid", Type: "dense"},
			"embed": {Provider: "lc", Name: "hybrid", Type: "embedding"},
		}},
		{name: "ollama and openai-compat", models: map[string]config.ModelConfig{
			"a": {Provider: "ol", Name: "m", Type: "dense"},
			"b": {Provider: "om", Name: "m", Type: "embedding"},
			"c": {Provider: "lc", Name: "m", Type: "dense"},
			"d": {Provider: "ol", Name: "x", Type: "dense", Capabilities: []string{"chat", "insert"}},
		}},
		{name: "equivalent lists", models: map[string]config.ModelConfig{
			"a": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"stream", "chat"}},
			"b": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat", "stream", "chat"}},
		}},
		{name: "conflicting lists", wantErr: true, models: map[string]config.ModelConfig{
			"a": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat"}},
			"b": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"tool_call"}},
		}},
		{name: "invalid list", wantErr: true, models: map[string]config.ModelConfig{
			"a": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"completion"}},
		}},
		{name: "empty provider", models: map[string]config.ModelConfig{
			"a": {Name: "m", Type: "dense", Capabilities: []string{"chat"}},
			"b": {Provider: "lc", Name: "m", Type: "dense"},
		}},
		{name: "unknown provider", models: map[string]config.ModelConfig{
			"a": {Provider: "nowhere", Name: "m", Type: "dense"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Providers: providers, Models: tc.models}
			overrides, oErr := buildCapabilityOverrides(cfg)
			floors, fErr := buildCapabilityFloors(cfg)
			builderErr := oErr != nil || fErr != nil
			if builderErr != tc.wantErr {
				t.Fatalf("builders: overrides err %v, floors err %v; want error %v", oErr, fErr, tc.wantErr)
			}

			keys := map[provider.ModelKey]bool{}
			for _, m := range tc.models {
				keys[provider.ModelKey{Provider: m.Provider, Model: m.Name}] = true
			}
			policyErr := false
			for key := range keys {
				policy, err := cfg.CapabilityPolicy(key.Provider, key.Model)
				if err != nil {
					policyErr = true
					continue
				}
				if builderErr {
					continue // the builders produced no maps to compare with
				}
				list, ok := overrides[key]
				if ok != (policy.Explicit != nil) {
					t.Errorf("%s: override present %v, policy Explicit %v", key, ok, policy.Explicit)
				}
				if ok {
					want, err := provider.ParseCapsStrict(list)
					if err != nil {
						t.Fatalf("%s: override %v does not parse: %v", key, list, err)
					}
					got, err := provider.ParseCapsStrict(policy.Explicit)
					if err != nil || got != want {
						t.Errorf("%s: policy Explicit %v parses to %v, %v; override %v parses to %v", key, policy.Explicit, got, err, list, want)
					}
				}
				floor, err := provider.ParseCapsStrict(floors[key])
				if err != nil {
					t.Fatalf("%s: floor %v does not parse: %v", key, floors[key], err)
				}
				if floor != policy.TypeCaps {
					t.Errorf("%s: floor %v parses to %v; policy TypeCaps %v", key, floors[key], floor, policy.TypeCaps)
				}
			}
			if policyErr != builderErr {
				t.Errorf("some policy errored: %v; builders errored: %v", policyErr, builderErr)
			}
		})
	}
}
