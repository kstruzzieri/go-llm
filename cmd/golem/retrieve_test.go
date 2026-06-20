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
		Models:   map[string]config.ModelConfig{"emb": {Name: "nomic", Provider: "ollama"}},
	}
	if got := embeddingSelector(cfg); got != "ollama/nomic" {
		t.Errorf("embeddingSelector = %q, want ollama/nomic", got)
	}
	if got := embeddingSelector(&config.Config{Defaults: map[string]string{}}); got != "" {
		t.Errorf("embeddingSelector with no default = %q, want empty", got)
	}
}
