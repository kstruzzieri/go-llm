package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func embedCfg() *config.Config {
	return &config.Config{
		Defaults: map[string]string{"embedding": "emb"},
		Models:   map[string]config.ModelConfig{"emb": {Name: "nomic", Provider: "ollama"}},
	}
}

func TestResolveRetriever_OmittedWhenNilRouter(t *testing.T) {
	tool, ok := resolveRetriever(context.Background(), embedCfg(), nil, filepath.Join(t.TempDir(), "x.db"))
	if ok || tool != nil {
		t.Errorf("expected omission for nil router, got ok=%v tool=%v", ok, tool)
	}
}

func TestResolveRetriever_OmittedWhenNoEmbeddingDefault(t *testing.T) {
	// Non-nil router + non-empty dbPath so execution reaches (and stops at) the
	// embedding-default guard rather than an earlier short-circuit.
	cfg := &config.Config{Defaults: map[string]string{}}
	tool, ok := resolveRetriever(context.Background(), cfg, &provider.Router{}, "/some/path.db")
	if ok || tool != nil {
		t.Errorf("expected omission with no defaults.embedding, got ok=%v tool=%v", ok, tool)
	}
}

func TestResolveRetriever_OmittedWhenDBMissing(t *testing.T) {
	// Non-nil router + resolvable embedding default so execution reaches the
	// os.Stat existence check; the missing path is the condition under test.
	// The zero-value router is never dereferenced because os.Stat fails first.
	tool, ok := resolveRetriever(context.Background(), embedCfg(), &provider.Router{}, filepath.Join(t.TempDir(), "missing.db"))
	if ok || tool != nil {
		t.Errorf("expected omission for missing DB file, got ok=%v tool=%v", ok, tool)
	}
}

func TestEmbeddingSelector(t *testing.T) {
	cfg := &config.Config{
		Defaults: map[string]string{"embedding": "emb"},
		Models:   map[string]config.ModelConfig{"emb": {Name: "nomic", Provider: "ollama"}},
	}
	if got := embeddingSelector(cfg); got != "ollama/nomic" {
		t.Errorf("embeddingSelector = %q, want ollama/nomic", got)
	}
	if got := embeddingSelector(&config.Config{Defaults: map[string]string{}}); got != "" {
		t.Errorf("embeddingSelector with no default = %q, want empty", got)
	}
}
