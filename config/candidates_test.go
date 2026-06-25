package config

import (
	"context"
	"fmt"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestResolveCandidates_PrimaryOnly(t *testing.T) {
	cfg := loadTestConfig(t)
	// Only the primary model for "embedding" is available (no fallbacks defined).
	checker := &mockChecker{models: []string{"qwen3-embedding:8b"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "embedding")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	if got[0].Name != "qwen3-embedding:8b" {
		t.Errorf("Name = %q, want %q", got[0].Name, "qwen3-embedding:8b")
	}
	if got[0].Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", got[0].Provider, "ollama")
	}
	if got[0].Role != "embedding" {
		t.Errorf("Role = %q, want %q", got[0].Role, "embedding")
	}
	if got[0].IsFallback {
		t.Error("IsFallback = true, want false")
	}
}

func TestResolveCandidates_PrimaryAndFallback(t *testing.T) {
	cfg := loadTestConfig(t)
	// Both primary (general=qwen3.5:27b) and fallback (lightweight=qwen3:8b) available.
	// chat -> general -> lightweight
	checker := &mockChecker{models: []string{"qwen3.5:27b", "qwen3:8b"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2", len(got))
	}

	// First candidate: primary model.
	if got[0].Name != "qwen3.5:27b" {
		t.Errorf("[0].Name = %q, want %q", got[0].Name, "qwen3.5:27b")
	}
	if got[0].IsFallback {
		t.Error("[0].IsFallback = true, want false")
	}

	// Second candidate: fallback model.
	if got[1].Name != "qwen3:8b" {
		t.Errorf("[1].Name = %q, want %q", got[1].Name, "qwen3:8b")
	}
	if !got[1].IsFallback {
		t.Error("[1].IsFallback = false, want true")
	}
}

func TestResolveCandidates_FallbackOnlyAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	// Primary not available, only fallback.
	// chat -> general (unavailable) -> lightweight (available)
	checker := &mockChecker{models: []string{"qwen3:8b"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	if got[0].Name != "qwen3:8b" {
		t.Errorf("Name = %q, want %q", got[0].Name, "qwen3:8b")
	}
	if got[0].Role != "lightweight" {
		t.Errorf("Role = %q, want %q", got[0].Role, "lightweight")
	}
	if !got[0].IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

func TestResolveCandidates_DeepChainOrdering(t *testing.T) {
	cfg := loadTestConfig(t)
	// completion -> coding -> general -> lightweight
	// All three models available — verify traversal order.
	checker := &mockChecker{models: []string{
		"qwen3-coder-next:latest",
		"qwen3.5:27b",
		"qwen3:8b",
	}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "completion")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(candidates) = %d, want 3", len(got))
	}

	// DFS order: coding (primary), then general (first fallback), then lightweight (general's fallback).
	// Note: coding also lists lightweight as a direct fallback, but general's subtree
	// is walked first (DFS), so lightweight appears via general before coding's
	// direct lightweight fallback. The visited set prevents revisiting.
	wantOrder := []struct {
		name       string
		role       string
		isFallback bool
	}{
		{"qwen3-coder-next:latest", "coding", false},
		{"qwen3.5:27b", "general", true},
		{"qwen3:8b", "lightweight", true},
	}

	for i, want := range wantOrder {
		if got[i].Name != want.name {
			t.Errorf("[%d].Name = %q, want %q", i, got[i].Name, want.name)
		}
		if got[i].Role != want.role {
			t.Errorf("[%d].Role = %q, want %q", i, got[i].Role, want.role)
		}
		if got[i].IsFallback != want.isFallback {
			t.Errorf("[%d].IsFallback = %v, want %v", i, got[i].IsFallback, want.isFallback)
		}
	}
}

func TestResolveCandidates_NoneAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(candidates) = %d, want 0", len(got))
	}
}

func TestResolveCandidates_UnknownUseCase(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:27b"}}

	_, err := cfg.ResolveCandidates(context.Background(), checker, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown use-case")
	}
}

func TestResolveCandidates_SideTaskFallbackUseCase(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:27b", "qwen3:8b"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "summarize")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	wantRoles := []string{"general", "lightweight"}
	if len(got) != len(wantRoles) {
		t.Fatalf("len(candidates) = %d, want %d: %+v", len(got), len(wantRoles), got)
	}
	for i, want := range wantRoles {
		if got[i].Role != want {
			t.Fatalf("candidate roles = %+v, want %v", got, wantRoles)
		}
	}
}

func TestResolveCandidates_UnknownDefaultRole(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Provider: "ollama"},
		},
		Defaults: map[string]string{
			"test": "missing",
		},
	}
	checker := &mockChecker{models: []string{"model-a"}}

	_, err := cfg.ResolveCandidates(context.Background(), checker, "test")
	if err == nil {
		t.Fatal("expected error for unknown default role")
	}
	if !contains(err.Error(), `unknown role "missing"`) {
		t.Errorf("error = %q, want substring %q", err.Error(), `unknown role "missing"`)
	}
}

