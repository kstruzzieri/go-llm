package provider

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// mockProvider is a minimal Provider implementation for Registry tests.
type mockProvider struct {
	name    string
	caps    Capability
	models  []ModelInfo
	healthy bool
	err     error
}

func (m *mockProvider) Name() string             { return m.name }
func (m *mockProvider) Capabilities() Capability  { return m.caps }

func (m *mockProvider) Health(_ context.Context) error {
	if !m.healthy {
		return m.err
	}
	return nil
}

func (m *mockProvider) Models(_ context.Context) ([]ModelInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.models, nil
}

func (m *mockProvider) Chat(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockProvider) ChatStream(_ context.Context, _ ChatRequest, _ func(ChatResponse) error) error {
	return fmt.Errorf("not implemented")
}

func (m *mockProvider) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockProvider) GenerateStream(_ context.Context, _ GenerateRequest, _ func(GenerateResponse) error) error {
	return fmt.Errorf("not implemented")
}

func (m *mockProvider) Embed(_ context.Context, _ EmbedRequest) (*EmbedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

// ---------------------------------------------------------------------------
// Core operations: Register, Get, Unregister, All
// ---------------------------------------------------------------------------

func TestRegistry_Register(t *testing.T) {
	r := NewRegistry()

	p := &mockProvider{name: "test-provider"}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register() error: %v", err)
	}

	// Verify the provider is retrievable.
	got, ok := r.Get("test-provider")
	if !ok {
		t.Fatal("expected provider to be found after Register")
	}
	if got.Name() != "test-provider" {
		t.Errorf("Name() = %q, want %q", got.Name(), "test-provider")
	}

	// Duplicate registration should fail.
	if err := r.Register(p); err == nil {
		t.Fatal("expected error for duplicate registration")
	}
}

func TestRegistry_Register_Nil(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestRegistry_Register_EmptyName(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: ""}
	if err := r.Register(p); err == nil {
		t.Fatal("expected error for empty provider name")
	}
}

func TestRegistry_Get(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	_ = r.Register(p)

	tests := []struct {
		name   string
		lookup string
		wantOK bool
	}{
		{"found", "ollama", true},
		{"not found", "missing", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := r.Get(tt.lookup)
			if ok != tt.wantOK {
				t.Errorf("Get(%q) ok = %v, want %v", tt.lookup, ok, tt.wantOK)
			}
			if tt.wantOK && got.Name() != "ollama" {
				t.Errorf("Get(%q) name = %q, want %q", tt.lookup, got.Name(), "ollama")
			}
		})
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	_ = r.Register(p)

	if err := r.Unregister("ollama"); err != nil {
		t.Fatalf("Unregister() error: %v", err)
	}

	if _, ok := r.Get("ollama"); ok {
		t.Error("expected provider to be removed after Unregister")
	}

	// Unregister again should fail.
	if err := r.Unregister("ollama"); err == nil {
		t.Fatal("expected error for unregistering non-existent provider")
	}
}

func TestRegistry_All(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(&mockProvider{name: "a"})
	_ = r.Register(&mockProvider{name: "b"})

	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() returned %d providers, want 2", len(all))
	}

	names := map[string]bool{}
	for _, p := range all {
		names[p.Name()] = true
	}
	if !names["a"] || !names["b"] {
		t.Errorf("All() names = %v, want {a, b}", names)
	}
}

func TestRegistry_All_Empty(t *testing.T) {
	r := NewRegistry()
	all := r.All()
	if len(all) != 0 {
		t.Errorf("All() on empty registry returned %d, want 0", len(all))
	}
}

// ---------------------------------------------------------------------------
// Resolution: Resolve, ProvidersForModel, RefreshModels
// ---------------------------------------------------------------------------

