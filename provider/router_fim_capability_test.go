package provider

import (
	"context"
	"errors"
	"testing"
)

// fimCapProvider is a minimal Provider that advertises a fixed capability
// set at the provider level. Per-profile gating in scoreCandidate filters on
// ModelProfile metadata, so each test seeds profiles with different cap masks
// and templates even though the underlying provider always reports the full
// superset.
type fimCapProvider struct {
	name   string
	models []ModelInfo
}

func (p *fimCapProvider) Name() string { return p.name }
func (p *fimCapProvider) Capabilities() Capability {
	return CapGenerate | CapInsert | CapStream
}
func (p *fimCapProvider) Health(context.Context) error { return nil }
func (p *fimCapProvider) Models(context.Context) ([]ModelInfo, error) {
	return p.models, nil
}
func (p *fimCapProvider) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return nil, nil
}
func (p *fimCapProvider) ChatStream(context.Context, ChatRequest, func(ChatResponse) error) error {
	return nil
}
func (p *fimCapProvider) Generate(_ context.Context, req GenerateRequest) (*GenerateResponse, error) {
	return &GenerateResponse{Provider: p.name, Model: req.Model, Response: "ok", Done: true}, nil
}
func (p *fimCapProvider) GenerateStream(_ context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	return fn(GenerateResponse{Provider: p.name, Model: req.Model, Response: "ok", Done: true})
}
func (p *fimCapProvider) Embed(context.Context, EmbedRequest) (*EmbedResponse, error) {
	return nil, nil
}

// routerWithFIMProfiles builds a real *Router whose ModelRegistry is seeded
// with the given profiles and whose Registry has its model index populated
// (via RefreshModels) so unqualified Route(req.Model = "shared-fim") calls
// reach LookupAny → ProvidersForModel → cached Lookup. Without RefreshModels
// the model index would be empty and the test would fail at lookup before
// the capability gate could be exercised.
func routerWithFIMProfiles(t *testing.T, profiles ...*ModelProfile) *Router {
	t.Helper()

	provReg := NewRegistry()
	byProvider := map[string][]string{}
	for _, profile := range profiles {
		byProvider[profile.Key.Provider] = append(byProvider[profile.Key.Provider], profile.Key.Model)
	}
	for providerName, models := range byProvider {
		modelInfos := make([]ModelInfo, 0, len(models))
		for _, name := range models {
			modelInfos = append(modelInfos, ModelInfo{Name: name})
		}
		prov := &fimCapProvider{name: providerName, models: modelInfos}
		if err := provReg.Register(prov); err != nil {
			t.Fatalf("register %s: %v", providerName, err)
		}
	}

	mr, err := NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("new model registry: %v", err)
	}
	mr.mu.Lock()
	for _, profile := range profiles {
		mr.profiles[profile.Key] = profile
	}
	mr.mu.Unlock()

	for providerName := range byProvider {
		if err := provReg.RefreshModels(context.Background(), providerName); err != nil {
			t.Fatalf("refresh models %s: %v", providerName, err)
		}
	}

	r := NewRouter(mr, provReg)
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRouter_FIMRequiresCapInsert(t *testing.T) {
	model := "shared-fim"
	r := routerWithFIMProfiles(t,
		&ModelProfile{Key: ModelKey{Provider: "plain", Model: model}, Caps: CapGenerate, ContextWindow: 8192},
		&ModelProfile{Key: ModelKey{Provider: "insert", Model: model}, Caps: CapGenerate | CapInsert, ContextWindow: 8192},
	)
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:          model,
		UseCase:        "fim",
		RequiredCaps:   CapGenerate | CapInsert,
		Prompt:         "prefix",
		Suffix:         "suffix",
		ExpectedOutput: DefaultExpectedOutput("fim"),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got := plan.Profile.Key.Provider; got != "insert" {
		t.Fatalf("Provider = %q, want %q", got, "insert")
	}
}

// TestRouter_FIMRejectsAllNonInsertCandidates proves the capability filter
// is a hard gate (capabilityGate=false eliminates the candidate) rather than
// a soft preference (low score but still selectable). With ONLY non-CapInsert
// candidates available, a soft-preference regression would still pick the
// best-scored candidate; the hard gate must surface ErrNoViableCandidate
// instead. This is the negative pair to TestRouter_FIMRequiresCapInsert.
func TestRouter_FIMRejectsAllNonInsertCandidates(t *testing.T) {
	model := "shared-fim"
	r := routerWithFIMProfiles(t,
		&ModelProfile{Key: ModelKey{Provider: "plain-a", Model: model}, Caps: CapGenerate, ContextWindow: 8192},
		&ModelProfile{Key: ModelKey{Provider: "plain-b", Model: model}, Caps: CapGenerate, ContextWindow: 8192},
	)
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:          model,
		UseCase:        "fim",
		RequiredCaps:   CapGenerate | CapInsert,
		Prompt:         "prefix",
		Suffix:         "suffix",
		ExpectedOutput: DefaultExpectedOutput("fim"),
	})
	if err == nil {
		t.Fatalf("Route returned plan=%+v with nil error; want ErrNoViableCandidate", plan)
	}
	if !errors.Is(err, ErrNoViableCandidate) {
		t.Errorf("Route err = %v, want errors.Is(_, ErrNoViableCandidate) — capability gate must hard-filter, not soft-prefer", err)
	}
}

func TestRouter_FIMTemplateSuffixSatisfiesCapInsertGate(t *testing.T) {
	model := "template-fim"
	r := routerWithFIMProfiles(t,
		&ModelProfile{
			Key:           ModelKey{Provider: "plain", Model: model},
			Caps:          CapGenerate,
			Template:      "{{ .Prompt }}",
			ContextWindow: 8192,
		},
		&ModelProfile{
			Key:           ModelKey{Provider: "template", Model: model},
			Caps:          CapGenerate,
			Template:      "{{ .Prompt }}{{ .Suffix }}",
			ContextWindow: 8192,
		},
	)
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:          model,
		UseCase:        "fim",
		RequiredCaps:   CapGenerate | CapInsert,
		Prompt:         "prefix",
		Suffix:         "suffix",
		ExpectedOutput: DefaultExpectedOutput("fim"),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got := plan.Profile.Key.Provider; got != "template" {
		t.Fatalf("Provider = %q, want %q (template .Suffix must satisfy CapInsert gate)", got, "template")
	}
}

func TestRouter_FIMStreamRequiresCapStream(t *testing.T) {
	model := "shared-fim"
	r := routerWithFIMProfiles(t,
		&ModelProfile{Key: ModelKey{Provider: "nostream", Model: model}, Caps: CapGenerate | CapInsert, ContextWindow: 8192},
		&ModelProfile{Key: ModelKey{Provider: "stream", Model: model}, Caps: CapGenerate | CapInsert | CapStream, ContextWindow: 8192},
	)
	plan, err := r.Route(context.Background(), RoutingRequest{
		Model:          model,
		UseCase:        "fim",
		RequiredCaps:   CapGenerate | CapInsert | CapStream,
		Prompt:         "prefix",
		Suffix:         "suffix",
		ExpectedOutput: DefaultExpectedOutput("fim"),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if got := plan.Profile.Key.Provider; got != "stream" {
		t.Fatalf("Provider = %q, want %q", got, "stream")
	}
}
