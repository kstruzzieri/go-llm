package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	name       string
	models     []ModelInfo
	modelsErr  error
	details    map[string]ModelInfo
	detailsErr error
	caps       Capability
}

func (m *mrMockProvider) Name() string             { return m.name }
func (m *mrMockProvider) Capabilities() Capability { return m.caps }

func (m *mrMockProvider) Health(_ context.Context) error { return nil }

func (m *mrMockProvider) Models(_ context.Context) ([]ModelInfo, error) {
	if m.modelsErr != nil {
		return nil, m.modelsErr
	}
	return m.models, nil
}

func (m *mrMockProvider) ModelInfo(_ context.Context, name string) (*ModelInfo, error) {
	if m.detailsErr != nil {
		return nil, m.detailsErr
	}
	if detail, ok := m.details[name]; ok {
		copy := detail
		return &copy, nil
	}
	return nil, fmt.Errorf("provider: model %q not found", name)
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

type mrRecordingFingerprintProber struct {
	detectCalls int
	chatCalls   int
}

func (m *mrRecordingFingerprintProber) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	m.detectCalls++
	return &fingerprint.KindDetection{
		Kind:         fingerprint.ModelKindChat,
		Source:       "capabilities",
		Capabilities: []string{"chat"},
	}, nil
}

func (m *mrRecordingFingerprintProber) ProbeChat(context.Context, string, any) (*fingerprint.ChatMetrics, error) {
	m.chatCalls++
	return &fingerprint.ChatMetrics{TokensPerSecond: 42}, nil
}

func (m *mrRecordingFingerprintProber) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("embedding should not be probed")
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
	if len(profile.FIM.StopTokens) != 2 {
		t.Errorf("StopTokens len = %d, want 2", len(profile.FIM.StopTokens))
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

func TestModelRegistry_ContextWindowOverrideWins(t *testing.T) {
	ctx := context.Background()
	prov := &mrMockProvider{name: "test", models: []ModelInfo{{Name: "model", ContextWindow: 65_536}}}
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{"test": prov}}, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	key := ModelKey{Provider: "test", Model: "model"}
	mr.SetContextWindowOverride(func(got ModelKey) int {
		if got == key {
			return 32_768
		}
		return 0
	})
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if profile.ContextWindow != 32_768 {
		t.Fatalf("ContextWindow = %d, want configured override 32768", profile.ContextWindow)
	}
}

func TestModelRegistry_FingerprintContextWindowFallback(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "test", Model: "unknown-model"}
	prov := &mrMockProvider{name: "test", models: []ModelInfo{{Name: key.Model}}}
	store := newMrMockFingerprintStore()
	store.profiles["test\x00unknown-model"] = &fingerprint.Profile{
		BackendID: key.Provider, ModelName: key.Model, ModelDigest: key.String(),
		ProfileVersion: fingerprint.CurrentProfileVersion, EffectiveContext: 24_576,
	}
	prober := &mrRecordingFingerprintProber{}
	mr, err := NewModelRegistry(
		&mrMockProviderRegistry{providers: map[string]Provider{"test": prov}},
		store,
		WithReadOnlyFingerprintProfiles(func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
			return &FingerprintProberSpec{Prober: prober, ModelDigest: key.String()}, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if profile.ContextWindow != 24_576 {
		t.Fatalf("ContextWindow = %d, want fingerprint fallback 24576", profile.ContextWindow)
	}
	if prober.detectCalls != 0 || prober.chatCalls != 0 {
		t.Fatalf("read-only fingerprint path probed: detect=%d chat=%d", prober.detectCalls, prober.chatCalls)
	}
}

func TestModelRegistry_ReadOnlyFingerprintRejectsStaleProfile(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "test", Model: "unknown-model"}
	for _, tt := range []struct {
		name    string
		digest  string
		version int
	}{
		{name: "digest mismatch", digest: "old-digest", version: fingerprint.CurrentProfileVersion},
		{name: "obsolete profile version", digest: key.String(), version: fingerprint.CurrentProfileVersion - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prov := &mrMockProvider{name: "test", models: []ModelInfo{{Name: key.Model}}}
			store := newMrMockFingerprintStore()
			store.profiles["test\x00unknown-model"] = &fingerprint.Profile{
				BackendID: key.Provider, ModelName: key.Model, ModelDigest: tt.digest,
				ProfileVersion: tt.version, EffectiveContext: 65_536,
			}
			mr, err := NewModelRegistry(
				&mrMockProviderRegistry{providers: map[string]Provider{"test": prov}},
				store,
				WithReadOnlyFingerprintProfiles(func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
					return &FingerprintProberSpec{Prober: &mrRecordingFingerprintProber{}, ModelDigest: key.String()}, nil
				}),
			)
			if err != nil {
				t.Fatalf("NewModelRegistry: %v", err)
			}
			profile, err := mr.Lookup(ctx, key)
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if profile.ContextWindow != 0 {
				t.Fatalf("ContextWindow = %d, want stale fingerprint ignored", profile.ContextWindow)
			}
		})
	}
}

