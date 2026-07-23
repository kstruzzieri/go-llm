package providerbootstrap

import (
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

// buildProviders returns (providers, ollamaClients, effectiveConfig, error).
// nil cfg synthesizes one default ollama provider AND a synthetic effective config;
// a non-nil cfg with zero Providers is an error (mcp parity).

func TestBuildProviders_NilConfigSynthesizesDefaultOllamaAndEffectiveConfig(t *testing.T) {
	provs, ollamaClients, eff, err := buildProviders(nil, "", "", "")
	if err != nil {
		t.Fatalf("buildProviders(nil) error: %v", err)
	}
	if len(provs) != 1 || provs[0].Name() != "ollama" {
		t.Fatalf("providers = %v, want [ollama]", provs)
	}
	if ollamaClients["ollama"] == nil {
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
	_, _, eff, err := buildProviders(nil, "http://127.0.0.1:9999", "", "")
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
	provs, _, eff, err := buildProviders(cfg, "", "", "")
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
	if _, _, _, err := buildProviders(cfg, "", "", ""); err == nil {
		t.Fatalf("expected error for non-nil config with zero providers")
	}
}

func TestBuildProviders_BadProviderNameErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"with/slash": {APIFormat: "ollama", BaseURL: "http://x"},
	}}
	if _, _, _, err := buildProviders(cfg, "", "", ""); err == nil {
		t.Fatalf("expected error for invalid provider name")
	}
}

func TestBuildProviders_UnsupportedAPIFormatErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"x": {APIFormat: "grpc-weird"},
	}}
	if _, _, _, err := buildProviders(cfg, "", "", ""); err == nil {
		t.Fatalf("expected error for unsupported api_format")
	}
}

func TestBuildProviders_OpenAICompatOverrideRewritesNamedProvider(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
		"other":    {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:9999"},
		"ollama":   {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	_, _, eff, err := buildProviders(cfg, "", "llamacpp", "http://127.0.0.1:8083")
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	if got := eff.Providers["llamacpp"].BaseURL; got != "http://127.0.0.1:8083" {
		t.Fatalf("llamacpp BaseURL = %q, want the override", got)
	}
	if got := eff.Providers["other"].BaseURL; got != "http://127.0.0.1:9999" {
		t.Fatalf("sibling provider BaseURL = %q, want untouched", got)
	}
	// Caller-owned config must NOT be mutated (copy-on-write).
	if got := cfg.Providers["llamacpp"].BaseURL; got != "http://127.0.0.1:8080" {
		t.Fatalf("caller config mutated: llamacpp BaseURL = %q", got)
	}
}

func TestBuildProviders_OpenAICompatOverrideValidation(t *testing.T) {
	base := &config.Config{Providers: map[string]config.ProviderConfig{
		"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
		"ollama":   {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
		"plain":    {APIFormat: "", BaseURL: "http://127.0.0.1:8082"},
	}}
	tests := []struct {
		name       string
		ocKey, url string
	}{
		{"provider without url", "llamacpp", ""},
		{"url without provider", "", "http://127.0.0.1:8083"},
		{"unknown provider", "nope", "http://127.0.0.1:8083"},
		{"non-openai-compat provider", "ollama", "http://127.0.0.1:8083"},
		// Empty api_format defaults to "ollama" internally (buildProvider's
		// switch, and the same default this validation applies), so it is
		// rejected as a non-openai-compat override target exactly like an
		// explicit "ollama" api_format.
		{"empty api_format target", "plain", "http://127.0.0.1:8083"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := buildProviders(base, "", tt.ocKey, tt.url); err == nil {
				t.Fatal("want error, got nil")
			}
		})
	}
}

func TestBuildProviders_NilConfigRejectsOpenAICompatOverride(t *testing.T) {
	// nil cfg synthesizes an ollama-only default config; overriding a
	// non-openai-compat provider ("ollama") must still be rejected.
	if _, _, _, err := buildProviders(nil, "", "ollama", "http://x"); err == nil {
		t.Fatal("want error, got nil")
	}
}

func TestBuildProviders_OllamaOverrideUnchangedBesideOCOverride(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
		"ollama":   {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	_, _, eff, err := buildProviders(cfg, "http://127.0.0.1:7777", "llamacpp", "http://127.0.0.1:8083")
	if err != nil {
		t.Fatalf("buildProviders: %v", err)
	}
	if got := eff.Providers["llamacpp"].BaseURL; got != "http://127.0.0.1:8083" {
		t.Fatalf("llamacpp BaseURL = %q, want oc override", got)
	}
	// Note: the ollama override is applied to the live client (pc local copy),
	// not written back into effective.Providers -- existing behavior, unchanged.
}
