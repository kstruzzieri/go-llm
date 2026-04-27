package provider

import (
	"context"
	"testing"
)

// fakeChainProvider implements Provider just enough to exercise routeChain.
// Generation methods return fixed responses; everything else is a no-op.
type fakeChainProvider struct {
	name   string
	models []string
}

func (f *fakeChainProvider) Name() string                                     { return f.name }
func (f *fakeChainProvider) Capabilities() Capability                         { return CapChat | CapGenerate | CapEmbed | CapStream }
func (f *fakeChainProvider) Health(ctx context.Context) error                 { return nil }
func (f *fakeChainProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	models := make([]ModelInfo, 0, len(f.models))
	for _, name := range f.models {
		models = append(models, ModelInfo{Name: name})
	}
	return models, nil
}
func (f *fakeChainProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{Provider: f.name, Model: req.Model, Content: "ok", Done: true}, nil
}
func (f *fakeChainProvider) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	return fn(ChatResponse{Provider: f.name, Model: req.Model, Content: "ok", Done: true})
}
func (f *fakeChainProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	return &GenerateResponse{Provider: f.name, Model: req.Model, Response: "ok", Done: true}, nil
}
func (f *fakeChainProvider) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	return fn(GenerateResponse{Provider: f.name, Model: req.Model, Response: "ok", Done: true})
}
func (f *fakeChainProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	return &EmbedResponse{Provider: f.name, Model: req.Model, Embeddings: [][]float64{{1}}}, nil
}

// newChainTestRegistry builds a ModelRegistry pre-seeded with profiles
// matching the given (provider, model) pairs.
func newChainTestRegistry(t *testing.T, pairs ...ModelKey) (*ModelRegistry, *Registry) {
	t.Helper()
	provReg := NewRegistry()
	byProvider := map[string][]string{}
	for _, k := range pairs {
		byProvider[k.Provider] = append(byProvider[k.Provider], k.Model)
	}
	for providerName, models := range byProvider {
		p := &fakeChainProvider{name: providerName, models: models}
		if err := provReg.Register(p); err != nil {
			t.Fatalf("register provider %q: %v", providerName, err)
		}
	}
	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("new model registry: %v", err)
	}
	// Pre-seed cache so Lookup does not hit /api/show.
	for _, k := range pairs {
		profile := &ModelProfile{
			Key:           k,
			Caps:          CapChat | CapGenerate | CapEmbed | CapStream,
			Quality:       TierGood,
			Speed:         TierGood,
			ContextWindow: 8192,
		}
		seedChainProfile(t, mr, profile)
	}
	for providerName := range byProvider {
		if err := provReg.RefreshModels(context.Background(), providerName); err != nil {
			t.Fatalf("refresh models %q: %v", providerName, err)
		}
	}
	return mr, provReg
}

func seedChainProfile(t *testing.T, mr *ModelRegistry, profile *ModelProfile) {
	t.Helper()
	mr.mu.Lock()
	defer mr.mu.Unlock()
	if mr.profiles == nil {
		mr.profiles = make(map[ModelKey]*ModelProfile)
	}
	mr.profiles[profile.Key] = profile
}

func TestRouter_routeChain_SingleStepHappyPath(t *testing.T) {
	mr, pr := newChainTestRegistry(t, ModelKey{Provider: "ollama", Model: "qwen3:8b"})
	r := NewRouter(mr, pr)
	defer r.Close()

	req := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat,
		PreferredChain: []string{"ollama/qwen3:8b"},
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}
	plan, err := r.Route(context.Background(), req)
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if plan.Profile.Key.Provider != "ollama" || plan.Profile.Key.Model != "qwen3:8b" {
		t.Errorf("plan.Profile.Key = %v, want ollama/qwen3:8b", plan.Profile.Key)
	}
	if got := len(plan.Fallbacks); got != 0 {
		t.Errorf("Fallbacks length = %d, want 0", got)
	}
}