func TestResolveCandidates_UnknownFallbackRole(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Provider: "ollama", Fallbacks: []string{"missing"}},
		},
		Defaults: map[string]string{
			"test": "a",
		},
	}
	checker := &mockChecker{models: []string{"model-a"}}

	_, err := cfg.ResolveCandidates(context.Background(), checker, "test")
	if err == nil {
		t.Fatal("expected error for unknown fallback role")
	}
	if !contains(err.Error(), `unknown role "missing"`) {
		t.Errorf("error = %q, want substring %q", err.Error(), `unknown role "missing"`)
	}
}

func TestResolveCandidates_NilChecker(t *testing.T) {
	cfg := loadTestConfig(t)
	_, err := cfg.ResolveCandidates(context.Background(), nil, "chat")
	if err == nil {
		t.Fatal("expected error for nil checker")
	}
}

func TestResolveCandidates_CheckerError(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{err: fmt.Errorf("connection refused")}

	_, err := cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err == nil {
		t.Fatal("expected error when checker fails")
	}
	if !contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want substring %q", err.Error(), "connection refused")
	}
}

func TestResolveCandidates_CircularFallback(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Provider: "ollama", Fallbacks: []string{"b"}},
			"b": {Name: "model-b", Provider: "ollama", Fallbacks: []string{"a"}}, // cycle: a → b → a
		},
		Defaults: map[string]string{
			"test": "a",
		},
	}
	// Both available — the cycle must not cause infinite recursion or duplicates.
	checker := &mockChecker{models: []string{"model-a", "model-b"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "test")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	// Should get both models: a (primary), b (fallback). No infinite loop.
	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2", len(got))
	}
	if got[0].Name != "model-a" {
		t.Errorf("[0].Name = %q, want %q", got[0].Name, "model-a")
	}
	if got[0].IsFallback {
		t.Error("[0].IsFallback = true, want false")
	}
	if got[1].Name != "model-b" {
		t.Errorf("[1].Name = %q, want %q", got[1].Name, "model-b")
	}
	if !got[1].IsFallback {
		t.Error("[1].IsFallback = false, want true")
	}
}

func TestResolveCandidates_DiamondNoDuplicates(t *testing.T) {
	// Diamond: a → {b, c}, both b and c fall back to d.
	// d should appear only once in candidates.
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Provider: "ollama", Fallbacks: []string{"b", "c"}},
			"b": {Name: "model-b", Provider: "ollama", Fallbacks: []string{"d"}},
			"c": {Name: "model-c", Provider: "ollama", Fallbacks: []string{"d"}},
			"d": {Name: "model-d", Provider: "ollama"},
		},
		Defaults: map[string]string{
			"test": "a",
		},
	}
	checker := &mockChecker{models: []string{"model-a", "model-b", "model-c", "model-d"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "test")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}

	// DFS order: a (primary), b (first fallback of a), d (fallback of b),
	// c (second fallback of a). d is NOT revisited via c's fallback because
	// the visited set marks it done after b's subtree.
	wantNames := []string{"model-a", "model-b", "model-d", "model-c"}
	if len(got) != len(wantNames) {
		t.Fatalf("len(candidates) = %d, want %d; got: %v", len(got), len(wantNames), candidateNames(got))
	}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Errorf("[%d].Name = %q, want %q", i, got[i].Name, want)
		}
	}
}

