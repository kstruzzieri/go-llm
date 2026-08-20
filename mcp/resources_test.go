package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/configview"
)

// TestConfigViewResource pins the additive go-llm://configview/v1 resource:
// it renders the configview snapshot, performs ZERO network I/O on read, and
// leaves the overloaded chat role requirement-less (candidates stay unknown).
func TestConfigViewResource(t *testing.T) {
	var hits atomic.Int64
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	cfgPath := filepath.Join(t.TempDir(), "models.json")
	cfgBody := `{
  "providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}},
	"models": {
	  "agent": {"name": "m1", "provider": "local", "type": "dense"},
	  "completion": {"name": "fim", "provider": "local", "type": "dense", "capabilities": ["generate", "insert"]}
	},
	"defaults": {"chat": "agent", "completion": "completion", "embedding": "agent"}
}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t, mock, WithConfig(cfgPath))
	defer env.cleanup()

	before := hits.Load()
	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://configview/v1",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}
	if got := hits.Load(); got != before {
		t.Fatalf("configview read performed %d network request(s); must be zero", got-before)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected non-empty contents")
	}

	var snap configview.Snapshot
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &snap); err != nil {
		t.Fatalf("configview/v1 not a Snapshot: %v", err)
	}
	if !snap.Ready {
		t.Fatalf("snapshot not ready: %+v", snap.Diagnostics)
	}
	if snap.Origin.Source != "explicit_path" {
		t.Fatalf("origin = %q, want explicit_path", snap.Origin.Source)
	}
	if strings.Contains(result.Contents[0].Text, cfgPath) {
		t.Fatal("resource leaks the config path")
	}
	// chat is deliberately requirement-less at the MCP boundary (it serves
	// both chat and generate ops); its candidates must be unknown, not guessed.
	sawChat := false
	for _, b := range snap.Bindings {
		if b.UseCase != "chat" {
			continue
		}
		sawChat = true
		for _, c := range b.Candidates {
			if c.Eligibility != configview.EligibilityUnknown {
				t.Fatalf("chat candidate %s = %s, want unknown (no declared shape)", c.Selector, c.Eligibility)
			}
		}
	}
	if !sawChat {
		t.Fatal("no chat binding in snapshot")
	}

	sawCompletion := false
	for _, b := range snap.Bindings {
		if b.UseCase != "completion" {
			continue
		}
		for _, c := range b.Candidates {
			if c.Selector != "local/fim" {
				continue
			}
			sawCompletion = true
			if c.Eligibility != configview.EligibilityEligible {
				t.Fatalf("completion candidate %s = %s, want eligible", c.Selector, c.Eligibility)
			}
		}
	}
	if !sawCompletion {
		t.Fatal("no local/fim completion candidate in snapshot")
	}
}

func TestConfigViewResourceReportsAutoDiscoveredOrigin(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "models.json")
	cfgBody := `{
  "providers": {"local": {"base_url": "http://localhost:1", "api_format": "openai-compat"}},
  "models": {"agent": {"name": "m1", "provider": "local", "type": "dense"}},
  "defaults": {"chat": "agent"}
}`
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_LLM_CONFIG", cfgPath)

	env := newTestEnv(t, http.NotFoundHandler())
	defer env.cleanup()

	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://configview/v1",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}

	var snap configview.Snapshot
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &snap); err != nil {
		t.Fatalf("configview/v1 not a Snapshot: %v", err)
	}
	if snap.Origin.Source != "env_override" {
		t.Fatalf("origin = %q, want env_override", snap.Origin.Source)
	}
	if strings.Contains(result.Contents[0].Text, cfgPath) {
		t.Fatal("resource leaks the auto-discovered config path")
	}
}

func TestHealthResource(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://health",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("expected non-empty contents")
	}

	var health map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &health); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}

	ollamaSection, ok := health["ollama"].(map[string]any)
	if !ok {
		t.Fatal("missing ollama section in health")
	}
	if _, ok := ollamaSection["available"]; !ok {
		t.Error("missing 'available' field in ollama section")
	}

	ragSection, ok := health["rag"].(map[string]any)
	if !ok {
		t.Fatal("missing rag section in health")
	}
	if enabled, ok := ragSection["enabled"].(bool); !ok || enabled {
		t.Error("rag.enabled should be false in test (RAG disabled)")
	}
}

func TestModelsResource(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://models",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}

	var models []map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(models) != 1 {
		t.Errorf("model count = %d, want 1", len(models))
	}
}

func TestRAGStatsResourceDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://rag/stats",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}

	var stats map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if enabled, ok := stats["enabled"].(bool); !ok || enabled {
		t.Error("expected enabled=false when RAG disabled")
	}
}

func TestConfigResource(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://config",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	if _, ok := cfg["has_config"]; !ok {
		t.Error("missing 'has_config' field")
	}
	if _, ok := cfg["resolved"]; !ok {
		t.Error("missing 'resolved' field")
	}
}

func TestModelsResourceRoutesThroughRegistry(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","size":4700000000}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	env := newTestEnv(t, mock)
	defer env.cleanup()

	res, err := env.session.ReadResource(context.Background(), &gomcp.ReadResourceParams{
		URI: "go-llm://models",
	})
	if err != nil {
		t.Fatalf("ReadResource() error = %v", err)
	}
	if len(res.Contents) == 0 {
		t.Fatalf("ReadResource() returned no contents")
	}
	// listedModelInfo carries a "provider" field; the old direct-ollama path
	// (raw ollama.ModelInfo) did not. Asserting it proves we now route through
	// s.listModels / the registry.
	var listed []listedModelInfo
	if err := json.Unmarshal([]byte(res.Contents[0].Text), &listed); err != nil {
		t.Fatalf("unmarshal listed models: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("model count = %d, want 1", len(listed))
	}
	if listed[0].Provider == "" {
		t.Errorf("provider field empty; resource did not route through the registry")
	}
}

func TestModelDetailResourceUsesProviderRegistry(t *testing.T) {
	openAIMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-fim","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openAIMock.Close()

	cfgPath := writeOpenAICompatOnlyConfig(t, t.TempDir(), openAIMock.URL)
	s, err := NewServer(context.Background(), WithConfig(cfgPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.handleModelDetailResource(context.Background(), &gomcp.ReadResourceRequest{
		Params: &gomcp.ReadResourceParams{URI: "go-llm://models/local-fim"},
	})
	if err != nil {
		t.Fatalf("handleModelDetailResource() error = %v", err)
	}

	var info struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &info); err != nil {
		t.Fatalf("unmarshal model detail: %v", err)
	}
	if info.Provider != "vllm-local" || info.Name != "local-fim" {
		t.Fatalf("model detail = %+v, want vllm-local/local-fim", info)
	}
}

func TestModelDetailResourceUsesDirectMetadataWhenListFails(t *testing.T) {
	showHit := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			http.Error(w, `{"error":"tags unavailable"}`, http.StatusServiceUnavailable)
		case "/api/show":
			showHit = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B","quantization_level":"Q4_K_M"},"digest":"abc123","template":"{{.Prompt}}"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	s, err := NewServer(context.Background(), WithOllamaURL(mock.URL), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.handleModelDetailResource(context.Background(), &gomcp.ReadResourceRequest{
		Params: &gomcp.ReadResourceParams{URI: "go-llm://models/qwen3:8b"},
	})
	if err != nil {
		t.Fatalf("handleModelDetailResource() error = %v", err)
	}
	if !showHit {
		t.Fatal("model detail resource did not fall back to direct model metadata")
	}

	var info struct {
		Provider      string `json:"provider"`
		Name          string `json:"name"`
		Family        string `json:"family"`
		ParameterSize string `json:"parameter_size"`
	}
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &info); err != nil {
		t.Fatalf("unmarshal model detail: %v", err)
	}
	if info.Provider != "ollama" || info.Name != "qwen3:8b" {
		t.Fatalf("model detail = %+v, want ollama/qwen3:8b", info)
	}
	if info.Family != "qwen3" || info.ParameterSize != "8B" {
		t.Fatalf("model detail = %+v, want qwen3 8B metadata", info)
	}
}