func TestRegistry_Resolve(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	_ = r.Register(p)

	tests := []struct {
		name    string
		key     ModelKey
		wantErr bool
	}{
		{"found", ModelKey{Provider: "ollama", Model: "qwen3:8b"}, false},
		{"not found", ModelKey{Provider: "missing", Model: "qwen3:8b"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.Resolve(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("Resolve() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got.Name() != "ollama" {
				t.Errorf("Resolve() provider name = %q, want %q", got.Name(), "ollama")
			}
		})
	}
}

func TestRegistry_RefreshModels(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{
		name: "ollama",
		models: []ModelInfo{
			{Name: "qwen3:8b"},
			{Name: "nomic-embed-text"},
		},
	}
	_ = r.Register(p)

	if err := r.RefreshModels(context.Background(), "ollama"); err != nil {
		t.Fatalf("RefreshModels() error: %v", err)
	}

	// Verify ProvidersForModel works after refresh.
	providers, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel(qwen3:8b) error: %v", err)
	}
	if len(providers) != 1 {
		t.Fatalf("expected 1 provider for qwen3:8b, got %d", len(providers))
	}
	if providers[0].Name() != "ollama" {
		t.Errorf("provider name = %q, want %q", providers[0].Name(), "ollama")
	}

	// Verify second model is also indexed.
	providers, err = r.ProvidersForModel("nomic-embed-text")
	if err != nil {
		t.Fatalf("ProvidersForModel(nomic-embed-text) error: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "ollama" {
		t.Errorf("unexpected providers for nomic-embed-text: %v", providers)
	}
}

func TestRegistry_RefreshModels_NotFound(t *testing.T) {
	r := NewRegistry()
	if err := r.RefreshModels(context.Background(), "missing"); err == nil {
		t.Fatal("expected error for missing provider")
	}
}

func TestRegistry_AddModelToIndex(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	if err := r.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := r.AddModelToIndex("qwen3:8b", "ollama"); err != nil {
		t.Fatalf("AddModelToIndex: %v", err)
	}

	providers, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "ollama" {
		t.Fatalf("providers = %v, want [ollama]", providers)
	}
}

func TestRegistry_AddModelToIndex_Idempotent(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	_ = r.Register(p)

	for i := 0; i < 3; i++ {
		if err := r.AddModelToIndex("qwen3:8b", "ollama"); err != nil {
			t.Fatalf("AddModelToIndex iteration %d: %v", i, err)
		}
	}

	providers, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("providers count = %d, want 1 (idempotency violated; duplicate entries materialised)", len(providers))
	}
}

func TestRegistry_AddModelToIndex_UnknownProvider(t *testing.T) {
	r := NewRegistry()
	if err := r.AddModelToIndex("qwen3:8b", "unregistered"); err == nil {
		t.Fatal("expected error for unregistered provider")
	}
	if _, err := r.ProvidersForModel("qwen3:8b"); err == nil {
		t.Fatal("ProvidersForModel must still report no providers after rejected AddModelToIndex")
	}
}

func TestRegistry_AddModelToIndex_EmptyArguments(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{name: "ollama"}
	_ = r.Register(p)

	if err := r.AddModelToIndex("", "ollama"); err == nil {
		t.Error("empty model must be rejected")
	}
	if err := r.AddModelToIndex("qwen3:8b", ""); err == nil {
		t.Error("empty provider name must be rejected")
	}
}

func TestRegistry_AddModelToIndex_CoexistsWithRefreshModels(t *testing.T) {
	// Seed via AddModelToIndex first, then RefreshModels: the bulk path must
	// not duplicate or lose the explicitly-seeded entry.
	r := NewRegistry()
	p := &mockProvider{
		name: "ollama",
		models: []ModelInfo{
			{Name: "qwen3:8b"},
			{Name: "nomic-embed-text"},
		},
	}
	_ = r.Register(p)

	if err := r.AddModelToIndex("qwen3:8b", "ollama"); err != nil {
		t.Fatalf("AddModelToIndex: %v", err)
	}
	if err := r.RefreshModels(context.Background(), "ollama"); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}

	providers, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel: %v", err)
	}
	if len(providers) != 1 {
		t.Errorf("providers = %d, want 1 (RefreshModels must not duplicate the seeded entry)", len(providers))
	}
}

func TestRegistry_RefreshModels_ProviderError(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{
		name: "broken",
		err:  fmt.Errorf("connection refused"),
	}
	_ = r.Register(p)

	err := r.RefreshModels(context.Background(), "broken")
	if err == nil {
		t.Fatal("expected error when provider.Models fails")
	}
}