func TestModelRegistry_Lookup_RunsFingerprintProberFactory(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "openai-local",
		caps: CapChat,
		models: []ModelInfo{
			{
				Name: "llama-local",
			},
		},
	}
	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"openai-local": prov},
	}
	fpStore := newMrMockFingerprintStore()
	prober := &mrRecordingFingerprintProber{}
	var factoryKey ModelKey
	var factoryRuntime *ModelInfo
	var factoryProvider Provider
	mr, err := NewModelRegistry(reg, fpStore, WithFingerprintProberFactory(
		func(_ context.Context, key ModelKey, runtime *ModelInfo, p Provider) (*FingerprintProberSpec, error) {
			factoryKey = key
			factoryRuntime = runtime
			factoryProvider = p
			return &FingerprintProberSpec{
				Prober:      prober,
				ModelDigest: "config-caps:chat",
			}, nil
		},
	))
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	key := ModelKey{Provider: "openai-local", Model: "llama-local"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if factoryKey != key {
		t.Fatalf("factory key = %v, want %v", factoryKey, key)
	}
	if factoryRuntime == nil || factoryRuntime.Name != "llama-local" {
		t.Fatalf("factory runtime = %+v, want llama-local", factoryRuntime)
	}
	if factoryProvider != prov {
		t.Fatalf("factory provider = %T, want original provider", factoryProvider)
	}
	if prober.detectCalls != 1 {
		t.Fatalf("DetectKind calls = %d, want 1", prober.detectCalls)
	}
	if prober.chatCalls != 1 {
		t.Fatalf("ProbeChat calls = %d, want 1", prober.chatCalls)
	}
	if profile.Caps&CapChat == 0 {
		t.Fatalf("profile Caps = %v, want CapChat from fingerprint profile", profile.Caps)
	}
	saved, err := fpStore.Get(ctx, "openai-local", "llama-local")
	if err != nil {
		t.Fatalf("fingerprint profile was not saved: %v", err)
	}
	if saved.ModelDigest != "config-caps:chat" {
		t.Fatalf("saved ModelDigest = %q, want factory digest", saved.ModelDigest)
	}
	if saved.GenerationTokensPerSecond != 42 {
		t.Fatalf("saved GenerationTokensPerSecond = %v, want 42", saved.GenerationTokensPerSecond)
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

func TestModelRegistry_Lookup_Qwen35NineBMTPUsesCatalog(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "llamacpp",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3.5:9b-mtp"},
		},
	}

	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{"llamacpp": prov},
	}

	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "llamacpp", Model: "qwen3.5:9b-mtp"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}

	if profile.Resources.RAMRequired != 6.0 {
		t.Errorf("RAMRequired = %f, want 6.0", profile.Resources.RAMRequired)
	}
	if profile.Resources.RAMRecommended != 9.0 {
		t.Errorf("RAMRecommended = %f, want 9.0", profile.Resources.RAMRecommended)
	}
	if profile.Quality != TierGood {
		t.Errorf("Quality = %v, want %v", profile.Quality, TierGood)
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
	if fimCfg.PrefixBudgetPct != 75 {
		t.Errorf("PrefixBudgetPct = %d, want 75", fimCfg.PrefixBudgetPct)
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

func TestModelRegistry_Lookup_FallsBackToDirectModelInfoOnListError(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name:      "ollama",
		caps:      CapChat,
		modelsErr: errors.New("tags unavailable"),
		details: map[string]ModelInfo{
			"qwen3:8b": {
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				Template:      "{{ .Prompt }}{{ .Suffix }}",
				Capabilities:  []string{"completion", "insert"},
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

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if profile.Name != "qwen3:8b" {
		t.Fatalf("profile.Name = %q, want %q", profile.Name, "qwen3:8b")
	}
	if !profile.SupportsFIM() {
		t.Fatal("expected fallback direct model info to preserve FIM support")
	}
}

func TestModelRegistry_Lookup_FallsBackToDirectModelInfoOnNameMismatch(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat,
		models: []ModelInfo{
			{Name: "qwen3:8b:listed"},
		},
		details: map[string]ModelInfo{
			"qwen3:8b": {
				Name:          "qwen3:8b",
				Family:        "qwen3",
				ParameterSize: "8B",
				Template:      "{{ .Prompt }}{{ .Suffix }}",
				Capabilities:  []string{"completion", "insert"},
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

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if profile.Name != "qwen3:8b" {
		t.Fatalf("profile.Name = %q, want %q", profile.Name, "qwen3:8b")
	}
	if profile.Family != "qwen3" {
		t.Fatalf("profile.Family = %q, want %q", profile.Family, "qwen3")
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

// TestModelRegistry_CapabilityOverride_Replaces verifies that a configured
// CapabilityOverride wholesale replaces the merge-derived Caps. This is the
// design contract that lets users carve down capabilities for backends
// missing endpoints (e.g. ["chat", "stream"] removes generate even when
// the runtime probe claimed it).
func TestModelRegistry_CapabilityOverride_Replaces(t *testing.T) {
	ctx := context.Background()

	// Runtime claims completion (chat|generate|stream) + tools.
	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream | CapToolCall,
		models: []ModelInfo{
			{
				Name:         "qwen3:8b",
				Family:       "qwen3",
				Capabilities: []string{"completion", "tools"},
			},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	// User carves down to chat+stream only — generate must be removed.
	mr.SetCapabilityOverride(func(key ModelKey) []string {
		if key.Model == "qwen3:8b" {
			return []string{"chat", "stream"}
		}
		return nil
	})

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	want := CapChat | CapStream
	if profile.Caps != want {
		t.Errorf("Caps = %v, want %v (override should wholesale replace, not merge)", profile.Caps, want)
	}
	if profile.Caps.Has(CapGenerate) {
		t.Error("CapGenerate present after override that excluded it; replace semantics violated")
	}
	if profile.Caps.Has(CapToolCall) {
		t.Error("CapToolCall present after override that excluded it; replace semantics violated")
	}
}

// TestModelRegistry_CapabilityOverride_InsertIsSingleBit pins the most
// important user-facing contract: ["chat", "insert"] means "exactly chat
// and insert" — NOT chat+generate+stream+insert via alias expansion.
//
// Regression test for the half-fix discovered in the second review cycle:
// ParseCapsStrict at the validation site enforces single-bit "insert", but
// the override apply site previously called the lenient parseCaps, which
// re-expanded "insert" to three bits — silently re-adding the very caps
// the REPLACES contract was supposed to remove.
func TestModelRegistry_CapabilityOverride_InsertIsSingleBit(t *testing.T) {
	ctx := context.Background()

	// Runtime template signals FIM, so all the bits an alias-expansion
	// would re-add are claimed by lower merge layers; if anything leaks
	// through into the override result, the test catches it.
	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream | CapInsert,
		models: []ModelInfo{
			{
				Name:         "fim-model",
				Family:       "qwen3-coder",
				Template:     "{{ .Prompt }}{{ .Suffix }}",
				Capabilities: []string{"completion", "insert"},
			},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	mr.SetCapabilityOverride(func(_ ModelKey) []string {
		return []string{"chat", "insert"}
	})

	profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "fim-model"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	want := CapChat | CapInsert
	if profile.Caps != want {
		t.Errorf("Caps = %v, want %v (override [\"chat\", \"insert\"] must NOT alias-expand insert to generate+stream+insert)", profile.Caps, want)
	}
	if profile.Caps.Has(CapGenerate) {
		t.Error("CapGenerate present after override [\"chat\", \"insert\"]; alias expansion at apply site is the bug this test exists to prevent")
	}
	if profile.Caps.Has(CapStream) {
		t.Error("CapStream present after override [\"chat\", \"insert\"]; alias expansion at apply site is the bug this test exists to prevent")
	}
}

// TestModelRegistry_CapabilityOverride_NonCanonicalTokensRejected verifies
// that a programmatic caller passing catalog aliases (rather than canonical
// single-bit names) sees the override IGNORED rather than silently expanded.
// Config validation rejects aliases upstream; reaching the apply site with
// an alias means the validation was bypassed, in which case we must
// fail safe (keep merged caps) instead of leaking expansion bits.
func TestModelRegistry_CapabilityOverride_NonCanonicalTokensRejected(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}

	for _, alias := range []string{"completion", "tools", "embedding"} {
		t.Run(alias, func(t *testing.T) {
			mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
			if err != nil {
				t.Fatalf("NewModelRegistry: %v", err)
			}
			mr.SetCapabilityOverride(func(_ ModelKey) []string {
				return []string{alias}
			})
			profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			// CapChat is the probe because:
			//   - It is present in the merged caps from the runtime
			//     "completion" alias (aliasedCapability expands
			//     "completion" -> CapChat|CapGenerate|CapStream).
			//   - Under a buggy silent expansion via parseCaps, "tools"
			//     and "embedding" would REPLACE Caps with bitmasks that
			//     EXCLUDE CapChat (CapToolCall and CapEmbed respectively),
			//     so CapChat-absent is the distinguishing canary that the
			//     override was wrongly applied.
			//   - The "completion" sub-case is weaker: silent expansion
			//     would yield CapChat|CapGenerate|CapStream which still
			//     includes CapChat, so the probe doesn't distinguish for
			//     that single token. The other two sub-cases share the
			//     same rejection code path and carry the proof.
			if !profile.Caps.Has(CapChat) {
				t.Errorf("merged caps lost after override [%q] was rejected; expected fail-safe to keep them, got %v", alias, profile.Caps)
			}
		})
	}
}

// TestModelRegistry_OverrideRejectionHook_FiresOnNonCanonicalToken verifies
// that rejections at the override apply site are observable via the
// installed hook — the fail-safe (keep merged caps) is preserved, AND the
// operator gets a signal so the misconfiguration doesn't go silent.
// Without the hook the only symptom is router decisions that don't match
// the config the operator wrote, which is exactly the silent-failure
// class we are surfacing here.
func TestModelRegistry_OverrideRejectionHook_FiresOnNonCanonicalToken(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	type rejection struct {
		key    ModelKey
		tokens []string
		err    error
	}
	var (
		mu         sync.Mutex
		rejections []rejection
	)
	mr.SetOverrideRejectionHook(func(k ModelKey, tokens []string, err error) {
		mu.Lock()
		defer mu.Unlock()
		rejections = append(rejections, rejection{k, tokens, err})
	})
	mr.SetCapabilityOverride(func(_ ModelKey) []string {
		return []string{"completion"} // multi-bit alias → rejected
	})

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	// Fail-safe still holds: merged caps preserved.
	if !profile.Caps.Has(CapChat) {
		t.Errorf("merged caps lost after rejection; expected fail-safe to keep them, got %v", profile.Caps)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(rejections) != 1 {
		t.Fatalf("rejection hook fired %d times, want 1", len(rejections))
	}
	r := rejections[0]
	if r.key != key {
		t.Errorf("rejection key = %v, want %v", r.key, key)
	}
	if len(r.tokens) != 1 || r.tokens[0] != "completion" {
		t.Errorf("rejection tokens = %v, want [completion]", r.tokens)
	}
	if r.err == nil {
		t.Error("rejection err = nil, want non-nil ParseCapsStrict error")
	}
}

// TestModelRegistry_OverrideRejectionHook_NotCalledOnAcceptedOverride
// guards against false-positive observability: when the override parses
// cleanly the hook MUST NOT fire, otherwise operators see noise on every
// healthy config and learn to ignore the signal.
func TestModelRegistry_OverrideRejectionHook_NotCalledOnAcceptedOverride(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	var fired int
	mr.SetOverrideRejectionHook(func(_ ModelKey, _ []string, _ error) {
		fired++
	})
	mr.SetCapabilityOverride(func(_ ModelKey) []string {
		return []string{"chat", "stream"} // canonical: accepted
	})

	if _, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"}); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if fired != 0 {
		t.Errorf("rejection hook fired %d times on accepted override, want 0", fired)
	}
}

// TestModelRegistry_CapabilityOverride_ZeroCapsGuard verifies the merge
// refuses to wholesale-replace with zero caps when the override returns
// a non-nil but effectively empty result. This protects against
// programmatic callers that bypass config validation and would otherwise
// produce profiles unusable by every router gate.
func TestModelRegistry_CapabilityOverride_ZeroCapsGuard(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}

	tests := []struct {
		name     string
		override func(ModelKey) []string
	}{
		{
			name:     "non-nil empty slice keeps merged caps",
			override: func(_ ModelKey) []string { return []string{} },
		},
		{
			name:     "all-unknown tokens (parse to zero) keeps merged caps",
			override: func(_ ModelKey) []string { return []string{"nonexistent", "alsobad"} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
			if err != nil {
				t.Fatalf("NewModelRegistry: %v", err)
			}
			mr.SetCapabilityOverride(tt.override)

			profile, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
			if err != nil {
				t.Fatalf("Lookup: %v", err)
			}
			if profile.Caps == 0 {
				t.Errorf("Caps = 0 after %s; merge must refuse zero-cap replacement to avoid crippling the profile", tt.name)
			}
			if !profile.Caps.Has(CapChat) {
				t.Errorf("Caps missing CapChat after %s = %v", tt.name, profile.Caps)
			}
		})
	}
}

// TestModelRegistry_CapabilityOverride_WipesRuntimeCapInsert documents
// that an override which omits "insert" wipes runtime-template-detected
// CapInsert. This is by design (user declaration wins) but the
// interaction is sharp enough to be worth pinning down with a test.
func TestModelRegistry_CapabilityOverride_WipesRuntimeCapInsert(t *testing.T) {
	ctx := context.Background()

	// A template that uses .Suffix triggers CapInsert in merge().
	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream | CapInsert,
		models: []ModelInfo{
			{
				Name:         "fim-model",
				Family:       "qwen3-coder",
				Template:     "{{ .Prompt }}{{ .Suffix }}",
				Capabilities: []string{"completion", "insert"},
			},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	// Without override, runtime template detection adds CapInsert.
	baseline, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "fim-model"})
	if err != nil {
		t.Fatalf("baseline Lookup: %v", err)
	}
	if !baseline.Caps.Has(CapInsert) {
		t.Fatal("precondition: expected runtime template to add CapInsert")
	}

	// Override carves down to chat+stream — explicit declaration wins,
	// including dropping runtime-detected CapInsert.
	mr.SetCapabilityOverride(func(_ ModelKey) []string {
		return []string{"chat", "stream"}
	})
	overridden, err := mr.Lookup(ctx, ModelKey{Provider: "ollama", Model: "fim-model"})
	if err != nil {
		t.Fatalf("overridden Lookup: %v", err)
	}
	if overridden.Caps.Has(CapInsert) {
		t.Errorf("Caps unexpectedly retained CapInsert after override = %v; user declaration must win over runtime detection", overridden.Caps)
	}
}

// TestModelRegistry_CapabilityOverride_FlushesCacheOnInstall verifies that
// SetCapabilityOverride invalidates already-cached profiles so a consumer
// that installs the override AFTER warming the cache (the natural wiring
// sequence: build registry -> RefreshModels -> install override) still
// sees the new policy on the very next Lookup. Without the flush, cached
// profiles would silently shadow the override.
func TestModelRegistry_CapabilityOverride_FlushesCacheOnInstall(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream | CapToolCall,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion", "tools"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// Warm the cache without an override installed — simulates the natural
	// startup sequence where Refresh populates the cache before config-driven
	// overrides are wired.
	warmed, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("warm Lookup: %v", err)
	}
	if !warmed.Caps.Has(CapGenerate) {
		t.Fatal("preconditions: expected CapGenerate before override")
	}

	// Now install the override — must invalidate the cached profile so the
	// next Lookup re-merges with the override applied.
	mr.SetCapabilityOverride(func(k ModelKey) []string {
		if k.Model == "qwen3:8b" {
			return []string{"chat", "stream"}
		}
		return nil
	})

	postOverride, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-override Lookup: %v", err)
	}
	want := CapChat | CapStream
	if postOverride.Caps != want {
		t.Errorf("Caps = %v, want %v (override installed AFTER warm cache must take effect on next Lookup)", postOverride.Caps, want)
	}

	// Clearing the override must also flush so the cached overridden profile
	// is not silently served after revert.
	mr.SetCapabilityOverride(nil)
	reverted, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-clear Lookup: %v", err)
	}
	if !reverted.Caps.Has(CapGenerate) {
		t.Errorf("Caps after clearing override missing CapGenerate; clearing must also flush cache, got %v", reverted.Caps)
	}
}

// gateMockProvider wraps mrMockProvider with a synchronization channel that
// blocks Models() until released. Used to deterministically interleave
// buildProfile with a concurrent SetCapabilityOverride for the TOCTOU test.
type gateMockProvider struct {
	*mrMockProvider
	entered chan struct{}
	release chan struct{}
}

func (g *gateMockProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.mrMockProvider.Models(ctx)
}

// TestModelRegistry_CapabilityOverride_TOCTOUDoesNotCacheStaleProfile verifies
// the version-counter guard: an in-flight buildProfile that snapshotted the
// OLD override must NOT write its result to the cache after SetCapabilityOverride
// has raced ahead and bumped the version. Without the guard, the stale-policy
// profile would land in the freshly-cleared map and shadow the swap until
// the next Refresh — a silent staleness bug the cache-flush alone cannot fix.
func TestModelRegistry_CapabilityOverride_TOCTOUDoesNotCacheStaleProfile(t *testing.T) {
	ctx := context.Background()

	base := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	gated := &gateMockProvider{
		mrMockProvider: base,
		entered:        make(chan struct{}, 1),
		release:        make(chan struct{}),
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": gated}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// Install initial override BEFORE the racing Lookup, so the in-flight
	// buildProfile snapshots a non-nil override that will go stale.
	oldOverride := func(_ ModelKey) []string { return []string{"chat", "generate"} }
	mr.SetCapabilityOverride(oldOverride)

	// Goroutine A: Lookup blocks inside Models() until release fires.
	type result struct {
		profile *ModelProfile
		err     error
	}
	done := make(chan result, 1)
	go func() {
		p, err := mr.Lookup(ctx, key)
		done <- result{p, err}
	}()

	// Wait for the racing Lookup to enter Models() — it has now snapshotted
	// the old override + old version.
	<-gated.entered

	// Goroutine B: install a different override mid-flight. This bumps the
	// version counter and clears the (currently empty) cache.
	newOverride := func(_ ModelKey) []string { return []string{"chat", "stream"} }
	mr.SetCapabilityOverride(newOverride)

	// Release the gated Models() call so buildProfile completes and reaches
	// the version-check at cache write time.
	close(gated.release)
	res := <-done
	if res.err != nil {
		t.Fatalf("Lookup: %v", res.err)
	}

	// The racing Lookup correctly reflects the override IT observed
	// (CapChat|CapGenerate) — returning the stale snapshot to that caller
	// is the right answer.
	wantStale := CapChat | CapGenerate
	if res.profile.Caps != wantStale {
		t.Errorf("racing Lookup result = %v, want %v (old override snapshot must apply to its own return value)", res.profile.Caps, wantStale)
	}

	// Critical: the stale-policy profile MUST NOT have been cached.
	// A subsequent Lookup must re-run merge under the NEW override.
	next, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("post-swap Lookup: %v", err)
	}
	wantFresh := CapChat | CapStream
	if next.Caps != wantFresh {
		t.Errorf("post-swap Lookup Caps = %v, want %v — stale profile shadowed the override swap (TOCTOU regression)", next.Caps, wantFresh)
	}
}

// TestModelRegistry_CapabilityOverride_ConcurrentSwap exercises hot-swapping
// the override concurrently with Lookup calls. Designed to run under `go test
// -race` to surface unprotected access to capOverride or profiles.
func TestModelRegistry_CapabilityOverride_ConcurrentSwap(t *testing.T) {
	ctx := context.Background()

	prov := &mrMockProvider{
		name: "ollama",
		caps: CapChat | CapGenerate | CapStream,
		models: []ModelInfo{
			{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
		},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	const writers = 4
	const readers = 8
	const iters = 50

	overrides := []CapabilityOverride{
		nil,
		func(_ ModelKey) []string { return []string{"chat", "stream"} },
		func(_ ModelKey) []string { return []string{"chat"} },
		func(_ ModelKey) []string { return nil },
	}
	thinkOverrides := []ThinkOverride{
		nil,
		func(_ ModelKey) (*ThinkMode, *ThinkTags) {
			m := ThinkAlways
			return &m, nil
		},
	}

	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for w := 0; w < writers; w++ {
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				mr.SetCapabilityOverride(overrides[(i+seed)%len(overrides)])
				mr.SetThinkOverride(thinkOverrides[(i+seed)%len(thinkOverrides)])
			}
		}(w)
	}
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				if _, err := mr.Lookup(ctx, key); err != nil {
					t.Errorf("Lookup: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// TestModelRegistry_CapabilityOverride_NilSkipped verifies that the override
// hook can return nil for a key to defer to merge-derived caps. Necessary so
// users can declare capabilities for some models and let others auto-derive.
// Compares against a baseline registry with no override to avoid hardcoding
// the static catalog's contribution for the test model.
func TestModelRegistry_CapabilityOverride_NilSkipped(t *testing.T) {
	ctx := context.Background()

	newProv := func() *mrMockProvider {
		return &mrMockProvider{
			name: "ollama",
			caps: CapChat | CapGenerate | CapStream,
			models: []ModelInfo{
				{Name: "qwen3:8b", Family: "qwen3", Capabilities: []string{"completion"}},
			},
		}
	}

	baselineReg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": newProv()}}
	baseline, err := NewModelRegistry(baselineReg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("baseline NewModelRegistry: %v", err)
	}
	baselineProfile, err := baseline.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("baseline Lookup: %v", err)
	}

	overrideReg := &mrMockProviderRegistry{providers: map[string]Provider{"ollama": newProv()}}
	withOverride, err := NewModelRegistry(overrideReg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("override NewModelRegistry: %v", err)
	}
	withOverride.SetCapabilityOverride(func(_ ModelKey) []string {
		return nil // defer to merge for all keys
	})

	overrideProfile, err := withOverride.Lookup(ctx, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("override Lookup: %v", err)
	}

	if overrideProfile.Caps != baselineProfile.Caps {
		t.Errorf("Caps with nil-returning override = %v, want %v (must match baseline)", overrideProfile.Caps, baselineProfile.Caps)
	}
}

// ---------------------------------------------------------------------------
// TestRecommend_RestrictToProvider
// ---------------------------------------------------------------------------

// TestRecommend_RestrictToProvider verifies that RecommendOpts.RestrictToProvider
// hard-filters candidates to a single provider instance, even when multiple
// providers advertise capable models. Distinct from PreferredProviders (which
// is currently a soft preference and remains unused). Unknown provider names
// surface as provider resolution errors rather than degrading silently to an
// empty candidate set.
func TestRecommend_RestrictToProvider(t *testing.T) {
	provA := &mrMockProvider{
		name:   "ollama-a",
		caps:   CapChat | CapGenerate,
		models: []ModelInfo{{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion"}}},
	}
	provB := &mrMockProvider{
		name:   "ollama-b",
		caps:   CapChat | CapGenerate,
		models: []ModelInfo{{Name: "qwen3:8b", Family: "qwen3", ParameterSize: "8B", QuantLevel: "Q4_K_M", ContextWindow: 32768, Capabilities: []string{"completion"}}},
	}

	reg := NewRegistry()
	if err := reg.Register(provA); err != nil {
		t.Fatalf("Register provA: %v", err)
	}
	if err := reg.Register(provB); err != nil {
		t.Fatalf("Register provB: %v", err)
	}
	ctx := context.Background()
	if err := reg.RefreshModels(ctx, "ollama-a"); err != nil {
		t.Fatalf("RefreshModels a: %v", err)
	}
	if err := reg.RefreshModels(ctx, "ollama-b"); err != nil {
		t.Fatalf("RefreshModels b: %v", err)
	}

	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	// Unscoped: both providers' profiles appear.
	all, err := mr.Recommend(ctx, RecommendOpts{RequiredCaps: CapChat})
	if err != nil {
		t.Fatalf("Recommend (unscoped): %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("unscoped len = %d, want 2", len(all))
	}

	// Scoped to ollama-a: only ollama-a's profile appears.
	scoped, err := mr.Recommend(ctx, RecommendOpts{RequiredCaps: CapChat, RestrictToProvider: "ollama-a"})
	if err != nil {
		t.Fatalf("Recommend (scoped): %v", err)
	}
	if len(scoped) != 1 {
		t.Fatalf("scoped len = %d, want 1", len(scoped))
	}
	if scoped[0].Key.Provider != "ollama-a" {
		t.Errorf("scoped[0].Provider = %q, want ollama-a", scoped[0].Key.Provider)
	}

	// Scoped to a non-existent provider: surface the typo as a provider
	// resolution error instead of silently degrading to an empty candidate set.
	_, err = mr.Recommend(ctx, RecommendOpts{RequiredCaps: CapChat, RestrictToProvider: "nonexistent"})
	if err == nil {
		t.Fatal("Recommend (nonexistent) returned nil error, want provider resolution error")
	}
}

// ---------------------------------------------------------------------------
// SetCapabilityFloor
// ---------------------------------------------------------------------------

// newTestRegistryWithModel builds a ModelRegistry over a single mock provider
// advertising one model with no runtime capability metadata — the
// openai-compat shape the capability floor exists for.
func newTestRegistryWithModel(t *testing.T, providerName, model string) *ModelRegistry {
	t.Helper()
	prov := &mrMockProvider{
		name:   providerName,
		models: []ModelInfo{{Name: model}},
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{providerName: prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore())
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	return mr
}

// TestSetCapabilityFloor_ORsBelowCatalogAndOverride verifies the floor
// OR-merges with the static catalog instead of replacing it: a catalog
// family hit carrying tool_call must survive a floor that omits it.
func TestSetCapabilityFloor_ORsBelowCatalogAndOverride(t *testing.T) {
	// "qwen3:8b" hits the catalog family entry whose caps include "tools".
	reg := newTestRegistryWithModel(t, "llamacpp", "qwen3:8b")
	reg.SetCapabilityFloor(func(key ModelKey) []string {
		return []string{"chat", "generate", "stream"}
	})
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	want := CapChat | CapGenerate | CapStream | CapToolCall
	if !p.Caps.Has(want) {
		t.Fatalf("caps = %v, want superset of %v", p.Caps, want)
	}
}

// TestSetCapabilityFloor_ExplicitOverrideStillReplaces verifies the explicit
// override keeps its wholesale-REPLACE semantics above the floor: floor and
// catalog both contribute tool_call, but an override without it wins.
func TestSetCapabilityFloor_ExplicitOverrideStillReplaces(t *testing.T) {
	reg := newTestRegistryWithModel(t, "llamacpp", "qwen3:8b")
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "generate", "stream"} })
	reg.SetCapabilityOverride(func(ModelKey) []string { return []string{"chat", "stream"} })
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if p.Caps != (CapChat | CapStream) {
		t.Fatalf("caps = %v, want exactly chat|stream", p.Caps)
	}
}

// TestSetCapabilityFloor_InvalidTokensDroppedWithHook verifies non-canonical
// floor tokens are rejected wholesale (never partially applied) and that the
// rejection hook fires so the misconfiguration is observable.
func TestSetCapabilityFloor_InvalidTokensDroppedWithHook(t *testing.T) {
	reg := newTestRegistryWithModel(t, "llamacpp", "unknown-model")
	var hookKey ModelKey
	reg.SetOverrideRejectionHook(func(key ModelKey, tokens []string, err error) { hookKey = key })
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"completion"} }) // multi-bit alias => strict-reject
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// "completion" would lenient-expand to chat|generate|stream; none of
	// those bits may leak through a rejected floor.
	if p.Caps.Has(CapChat) || p.Caps.Has(CapGenerate) || p.Caps.Has(CapStream) {
		t.Fatalf("rejected floor partially applied: caps = %v", p.Caps)
	}
	if hookKey.Model != "unknown-model" {
		t.Fatalf("rejection hook not fired for floor tokens")
	}
}

// TestSetCapabilityFloor_InvalidatesCache verifies installing a floor
// flushes the profile cache so already-warm keys pick up the new policy on
// the next Lookup (same invalidation contract as the override; the TOCTOU
// version guard is shared machinery covered by the override tests).
func TestSetCapabilityFloor_InvalidatesCache(t *testing.T) {
	reg := newTestRegistryWithModel(t, "llamacpp", "unknown-model")
	if _, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"}); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup after floor: %v", err)
	}
	if !p.Caps.Has(CapChat | CapStream) {
		t.Fatalf("floor not applied after cache flush: %v", p.Caps)
	}
}

// TestSetCapabilityFloor_NilClearsFloor verifies SetCapabilityFloor(nil)
// removes the floor: a catalog-miss model that only had floor-supplied bits
// loses them on the next Lookup.
func TestSetCapabilityFloor_NilClearsFloor(t *testing.T) {
	reg := newTestRegistryWithModel(t, "llamacpp", "unknown-model")
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup with floor: %v", err)
	}
	if !p.Caps.Has(CapChat | CapStream) {
		t.Fatalf("floor not applied: %v", p.Caps)
	}
	reg.SetCapabilityFloor(nil)
	p, err = reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup after clearing floor: %v", err)
	}
	if p.Caps.Has(CapChat) || p.Caps.Has(CapStream) {
		t.Fatalf("floor bits survived SetCapabilityFloor(nil): %v", p.Caps)
	}
}
