package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestNewServerDefaults(t *testing.T) {
	ctx := context.Background()
	// RAG disabled so we don't need a real SQLite DB.
	s, err := NewServer(ctx, WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.ollamaURL != defaultOllamaURL {
		t.Errorf("ollamaURL = %q, want %q", s.ollamaURL, defaultOllamaURL)
	}
	if s.client == nil {
		t.Error("client is nil, want non-nil")
	}
	if s.mcpServer == nil {
		t.Error("mcpServer is nil, want non-nil")
	}
	if s.ragDisabled != true {
		t.Error("ragDisabled = false, want true")
	}
}

func TestNewServerWithOptions(t *testing.T) {
	ctx := context.Background()
	customURL := "http://custom:1234"

	s, err := NewServer(ctx,
		WithOllamaURL(customURL),
		WithRAGDisabled(),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.ollamaURL != customURL {
		t.Errorf("ollamaURL = %q, want %q", s.ollamaURL, customURL)
	}
	if s.ragDisabled != true {
		t.Error("ragDisabled = false, want true")
	}
	// Store should be nil when RAG is disabled.
	if s.store != nil {
		t.Error("store is non-nil, want nil when RAG disabled")
	}
}

func TestNewServerUsesConfiguredOllamaProvider(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3.5:27b"},{"name":"qwen3.5:35b-a3b"},{"name":"qwen3-coder-next:latest"},{"name":"qwen3-embedding:8b"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	cfgPath := writeTestConfig(t, t.TempDir(), mock.URL)

	s, err := NewServer(context.Background(), WithConfig(cfgPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.ollamaURL != mock.URL {
		t.Fatalf("ollamaURL = %q, want %q", s.ollamaURL, mock.URL)
	}
	if !s.ollamaAvailable {
		t.Fatal("ollamaAvailable = false, want true")
	}
	if s.Completer() == nil {
		t.Fatal("Completer() = nil, want resolved completion provider")
	}
}

func TestNewServerReturnsErrorForInvalidAutoDiscoveredConfig(t *testing.T) {
	tmpDir := t.TempDir()
	unsetEnv(t, "GO_LLM_CONFIG")
	t.Setenv("HOME", tmpDir)
	t.Chdir(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "models.json"), []byte("{"), 0o600); err != nil {
		t.Fatalf("WriteFile(models.json) error = %v", err)
	}

	_, err := NewServer(context.Background(), WithRAGDisabled())
	if err == nil {
		t.Fatal("NewServer() error = nil, want parse error")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error = %q, want parse failure", err)
	}
}

func TestNewServerWithoutAutoDiscoveredConfigContinues(t *testing.T) {
	tmpDir := t.TempDir()
	unsetEnv(t, "GO_LLM_CONFIG")
	t.Setenv("HOME", tmpDir)
	t.Chdir(tmpDir)

	s, err := NewServer(context.Background(),
		WithOllamaURL("http://127.0.0.1:1"),
		WithRAGDisabled(),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer s.Close()

	if s.cfg != nil {
		t.Fatalf("cfg = %#v, want nil when no config is found", s.cfg)
	}
}

func TestRebuildDerivedClientsRequiresResolvedEmbedding(t *testing.T) {
	s := &Server{
		client: ollama.NewClient(),
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"ollama": {BaseURL: defaultOllamaURL},
			},
			Models: map[string]config.ModelConfig{
				"embedding": {Name: "qwen3-embedding:8b", Provider: "ollama", Type: "embedding"},
			},
			Defaults: map[string]string{
				"embedding": "embedding",
			},
		},
		store:    stubVectorStore{},
		resolved: make(map[string]config.ResolvedModel),
	}

	s.rebuildDerivedClients()
	if s.Indexer() != nil {
		t.Fatal("Indexer() != nil without a resolved embedding model")
	}
	if s.Retriever() != nil {
		t.Fatal("Retriever() != nil without a resolved embedding model")
	}

	s.resolved["embedding"] = config.ResolvedModel{Name: "qwen3-embedding:8b", Role: "embedding"}
	s.rebuildDerivedClients()
	if s.Indexer() == nil {
		t.Fatal("Indexer() = nil, want non-nil after embedding resolution")
	}
	if s.Retriever() == nil {
		t.Fatal("Retriever() = nil, want non-nil after embedding resolution")
	}
}

type stubVectorStore struct{}

func (stubVectorStore) Store(context.Context, []rag.Chunk, [][]float64) error { return nil }

func (stubVectorStore) Search(context.Context, []float64, int) ([]rag.SearchResult, error) {
	return nil, nil
}

func (stubVectorStore) DeleteBySource(context.Context, string) error { return nil }

func (stubVectorStore) Stats(context.Context) (rag.StoreStats, error) { return rag.StoreStats{}, nil }

func (stubVectorStore) Close() error { return nil }

func writeTestConfig(t *testing.T, dir, baseURL string) string {
	t.Helper()

	path := filepath.Join(dir, "models.json")
	data := `{
  "providers": {
    "ollama": {
      "base_url": "` + baseURL + `",
      "timeout": "17s"
    }
  },
  "models": {
    "general": {"name": "qwen3.5:27b", "provider": "ollama", "type": "dense", "fallbacks": ["lightweight"]},
    "fast": {"name": "qwen3.5:35b-a3b", "provider": "ollama", "type": "moe", "fallbacks": ["lightweight"]},
    "coding": {"name": "qwen3-coder-next:latest", "provider": "ollama", "type": "dense", "fallbacks": ["general", "lightweight"]},
    "lightweight": {"name": "qwen3:8b", "provider": "ollama", "type": "dense"},
    "embedding": {"name": "qwen3-embedding:8b", "provider": "ollama", "type": "embedding", "dimensions": 4096}
  },
  "defaults": {
    "chat": "general",
    "completion": "coding",
    "embedding": "embedding",
    "agent": "fast",
    "analysis": "general"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()

	oldValue, hadValue := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("Unsetenv(%q) error = %v", key, err)
	}
	t.Cleanup(func() {
		var err error
		if hadValue {
			err = os.Setenv(key, oldValue)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			panic(err)
		}
	})
}
