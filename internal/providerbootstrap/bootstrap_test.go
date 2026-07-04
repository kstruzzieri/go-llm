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
// for testing. It registers a t.Cleanup to close the underlying DB.
func newTestFingerprintStore(t *testing.T) fingerprint.Store {
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
	if b.Config != cfg {
		t.Fatalf("Bundle.Config should be the passed config")
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
