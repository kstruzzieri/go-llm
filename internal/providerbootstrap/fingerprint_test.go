package providerbootstrap

import (
	"context"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestProberFactory_OllamaKeyBuildsProber(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {APIFormat: "ollama", BaseURL: "http://localhost:11434"},
	}}
	factory := proberFactory(cfg, ollama.NewClient())
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("ollama"))
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "ollama", Model: "qwen"}, nil, op)
	if err != nil {
		t.Fatalf("factory error: %v", err)
	}
	if spec == nil || spec.Prober == nil {
		t.Fatalf("expected non-nil prober spec")
	}
}

func TestProberFactory_UnknownProviderReturnsNil(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	factory := proberFactory(cfg, ollama.NewClient())
	spec, err := factory(context.Background(), provider.ModelKey{Provider: "missing", Model: "m"}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spec != nil {
		t.Fatalf("expected nil spec for unknown provider")
	}
}

func TestProberFactory_OpenAICompatWrongTypeErrors(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc": {APIFormat: "openai-compat", BaseURL: "http://localhost:8080"},
	}}
	factory := proberFactory(cfg, ollama.NewClient())
	op := provider.NewOllamaProvider(ollama.NewClient(), provider.WithProviderName("lc"))
	if _, err := factory(context.Background(), provider.ModelKey{Provider: "lc", Model: "m"}, nil, op); err == nil {
		t.Fatalf("expected type-mismatch error when openai-compat provider is not *openaicompat.Provider")
	}
}

func TestFingerprintDigestForModel(t *testing.T) {
	if got := fingerprintDigestForModel(provider.ModelKey{Provider: "p", Model: "m"}, nil, []string{"chat"}); got != "config-caps:chat" {
		t.Fatalf("digest = %q", got)
	}
	if got := fingerprintDigestForModel(provider.ModelKey{Provider: "p", Model: "m"}, nil, nil); got != "p/m" {
		t.Fatalf("digest = %q, want p/m", got)
	}
}
