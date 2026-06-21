package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

func embedCfg() *config.Config {
	return &config.Config{
		Defaults: map[string]string{"embedding": "emb"},
		Models:   map[string]config.ModelConfig{"emb": {Name: "nomic", Provider: "ollama"}},
	}
}

func TestResolveRetriever_NilWhenNotRequested(t *testing.T) {
	// dbPath == "" => retrieve was not requested: omit quietly, NO error.
	tool, err := resolveRetriever(context.Background(), embedCfg(), &provider.Router{}, "")
	if tool != nil || err != nil {
		t.Errorf("not-requested should be (nil, nil), got tool=%v err=%v", tool, err)
	}
}

func TestResolveRetriever_ErrorsWhenNoProvider(t *testing.T) {
	// -rag-db given but no router => requested-but-failed: surface an error.
	tool, err := resolveRetriever(context.Background(), embedCfg(), nil, filepath.Join(t.TempDir(), "x.db"))
	if tool != nil || err == nil {
		t.Errorf("nil router with -rag-db should error, got tool=%v err=%v", tool, err)
	}
}

func TestResolveRetriever_ErrorsWhenNoEmbeddingDefault(t *testing.T) {
	// Non-nil router + non-empty dbPath so execution reaches the embedding-default
	// guard; with no defaults.embedding it is requested-but-failed.
	cfg := &config.Config{Defaults: map[string]string{}}
	tool, err := resolveRetriever(context.Background(), cfg, &provider.Router{}, "/some/path.db")
	if tool != nil || err == nil {
		t.Errorf("no defaults.embedding with -rag-db should error, got tool=%v err=%v", tool, err)
	}
}

func TestResolveRetriever_ErrorsWhenDBMissing(t *testing.T) {
	// Resolvable embedding default so execution reaches the os.Stat existence
	// check; the missing path is the condition under test. The zero-value router
	// is never dereferenced because os.Stat fails first.
	tool, err := resolveRetriever(context.Background(), embedCfg(), &provider.Router{}, filepath.Join(t.TempDir(), "missing.db"))
	if tool != nil || err == nil {
		t.Errorf("missing -rag-db file should error, got tool=%v err=%v", tool, err)
	}
}

func TestEmbeddingSelector(t *testing.T) {
	cfg := &config.Config{
		Defaults: map[string]string{"embedding": "emb"},
		Models: map[string]config.ModelConfig{
			"emb":    {Name: "nomic", Provider: "ollama", Fallbacks: []string{"backup"}},
			"backup": {Name: "nomic-fallback", Provider: "ollama"},
		},
	}
	got, err := embeddingChain(cfg)
	if err != nil {
		t.Fatalf("embeddingChain: %v", err)
	}
	want := []string{"ollama/nomic", "ollama/nomic-fallback"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("embeddingChain = %v, want %v", got, want)
	}
	if got, err := embeddingChain(&config.Config{Defaults: map[string]string{}}); err == nil || got != nil {
		t.Errorf("embeddingChain with no default = %v, %v; want nil, error", got, err)
	}
}

type fakeEmbedPlan struct {
	resp *provider.EmbedResponse
}

func (p fakeEmbedPlan) ExecuteEmbed(context.Context) (*provider.EmbedResponse, error) {
	return p.resp, nil
}

func TestChainEmbedderRoutesWithPreferredChain(t *testing.T) {
	var captured provider.RoutingRequest
	embedder := newChainEmbedder(func(ctx context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		captured = rr
		return fakeEmbedPlan{resp: &provider.EmbedResponse{
			Model:      "nomic-fallback",
			Provider:   "ollama",
			Embeddings: [][]float64{{1, 2, 3}},
			RouteOutcome: &provider.RouteOutcome{
				ActualModel: provider.ModelKey{Provider: "ollama", Model: "nomic-fallback"},
			},
		}}, nil
	}, []string{"ollama/nomic", "ollama/nomic-fallback"})

	res, err := embedder.Embed(context.Background(), "ollama/nomic", []string{"query"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if captured.Model != "" {
		t.Errorf("RoutingRequest.Model = %q, want empty so chain controls selection", captured.Model)
	}
	if !captured.StrictChain {
		t.Error("StrictChain = false, want true")
	}
	if len(captured.PreferredChain) != 2 || captured.PreferredChain[1] != "ollama/nomic-fallback" {
		t.Errorf("PreferredChain = %v, want embedding fallback chain", captured.PreferredChain)
	}
	if captured.RequiredCaps != provider.CapEmbed {
		t.Errorf("RequiredCaps = %v, want CapEmbed", captured.RequiredCaps)
	}
	if res.VectorSpaceID != "ollama/nomic-fallback" {
		t.Errorf("VectorSpaceID = %q, want actual routed model", res.VectorSpaceID)
	}
}

var _ rag.Embedder = newChainEmbedder(nil, nil)
