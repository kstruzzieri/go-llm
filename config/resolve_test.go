package config

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type mockChecker struct {
	models []string
	err    error
}

func (m *mockChecker) AvailableModels(_ context.Context) ([]string, error) {
	return m.models, m.err
}

type mockProviderChecker struct {
	keys []provider.ModelKey
	err  error
}

func (m *mockProviderChecker) AvailableModels(_ context.Context) ([]string, error) {
	seen := map[string]bool{}
	var models []string
	for _, key := range m.keys {
		if key.Model == "" || seen[key.Model] {
			continue
		}
		seen[key.Model] = true
		models = append(models, key.Model)
	}
	return models, m.err
}

func (m *mockProviderChecker) AvailableModelKeys(_ context.Context) ([]provider.ModelKey, error) {
	return m.keys, m.err
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
	if got.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", got.Provider, "ollama")
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
	if got.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", got.Provider, "ollama")
	}
	if !got.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

// TestResolve_EmptyProviderErrors verifies the resolver surfaces an empty
// Provider as a real config error rather than silently inserting "ollama".
// Empty Provider is only reachable on Configs constructed programmatically
// without going through Load+applyDefaults; lying about the owner would
// route downstream calls to a provider the caller never declared.
func TestResolve_EmptyProviderErrors(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"ollama": {BaseURL: "http://localhost:11434"},
		},
		Models: map[string]ModelConfig{
			"chat-role": {Name: "some-model", Type: "dense"}, // Provider intentionally empty
		},
		Defaults: map[string]string{"chat": "chat-role"},
	}
	checker := &mockChecker{models: []string{"some-model"}}

	_, err := cfg.Resolve(context.Background(), checker, "chat")
	if err == nil {
		t.Fatal("expected error for empty Provider, got nil")
	}
	if !strings.Contains(err.Error(), "empty provider") {
		t.Errorf("error should mention empty provider, got: %v", err)
	}
}

