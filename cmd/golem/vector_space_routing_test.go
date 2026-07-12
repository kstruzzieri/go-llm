package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

type routingEmbedProvider struct {
	mu        sync.Mutex
	lastModel string
	models    []string
	embedErr  error
}

func (*routingEmbedProvider) Name() string                      { return "embed-test" }
func (*routingEmbedProvider) Capabilities() provider.Capability { return provider.CapEmbed }
func (*routingEmbedProvider) Health(context.Context) error      { return nil }
func (p *routingEmbedProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	models := make([]provider.ModelInfo, len(p.models))
	for i, name := range p.models {
		models[i] = provider.ModelInfo{Name: name, Capabilities: []string{"embedding"}, ContextWindow: 8192}
	}
	return models, nil
}
func (*routingEmbedProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("chat unsupported")
}
func (*routingEmbedProvider) ChatStream(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
	return errors.New("chat stream unsupported")
}
func (*routingEmbedProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return nil, errors.New("generate unsupported")
}
func (*routingEmbedProvider) GenerateStream(context.Context, provider.GenerateRequest, func(provider.GenerateResponse) error) error {
	return errors.New("generate stream unsupported")
}
func (p *routingEmbedProvider) Embed(_ context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
	p.mu.Lock()
	p.lastModel = req.Model
	err := p.embedErr
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &provider.EmbedResponse{
		Model: req.Model, Provider: p.Name(), Embeddings: [][]float64{{1, 0, 0}},
	}, nil
}
func (p *routingEmbedProvider) failEmbedding(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.embedErr = err
}
func (p *routingEmbedProvider) embeddedModel() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastModel
}

func testRoutingEmbedder(t *testing.T, available ...string) (*config.Config, *provider.Router, *routingEmbedProvider) {
	t.Helper()
	if len(available) == 0 {
		available = []string{"a", "b"}
	}
	p := &routingEmbedProvider{models: available}
	providers := provider.NewRegistry()
	if err := providers.Register(p); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	if err := providers.RefreshModels(context.Background(), p.Name()); err != nil {
		t.Fatalf("refresh provider models: %v", err)
	}
	models, err := provider.NewModelRegistry(providers, nil)
	if err != nil {
		t.Fatalf("build model registry: %v", err)
	}
	router := provider.NewRouter(models, providers)
	t.Cleanup(func() { _ = router.Close() })
	return &config.Config{
		Defaults: map[string]string{"embedding": "a"},
		Models: map[string]config.ModelConfig{
			"a": {Name: "a", Provider: p.Name(), Capabilities: []string{"embedding"}, Fallbacks: []string{"b"}},
			"b": {Name: "b", Provider: p.Name(), Capabilities: []string{"embedding"}},
		},
	}, router, p
}

func TestBuildGatedRetriever_PinsStoredVectorSpace(t *testing.T) {
	tests := []struct {
		name      string
		stored    string
		wantModel string
	}{
		{name: "fallback", stored: "embed-test/b", wantModel: "b"},
		{name: "primary", stored: "embed-test/a", wantModel: "a"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "explicit.db")
			seedIndex(t, dbPath, "workspace:ignored", tc.stored)
			cfg, router, p := testRoutingEmbedder(t)

			reader, _, _, _, err := buildGatedRetriever(
				context.Background(), cfg, router, dbPath, expectedVectorSpaces(cfg), "",
			)
			if err != nil {
				t.Fatalf("buildGatedRetriever: %v", err)
			}
			defer reader.closeAfterDrain()
			result, err := reader.tool.Invoke(context.Background(), json.RawMessage(`{"query":"find A"}`))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.IsError {
				t.Fatalf("retrieve result = %q, want success", result.Content)
			}
			if got := p.embeddedModel(); got != tc.wantModel {
				t.Fatalf("embedded model = %q, want %s for stored vector space %s", got, tc.wantModel, tc.stored)
			}
		})
	}
}

