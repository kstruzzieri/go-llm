package providerbootstrap

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
	_ "modernc.org/sqlite"
)

func TestBundleCloseNilSafe(t *testing.T) {
	var b *Bundle
	if err := b.Close(); err != nil {
		t.Fatalf("nil Bundle.Close() = %v, want nil", err)
	}
	if err := (&Bundle{}).Close(); err != nil {
		t.Fatalf("empty Bundle.Close() = %v, want nil", err)
	}
}

// newTestFingerprintStore creates an in-memory SQLite-backed fingerprint store
// for testing. The concrete *SQLiteStore satisfies both fingerprint.Store and
// fingerprint.CapProbeStore. It registers a t.Cleanup to close the underlying DB.
func newTestFingerprintStore(t *testing.T) *fingerprint.SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test fingerprint db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := fingerprint.NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("fingerprint.NewStore: %v", err)
	}
	return store
}

func TestNew_NilConfigBuildsRouter(t *testing.T) {
	b, err := New(context.Background(), Options{})
	if err != nil {
		t.Fatalf("New(nil cfg) error: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.Router == nil || b.Models == nil || b.Providers == nil {
		t.Fatalf("New returned incomplete bundle: %+v", b)
	}
	if b.Config == nil {
		t.Fatalf("expected synthetic Config to be set on Bundle")
	}
}

func TestNew_ConfigContextWindowPopulatesMergedProfile(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"byo-model"}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat", BaseURL: srv.URL}},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "byo-model", Type: "dense", ContextWindow: 32_768},
		},
	}
	bundle, err := New(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	profile, err := bundle.Models.Lookup(ctx, provider.ModelKey{Provider: "lc", Model: "byo-model"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if profile.ContextWindow != 32_768 {
		t.Fatalf("ContextWindow = %d, want configured 32768", profile.ContextWindow)
	}
}

func TestNew_FingerprintProfileStoreReadsWithoutProfiling(t *testing.T) {
	ctx := context.Background()
	var chatCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"byo-model"}]}`))
		case "/v1/chat/completions":
			chatCalls.Add(1)
			http.Error(w, "profiling must remain disabled", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := newTestFingerprintStore(t)
	if err := store.Save(ctx, fingerprint.Profile{
		BackendID:        "lc",
		ModelName:        "byo-model",
		ModelDigest:      "config-caps:chat,generate,stream",
		ProfileVersion:   fingerprint.CurrentProfileVersion,
		EffectiveContext: 24_576,
		TestedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("save fingerprint profile: %v", err)
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat", BaseURL: srv.URL}},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "byo-model", Type: "dense"},
		},
	}
	bundle, err := New(ctx, Options{Config: cfg, FingerprintProfileStore: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	profile, err := bundle.Models.Lookup(ctx, provider.ModelKey{Provider: "lc", Model: "byo-model"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if profile.ContextWindow != 24_576 {
		t.Fatalf("ContextWindow = %d, want persisted 24576", profile.ContextWindow)
	}
	if got := chatCalls.Load(); got != 0 {
		t.Fatalf("profiling chat calls = %d, want 0", got)
	}
}

// TestNew_FingerprintProfileStoreDoesNotProfileStaleProfile is the
// discriminating twin of the fresh-profile test above: a FRESH profile
// short-circuits EnsureProfile in ANY mode, so only a STALE one can prove the
// bundle wired read-only enrichment rather than full profiling. Full
// profiling would re-probe here (chat calls > 0); read-only must drop the
// stale profile silently and never probe.
func TestNew_FingerprintProfileStoreDoesNotProfileStaleProfile(t *testing.T) {
	ctx := context.Background()
	var chatCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"byo-model"}]}`))
		case "/v1/chat/completions":
			chatCalls.Add(1)
			http.Error(w, "profiling must remain disabled", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	store := newTestFingerprintStore(t)
	if err := store.Save(ctx, fingerprint.Profile{
		BackendID:        "lc",
		ModelName:        "byo-model",
		ModelDigest:      "config-caps:chat", // stale: current identity is chat,generate,stream
		ProfileVersion:   fingerprint.CurrentProfileVersion,
		EffectiveContext: 65_536,
		TestedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("save fingerprint profile: %v", err)
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat", BaseURL: srv.URL}},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "byo-model", Type: "dense"},
		},
	}
	bundle, err := New(ctx, Options{Config: cfg, FingerprintProfileStore: store})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	profile, err := bundle.Models.Lookup(ctx, provider.ModelKey{Provider: "lc", Model: "byo-model"})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if profile.ContextWindow != 0 {
		t.Fatalf("ContextWindow = %d, want 0 (stale fingerprint context must not leak)", profile.ContextWindow)
	}
	if got := chatCalls.Load(); got != 0 {
		t.Fatalf("profiling chat calls = %d, want 0 (read-only store must never probe)", got)
	}
}