func TestResolveCandidates_ConsistentWithResolve(t *testing.T) {
	cfg := loadTestConfig(t)

	tests := []struct {
		name    string
		models  []string
		useCase string
	}{
		{"primary available", []string{"qwen3.5:27b", "qwen3:8b"}, "chat"},
		{"fallback only", []string{"qwen3:8b"}, "chat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &mockChecker{models: tt.models}

			resolved, err := cfg.Resolve(context.Background(), checker, tt.useCase)
			if err != nil {
				t.Fatalf("Resolve() error: %v", err)
			}

			candidates, err := cfg.ResolveCandidates(context.Background(), checker, tt.useCase)
			if err != nil {
				t.Fatalf("ResolveCandidates() error: %v", err)
			}
			if len(candidates) == 0 {
				t.Fatal("expected at least one candidate")
			}

			// The first candidate must match what Resolve returns.
			if candidates[0].Name != resolved.Name {
				t.Errorf("first candidate Name = %q, Resolve Name = %q", candidates[0].Name, resolved.Name)
			}
			if candidates[0].Role != resolved.Role {
				t.Errorf("first candidate Role = %q, Resolve Role = %q", candidates[0].Role, resolved.Role)
			}
			if candidates[0].Provider != resolved.Provider {
				t.Errorf("first candidate Provider = %q, Resolve Provider = %q", candidates[0].Provider, resolved.Provider)
			}
			if candidates[0].IsFallback != resolved.IsFallback {
				t.Errorf("first candidate IsFallback = %v, Resolve IsFallback = %v", candidates[0].IsFallback, resolved.IsFallback)
			}
		})
	}
}

func TestResolveCandidates_ProviderAwareAvailability(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: "http://localhost:11434"},
			"vllm":   {BaseURL: "http://localhost:8080", APIFormat: "openai-compat"},
		},
		Models: map[string]ModelConfig{
			"chat": {Name: "shared-model", Provider: "vllm", Type: "dense"},
		},
		Defaults: map[string]string{"chat": "chat"},
	}
	checker := &mockProviderChecker{keys: []provider.ModelKey{
		{Provider: "ollama", Model: "shared-model"},
	}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("ResolveCandidates() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(candidates) = %d, want 0 for provider-specific miss", len(got))
	}

	checker.keys = append(checker.keys, provider.ModelKey{Provider: "vllm", Model: "shared-model"})
	got, err = cfg.ResolveCandidates(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("ResolveCandidates() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(got))
	}
	if got[0].Provider != "vllm" || got[0].Name != "shared-model" {
		t.Fatalf("candidate = %+v, want vllm/shared-model", got[0])
	}
}

func TestResolveCandidates_DuplicateModelNames(t *testing.T) {
	// Two roles map to the same model name. Both should appear as candidates
	// since the orchestration layer cares about roles, not just model names.
	cfg := &Config{
		Models: map[string]ModelConfig{
			"primary": {Name: "shared-model", Provider: "ollama", Fallbacks: []string{"backup"}},
			"backup":  {Name: "shared-model", Provider: "ollama"},
		},
		Defaults: map[string]string{
			"test": "primary",
		},
	}
	checker := &mockChecker{models: []string{"shared-model"}}

	got, err := cfg.ResolveCandidates(context.Background(), checker, "test")
	if err != nil {
		t.Fatalf("ResolveCandidates() error: %v", err)
	}
	// Both roles should appear — tracked by role, not model name.
	if len(got) != 2 {
		t.Fatalf("len(candidates) = %d, want 2", len(got))
	}
	if got[0].Role != "primary" {
		t.Errorf("[0].Role = %q, want %q", got[0].Role, "primary")
	}
	if got[0].IsFallback {
		t.Error("[0].IsFallback = true, want false")
	}
	if got[1].Role != "backup" {
		t.Errorf("[1].Role = %q, want %q", got[1].Role, "backup")
	}
	if !got[1].IsFallback {
		t.Error("[1].IsFallback = false, want true")
	}
}

// candidateNames extracts names from a candidate slice for debug output.
func candidateNames(candidates []CandidateModel) []string {
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = c.Name
	}
	return names
}
