package main

import (
	"context"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// qwenYaRNProvider lists one catalog-known model served with a YaRN window.
// qwen2.5's catalog ceiling is 32768 (Qwen/Qwen2.5-7B-Instruct: "The current
// config.json is set for context length up to 32,768 tokens", YaRN
// original_max_position_embeddings 32768, "Full 131,072 tokens").
type qwenYaRNProvider struct{}

func (qwenYaRNProvider) Name() string { return "yarn" }
func (qwenYaRNProvider) Capabilities() provider.Capability {
	return provider.CapChat | provider.CapStream
}
func (qwenYaRNProvider) Health(context.Context) error { return nil }
func (qwenYaRNProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{
		Name: "qwen2.5:7b", Family: "qwen2.5", ParameterSize: "7B",
		ContextWindow: 131072, Capabilities: []string{"completion", "tools"},
	}}, nil
}
func (qwenYaRNProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("unused")
}
func (qwenYaRNProvider) ChatStream(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
	return errors.New("unused")
}
func (qwenYaRNProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return nil, errors.New("unused")
}
func (qwenYaRNProvider) GenerateStream(context.Context, provider.GenerateRequest, func(provider.GenerateResponse) error) error {
	return errors.New("unused")
}
func (qwenYaRNProvider) Embed(context.Context, provider.EmbedRequest) (*provider.EmbedResponse, error) {
	return nil, errors.New("unused")
}

// newQwen25YaRNRegistry builds the REAL provider and model registries (no
// fakes past the wire) so the ceiling arrives through the production merge.
func newQwen25YaRNRegistry(t *testing.T) (*provider.Registry, *provider.ModelRegistry) {
	t.Helper()
	providers := provider.NewRegistry()
	if err := providers.Register(qwenYaRNProvider{}); err != nil {
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

// #479 end to end: Golem's per-turn input ceiling shrinks for quality-sensitive
// routes (chat, planning) and keeps the full window for the agent loop.
//
//	chat, planning: 32768 - 2048 (implicit chat-default reserve) = 30720
//	agent:          131072 - 2048                                 = 129024
func TestResolveInputCeiling_QualityCtxCeilingFromCatalog(t *testing.T) {
	_, models := newQwen25YaRNRegistry(t)
	chain := []string{"yarn/qwen2.5:7b"}
	for useCase, want := range map[string]int{"chat": 30720, "planning": 30720, "agent": 129024} {
		got := resolveInputCeiling(context.Background(), models, chain, useCase, 0, 0, false)
		if got.ceiling != want || got.source != inputCeilingChainMinimum {
			t.Errorf("%s: ceiling=%d source=%q, want %d %q", useCase, got.ceiling, got.source, want, inputCeilingChainMinimum)
		}
	}
}

// #479: golem models -json inventory carries the merged ceiling.
func TestBuildInventoryFromRegistry_QualityCtxCeiling(t *testing.T) {
	providers, models := newQwen25YaRNRegistry(t)
	inv, err := buildInventoryFromRegistry(context.Background(), providers, models, nil)
	if err != nil {
		t.Fatalf("buildInventoryFromRegistry: %v", err)
	}
	if len(inv.Models) != 1 || inv.Models[0].QualityCtxCeiling != 32768 {
		t.Fatalf("inventory = %+v, want one model with QualityCtxCeiling 32768", inv.Models)
	}
}