func TestNew_ProberFactoryInstalledWithFingerprintStore(t *testing.T) {
	// With a fingerprint store, New must wire the prober factory (parity with mcp).
	// Assert indirectly: New succeeds and a registry was built. A deeper assertion
	// belongs to the MCP parity suite (Task 6).
	b, err := New(context.Background(), Options{FingerprintStore: newTestFingerprintStore(t)})
	if err != nil {
		t.Fatalf("New with fp store error: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.Models == nil {
		t.Fatalf("expected model registry")
	}
}

func TestNew_FingerprintStoreUsesProviderSpecificOllamaClient(t *testing.T) {
	ctx := context.Background()
	var providerASawProviderBModel atomic.Bool
	providerA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"a-model"}]}`))
		case "/api/show":
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad show request", http.StatusBadRequest)
				return
			}
			if body.Name == "b-model" {
				providerASawProviderBModel.Store(true)
				http.Error(w, "wrong provider", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer providerA.Close()

	var providerBShowCalls atomic.Int32
	providerB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"b-model"}]}`))
		case "/api/show":
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad show request", http.StatusBadRequest)
				return
			}
			if body.Name != "b-model" {
				http.Error(w, "unexpected model", http.StatusNotFound)
				return
			}
			providerBShowCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer providerB.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"a": {APIFormat: "ollama", BaseURL: providerA.URL},
			"b": {APIFormat: "ollama", BaseURL: providerB.URL},
		},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "b", Name: "b-model", Type: "dense"},
		},
	}
	bundle, err := New(ctx, Options{Config: cfg, FingerprintStore: newTestFingerprintStore(t)})
	if err != nil {
		t.Fatalf("New(multi-ollama cfg) error: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	profile, err := bundle.Models.Lookup(ctx, provider.ModelKey{Provider: "b", Model: "b-model"})
	if err != nil {
		t.Fatalf("Lookup(b/b-model) error = %v", err)
	}
	if !profile.Caps.Has(provider.CapChat) {
		t.Fatalf("profile caps = %v, want chat capability from provider B", profile.Caps)
	}
	if providerASawProviderBModel.Load() {
		t.Fatal("provider B fingerprint probe used provider A's Ollama client")
	}
	if got := providerBShowCalls.Load(); got < 3 {
		t.Fatalf("provider B /api/show calls = %d, want startup, runtime lookup, and fingerprint probe", got)
	}
}

func TestNew_CapabilityProbeStoreInstallsCapProberNotFullFactory(t *testing.T) {
	// Golem's capability-only mode: Options{CapabilityProbeStore: store} with
	// FingerprintStore nil must wire the on-demand capability prober WITHOUT
	// full fingerprint profiling. Behaviorally: Lookup performs no probe
	// chat-completions calls; ResolveToolCall does.
	ctx := context.Background()
	var chatCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mystery-model-x"}]}`))
		case "/v1/chat/completions":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}],"usage":{"completion_tokens":4}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {APIFormat: "openai-compat", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			// No explicit Capabilities: derived dense caps become a floor
			// (chat/generate/stream) without tool_call, so resolution must probe.
			"chat": {Provider: "lc", Name: "mystery-model-x", Type: "dense"},
		},
	}
	b, err := New(ctx, Options{Config: cfg, CapabilityProbeStore: newTestFingerprintStore(t)})
	if err != nil {
		t.Fatalf("New(capability-probe store) error: %v", err)
	}
	defer func() { _ = b.Close() }()

	key := provider.ModelKey{Provider: "lc", Model: "mystery-model-x"}
	profile, err := b.Models.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if profile.Caps.Has(provider.CapToolCall) {
		t.Fatalf("precondition: undeclared model must not already claim tool_call, caps = %v", profile.Caps)
	}
	if got := chatCalls.Load(); got != 0 {
		t.Fatalf("Lookup made %d chat-completions probe call(s), want 0 (Lookup must never probe in capability-only mode)", got)
	}

	state, err := b.Models.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("ResolveToolCall state = %q, want %q", state, fingerprint.CapProbeYes)
	}
	if got := chatCalls.Load(); got < 1 {
		t.Fatalf("ResolveToolCall made %d probe call(s), want >= 1", got)
	}
}

