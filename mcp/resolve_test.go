package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

type guardedOllamaBackend struct {
	name     string
	model    string
	tagHits  atomic.Int64
	pullHits atomic.Int64
}

func newGuardedOllamaRegistry(t *testing.T, backends ...*guardedOllamaBackend) (*provider.Registry, *provider.DestinationGate) {
	t.Helper()

	type endpoint struct {
		backend *guardedOllamaBackend
		url     string
		dest    provider.Destination
	}
	endpoints := make([]endpoint, 0, len(backends))
	edges := make([]provider.DestinationEdge, 0, len(backends))
	for _, backend := range backends {
		backend := backend
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/tags":
				backend.tagHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"models":[{"name":%q}]}`, backend.model)
			case "/api/pull":
				backend.pullHits.Add(1)
				w.WriteHeader(http.StatusOK)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)

		dest, err := provider.NewDestination(backend.name, srv.URL)
		if err != nil {
			t.Fatalf("NewDestination(%q) error = %v", backend.name, err)
		}
		endpoints = append(endpoints, endpoint{backend: backend, url: srv.URL, dest: dest})
		edges = append(edges, provider.DestinationEdge{
			Purpose:     provider.DestinationPurposeModelRefresh,
			Destination: dest,
		})
	}

	manifest, err := provider.NewDestinationManifest(edges...)
	if err != nil {
		t.Fatalf("NewDestinationManifest() error = %v", err)
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.DestinationPolicy{}, manifest); err != nil {
		t.Fatalf("DestinationGate.Install() error = %v", err)
	}

	reg := provider.NewRegistry()
	for _, endpoint := range endpoints {
		client, err := provider.GuardHTTPClient(gate, endpoint.dest, nil)
		if err != nil {
			t.Fatalf("GuardHTTPClient(%q) error = %v", endpoint.backend.name, err)
		}
		p := provider.NewOllamaProvider(
			ollama.NewClient(ollama.WithBaseURL(endpoint.url), ollama.WithHTTPClient(client)),
			provider.WithProviderName(endpoint.backend.name),
		)
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register(%q) error = %v", endpoint.backend.name, err)
		}
	}
	return reg, gate
}

func TestResolveModelExplicit(t *testing.T) {
	s := &Server{
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModel(context.Background(), "explicit-model", "chat")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "explicit-model" {
		t.Errorf("resolveModel() = %q, want %q", got, "explicit-model")
	}
}

func TestResolveModelTargetDoesNotTreatUnknownSlashPrefixAsProvider(t *testing.T) {
	s := &Server{
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"vllm-local": {BaseURL: "http://localhost:8080", APIFormat: "openai-compat"},
			},
			Models: map[string]config.ModelConfig{
				"completion": {Name: "org/local-fim", Provider: "vllm-local", Type: "dense"},
			},
			Defaults: map[string]string{"completion": "completion"},
		},
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModelTarget(context.Background(), "org/local-fim", "completion")
	if err != nil {
		t.Fatalf("resolveModelTarget() error = %v", err)
	}
	if got.Name != "org/local-fim" {
		t.Fatalf("Name = %q, want org/local-fim", got.Name)
	}
	if got.Provider != "vllm-local" {
		t.Fatalf("Provider = %q, want vllm-local", got.Provider)
	}
}

func TestProviderRegistryModelCheckerRefreshesModelIndex(t *testing.T) {
	reg := provider.NewRegistry()
	p := &mutableModelProvider{
		fakeRouteProvider: &fakeRouteProvider{name: "vllm-local"},
		models:            []provider.ModelInfo{{Name: "old-model"}},
	}
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	checker := providerRegistryModelChecker{registry: reg}

	keys, err := checker.AvailableModelKeys(context.Background())
	if err != nil {
		t.Fatalf("AvailableModelKeys() error = %v", err)
	}
	if len(keys) != 1 || keys[0].Model != "old-model" {
		t.Fatalf("initial keys = %+v, want old-model", keys)
	}

	p.models = []provider.ModelInfo{{Name: "new-model"}}
	keys, err = checker.AvailableModelKeys(context.Background())
	if err != nil {
		t.Fatalf("AvailableModelKeys() after update error = %v", err)
	}
	if len(keys) != 1 || keys[0].Model != "new-model" {
		t.Fatalf("updated keys = %+v, want new-model", keys)
	}
	if _, err := reg.ProvidersForModel("old-model"); err == nil {
		t.Fatal("ProvidersForModel(old-model) error = nil, want stale entry removed")
	}
	providers, err := reg.ProvidersForModel("new-model")
	if err != nil {
		t.Fatalf("ProvidersForModel(new-model) error = %v", err)
	}
	if len(providers) != 1 || providers[0].Name() != "vllm-local" {
		t.Fatalf("ProvidersForModel(new-model) = %+v, want vllm-local", providers)
	}
}

func TestInferProviderForExplicitModelBindsRefreshPerProvider(t *testing.T) {
	a := &guardedOllamaBackend{name: "ollama-a", model: "model-a"}
	b := &guardedOllamaBackend{name: "ollama-b", model: "model-b"}
	reg, gate := newGuardedOllamaRegistry(t, a, b)
	s := &Server{providerRegistry: reg, destGate: gate}

	got, err := s.inferProviderForExplicitModel(context.Background(), "model-b")
	if err != nil {
		t.Fatalf("inferProviderForExplicitModel(%q) error = %v", "model-b", err)
	}
	if got != "ollama-b" {
		t.Errorf("inferProviderForExplicitModel(%q) = %q, want %q", "model-b", got, "ollama-b")
	}
	if got := a.tagHits.Load(); got != 1 {
		t.Errorf("provider %q refresh hits = %d, want 1", a.name, got)
	}
	if got := b.tagHits.Load(); got != 1 {
		t.Errorf("provider %q refresh hits = %d, want 1", b.name, got)
	}
}

type mutableModelProvider struct {
	*fakeRouteProvider
	models []provider.ModelInfo
}

func (p *mutableModelProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	out := make([]provider.ModelInfo, len(p.models))
	copy(out, p.models)
	return out, nil
}

func TestResolveModelFromCache(t *testing.T) {
	s := &Server{
		cfg: &config.Config{}, // non-nil config
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "qwen2.5:72b", Role: "large-chat"},
		},
	}

	got, err := s.resolveModel(context.Background(), "", "chat")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "qwen2.5:72b" {
		t.Errorf("resolveModel() = %q, want %q", got, "qwen2.5:72b")
	}
}

func TestResolveModelNoConfigNoExplicit(t *testing.T) {
	s := &Server{
		cfg:      nil,
		resolved: make(map[string]config.ResolvedModel),
	}

	_, err := s.resolveModel(context.Background(), "", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "model parameter required") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "model parameter required")
	}
}

func TestResolveModelDefaultsUnavailable(t *testing.T) {
	s := &Server{
		cfg:      &config.Config{}, // non-nil config
		resolved: make(map[string]config.ResolvedModel),
	}

	_, err := s.resolveModel(context.Background(), "", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "defaults unavailable") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "defaults unavailable")
	}
}

func TestResolveModelUseCaseNotConfigured(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "qwen2.5:72b", Role: "large-chat"},
		},
	}

	_, err := s.resolveModel(context.Background(), "", "embedding")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "no model configured for use-case") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "no model configured for use-case")
	}
}

func TestResolveModelDidNotResolve(t *testing.T) {
	s := &Server{
		cfg: &config.Config{},
		resolved: map[string]config.ResolvedModel{
			"chat": {Name: "", Role: "large-chat"},
		},
	}

	_, err := s.resolveModel(context.Background(), "", "chat")
	if err == nil {
		t.Fatal("resolveModel() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "did not resolve") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "did not resolve")
	}
}

func TestResolveModelRefreshesDefaultsAfterOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3-coder-next:latest"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := &Server{
		client: ollama.NewClient(ollama.WithBaseURL(srv.URL)),
		cfg: &config.Config{
			Defaults: map[string]string{"completion": "coding"},
			Models: map[string]config.ModelConfig{
				"coding": {Name: "qwen3-coder-next:latest", Provider: "ollama"},
			},
		},
		resolved: make(map[string]config.ResolvedModel),
	}

	got, err := s.resolveModel(context.Background(), "", "completion")
	if err != nil {
		t.Fatalf("resolveModel() error = %v", err)
	}
	if got != "qwen3-coder-next:latest" {
		t.Fatalf("resolveModel() = %q, want %q", got, "qwen3-coder-next:latest")
	}
	if len(s.resolved) == 0 {
		t.Fatal("resolved cache was not refreshed")
	}
}
