package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// listedProvider lists a fixed set of models and serves nothing else.
type listedProvider struct{ models []provider.ModelInfo }

func (listedProvider) Name() string { return "yarn" }
func (listedProvider) Capabilities() provider.Capability {
	return provider.CapChat | provider.CapStream
}
func (listedProvider) Health(context.Context) error { return nil }
func (p listedProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return p.models, nil
}
func (listedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("unused")
}
func (listedProvider) ChatStream(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
	return errors.New("unused")
}
func (listedProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return nil, errors.New("unused")
}
func (listedProvider) GenerateStream(context.Context, provider.GenerateRequest, func(provider.GenerateResponse) error) error {
	return errors.New("unused")
}
func (listedProvider) Embed(context.Context, provider.EmbedRequest) (*provider.EmbedResponse, error) {
	return nil, errors.New("unused")
}

// newListedRegistry builds the REAL provider and model registries (no fakes
// past the wire) over one listed model, so the ceiling arrives through the
// production merge.
func newListedRegistry(t *testing.T, info provider.ModelInfo) (*provider.Registry, *provider.ModelRegistry) {
	t.Helper()
	providers := provider.NewRegistry()
	if err := providers.Register(listedProvider{models: []provider.ModelInfo{info}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := providers.RefreshModels(context.Background(), "yarn"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	models, err := provider.NewModelRegistry(providers, nil)
	if err != nil {
		t.Fatalf("model registry: %v", err)
	}
	return providers, models
}

// newQwen35YaRNRegistry lists qwen3.5:9b served with a YaRN window of 1010000.
// qwen3.5's catalog ceiling is 262144 (Qwen/Qwen3.5-9B, Qwen/Qwen3.5-27B:
// "Context Length: 262,144 natively and extensible up to 1,010,000 tokens").
func newQwen35YaRNRegistry(t *testing.T) (*provider.Registry, *provider.ModelRegistry) {
	t.Helper()
	return newListedRegistry(t, provider.ModelInfo{
		Name: "qwen3.5:9b", Family: "qwen3.5", ParameterSize: "9B",
		ContextWindow: 1010000, Capabilities: []string{"completion", "tools"},
	})
}

// #479 end to end: Golem's per-turn input ceiling shrinks for quality-sensitive
// routes (chat, planning) and keeps the full window for the agent loop.
//
//	chat, planning: 262144 - 2048 (implicit chat-default reserve) = 260096
//	agent:          1010000 - 2048                                = 1007952
func TestResolveInputCeiling_QualityCtxCeilingFromCatalog(t *testing.T) {
	_, models := newQwen35YaRNRegistry(t)
	chain := []string{"yarn/qwen3.5:9b"}
	for useCase, want := range map[string]int{"chat": 260096, "planning": 260096, "agent": 1007952} {
		got := resolveInputCeiling(context.Background(), models, chain, useCase, 0, 0, false)
		if got.ceiling != want || got.source != inputCeilingChainMinimum {
			t.Errorf("%s: ceiling=%d source=%q, want %d %q", useCase, got.ceiling, got.source, want, inputCeilingChainMinimum)
		}
	}
}

// #479: golem models -json inventory carries the merged ceiling.
func TestBuildInventoryFromRegistry_QualityCtxCeiling(t *testing.T) {
	providers, models := newQwen35YaRNRegistry(t)
	inv, err := buildInventoryFromRegistry(context.Background(), providers, models, nil)
	if err != nil {
		t.Fatalf("buildInventoryFromRegistry: %v", err)
	}
	if len(inv.Models) != 1 || inv.Models[0].QualityCtxCeiling != 262144 {
		t.Fatalf("inventory = %+v, want one model with QualityCtxCeiling 262144", inv.Models)
	}
}