func TestNew_FingerprintStoreSatisfiesCapProbeStoreFallback(t *testing.T) {
	// MCP path: with a FingerprintStore (whose SQLiteStore also satisfies
	// fingerprint.CapProbeStore) and NO explicit CapabilityProbeStore, the
	// interface-assert fallback must still enable ResolveToolCall. This proves
	// the fallback branch wires the resolver, not just compiles.
	//
	// Unlike the capability-only test, a FingerprintStore is wired here, so
	// Lookup MAY trigger full profiling — do not assert zero probe calls.
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mystery-model-x"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}],"usage":{"completion_tokens":4}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {APIFormat: "openai-compat", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "mystery-model-x", Type: "dense"},
		},
	}
	b, err := New(ctx, Options{Config: cfg, FingerprintStore: newTestFingerprintStore(t)})
	if err != nil {
		t.Fatalf("New(fingerprint store, no explicit cap store) error: %v", err)
	}
	defer func() { _ = b.Close() }()

	key := provider.ModelKey{Provider: "lc", Model: "mystery-model-x"}
	state, err := b.Models.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("ResolveToolCall state = %q, want %q (fallback resolver must be enabled)", state, fingerprint.CapProbeYes)
	}
}

func TestNew_OpenAICompatConfigInstallsOverridesAndBuilds(t *testing.T) {
	// Exercises the openai-compat buildProvider branch (APIKey + Timeout) and the
	// installCapabilityOverrides install path end-to-end through New. The provider
	// URL is unreachable, so RefreshModels fails into a warning, but registration
	// succeeds (registered > 0) and the bundle is built coherently.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {
				APIFormat: "openai-compat",
				BaseURL:   "http://127.0.0.1:1",
				APIKey:    "test-key",
				Timeout:   config.Duration{Duration: 2 * time.Second},
			},
		},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "qwen", Capabilities: []string{"chat", "tool_call"}},
		},
	}
	b, err := New(context.Background(), Options{Config: cfg})
	if err != nil {
		t.Fatalf("New(openai-compat cfg) error: %v", err)
	}
	defer func() { _ = b.Close() }()
	if b.Router == nil || b.Models == nil || b.Providers == nil {
		t.Fatalf("New returned incomplete bundle: %+v", b)
	}
	// #477 D7: Bundle.Config is the materialized EFFECTIVE config — always a
	// copy, so overrides and normalization can never mutate the caller's
	// value. Same content (modulo defaulted api_format), different pointer.
	if b.Config == cfg {
		t.Fatalf("Bundle.Config must be the materialized copy, not the caller's config")
	}
	if got := b.Config.Providers["lc"].BaseURL; got != cfg.Providers["lc"].BaseURL {
		t.Fatalf("effective base URL diverged with no override: %q", got)
	}
}