func TestRegistry_RefreshModels_UpdatesIndex(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{
		name: "ollama",
		models: []ModelInfo{
			{Name: "model-a"},
		},
	}
	_ = r.Register(p)
	_ = r.RefreshModels(context.Background(), "ollama")

	// Verify model-a is indexed.
	_, err := r.ProvidersForModel("model-a")
	if err != nil {
		t.Fatalf("expected model-a to be indexed: %v", err)
	}

	// Update available models -- model-a removed, model-b added.
	p.models = []ModelInfo{
		{Name: "model-b"},
	}
	_ = r.RefreshModels(context.Background(), "ollama")

	// model-a should no longer be indexed.
	_, err = r.ProvidersForModel("model-a")
	if err == nil {
		t.Error("expected model-a to be removed from index after refresh")
	}

	// model-b should now be indexed.
	providers, err := r.ProvidersForModel("model-b")
	if err != nil {
		t.Fatalf("expected model-b to be indexed: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "ollama" {
		t.Errorf("unexpected providers for model-b: %v", providers)
	}
}

func TestRegistry_ProvidersForModel_NotFound(t *testing.T) {
	r := NewRegistry()
	_, err := r.ProvidersForModel("nonexistent")
	if err == nil {
		t.Fatal("expected error for model with no providers")
	}
}

func TestRegistry_ProvidersForModel_MultipleProviders(t *testing.T) {
	r := NewRegistry()
	p1 := &mockProvider{
		name:   "ollama",
		models: []ModelInfo{{Name: "qwen3:8b"}},
	}
	p2 := &mockProvider{
		name:   "lmstudio",
		models: []ModelInfo{{Name: "qwen3:8b"}, {Name: "llama3:8b"}},
	}
	_ = r.Register(p1)
	_ = r.Register(p2)
	_ = r.RefreshModels(context.Background(), "ollama")
	_ = r.RefreshModels(context.Background(), "lmstudio")

	providers, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel() error: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("expected 2 providers for qwen3:8b, got %d", len(providers))
	}

	// llama3:8b should only have lmstudio.
	providers, err = r.ProvidersForModel("llama3:8b")
	if err != nil {
		t.Fatalf("ProvidersForModel(llama3:8b) error: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "lmstudio" {
		t.Errorf("expected lmstudio for llama3:8b, got %v", providers)
	}
}

func TestRegistry_Unregister_ClearsModelIndex(t *testing.T) {
	r := NewRegistry()
	p := &mockProvider{
		name:   "ollama",
		models: []ModelInfo{{Name: "qwen3:8b"}},
	}
	_ = r.Register(p)
	_ = r.RefreshModels(context.Background(), "ollama")

	// Verify model is indexed.
	_, err := r.ProvidersForModel("qwen3:8b")
	if err != nil {
		t.Fatalf("expected model to be indexed: %v", err)
	}

	// Unregister.
	_ = r.Unregister("ollama")

	// Model should no longer be indexed.
	_, err = r.ProvidersForModel("qwen3:8b")
	if err == nil {
		t.Error("expected model to be removed from index after Unregister")
	}
}

func TestRegistry_Unregister_PreservesOtherProviderIndex(t *testing.T) {
	r := NewRegistry()
	p1 := &mockProvider{
		name:   "ollama",
		models: []ModelInfo{{Name: "shared-model"}},
	}
	p2 := &mockProvider{
		name:   "lmstudio",
		models: []ModelInfo{{Name: "shared-model"}},
	}
	_ = r.Register(p1)
	_ = r.Register(p2)
	_ = r.RefreshModels(context.Background(), "ollama")
	_ = r.RefreshModels(context.Background(), "lmstudio")

	// Unregister ollama -- lmstudio should still be indexed for the shared model.
	_ = r.Unregister("ollama")

	providers, err := r.ProvidersForModel("shared-model")
	if err != nil {
		t.Fatalf("expected shared-model to still be indexed: %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "lmstudio" {
		t.Errorf("expected only lmstudio for shared-model, got %v", providers)
	}
}

// ---------------------------------------------------------------------------
// Thread safety
// ---------------------------------------------------------------------------

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	const numGoroutines = 50

	// Pre-register a provider for read operations.
	seed := &mockProvider{
		name:   "seed",
		models: []ModelInfo{{Name: "seed-model"}},
	}
	_ = r.Register(seed)
	_ = r.RefreshModels(context.Background(), "seed")

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()

			name := fmt.Sprintf("provider-%d", idx)
			p := &mockProvider{
				name:   name,
				models: []ModelInfo{{Name: fmt.Sprintf("model-%d", idx)}},
			}

			// Register.
			_ = r.Register(p)

			// Get.
			r.Get(name)

			// All.
			r.All()

			// Resolve.
			_, _ = r.Resolve(ModelKey{Provider: name, Model: "any"})

			// RefreshModels.
			_ = r.RefreshModels(context.Background(), name)

			// ProvidersForModel.
			_, _ = r.ProvidersForModel("seed-model")

			// Unregister.
			_ = r.Unregister(name)
		}(i)
	}

	wg.Wait()

	// The seed provider should still be intact.
	if _, ok := r.Get("seed"); !ok {
		t.Error("seed provider should still exist after concurrent operations")
	}
}
