package providerbootstrap

import (
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

// buildProviders returns (providers, ollamaClient, effectiveConfig, error).
// nil cfg synthesizes one default ollama provider AND a synthetic effective config;
// a non-nil cfg with zero Providers is an error (mcp parity).

func TestBuildProviders_NilConfigSynthesizesDefaultOllamaAndEffectiveConfig(t *testing.T) {
	provs, ollamaClient, eff, err := buildProviders(nil, "")
	if err != nil {
		t.Fatalf("buildProviders(nil) error: %v", err)
	}
	if len(provs) != 1 || provs[0].Name() != "ollama" {
		t.Fatalf("providers = %v, want [ollama]", provs)
	}
	if ollamaClient == nil {
		t.Fatalf("expected ollama client to be captured")
	}
	if eff == nil {
		t.Fatalf("expected synthetic effective config, got nil")
	}
	if _, ok := eff.Providers["ollama"]; !ok {
		t.Fatalf("synthetic effective config missing the ollama provider: %+v", eff.Providers)
	}
}

func TestBuildProviders_OverrideURLAppliesToDefault(t *testing.T) {
	_, _, eff, err := buildProviders(nil, "http://127.0.0.1:9999")
	if err != nil {
		t.Fatalf("buildProviders override error: %v", err)
	}
	if got := eff.Providers["ollama"].BaseURL; got != "http://127.0.0.1:9999" {
		t.Fatalf("default ollama BaseURL = %q, want the override", got)
	}
}

func TestBuildProviders_ConfigProvidersSortedAndNamed(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"zeta":  {APIFormat: "openai-compat", BaseURL: "http://localhost:8081"},
		"alpha": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	provs, _, eff, err := buildProviders(cfg, "")
	if err != nil {
		t.Fatalf("buildProviders error: %v", err)
	}
	if len(provs) != 2 || provs[0].Name() != "alpha" || provs[1].Name() != "zeta" {
		t.Fatalf("providers not sorted by key: %v", provs)
	}
	if eff != cfg {
		t.Fatalf("effective config should be the passed config when non-nil")
	}
}

func TestBuildProviders_EmptyConfigProvidersErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	if _, _, _, err := buildProviders(cfg, ""); err == nil {
		t.Fatalf("expected error for non-nil config with zero providers")
	}
}

func TestBuildProviders_BadProviderNameErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"with/slash": {APIFormat: "ollama", BaseURL: "http://x"},
	}}
	if _, _, _, err := buildProviders(cfg, ""); err == nil {
		t.Fatalf("expected error for invalid provider name")
	}
}

func TestBuildProviders_UnsupportedAPIFormatErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"x": {APIFormat: "grpc-weird"},
	}}
	if _, _, _, err := buildProviders(cfg, ""); err == nil {
		t.Fatalf("expected error for unsupported api_format")
	}
}
