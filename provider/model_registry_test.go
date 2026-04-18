package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ---------------------------------------------------------------------------
// Test helpers: mock provider resolver and mock providers for ModelRegistry
// ---------------------------------------------------------------------------

// mrMockProviderRegistry implements ProviderResolver for ModelRegistry tests.
// The prefix "mr" distinguishes it from the mockProvider in registry_test.go.
type mrMockProviderRegistry struct {
	providers map[string]Provider
}

func (m *mrMockProviderRegistry) Resolve(key ModelKey) (Provider, error) {
	p, ok := m.providers[key.Provider]
	if !ok {
		return nil, fmt.Errorf("provider: unknown provider %q", key.Provider)
	}
	return p, nil
}

func (m *mrMockProviderRegistry) ProvidersForModel(model string) ([]Provider, error) {
	var matches []Provider
	for _, p := range m.providers {
		models, _ := p.Models(context.Background())
		for _, mi := range models {
			if mi.Name == model {
				matches = append(matches, p)
			}
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("provider: no provider found for model %q", model)
	}
	return matches, nil
}

func (m *mrMockProviderRegistry) All() []Provider {
	all := make([]Provider, 0, len(m.providers))
	for _, p := range m.providers {
		all = append(all, p)
	}
	return all
}

// mrMockProvider implements a minimal Provider for ModelRegistry tests.
type mrMockProvider struct {
	name   string
	models []ModelInfo
	caps   Capability
}

func (m *mrMockProvider) Name() string             { return m.name }
func (m *mrMockProvider) Capabilities() Capability { return m.caps }

func (m *mrMockProvider) Health(_ context.Context) error { return nil }

func (m *mrMockProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return m.models, nil
}

func (m *mrMockProvider) Chat(_ context.Context, _ ChatRequest) (*ChatResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mrMockProvider) ChatStream(_ context.Context, _ ChatRequest, _ func(ChatResponse) error) error {
	return errors.New("not implemented")
}

func (m *mrMockProvider) Generate(_ context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	return nil, errors.New("not implemented")
}

func (m *mrMockProvider) GenerateStream(_ context.Context, _ GenerateRequest, _ func(GenerateResponse) error) error {
	return errors.New("not implemented")
}

func (m *mrMockProvider) Embed(_ context.Context, _ EmbedRequest) (*EmbedResponse, error) {
	return nil, errors.New("not implemented")
}

// mrCountingProvider wraps mrMockProvider to count Models() calls.
type mrCountingProvider struct {
	*mrMockProvider
	callCount *int
}

func (c *mrCountingProvider) Models(_ context.Context) ([]ModelInfo, error) {
	*c.callCount++
	return c.models, nil
}

// mrMockFingerprintStore implements fingerprint.Store for testing.
type mrMockFingerprintStore struct {
	profiles map[string]*fingerprint.Profile
}

func newMrMockFingerprintStore() *mrMockFingerprintStore {
	return &mrMockFingerprintStore{
		profiles: make(map[string]*fingerprint.Profile),
	}
}

func (m *mrMockFingerprintStore) Get(_ context.Context, backendID, modelName string) (*fingerprint.Profile, error) {
	key := backendID + "\x00" + modelName
	p, ok := m.profiles[key]
	if !ok {
		return nil, fingerprint.ErrNotFound
	}
	return p, nil
}

func (m *mrMockFingerprintStore) GetFailure(_ context.Context, _, _ string) (*fingerprint.FailureInfo, error) {
	return nil, fingerprint.ErrNotFound
}

func (m *mrMockFingerprintStore) Save(_ context.Context, profile fingerprint.Profile) error {
	key := profile.BackendID + "\x00" + profile.ModelName
	m.profiles[key] = &profile
	return nil
}

func (m *mrMockFingerprintStore) NeedsFingerprint(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}

func (m *mrMockFingerprintStore) SaveFailure(_ context.Context, _, _, _, _ string) error {
	return nil
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Lookup_CatalogMatch
// ---------------------------------------------------------------------------

func TestModelRegistry_Lookup_CatalogMatch(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream | CapEmbed,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 40960,
				Digest:        "abc123",
				Capabilities:  []string{"completion", "tools"},
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	// Verify runtime fields merged.
	if profile.Key != key {
		t.Errorf("Key = %v, want %v", profile.Key, key)
	}
	if profile.Resources.ParameterSize != "8B" {
		t.Errorf("ParameterSize = %q, want %q", profile.Resources.ParameterSize, "8B")
	}
	if profile.Resources.QuantLevel != "Q4_K_M" {
		t.Errorf("QuantLevel = %q, want %q", profile.Resources.QuantLevel, "Q4_K_M")
	}
	if profile.ContextWindow != 40960 {
		t.Errorf("ContextWindow = %d, want %d", profile.ContextWindow, 40960)
	}
	if profile.Digest != "abc123" {
		t.Errorf("Digest = %q, want %q", profile.Digest, "abc123")
	}

	// Verify static catalog fields merged (qwen3 has FIM + toggle think).
	if profile.FIM == nil {
		t.Fatal("expected FIM config from catalog, got nil")
	}
	if profile.FIM.Prefix != "<|fim_prefix|>" {
		t.Errorf("FIM.Prefix = %q, want %q", profile.FIM.Prefix, "<|fim_prefix|>")
	}
	if profile.ThinkMode != ThinkToggle {
		t.Errorf("ThinkMode = %v, want ThinkToggle", profile.ThinkMode)
	}

	// Verify source is merged.
	if profile.Source != SourceMerged {
		t.Errorf("Source = %v, want SourceMerged", profile.Source)
	}

	// Verify family was resolved.
	if profile.Family != "qwen3" {
		t.Errorf("Family = %q, want %q", profile.Family, "qwen3")
	}

	// Verify catalog-provided resource data was populated.
	if profile.Resources.RAMRequired == 0 {
		t.Error("expected RAMRequired > 0 from catalog")
	}
}

func TestModelRegistry_Lookup_LatestVariantCatalogMatch(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapEmbed,
		models: []ModelInfo{
			{
				Name:          "nomic-embed-text:latest",
				Family:        "nomic-embed-text",
				ParameterSize: "137M",
				ContextWindow: 8192,
				Digest:        "embed123",
				Capabilities:  []string{"embedding"},
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "nomic-embed-text:latest"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	if profile.Resources.RAMRequired != 0.5 {
		t.Errorf("RAMRequired = %f, want 0.5", profile.Resources.RAMRequired)
	}
	if profile.Resources.RAMRecommended != 1.0 {
		t.Errorf("RAMRecommended = %f, want 1.0", profile.Resources.RAMRecommended)
	}
	if profile.Quality != TierGood {
		t.Errorf("Quality = %v, want %v", profile.Quality, TierGood)
	}
	if profile.Speed != TierGreat {
		t.Errorf("Speed = %v, want %v", profile.Speed, TierGreat)
	}
}

func TestModelRegistry_Lookup_Qwen3CoderNextLatestCatalogMatch(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{
				Name:          "qwen3-coder-next:latest",
				Family:        "qwen3-coder-next",
				ParameterSize: "80B",
				ContextWindow: 262144,
				Digest:        "coder123",
				Capabilities:  []string{"completion", "tools"},
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3-coder-next:latest"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	if profile.Resources.RAMRequired != 46.0 {
		t.Errorf("RAMRequired = %f, want 46.0", profile.Resources.RAMRequired)
	}
	if profile.Resources.RAMRecommended != 56.0 {
		t.Errorf("RAMRecommended = %f, want 56.0", profile.Resources.RAMRecommended)
	}
	if profile.Quality != TierBest {
		t.Errorf("Quality = %v, want %v", profile.Quality, TierBest)
	}
	if profile.Speed != TierGreat {
		t.Errorf("Speed = %v, want %v", profile.Speed, TierGreat)
	}
	if profile.FIM == nil {
		t.Fatal("expected FIM config from catalog, got nil")
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Lookup_UnknownModel
// ---------------------------------------------------------------------------

func TestModelRegistry_Lookup_UnknownModel(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{
				Name:          "some-new-model:7b",
				Family:        "somenew",
				ParameterSize: "7B",
				QuantLevel:    "Q4_K_M",
				ContextWindow: 8192,
				Digest:        "new123",
				Capabilities:  []string{"completion"},
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "some-new-model:7b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	// Dynamic inference should still produce a usable profile.
	if profile.ContextWindow != 8192 {
		t.Errorf("ContextWindow = %d, want %d", profile.ContextWindow, 8192)
	}
	if profile.Resources.ParameterSize != "7B" {
		t.Errorf("ParameterSize = %q, want %q", profile.Resources.ParameterSize, "7B")
	}
	// RAM should be estimated from param size + quant.
	if profile.Resources.RAMRequired <= 0 {
		t.Errorf("expected estimated RAMRequired > 0, got %f", profile.Resources.RAMRequired)
	}
	// Quality should be inferred from 7B param count.
	if profile.Quality != TierGood {
		t.Errorf("Quality = %v, want TierGood (inferred from 7B)", profile.Quality)
	}
	// Source should still be merged (even though catalog didn't match).
	if profile.Source != SourceMerged {
		t.Errorf("Source = %v, want SourceMerged", profile.Source)
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Lookup_CacheHit
// ---------------------------------------------------------------------------

func TestModelRegistry_Lookup_CacheHit(t *testing.T) {
	ctx := context.Background()

	callCount := 0
	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				ContextWindow: 40960,
				Digest:        "abc123",
			},
		},
	}

	countingProv := &mrCountingProvider{
		mrMockProvider: prov,
		callCount:      &callCount,
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": countingProv},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// First lookup -- should call provider.
	_, err = mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("first Lookup() error: %v", err)
	}

	// Second lookup -- should use cache, no provider call.
	_, err = mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("second Lookup() error: %v", err)
	}

	if callCount != 1 {
		t.Errorf("expected 1 provider call, got %d (cache miss on second call)", callCount)
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Refresh
// ---------------------------------------------------------------------------

func TestModelRegistry_Refresh(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				ContextWindow: 40960,
				Digest:        "abc123",
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// Lookup with initial digest.
	profile1, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("first Lookup() error: %v", err)
	}
	if profile1.Digest != "abc123" {
		t.Fatalf("initial Digest = %q, want %q", profile1.Digest, "abc123")
	}

	// Simulate model update by changing digest.
	prov.models[0].Digest = "def456"

	// Normal Lookup should still return cached (stale) profile.
	profile2, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("cached Lookup() error: %v", err)
	}
	if profile2.Digest != "abc123" {
		t.Errorf("cached Digest = %q, want %q (should be stale)", profile2.Digest, "abc123")
	}

	// Refresh should detect staleness and re-merge.
	profile3, err := mr.Refresh(ctx, key)
	if err != nil {
		t.Fatalf("Refresh() error: %v", err)
	}
	if profile3.Digest != "def456" {
		t.Errorf("refreshed Digest = %q, want %q", profile3.Digest, "def456")
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_LookupAny
// ---------------------------------------------------------------------------

func TestModelRegistry_LookupAny(t *testing.T) {
	ctx := context.Background()

	p1 := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", ContextWindow: 40960, Digest: "aaa"},
		},
	}

	p2 := &mrMockProvider{
		name: "lmstudio",
		caps: CapChat,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", ContextWindow: 40960, Digest: "bbb"},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": p1, "lmstudio": p2},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	profiles, err := mr.LookupAny(ctx, "qwen3:8b")
	if err != nil {
		t.Fatalf("LookupAny() error: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("LookupAny() returned %d profiles, want 2", len(profiles))
	}

	// Should have one from each provider.
	providerNames := make(map[string]bool)
	for _, p := range profiles {
		providerNames[p.Key.Provider] = true
	}
	if !providerNames["ollama"] || !providerNames["lmstudio"] {
		t.Errorf("expected profiles from both providers, got %v", providerNames)
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_FIMConfigFor
// ---------------------------------------------------------------------------

func TestModelRegistry_FIMConfigFor(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				ContextWindow: 40960,
				Digest:        "abc123",
			},
			{
				Name:          "llama3.1:8b",
				Family:        "llama",
				ParameterSize: "8B",
				ContextWindow: 131072,
				Digest:        "xyz789",
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	// qwen3 has FIM tokens in the catalog.
	fimCfg, err := mr.FIMConfigFor(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("FIMConfigFor(qwen3) error: %v", err)
	}
	if fimCfg == nil {
		t.Fatal("expected FIMConfig for qwen3, got nil")
	}
	if fimCfg.Prefix != "<|fim_prefix|>" {
		t.Errorf("FIM.Prefix = %q, want %q", fimCfg.Prefix, "<|fim_prefix|>")
	}

	// llama3.1 has no FIM in the catalog.
	fimCfg, err = mr.FIMConfigFor(ctx, ModelKey{Provider: "ollama", Model: "llama3.1:8b"})
	if err != nil {
		t.Fatalf("FIMConfigFor(llama3.1) error: %v", err)
	}
	if fimCfg != nil {
		t.Errorf("expected nil FIMConfig for llama3.1, got %+v", fimCfg)
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Recommend
// ---------------------------------------------------------------------------

func TestModelRegistry_Recommend(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapEmbed | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", ContextWindow: 40960, Digest: "a1", Capabilities: []string{"completion", "tools"}},
			{Name: "qwen3:32b", Family: "qwen3", ParameterSize: "32B", ContextWindow: 40960, Digest: "a2", Capabilities: []string{"completion", "tools"}},
			{Name: "nomic-embed-text:latest", Family: "nomic-embed-text", ParameterSize: "137M", ContextWindow: 8192, Digest: "a3", Capabilities: []string{"embedding"}},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	t.Run("filter by CapGenerate", func(t *testing.T) {
		profiles, err := mr.Recommend(ctx, RecommendOpts{
			RequiredCaps: CapGenerate,
		})
		if err != nil {
			t.Fatalf("Recommend() error: %v", err)
		}

		// Should exclude embedding model which lacks CapGenerate.
		for _, p := range profiles {
			if p.Caps&CapGenerate == 0 {
				t.Errorf("recommended model %q lacks CapGenerate", p.Name)
			}
		}
		if len(profiles) == 0 {
			t.Fatal("expected at least one recommendation with CapGenerate")
		}
	})

	t.Run("filter by CapGenerate and RAM", func(t *testing.T) {
		profiles, err := mr.Recommend(ctx, RecommendOpts{
			RequiredCaps: CapGenerate,
			AvailableRAM: 10.0,
		})
		if err != nil {
			t.Fatalf("Recommend() error: %v", err)
		}

		// 32b model requires ~20GB, so it should be excluded with 10GB limit.
		for _, p := range profiles {
			if p.Resources.RAMRequired > 10.0 {
				t.Errorf("recommended model %q needs %.1f GB RAM, exceeds 10 GB limit",
					p.Name, p.Resources.RAMRequired)
			}
		}
	})

	t.Run("filter by CapEmbed for embedding", func(t *testing.T) {
		profiles, err := mr.Recommend(ctx, RecommendOpts{
			RequiredCaps: CapEmbed,
		})
		if err != nil {
			t.Fatalf("Recommend(embedding) error: %v", err)
		}

		if len(profiles) == 0 {
			t.Fatal("expected at least one embedding recommendation")
		}
		for _, p := range profiles {
			if p.Caps&CapEmbed == 0 {
				t.Errorf("recommended model %q lacks CapEmbed", p.Name)
			}
		}
	})

	t.Run("filter latest-only variant by RAM", func(t *testing.T) {
		profiles, err := mr.Recommend(ctx, RecommendOpts{
			RequiredCaps: CapEmbed,
			AvailableRAM: 0.4,
		})
		if err != nil {
			t.Fatalf("Recommend(embedding, RAM) error: %v", err)
		}
		if len(profiles) != 0 {
			t.Fatalf("expected no recommendations under 0.4 GB RAM, got %d", len(profiles))
		}
	})

	t.Run("sorted by quality descending", func(t *testing.T) {
		profiles, err := mr.Recommend(ctx, RecommendOpts{
			RequiredCaps: CapGenerate,
		})
		if err != nil {
			t.Fatalf("Recommend() error: %v", err)
		}

		for i := 1; i < len(profiles); i++ {
			if profiles[i].Quality > profiles[i-1].Quality {
				t.Errorf("profiles not sorted: %q (quality=%v) ranked after %q (quality=%v)",
					profiles[i].Name, profiles[i].Quality,
					profiles[i-1].Name, profiles[i-1].Quality)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// TestModelRegistry_FingerprintEnrichment
// ---------------------------------------------------------------------------

func TestModelRegistry_FingerprintEnrichment(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				ContextWindow: 40960,
				Digest:        "abc123",
			},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	fpStore := newMrMockFingerprintStore()

	// Pre-populate fingerprint data.
	fpStore.profiles["ollama\x00qwen3:8b"] = &fingerprint.Profile{
		BackendID:                 "ollama",
		ModelName:                 "qwen3:8b",
		ModelDigest:               "abc123",
		ModelKind:                 fingerprint.ModelKindChat,
		Capabilities:              []string{"completion", "tools"},
		GenerationTokensPerSecond: 42.5,
		PromptLatency:             50 * time.Millisecond,
		PeakMemoryMB:              6500,
		GPULayersUsed:             33,
		TestedAt:                  time.Now(),
		ProfileVersion:            1,
	}

	mr, err := NewModelRegistry(reg, fpStore)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	// Verify the profile was enriched from fingerprint data.
	if profile.Source != SourceMerged {
		t.Errorf("Source = %v, want SourceMerged", profile.Source)
	}
	if profile.FIM == nil {
		t.Error("expected FIM config from catalog after merge")
	}

	// Fingerprint reported 6500MB peak -- verify RAM was updated.
	// 6500 MB = 6.3476 GB, which should be at least as high as
	// the catalog's 5.0 GB for qwen3:8b.
	expectedMinRAM := 6500.0 / 1024.0
	if profile.Resources.RAMRequired < expectedMinRAM {
		t.Errorf("RAMRequired = %.2f, expected >= %.2f (from fingerprint PeakMemoryMB=6500)",
			profile.Resources.RAMRequired, expectedMinRAM)
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_NilFingerprintStore
// ---------------------------------------------------------------------------

func TestModelRegistry_NilFingerprintStore(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", ContextWindow: 40960, Digest: "abc"},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	// Pass nil fingerprint store -- should not panic.
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if profile.FIM == nil {
		t.Error("expected FIM config from catalog even without fingerprint store")
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Lookup_ProviderError
// ---------------------------------------------------------------------------

func TestModelRegistry_Lookup_ProviderError(t *testing.T) {
	ctx := context.Background()

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	// Unknown provider should return error.
	_, err = mr.Lookup(ctx, ModelKey{Provider: "missing", Model: "qwen3:8b"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_Lookup_ModelNotFound
// ---------------------------------------------------------------------------

func TestModelRegistry_Lookup_ModelNotFound(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name:   "ollama",
		caps:   CapChat,
		models: []ModelInfo{},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	_, err = mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "nonexistent:7b"})
	if err == nil {
		t.Fatal("expected error for model not found on provider")
	}
}

// ---------------------------------------------------------------------------
// TestModelRegistry_All
// ---------------------------------------------------------------------------

func TestModelRegistry_All(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapEmbed,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", ContextWindow: 40960, Digest: "a1"},
			{Name: "llama3:8b", Family: "llama3", ParameterSize: "8B", ContextWindow: 8192, Digest: "a2"},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"ollama": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	profiles, err := mr.All(ctx)
	if err != nil {
		t.Fatalf("All() error: %v", err)
	}

	if len(profiles) != 2 {
		t.Fatalf("All() returned %d profiles, want 2", len(profiles))
	}

	names := make(map[string]bool)
	for _, p := range profiles {
		names[p.Name] = true
	}
	if !names["qwen3:8b"] || !names["llama3:8b"] {
		t.Errorf("All() names = %v, want qwen3:8b and llama3:8b", names)
	}
}

// ---------------------------------------------------------------------------
// TestEstimateResources
// ---------------------------------------------------------------------------

func TestEstimateResources(t *testing.T) {
	tests := []struct {
		name      string
		paramSize string
		quant     string
		wantMin   float64 // minimum RAM required (sanity check)
		wantMax   float64 // maximum RAM required (sanity check)
	}{
		{
			name:      "8B Q4_K_M",
			paramSize: "8B",
			quant:     "Q4_K_M",
			wantMin:   4.0,
			wantMax:   8.0,
		},
		{
			name:      "70B Q4_K_M",
			paramSize: "70B",
			quant:     "Q4_K_M",
			wantMin:   40.0,
			wantMax:   55.0,
		},
		{
			name:      "8B FP16",
			paramSize: "8B",
			quant:     "fp16",
			wantMin:   15.0,
			wantMax:   25.0,
		},
		{
			name:      "137M Q4_K_M (embedding model)",
			paramSize: "137M",
			quant:     "Q4_K_M",
			wantMin:   0.05,
			wantMax:   0.3,
		},
		{
			name:      "8x7B Q4_K_M (MoE)",
			paramSize: "8x7B",
			quant:     "Q4_K_M",
			wantMin:   30.0,
			wantMax:   45.0,
		},
		{
			name:      "empty param size",
			paramSize: "",
			quant:     "Q4_K_M",
			wantMin:   0,
			wantMax:   0,
		},
		{
			name:      "3.8B Q4_K_M",
			paramSize: "3.8B",
			quant:     "Q4_K_M",
			wantMin:   2.0,
			wantMax:   4.0,
		},
		{
			name:      "8B Q8_0",
			paramSize: "8B",
			quant:     "Q8_0",
			wantMin:   8.0,
			wantMax:   12.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rp := estimateResources(tt.paramSize, tt.quant)
			if rp.RAMRequired < tt.wantMin {
				t.Errorf("RAMRequired = %.2f, want >= %.2f", rp.RAMRequired, tt.wantMin)
			}
			if rp.RAMRequired > tt.wantMax {
				t.Errorf("RAMRequired = %.2f, want <= %.2f", rp.RAMRequired, tt.wantMax)
			}
			// Recommended should be at least as high as required. For very small
			// models (e.g. 137M), rounding to one decimal can make them equal.
			if rp.RAMRequired > 0 && rp.RAMRecommended < rp.RAMRequired {
				t.Errorf("RAMRecommended (%.2f) should be >= RAMRequired (%.2f)",
					rp.RAMRecommended, rp.RAMRequired)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestInferProfile
// ---------------------------------------------------------------------------

func TestInferProfile(t *testing.T) {
	tests := []struct {
		name          string
		info          *ModelInfo
		wantThink     ThinkMode
		wantQuality   Tier
		wantSpeed     Tier
		wantNilResult bool
	}{
		{
			name:          "nil info",
			info:          nil,
			wantNilResult: true,
		},
		{
			name:        "small model (2B)",
			info:        &ModelInfo{ParameterSize: "2B", Family: "unknown"},
			wantThink:   ThinkAuto,
			wantQuality: TierBasic,
			wantSpeed:   TierGreat,
		},
		{
			name:        "medium model (8B)",
			info:        &ModelInfo{ParameterSize: "8B", Family: "unknown"},
			wantThink:   ThinkAuto,
			wantQuality: TierGood,
			wantSpeed:   TierGood,
		},
		{
			name:        "large model (70B)",
			info:        &ModelInfo{ParameterSize: "70B", Family: "unknown"},
			wantThink:   ThinkAuto,
			wantQuality: TierGreat,
			wantSpeed:   TierBasic,
		},
		{
			name:        "qwen3 family infers toggle think",
			info:        &ModelInfo{ParameterSize: "8B", Family: "qwen3"},
			wantThink:   ThinkToggle,
			wantQuality: TierGood,
			wantSpeed:   TierGood,
		},
		{
			name:        "deepseek-r1 family infers always think",
			info:        &ModelInfo{ParameterSize: "8B", Family: "deepseek-r1"},
			wantThink:   ThinkAlways,
			wantQuality: TierGood,
			wantSpeed:   TierGood,
		},
		{
			name:        "gemma3 family infers toggle think",
			info:        &ModelInfo{ParameterSize: "12B", Family: "gemma3"},
			wantThink:   ThinkToggle,
			wantQuality: TierGood,
			wantSpeed:   TierGood,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := inferProfile(tt.info)
			if tt.wantNilResult {
				if result != nil {
					t.Errorf("expected nil result, got %+v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if result.ThinkMode != tt.wantThink {
				t.Errorf("ThinkMode = %v, want %v", result.ThinkMode, tt.wantThink)
			}
			if result.Quality != tt.wantQuality {
				t.Errorf("Quality = %v, want %v", result.Quality, tt.wantQuality)
			}
			if result.Speed != tt.wantSpeed {
				t.Errorf("Speed = %v, want %v", result.Speed, tt.wantSpeed)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestParseParamCount
// ---------------------------------------------------------------------------

func TestParseParamCount(t *testing.T) {
	tests := []struct {
		input string
		want  float64
	}{
		{"8B", 8.0},
		{"70b", 70.0},
		{"3.8B", 3.8},
		{"0.6b", 0.6},
		{"137M", 0.137},
		{"8x7b", 56.0},
		{"", 0},
		{"invalid", 0},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseParamCount(tt.input)
			if got != tt.want {
				t.Errorf("parseParamCount(%q) = %f, want %f", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestQuantBytesPerParam
// ---------------------------------------------------------------------------

func TestQuantBytesPerParam(t *testing.T) {
	tests := []struct {
		quant string
		want  float64
	}{
		{"Q4_K_M", 0.55},
		{"q4_0", 0.55},
		{"Q5_K_M", 0.7},
		{"Q6_K", 0.8},
		{"Q8_0", 1.0},
		{"fp16", 2.0},
		{"f16", 2.0},
		{"fp32", 4.0},
		{"f32", 4.0},
		{"", 0.55},        // default
		{"unknown", 0.55}, // default
	}

	for _, tt := range tests {
		t.Run(tt.quant, func(t *testing.T) {
			got := quantBytesPerParam(tt.quant)
			if got != tt.want {
				t.Errorf("quantBytesPerParam(%q) = %f, want %f", tt.quant, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestSortProfiles
// ---------------------------------------------------------------------------

func TestSortProfiles(t *testing.T) {
	profiles := []*ModelProfile{
		{Name: "basic-fast", Quality: TierBasic, Speed: TierGreat},
		{Name: "great-slow", Quality: TierGreat, Speed: TierBasic},
		{Name: "good-medium", Quality: TierGood, Speed: TierGood},
		{Name: "great-fast", Quality: TierGreat, Speed: TierGreat},
		{Name: "good-fast", Quality: TierGood, Speed: TierGreat},
	}

	sortProfiles(profiles)

	// Expected order: great-fast, great-slow, good-fast, good-medium, basic-fast
	expected := []string{"great-fast", "great-slow", "good-fast", "good-medium", "basic-fast"}
	for i, want := range expected {
		if profiles[i].Name != want {
			t.Errorf("position %d: got %q, want %q", i, profiles[i].Name, want)
		}
	}
}