// TestResolve_FallbackCrossProvider verifies the resolved Provider tracks
// the fallback's owner, not the originally-requested one. Per design:
// requested role "coding"/provider "local-a" falling back to role "fast"/
// provider "local-b" must yield Provider="local-b".
func TestResolve_FallbackCrossProvider(t *testing.T) {
	crossProvider := writeTempJSON(t, `{
		"providers": {
			"local-a": {"base_url": "http://localhost:11434"},
			"local-b": {"base_url": "http://localhost:8080", "api_format": "openai-compat"}
		},
		"models": {
			"primary":  {"name": "model-a", "provider": "local-a", "type": "dense", "fallbacks": ["backup"]},
			"backup":   {"name": "model-b", "provider": "local-b", "type": "dense"}
		},
		"defaults": {"chat": "primary"}
	}`)
	cfg, err := Load(crossProvider)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// Only the backup model is available, forcing fallback.
	checker := &mockChecker{models: []string{"model-b"}}

	got, err := cfg.Resolve(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Name != "model-b" {
		t.Errorf("Name = %q, want %q", got.Name, "model-b")
	}
	if got.Role != "backup" {
		t.Errorf("Role = %q, want %q", got.Role, "backup")
	}
	if got.Provider != "local-b" {
		t.Errorf("Provider = %q, want %q (must track resolved owner, not requested)", got.Provider, "local-b")
	}
	if !got.IsFallback {
		t.Error("IsFallback = false, want true")
	}
}

func TestResolve_ProviderAwareAvailability(t *testing.T) {
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

	_, err := cfg.Resolve(context.Background(), checker, "chat")
	if err == nil {
		t.Fatal("Resolve() error = nil, want provider-specific miss")
	}
	if !strings.Contains(err.Error(), `role "chat"`) {
		t.Fatalf("error = %q, want unresolved chat role", err)
	}

	checker.keys = append(checker.keys, provider.ModelKey{Provider: "vllm", Model: "shared-model"})
	got, err := cfg.Resolve(context.Background(), checker, "chat")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got.Provider != "vllm" || got.Name != "shared-model" {
		t.Fatalf("Resolve() = %+v, want vllm/shared-model", got)
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

func TestResolve_SideTaskFallbackUseCase(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:27b"}}

	got, err := cfg.Resolve(context.Background(), checker, "summarize")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Role != "general" || got.Name != "qwen3.5:27b" {
		t.Fatalf("Resolve(\"summarize\") = %+v, want analysis/general fallback", got)
	}
}

func TestResolve_ApprovalFallbackUsesAgentDefault(t *testing.T) {
	cfg := loadTestConfig(t)
	checker := &mockChecker{models: []string{"qwen3.5:35b-a3b", "qwen3.5:27b"}}

	got, err := cfg.Resolve(context.Background(), checker, "approval")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Role != "fast" || got.Name != "qwen3.5:35b-a3b" {
		t.Fatalf("Resolve(\"approval\") = %+v, want agent/fast fallback", got)
	}
}

func TestResolve_ExplicitSideTaskDefaultWins(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Defaults["summarize"] = "lightweight"
	checker := &mockChecker{models: []string{"qwen3.5:27b", "qwen3:8b"}}

	got, err := cfg.Resolve(context.Background(), checker, "summarize")
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	if got.Role != "lightweight" || got.Name != "qwen3:8b" {
		t.Fatalf("Resolve(\"summarize\") = %+v, want explicit lightweight role", got)
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

func TestResolveAll_OnlyExplicitDefaults(t *testing.T) {
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
	for _, useCase := range []string{"summarize", "route", "rerank", "verify", "extract", "approval", "vision"} {
		if _, ok := results[useCase]; ok {
			t.Fatalf("ResolveAll() included virtual side-task %q in %+v", useCase, results)
		}
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
			"a": {Name: "model-a", Provider: "ollama", Fallbacks: []string{"b", "c"}},
			"b": {Name: "model-b", Provider: "ollama", Fallbacks: []string{"d"}},
			"c": {Name: "model-c", Provider: "ollama", Fallbacks: []string{"d"}},
			"d": {Name: "model-d", Provider: "ollama"},
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
			"a": {Name: "model-a", Provider: "ollama", Fallbacks: []string{"b"}},
			"b": {Name: "model-b", Provider: "ollama", Fallbacks: []string{"a"}}, // cycle: a → b → a
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

func TestConfig_RoleFallbackChain_LinearChain(t *testing.T) {
	cfg := &Config{
		Defaults: map[string]string{"chat": "primary"},
		Models: map[string]ModelConfig{
			"primary":  {Name: "p:8b", Provider: "ollama", Fallbacks: []string{"middle"}},
			"middle":   {Name: "m:8b", Provider: "ollama", Fallbacks: []string{"backstop"}},
			"backstop": {Name: "b:8b", Provider: "ollama"},
		},
	}
	chain, err := cfg.RoleFallbackChain("chat")
	if err != nil {
		t.Fatalf("RoleFallbackChain: %v", err)
	}
	want := []string{"ollama/p:8b", "ollama/m:8b", "ollama/b:8b"}
	if len(chain) != len(want) {
		t.Fatalf("chain length = %d, want %d (got %v)", len(chain), len(want), chain)
	}
	for i, s := range want {
		if chain[i] != s {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i], s)
		}
	}
}

func TestConfig_RoleFallbackChain_DiamondDedupes(t *testing.T) {
	// Diamond: chat → [A, B], A → [shared], B → [shared].
	// Expected chain: chat, A, shared, B (shared appears once on first sight).
	cfg := &Config{
		Defaults: map[string]string{"chat": "root"},
		Models: map[string]ModelConfig{
			"root":   {Name: "r:8b", Provider: "ollama", Fallbacks: []string{"a", "b"}},
			"a":      {Name: "a:8b", Provider: "ollama", Fallbacks: []string{"shared"}},
			"b":      {Name: "b:8b", Provider: "ollama", Fallbacks: []string{"shared"}},
			"shared": {Name: "s:8b", Provider: "ollama"},
		},
	}
	chain, err := cfg.RoleFallbackChain("chat")
	if err != nil {
		t.Fatalf("RoleFallbackChain: %v", err)
	}
	want := []string{"ollama/r:8b", "ollama/a:8b", "ollama/s:8b", "ollama/b:8b"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i, s := range want {
		if chain[i] != s {
			t.Errorf("chain[%d] = %q, want %q", i, chain[i], s)
		}
	}
	// Assert that "ollama/s:8b" appears exactly once.
	count := 0
	for _, s := range chain {
		if s == "ollama/s:8b" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ollama/s:8b appeared %d times, want 1", count)
	}
}

func TestConfig_RoleFallbackChain_CycleErrors(t *testing.T) {
	cfg := &Config{
		Defaults: map[string]string{"chat": "a"},
		Models: map[string]ModelConfig{
			"a": {Name: "a:8b", Provider: "ollama", Fallbacks: []string{"b"}},
			"b": {Name: "b:8b", Provider: "ollama", Fallbacks: []string{"a"}},
		},
	}
	_, err := cfg.RoleFallbackChain("chat")
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	if !strings.Contains(err.Error(), "circular fallback") {
		t.Errorf("err = %v, want \"circular fallback\" in message", err)
	}
}

func TestConfig_RoleFallbackChain_UnknownUseCase(t *testing.T) {
	cfg := &Config{Defaults: map[string]string{}, Models: map[string]ModelConfig{}}
	_, err := cfg.RoleFallbackChain("unknown")
	if err == nil {
		t.Fatal("expected unknown-use-case error, got nil")
	}
}

func TestConfig_RoleFallbackChain_SideTaskFallbackUseCase(t *testing.T) {
	cfg := loadTestConfig(t)

	chain, err := cfg.RoleFallbackChain("summarize")
	if err != nil {
		t.Fatalf("RoleFallbackChain(\"summarize\"): %v", err)
	}
	want := []string{"ollama/qwen3.5:27b", "ollama/qwen3:8b"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}
}

func TestConfig_RoleFallbackChain_ApprovalFallbackUseCase(t *testing.T) {
	cfg := loadTestConfig(t)

	chain, err := cfg.RoleFallbackChain("approval")
	if err != nil {
		t.Fatalf("RoleFallbackChain(\"approval\"): %v", err)
	}
	want := []string{"ollama/qwen3.5:35b-a3b", "ollama/qwen3:8b"}
	if len(chain) != len(want) {
		t.Fatalf("chain = %v, want %v", chain, want)
	}
	for i := range want {
		if chain[i] != want[i] {
			t.Fatalf("chain = %v, want %v", chain, want)
		}
	}
}

func TestConfig_RoleFallbackChain_UnknownRole(t *testing.T) {
	cfg := &Config{
		Defaults: map[string]string{"chat": "missing"},
		Models:   map[string]ModelConfig{},
	}
	_, err := cfg.RoleFallbackChain("chat")
	if err == nil {
		t.Fatal("expected unknown-role error, got nil")
	}
}

func TestConfig_RoleChain_LinearAndDedupe(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"coding": {Name: "coder", Provider: "local", Fallbacks: []string{"fast"}},
			"fast":   {Name: "speedy", Provider: "local"},
		},
	}
	got, err := cfg.RoleChain("coding")
	if err != nil {
		t.Fatalf("RoleChain: %v", err)
	}
	want := []string{"local/coder", "local/speedy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RoleChain = %v, want %v", got, want)
	}
}

func TestConfig_RoleChain_UnknownRole(t *testing.T) {
	cfg := &Config{Models: map[string]ModelConfig{}}
	if _, err := cfg.RoleChain("coding"); err == nil {
		t.Fatal("RoleChain(unknown) = nil error, want error")
	}
}

func TestConfig_RoleChain_CycleErrors(t *testing.T) {
	cfg := &Config{
		Models: map[string]ModelConfig{
			"a": {Name: "a", Provider: "local", Fallbacks: []string{"b"}},
			"b": {Name: "b", Provider: "local", Fallbacks: []string{"a"}},
		},
	}
	if _, err := cfg.RoleChain("a"); err == nil {
		t.Fatal("RoleChain(cycle) = nil error, want error")
	}
}
