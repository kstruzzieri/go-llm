package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestNewServerDefaults(t *testing.T) {
	ctx := context.Background()
	// RAG disabled so we don't need a real SQLite DB.
	s, err := NewServer(ctx, WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

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
	defer func() { _ = s.Close() }()

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

func TestConfiguredProvidersDoesNotOverrideOpenAICompatProviderNamedOllama(t *testing.T) {
	var openAIHit atomic.Bool
	openAIMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAIHit.Store(true)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-fim","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openAIMock.Close()

	var overrideHit atomic.Bool
	overrideMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overrideHit.Store(true)
		http.NotFound(w, r)
	}))
	defer overrideMock.Close()

	s := &Server{
		ollamaURL:         overrideMock.URL,
		ollamaURLExplicit: true,
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"ollama": {BaseURL: openAIMock.URL, APIFormat: "openai-compat"},
			},
		},
	}

	// Black-box parity guard over providerbootstrap wiring: an openai-compat
	// provider named "ollama" must reach its own base URL, must NOT inherit the
	// WithOllamaURL override, and must leave s.ollamaProv nil (so the warmth
	// source is not wired for it).
	if err := s.ensureModelRegistry(context.Background()); err != nil {
		t.Fatalf("ensureModelRegistry() error = %v", err)
	}
	if s.ollamaProv != nil {
		t.Fatal("ollamaProv = non-nil, want nil for openai-compatible provider named ollama")
	}
	if s.warmthSource != nil {
		t.Fatal("warmthSource = non-nil, want nil for openai-compatible provider named ollama")
	}
	prov, ok := s.providerRegistry.Get("ollama")
	if !ok {
		t.Fatal("provider \"ollama\" not registered")
	}
	models, err := prov.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error = %v", err)
	}
	if len(models) != 1 || models[0].Name != "local-fim" {
		t.Fatalf("Models() = %+v, want local-fim from openai-compatible endpoint", models)
	}
	if !openAIHit.Load() {
		t.Fatal("openai-compatible base URL was not used")
	}
	if overrideHit.Load() {
		t.Fatal("WithOllamaURL override was incorrectly applied to openai-compatible provider named ollama")
	}
}

func TestEnsureModelRegistryConcurrentInitInstallsExactlyOneStack(t *testing.T) {
	// Drive the double-checked-locking init race directly: many goroutines call
	// ensureModelRegistry at once. Losers must discard their freshly built bundle
	// (close it) and keep the winner's; the installed stack must be coherent and
	// fields must never be spliced across bundles. Run under -race to catch any
	// unsynchronized access. A mock openai-compat endpoint gives New real work.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer mock.Close()

	s := &Server{
		ollamaURL: "http://127.0.0.1:0",
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"lc": {BaseURL: mock.URL, APIFormat: "openai-compat"},
			},
		},
	}
	defer func() {
		if s.router != nil {
			_ = s.router.Close()
		}
	}()

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			errs[idx] = s.ensureModelRegistry(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: ensureModelRegistry() error = %v", i, err)
		}
	}
	if s.router == nil || s.modelRegistry == nil || s.providerRegistry == nil {
		t.Fatalf("expected a coherent installed stack, got router=%v models=%v providers=%v",
			s.router, s.modelRegistry, s.providerRegistry)
	}
	// A subsequent call must take the fast path and not replace the stack.
	installed := s.providerRegistry
	if err := s.ensureModelRegistry(context.Background()); err != nil {
		t.Fatalf("post-init ensureModelRegistry() error = %v", err)
	}
	if s.providerRegistry != installed {
		t.Fatal("provider registry was replaced after init; fast path did not hold")
	}
}

func TestEnsureModelRegistryRejectsInvalidProviderKeys(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr string
	}{
		{
			name:    "empty",
			key:     "",
			wantErr: "provider name must not be empty",
		},
		{
			name:    "slash",
			key:     "team/local",
			wantErr: `provider name "team/local" must not contain "/"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{
				cfg: &config.Config{
					Providers: map[string]config.ProviderConfig{
						tt.key: {BaseURL: "http://localhost:11434", APIFormat: "ollama"},
					},
				},
			}

			err := s.ensureModelRegistry(context.Background())
			if err == nil {
				t.Fatal("ensureModelRegistry() error = nil, want invalid provider key error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
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
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"template":"{{ .Prompt }}{{ .Suffix }}","capabilities":["completion","insert"]}`))
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
	defer func() { _ = s.Close() }()

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

