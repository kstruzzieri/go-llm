package providerbootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestAPIKeyEnvExpansionReachesTransport(t *testing.T) {
	var gotAuth atomic.Value
	gotAuth.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "test-model"}}})
		case "/v1/chat/completions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("GOLEM_IT_KEY", "sk-expanded-secret")

	cfgJSON := `{
  "providers": {
    "hosted": {"base_url": "` + srv.URL + `", "api_format": "openai-compat", "api_key": "${GOLEM_IT_KEY}"}
  },
  "models": {"agent": {"name": "test-model", "provider": "hosted", "type": "dense"}},
  "defaults": {"agent": "agent"}
}`
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	ctx := context.Background()
	bundle, err := New(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatalf("providerbootstrap.New: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	p, ok := bundle.Providers.Get("hosted")
	if !ok {
		t.Fatal("provider \"hosted\" not registered")
	}

	_, err = p.Chat(ctx, provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if got := gotAuth.Load().(string); got != "Bearer sk-expanded-secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-expanded-secret")
	}
}
