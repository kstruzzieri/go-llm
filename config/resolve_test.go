package config

import (
	"context"
	"fmt"
	"testing"
)

type mockChecker struct {
	models []string
	err    error
}

func (m *mockChecker) AvailableModels(_ context.Context) ([]string, error) {
	return m.models, m.err
}

func loadTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := Load("testdata/valid.json")
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return cfg
}

func TestResolve_PrimaryAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:27b", "qwen3:8b"}}

	got, err := cfg.Resolve(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Name != "qwen3.5:27b" {
		t.Errorf("Name = %q, want %q", got.Name, "qwen3.5:27b")
	}
	if got.Role != "general" {
		t.Errorf("Role = %q, want %q", got.Role, "general")
	}
	if got.IsFallback {
		t.Error("IsFallback = true, want false")
	}
}

func TestResolve_FallbackUsed(t *testing.T) {
	cfg := loadTestConfig(t)
	// Only lightweight model is available; primary "general" (qwen3.5:27b) is not.
	checker := &mockChecker{models: []string{"qwen3:8b"}}

	got, err := cfg.Resolve(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Name != "qwen3:8b" {
		t.Errorf("Name = %q, want %q", got.Name, "qwen3:8b")
	}
	if got.Role != "lightweight" {
		t.Errorf("Role = %q, want %q", got.Role, "lightweight")
	}
	if !got.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

func TestResolve_NoneAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{}}

	_, err := cfg.Resolve(context.Background(), checker, "chat")
	if err == nil {
		t.Fatal("expected error when no models are available")
	}
}

func TestResolve_UnknownUseCase(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:27b"}}

	_, err := cfg.Resolve(context.Background(), checker, "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown use-case")
	}
}

func TestResolve_CheckerError(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{err: fmt.Errorf("connection refused")}

	_, err := cfg.Resolve(context.Background(), checker, "chat")
	if err == nil {
		t.Fatal("expected error when checker fails")
	}
	if !contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want substring %q", err.Error(), "connection refused")
	}
}

func TestResolve_NilChecker(t *testing.T) {
	cfg := loadTestConfig(t)
	_, err := cfg.Resolve(context.Background(), nil, "chat")
	if err == nil {
		t.Fatal("expected error for nil checker")
	}
	_, err = cfg.ResolveAll(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil checker in ResolveAll")
	}
}

func TestResolveAll_AllAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{
		"qwen3.5:27b",
		"qwen3.5:35b-a3b",
		"qwen3-coder-next:latest",
		"qwen3:8b",
		"qwen3-embedding:8b",
	}}

	results, err := cfg.ResolveAll(context.Background(), checker)
	if err != nil {
		t.Fatalf("ResolveAll() error: %v", err)
	}
	if got := len(results); got != len(cfg.Defaults) {
		t.Errorf("len(results) = %d, want %d", got, len(cfg.Defaults))
	}
	// All should be primary (not fallback).
	for useCase, rm := range results {
		if rm.IsFallback {
			t.Errorf("use-case %q: IsFallback = true, want false", useCase)
		}
	}
}

func TestResolveAll_PartialAvailability(t *testing.T) {
	cfg := loadTestConfig(t)
	// Only lightweight and embedding models are available.
	checker := &mockChecker{models: []string{"qwen3:8b", "qwen3-embedding:8b"}}

	results, err := cfg.ResolveAll(context.Background(), checker)
	// Some use-cases should resolve (via fallback), but "completion" coding model
	// falls back to general then lightweight — lightweight is available.
	// All non-embedding use-cases should fallback to lightweight.
	if err != nil {
		t.Fatalf("ResolveAll() error: %v", err)
	}

	// chat -> general (not available) -> fallback lightweight (available)
	chat, ok := results["chat"]
	if !ok {
		t.Fatal("expected 'chat' in results")
	}
	if chat.Name != "qwen3:8b" {
		t.Errorf("chat.Name = %q, want %q", chat.Name, "qwen3:8b")
	}
	if !chat.IsFallback {
		t.Error("chat.IsFallback = false, want true")
	}

	// analysis -> general (not available) -> fallback lightweight (available)
	analysis, ok := results["analysis"]
	if !ok {
		t.Fatal("expected 'analysis' in results")
	}
	if analysis.Name != "qwen3:8b" {
		t.Errorf("analysis.Name = %q, want %q", analysis.Name, "qwen3:8b")
	}
	if !analysis.IsFallback {
		t.Error("analysis.IsFallback = false, want true")
	}

	// embedding -> embedding (available, primary)
	emb, ok := results["embedding"]
	if !ok {
		t.Fatal("expected 'embedding' in results")
	}
	if emb.Name != "qwen3-embedding:8b" {
		t.Errorf("embedding.Name = %q, want %q", emb.Name, "qwen3-embedding:8b")
	}
	if emb.IsFallback {
		t.Error("embedding.IsFallback = true, want false")
	}
}

func TestResolveAll_NoneAvailable(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{}}

	_, err := cfg.ResolveAll(context.Background(), checker)
	if err == nil {
		t.Fatal("expected error when no models are available")
	}
}

// TestResolve_DiamondFallbackGraph verifies that shared fallback nodes
// in a diamond-shaped graph are not falsely detected as circular.
// Graph: a → {b, c}, both b and c fall back to d.
// When b's path through d fails (d's model unavailable), d must still
// be explorable through c's path without a circular fallback error.
func TestResolve_DiamondFallbackGraph(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Fallbacks: []string{"b", "c"}},
			"b": {Name: "model-b", Fallbacks: []string{"d"}},
			"c": {Name: "model-c", Fallbacks: []string{"d"}},
			"d": {Name: "model-d"},
		},
		Defaults: map[string]string{
			"test": "a",
		},
	}

	// Only model-d is available — resolution must walk a → b → d (or a → c → d).
	checker := &mockChecker{models: []string{"model-d"}}

	got, err := cfg.Resolve(context.Background(), checker, "test")
	if err != nil {
		t.Fatalf("Resolve() error: %v (diamond fallback falsely detected as cycle?)", err)
	}
	if got.Name != "model-d" {
		t.Errorf("Name = %q, want %q", got.Name, "model-d")
	}
	if !got.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

// TestResolve_CircularFallbackTerminates verifies that circular fallback chains
// terminate (don't infinite loop) and return an error.
func TestResolve_CircularFallbackTerminates(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "model-a", Fallbacks: []string{"b"}},
			"b": {Name: "model-b", Fallbacks: []string{"a"}}, // cycle: a → b → a
		},
		Defaults: map[string]string{
			"test": "a",
		},
	}

	checker := &mockChecker{models: []string{}} // nothing available, forces full walk

	_, err := cfg.Resolve(context.Background(), checker, "test")
	if err == nil {
		t.Fatal("expected error for circular fallback chain")
	}
	// The function must terminate and return an error — not hang.
	// The error will be "no available model" since the circular error
	// is caught internally to prevent infinite recursion.
}