func TestNew_ModelSamplingDefaultsReachSelectedProvider(t *testing.T) {
	ctx := context.Background()
	var captured struct {
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		TopK        *int     `json:"top_k"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:8b"}]}`))
		case "/v1/chat/completions":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode chat request: %v", err)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {APIFormat: "openai-compat", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			"chat": {
				Provider: "lc", Name: "qwen3:8b", Type: "dense",
				Options: &config.SamplingOptions{
					Temperature: provider.Ptr(0.7),
					TopP:        provider.Ptr(0.9),
					TopK:        provider.Ptr(40),
				},
			},
		},
	}
	b, err := New(ctx, Options{
		Config: cfg,
		RouterOptions: []provider.RouterOption{provider.WithModelDefaults(map[provider.ModelKey]provider.SamplingDefaults{
			{Provider: "lc", Model: "qwen3:8b"}: {TopK: provider.Ptr(20)},
		})},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()

	_, err = b.Router.Chat(ctx, provider.ChatRequest{
		Model:   "lc/qwen3:8b",
		Options: provider.ModelOptions{Temperature: provider.Ptr(0.0)},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.Temperature == nil || *captured.Temperature != 0 {
		t.Errorf("Temperature = %v, want explicit request zero", captured.Temperature)
	}
	if captured.TopP == nil || *captured.TopP != 0.9 {
		t.Errorf("TopP = %v, want model default 0.9", captured.TopP)
	}
	if captured.TopK == nil || *captured.TopK != 20 {
		t.Errorf("TopK = %v, want caller override 20", captured.TopK)
	}
}

func TestNew_ModelSamplingDefaultsPreserveZeroForNativeOllama(t *testing.T) {
	ctx := context.Background()
	var captured struct {
		Options struct {
			Temperature *float64 `json:"temperature"`
			TopP        *float64 `json:"top_p"`
			TopK        *int     `json:"top_k"`
		} `json:"options"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion"]}`))
		case "/api/generate":
			if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
				t.Errorf("decode generate request: %v", err)
			}
			_, _ = w.Write([]byte(`{"model":"qwen3:8b","response":"ok","done":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"ollama": {APIFormat: "ollama", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			"chat": {
				Provider: "ollama", Name: "qwen3:8b", Type: "dense",
				Options: &config.SamplingOptions{
					Temperature: provider.Ptr(0.0),
					TopP:        provider.Ptr(0.9),
					TopK:        provider.Ptr(0),
				},
			},
		},
	}
	b, err := New(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()

	if _, err := b.Router.Generate(ctx, provider.GenerateRequest{
		Model: "ollama/qwen3:8b", Prompt: "hello",
	}); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if captured.Options.Temperature == nil || *captured.Options.Temperature != 0 {
		t.Errorf("Temperature = %v, want configured zero", captured.Options.Temperature)
	}
	if captured.Options.TopP == nil || *captured.Options.TopP != 0.9 {
		t.Errorf("TopP = %v, want configured 0.9", captured.Options.TopP)
	}
	if captured.Options.TopK == nil || *captured.Options.TopK != 0 {
		t.Errorf("TopK = %v, want configured zero", captured.Options.TopK)
	}
}

func TestNew_OpenAICompatURLOverride(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
	}}
	b, err := New(context.Background(), Options{
		Config:                          cfg,
		OpenAICompatURLOverrideProvider: "llamacpp",
		OpenAICompatURLOverride:         "http://127.0.0.1:8083",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()
	if got := b.Config.Providers["llamacpp"].BaseURL; got != "http://127.0.0.1:8083" {
		t.Fatalf("Bundle.Config BaseURL = %q, want override (diagnostics must match the live client)", got)
	}
}

func TestBootstrapInstallsThinkOverride(t *testing.T) {
	// End-to-end: think_mode/think_tags declared in config must surface on the
	// registry profile after New; models without declarations stay untouched.
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"custom-a"},{"id":"custom-b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {APIFormat: "openai-compat", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			"chat": {Provider: "lc", Name: "custom-a", Type: "dense",
				ThinkMode: "always",
				ThinkTags: &config.ThinkTagsConfig{Open: "<r>", Close: "</r>"}},
			"plain": {Provider: "lc", Name: "custom-b", Type: "dense"},
		},
	}
	b, err := New(ctx, Options{Config: cfg})
	if err != nil {
		t.Fatalf("New(think override cfg) error: %v", err)
	}
	defer func() { _ = b.Close() }()

	overridden, err := b.Models.Lookup(ctx, provider.ModelKey{Provider: "lc", Model: "custom-a"})
	if err != nil {
		t.Fatalf("Lookup(custom-a): %v", err)
	}
	if overridden.ThinkMode != provider.ThinkAlways {
		t.Fatalf("ThinkMode = %v, want ThinkAlways", overridden.ThinkMode)
	}
	if overridden.ThinkTags == nil || *overridden.ThinkTags != (provider.ThinkTags{Open: "<r>", Close: "</r>"}) {
		t.Fatalf("ThinkTags = %v, want <r>/</r>", overridden.ThinkTags)
	}

	untouched, err := b.Models.Lookup(ctx, provider.ModelKey{Provider: "lc", Model: "custom-b"})
	if err != nil {
		t.Fatalf("Lookup(custom-b): %v", err)
	}
	// inferProfile defaults unknown models to ThinkAuto — that is the
	// no-config baseline the override must leave alone.
	if untouched.ThinkMode != provider.ThinkAuto {
		t.Fatalf("untouched ThinkMode = %v, want no-config baseline ThinkAuto", untouched.ThinkMode)
	}
	if untouched.ThinkTags != nil {
		t.Fatalf("untouched ThinkTags = %v, want nil", untouched.ThinkTags)
	}
}

func TestNew_BestEffortRefreshRecordsWarnings(t *testing.T) {
	// Point ollama at an unreachable URL so RefreshModels fails; New must still
	// succeed and record a Warning rather than error.
	b, err := New(context.Background(), Options{OllamaURLOverride: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("New should tolerate refresh failure, got: %v", err)
	}
	defer func() { _ = b.Close() }()
	if len(b.Warnings) == 0 {
		t.Fatalf("expected a best-effort refresh warning")
	}
}
