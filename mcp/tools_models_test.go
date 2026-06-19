package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

func TestListModelsToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","size":4700000000},{"name":"nomic-embed-text","size":274000000}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "list_models",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var models []ollama.ModelInfo
	if err := json.Unmarshal([]byte(text), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("model count = %d, want 2", len(models))
	}
}

func TestListModelsToolUsesProviderRegistryWhenOllamaUnavailable(t *testing.T) {
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

	result, err := s.handleListModels(context.Background(), &gomcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("handleListModels() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("handleListModels() isError = true, content = %v", extractText(result))
	}

	var models []struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal([]byte(extractText(result)), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("model count = %d, want 1", len(models))
	}
	if models[0].Provider != "vllm-local" || models[0].Name != "local-fim" {
		t.Fatalf("model = %+v, want vllm-local/local-fim", models[0])
	}
}

func TestShowModelToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			// Ollama's /api/show response nests model details under "details".
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B","quantization_level":"Q4_K_M"},"digest":"abc123","template":"{{.Prompt}}"}`))

		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "show_model",
		Arguments: map[string]any{
			"name": "qwen3:8b",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var info ollama.ModelInfo
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("unmarshal model info: %v", err)
	}
	if info.Family != "qwen3" {
		t.Errorf("family = %q, want %q", info.Family, "qwen3")
	}
}

