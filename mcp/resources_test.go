package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