func TestNewServerUsesSingleNamedOllamaProviderForLegacyClient(t *testing.T) {
	var pullHit atomic.Bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"local-model"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion"]}`))
		case "/api/pull":
			pullHit.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	path := filepath.Join(t.TempDir(), "models.json")
	data := `{
  "providers": {
    "local-ollama": {
      "base_url": "` + mock.URL + `",
      "timeout": "17s",
      "api_format": "ollama"
    }
  },
  "models": {
    "general": {"name": "local-model", "provider": "local-ollama", "type": "dense"}
  },
  "defaults": {
    "chat": "general"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	s, err := NewServer(context.Background(), WithConfig(path), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.ollamaURL != mock.URL {
		t.Fatalf("ollamaURL = %q, want named Ollama provider URL %q", s.ollamaURL, mock.URL)
	}
	if !s.ollamaAvailable {
		t.Fatal("ollamaAvailable = false, want true")
	}

	result, err := s.handlePullModel(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{
			Arguments: json.RawMessage(`{"name":"local-model"}`),
		},
	})
	if err != nil {
		t.Fatalf("handlePullModel() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("handlePullModel() isError = true, content = %v", extractText(result))
	}
	if !pullHit.Load() {
		t.Fatal("pull_model did not use the named Ollama provider URL")
	}
}

func TestNewServerRegistersConfiguredProvidersAndOverridesCapabilities(t *testing.T) {
	ollamaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"template":"{{ .Prompt }}{{ .Suffix }}","capabilities":["completion","insert"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollamaMock.Close()

	var sawBearer atomic.Bool
	openAIMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-token" {
			sawBearer.Store(true)
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-chat","object":"model"},{"id":"local-carved","object":"model"},{"id":"local-fim","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openAIMock.Close()

	cfgPath := writeProviderWiringConfig(t, t.TempDir(), ollamaMock.URL, openAIMock.URL)
	s, err := NewServer(context.Background(), WithConfig(cfgPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

	if s.providerRegistry == nil {
		t.Fatal("providerRegistry = nil")
	}
	if got, want := s.providerRegistry.Names(), []string{"ollama", "vllm-local"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("providerRegistry.Names() = %v, want %v", got, want)
	}
	if _, ok := s.providerRegistry.Get("vllm-local"); !ok {
		t.Fatal("OpenAI-compatible provider instance vllm-local was not registered")
	}
	if !sawBearer.Load() {
		t.Fatal("OpenAI-compatible provider did not use configured API key")
	}

	resolved := s.snapshotResolved()
	if got := resolved["completion"].Provider; got != "vllm-local" {
		t.Fatalf("resolved completion provider = %q, want vllm-local", got)
	}

	profile, err := s.modelRegistry.Lookup(context.Background(), provider.ModelKey{Provider: "vllm-local", Model: "local-chat"})
	if err != nil {
		t.Fatalf("Lookup(vllm-local/local-chat) error = %v", err)
	}
	if !profile.Caps.Has(provider.CapChat | provider.CapGenerate | provider.CapStream) {
		t.Fatalf("local-chat caps = %v, want openai-compatible type-derived chat+generate+stream floor", profile.Caps)
	}

	profile, err = s.modelRegistry.Lookup(context.Background(), provider.ModelKey{Provider: "vllm-local", Model: "local-carved"})
	if err != nil {
		t.Fatalf("Lookup(vllm-local/local-carved) error = %v", err)
	}
	if !profile.Caps.Has(provider.CapChat) || !profile.Caps.Has(provider.CapStream) {
		t.Fatalf("local-carved caps = %v, want chat+stream override", profile.Caps)
	}
	if profile.Caps.Has(provider.CapGenerate) {
		t.Fatalf("local-carved caps = %v, want config override to remove generate", profile.Caps)
	}
}

func TestNewServerRejectsConflictingCapabilityOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	data := `{
  "providers": {
    "vllm-local": {
      "base_url": "http://127.0.0.1:1",
      "timeout": "17s",
      "api_format": "openai-compat"
    }
  },
  "models": {
    "chat": {"name": "shared-model", "provider": "vllm-local", "type": "dense", "capabilities": ["chat", "stream"]},
    "completion": {"name": "shared-model", "provider": "vllm-local", "type": "dense", "capabilities": ["generate", "stream", "insert"]}
  },
  "defaults": {
    "chat": "chat",
    "completion": "completion"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}

	_, err := NewServer(context.Background(),
		WithConfig(path),
		WithOllamaURL("http://127.0.0.1:1"),
		WithRAGDisabled(),
	)
	if err == nil {
		t.Fatal("NewServer() error = nil, want conflicting capability override error")
	}
	if !strings.Contains(err.Error(), "conflicting capability overrides for vllm-local/shared-model") {
		t.Fatalf("NewServer() error = %q, want conflicting capability override message", err)
	}
}

func TestModelKeyForCompletionInfersConfiguredProvider(t *testing.T) {
	ollamaMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["embedding"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ollamaMock.Close()

	openAIMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-chat","object":"model"},{"id":"local-carved","object":"model"},{"id":"local-fim","object":"model"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer openAIMock.Close()

	cfgPath := writeProviderWiringConfig(t, t.TempDir(), ollamaMock.URL, openAIMock.URL)
	s, err := NewServer(context.Background(), WithConfig(cfgPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	defer func() { _ = s.Close() }()

	key, err := s.modelKeyForCompletion(context.Background(), "local-fim", "")
	if err != nil {
		t.Fatalf("modelKeyForCompletion() error = %v", err)
	}
	if key.Provider != "vllm-local" || key.Model != "local-fim" {
		t.Fatalf("modelKeyForCompletion() = %v, want vllm-local/local-fim", key)
	}
}

func TestNewCompletionProviderPinsResolvedProvider(t *testing.T) {
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

	router := newRecordingRouteEngine("routed-fim")
	s.mu.Lock()
	s.router = router
	s.mu.Unlock()

	p, err := s.newCompletionProvider(context.Background(), "local-fim", "vllm-local")
	if err != nil {
		t.Fatalf("newCompletionProvider() error = %v", err)
	}
	resp, err := p.Complete(context.Background(), completion.FIMRequest{
		Prefix: "func f() int { return ",
		Suffix: " }",
	})
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if resp.Completion != "routed-fim" {
		t.Fatalf("Completion = %q, want routed-fim", resp.Completion)
	}
	if router.last.Model != "vllm-local/local-fim" {
		t.Fatalf("RoutingRequest.Model = %q, want vllm-local/local-fim", router.last.Model)
	}
	if router.last.Provider != "" {
		t.Fatalf("RoutingRequest.Provider = %q, want empty because qualified Model is authoritative", router.last.Provider)
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
	defer func() { _ = s.Close() }()

	if s.cfg != nil {
		t.Fatalf("cfg = %#v, want nil when no config is found", s.cfg)
	}
	if s.modelRegistry == nil {
		t.Fatal("modelRegistry = nil, want degraded-mode registry for later recovery")
	}
	if s.ollamaProv == nil {
		t.Fatal("ollamaProv = nil, want provider initialized in degraded mode")
	}
}

func TestNewServerClosesBootstrapResourcesOnTranscriptStartupFailure(t *testing.T) {
	psStarted := make(chan struct{})
	psDone := make(chan struct{})
	releasePS := make(chan struct{})
	var psOnce sync.Once
	var psDoneOnce sync.Once

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			select {
			case <-psStarted:
			case <-time.After(2 * time.Second):
				http.Error(w, "warmth poll did not start", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/ps":
			psOnce.Do(func() { close(psStarted) })
			select {
			case <-r.Context().Done():
				psDoneOnce.Do(func() { close(psDone) })
			case <-releasePS:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		close(releasePS)
		mock.Close()
	}()

	cfgPath := writeTestConfig(t, t.TempDir(), mock.URL)
	blockingParent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blockingParent, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", blockingParent, err)
	}

	_, err := NewServer(context.Background(),
		WithConfig(cfgPath),
		WithRAGDisabled(),
		WithTranscriptStore(filepath.Join(blockingParent, "transcripts.db")),
	)
	if err == nil {
		t.Fatal("NewServer() error = nil, want transcript startup failure")
	}
	if !strings.Contains(err.Error(), "create transcript directory") {
		t.Fatalf("NewServer() error = %q, want transcript directory failure", err)
	}

	select {
	case <-psDone:
	case <-time.After(2 * time.Second):
		t.Fatal("warmth poll was not cancelled after startup failure")
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

	s.rebuildDerivedClients(context.Background())
	if s.Indexer() != nil {
		t.Fatal("Indexer() != nil without a resolved embedding model")
	}
	if s.Retriever() != nil {
		t.Fatal("Retriever() != nil without a resolved embedding model")
	}

	s.resolved["embedding"] = config.ResolvedModel{Name: "qwen3-embedding:8b", Role: "embedding"}
	s.rebuildDerivedClients(context.Background())
	if s.Indexer() == nil {
		t.Fatal("Indexer() = nil, want non-nil after embedding resolution")
	}
	if s.Retriever() == nil {
		t.Fatal("Retriever() = nil, want non-nil after embedding resolution")
	}
}

func TestRebuildDerivedClientsDoesNotRepublishAfterClose(t *testing.T) {
	tagsStarted := make(chan struct{}, 1)
	releaseTags := make(chan struct{})

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			tagsStarted <- struct{}{}
			<-releaseTags
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"template":"{{ .Prompt }}{{ .Suffix }}","capabilities":["completion","insert"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()

	s := &Server{
		client: ollama.NewClient(ollama.WithBaseURL(mock.URL)),
		cfg: &config.Config{
			Providers: map[string]config.ProviderConfig{
				"ollama": {BaseURL: mock.URL},
			},
			Models: map[string]config.ModelConfig{
				"completion": {Name: "qwen3:8b", Provider: "ollama", Type: "dense"},
				"embedding":  {Name: "qwen3-embedding:8b", Provider: "ollama", Type: "embedding"},
			},
			Defaults: map[string]string{
				"completion": "completion",
				"embedding":  "embedding",
			},
		},
		store: stubVectorStore{},
		resolved: map[string]config.ResolvedModel{
			"completion": {Name: "qwen3:8b", Role: "completion"},
			"embedding":  {Name: "qwen3-embedding:8b", Role: "embedding"},
		},
	}

	done := make(chan struct{})
	go func() {
		s.rebuildDerivedClients(context.Background())
		close(done)
	}()

	<-tagsStarted
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	close(releaseTags)
	<-done

	if s.Completer() != nil {
		t.Fatal("Completer() != nil after Close won the race")
	}
	if s.Indexer() != nil {
		t.Fatal("Indexer() != nil after Close won the race")
	}
	if s.Retriever() != nil {
		t.Fatal("Retriever() != nil after Close won the race")
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

func writeProviderWiringConfig(t *testing.T, dir, ollamaURL, openAIURL string) string {
	t.Helper()

	path := filepath.Join(dir, "models.json")
	data := `{
  "providers": {
    "ollama": {
      "base_url": "` + ollamaURL + `",
      "timeout": "17s",
      "api_format": "ollama"
    },
    "vllm-local": {
      "base_url": "` + openAIURL + `",
      "timeout": "17s",
      "api_format": "openai-compat",
      "api_key": "test-token"
    }
  },
  "models": {
    "general": {"name": "local-chat", "provider": "vllm-local", "type": "dense"},
    "carved": {"name": "local-carved", "provider": "vllm-local", "type": "dense", "capabilities": ["chat", "stream"]},
    "completion": {"name": "local-fim", "provider": "vllm-local", "type": "dense", "capabilities": ["generate", "stream", "insert"]},
    "embedding": {"name": "qwen3:8b", "provider": "ollama", "type": "embedding"}
  },
  "defaults": {
    "chat": "general",
    "completion": "completion",
    "embedding": "embedding"
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	return path
}

func writeOpenAICompatOnlyConfig(t *testing.T, dir, openAIURL string) string {
	t.Helper()

	path := filepath.Join(dir, "models.json")
	data := `{
  "providers": {
    "vllm-local": {
      "base_url": "` + openAIURL + `",
      "timeout": "17s",
      "api_format": "openai-compat"
    }
  },
  "models": {
    "completion": {"name": "local-fim", "provider": "vllm-local", "type": "dense", "capabilities": ["generate", "stream", "insert"]}
  },
  "defaults": {
    "completion": "completion"
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

func TestServer_Close_ClosesRouter(t *testing.T) {
	s, err := NewServer(context.Background(), WithRAGDisabled())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.router == nil {
		// In environments where ensureModelRegistry can't run (no Ollama
		// client), the Router may be nil — skip rather than fail.
		t.Skip("router not constructed; skip")
	}
	router := s.router
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Calling a Router method after Close must return ErrRouterClosed.
	_, err = router.Route(context.Background(), provider.RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: provider.CapChat,
		Model:        "ollama/qwen3:8b",
	})
	if !errors.Is(err, provider.ErrRouterClosed) {
		t.Errorf("post-Close Route err = %v, want ErrRouterClosed", err)
	}
}