func TestShowModelToolUsesProviderRegistry(t *testing.T) {
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

	result, err := s.handleShowModel(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{
			Arguments: json.RawMessage(`{"name":"local-fim"}`),
		},
	})
	if err != nil {
		t.Fatalf("handleShowModel() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("handleShowModel() isError = true, content = %v", extractText(result))
	}

	var info struct {
		Provider     string   `json:"provider"`
		Name         string   `json:"name"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(extractText(result)), &info); err != nil {
		t.Fatalf("unmarshal model info: %v", err)
	}
	if info.Provider != "vllm-local" || info.Name != "local-fim" {
		t.Fatalf("model info = %+v, want vllm-local/local-fim", info)
	}
	if !containsAll(info.Capabilities, "generate", "stream", "insert") {
		t.Fatalf("capabilities = %v, want generate, stream, insert", info.Capabilities)
	}
}

func TestShowModelToolQueriesLiveInventoryBeforeCachedProfile(t *testing.T) {
	reg := provider.NewRegistry()
	p := &mutableModelProvider{
		fakeRouteProvider: &fakeRouteProvider{name: "vllm-local"},
		models:            []provider.ModelInfo{{Name: "old-model"}},
	}
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	mr, err := provider.NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	s := &Server{
		providerRegistry: reg,
		modelRegistry:    mr,
	}

	key := provider.ModelKey{Provider: "vllm-local", Model: "old-model"}
	if _, err := s.lookupProviderModelInfo(context.Background(), key); err != nil {
		t.Fatalf("initial lookupProviderModelInfo() error = %v", err)
	}

	p.models = []provider.ModelInfo{{Name: "new-model"}}
	_, err = s.lookupProviderModelInfo(context.Background(), key)
	if err == nil {
		t.Fatal("lookupProviderModelInfo() error = nil, want stale cached model rejected")
	}
	if !strings.Contains(err.Error(), `model "old-model" not found`) {
		t.Fatalf("lookupProviderModelInfo() error = %q, want stale model not found", err)
	}
}

func containsAll(got []string, want ...string) bool {
	seen := make(map[string]bool, len(got))
	for _, item := range got {
		seen[item] = true
	}
	for _, item := range want {
		if !seen[item] {
			return false
		}
	}
	return true
}

func TestShowModelToolEmptyName(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "show_model",
		Arguments: map[string]any{
			"name": "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty name")
	}
	if text := extractText(result); !strings.Contains(text, "name must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "name must not be empty")
	}
}

func TestPullModelToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/pull":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "pull_model",
		Arguments: map[string]any{
			"name": "qwen3:8b",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	text := extractText(result)
	if !strings.Contains(text, "pulled successfully") {
		t.Errorf("result = %q, want to contain %q", text, "pulled successfully")
	}
}

func TestPullModelToolOllamaError(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/pull":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "pull_model",
		Arguments: map[string]any{
			"name": "nonexistent",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true on pull failure")
	}
	if text := extractText(result); !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}

// pullerForModel selects the provider that owns the model when that provider is
// a ModelPuller.
func TestPullerForModel_OllamaIsPuller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	if err := reg.Register(provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.AddModelToIndex("qwen3:8b", "ollama"); err != nil {
		t.Fatalf("AddModelToIndex: %v", err)
	}

	puller, err := pullerForModel(reg, "qwen3:8b", "")
	if err != nil {
		t.Fatalf("pullerForModel() error = %v", err)
	}
	if puller == nil {
		t.Fatalf("pullerForModel returned nil for an Ollama-backed model; want a puller")
	}
}

// pullerForModel returns nil when only a file-managed (openai-compat) provider
// owns the model — pull is unsupported there.
func TestPullerForModel_OpenAICompatNotPuller(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"llama-3"}]}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	oc := openaicompat.NewProvider(openaicompat.NewClient(srv.URL),
		openaicompat.WithProviderName("llamacpp"))
	if err := reg.Register(oc); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := reg.AddModelToIndex("llama-3", "llamacpp"); err != nil {
		t.Fatalf("AddModelToIndex: %v", err)
	}

	puller, err := pullerForModel(reg, "llama-3", "")
	if err != nil {
		t.Fatalf("pullerForModel() error = %v", err)
	}
	if puller != nil {
		t.Errorf("pullerForModel returned non-nil for a file-managed backend; want nil")
	}
}

func TestPullerForModel_UnknownModelWithMultiplePullersRequiresProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	for _, name := range []string{"ollama-a", "ollama-b"} {
		p := provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)),
			provider.WithProviderName(name))
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}

	puller, err := pullerForModel(reg, "not-yet-installed", "")
	if err == nil {
		t.Fatalf("pullerForModel() error = nil, want ambiguous-provider error; puller = %#v", puller)
	}
	if !strings.Contains(err.Error(), "multiple pull-capable providers") {
		t.Fatalf("pullerForModel() error = %q, want multiple pull-capable providers", err)
	}
}

func TestPullerForModel_ExplicitProviderResolvesUnknownModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	p := provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)),
		provider.WithProviderName("shared-ollama"))
	if err := reg.Register(p); err != nil {
		t.Fatalf("Register: %v", err)
	}

	puller, err := pullerForModel(reg, "not-yet-installed", "shared-ollama")
	if err != nil {
		t.Fatalf("pullerForModel() error = %v", err)
	}
	if puller == nil {
		t.Fatalf("pullerForModel returned nil for explicit pull-capable provider")
	}
}

// With a registry present, an unresolvable model must not silently hit the
// direct legacy Ollama client; it should return a clear not-found error
// instead.
//
// The model must genuinely fail provider resolution to exercise the guard. A
// single-provider registry auto-resolves any unknown model to that sole
// provider (see inferProviderForExplicitModel), so this test registers two
// providers whose inventories are empty: resolution then returns no provider
// and showModel reaches the fallback branch. A recording legacy client proves
// the guard prevents the direct /api/show fallback.
func TestShowModel_NoOllamaFallbackWhenRegistryPresent(t *testing.T) {
	emptyInventory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer emptyInventory.Close()

	legacyShowHit := false
	legacySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			legacyShowHit = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"details":{}}`))
	}))
	defer legacySrv.Close()

	reg := provider.NewRegistry()
	for _, name := range []string{"ollama-a", "ollama-b"} {
		p := provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(emptyInventory.URL)),
			provider.WithProviderName(name))
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register(%s): %v", name, err)
		}
	}
	mr, err := provider.NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error = %v", err)
	}
	s := &Server{
		providerRegistry: reg,
		modelRegistry:    mr,
		client:           ollama.NewClient(ollama.WithBaseURL(legacySrv.URL)),
	}

	_, err = s.showModel(context.Background(), "definitely-not-a-real-model", "")
	if err == nil {
		t.Fatalf("showModel() error = nil, want not-found error")
	}
	if !strings.Contains(err.Error(), "not found in any registered provider") {
		t.Fatalf("showModel() error = %q, want registered-provider not-found", err)
	}
	if legacyShowHit {
		t.Errorf("showModel hit the direct Ollama /api/show despite a registry being present")
	}
}