func TestBuildGatedRetriever_RequiredVectorSpaceUnavailableDoesNotUsePrimary(t *testing.T) {
	const stored = "embed-test/b"
	dbPath := filepath.Join(t.TempDir(), "explicit.db")
	seedIndex(t, dbPath, "workspace:ignored", stored)
	cfg, router, p := testRoutingEmbedder(t, "a")

	reader, _, _, _, err := buildGatedRetriever(
		context.Background(), cfg, router, dbPath, expectedVectorSpaces(cfg), "",
	)
	if err != nil {
		t.Fatalf("buildGatedRetriever: %v", err)
	}
	defer reader.closeAfterDrain()
	result, err := reader.tool.Invoke(context.Background(), json.RawMessage(`{"query":"find A"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError {
		t.Fatalf("retrieve result = %q, want unavailable-space error", result.Content)
	}
	if !strings.Contains(result.Content, stored) {
		t.Fatalf("retrieve result = %q, want required vector space %s", result.Content, stored)
	}
	if got := p.embeddedModel(); got != "" {
		t.Fatalf("embedded model = %q, want no fallback embedding call", got)
	}
}

func TestBuildGatedRetriever_ExecutionFailureNamesRequiredVectorSpace(t *testing.T) {
	const stored = "embed-test/b"
	dbPath := filepath.Join(t.TempDir(), "explicit.db")
	seedIndex(t, dbPath, "workspace:ignored", stored)
	cfg, router, p := testRoutingEmbedder(t)
	p.failEmbedding(errors.New("backend offline"))

	reader, _, _, _, err := buildGatedRetriever(
		context.Background(), cfg, router, dbPath, expectedVectorSpaces(cfg), "",
	)
	if err != nil {
		t.Fatalf("buildGatedRetriever: %v", err)
	}
	defer reader.closeAfterDrain()
	result, err := reader.tool.Invoke(context.Background(), json.RawMessage(`{"query":"find A"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !result.IsError {
		t.Fatalf("retrieve result = %q, want execution error", result.Content)
	}
	for _, want := range []string{stored, "backend offline"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("retrieve result = %q, want %q", result.Content, want)
		}
	}
	if got := p.embeddedModel(); got != "b" {
		t.Fatalf("embedded model = %q, want required model b", got)
	}
}

func TestEnableRetrieve_PinsStoredVectorSpaceForExplicitAndAutoIndexes(t *testing.T) {
	const stored = "embed-test/b"
	tests := []struct {
		name string
		opts func(string) retrieveOpts
	}{
		{
			name: "explicit rag-db",
			opts: func(dbPath string) retrieveOpts {
				return retrieveOpts{ragDB: dbPath}
			},
		},
		{
			name: "auto-discovered index",
			opts: func(dbPath string) retrieveOpts {
				return retrieveOpts{
					autoDBPath: dbPath, workspaceID: "workspace:test",
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "index.db")
			seedIndex(t, dbPath, "workspace:test", stored)
			cfg, router, p := testRoutingEmbedder(t)

			got := enableRetrieve(context.Background(), cfg, router, tc.opts(dbPath))
			if got.tool == nil {
				t.Fatalf("retrieve disabled: %v", got.warns)
			}
			result, err := got.tool.Invoke(context.Background(), json.RawMessage(`{"query":"find A"}`))
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if result.IsError {
				t.Fatalf("retrieve result = %q, want success", result.Content)
			}
			if got := p.embeddedModel(); got != "b" {
				t.Fatalf("embedded model = %q, want b for stored vector space %s", got, stored)
			}
		})
	}
}

func TestBuildGatedRetriever_LegacyCorpusKeepsConfiguredChainBehavior(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	if err := store.ReplaceSourceWithHash(
		context.Background(),
		"legacy.go",
		[]rag.Chunk{{ID: "legacy", Content: "package legacy", Source: "legacy.go", StartLine: 1, EndLine: 1}},
		[][]float64{{1, 0, 0}},
		"legacy-hash",
	); err != nil {
		t.Fatalf("seed legacy store: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close legacy store: %v", err)
	}
	cfg, router, p := testRoutingEmbedder(t)

	reader, _, dec, _, err := buildGatedRetriever(
		context.Background(), cfg, router, dbPath, expectedVectorSpaces(cfg), "",
	)
	if err != nil {
		t.Fatalf("buildGatedRetriever: %v", err)
	}
	defer reader.closeAfterDrain()
	if dec.kind != vsLegacy {
		t.Fatalf("gate kind = %v, want vsLegacy", dec.kind)
	}
	result, err := reader.tool.Invoke(context.Background(), json.RawMessage(`{"query":"find legacy"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.IsError {
		t.Fatalf("retrieve result = %q, want legacy compatibility success", result.Content)
	}
	if got := p.embeddedModel(); got != "a" {
		t.Fatalf("embedded model = %q, want configured primary a for an unpinned legacy corpus", got)
	}
}
