package providerbootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// slotBackends is pure: no network. Remote/tunnel/docker cases are the
// operator's call via the flag — host is irrelevant.
func TestSlotBackendsSelector(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc":       {BaseURL: "http://127.0.0.1:8090", APIFormat: "openai-compat", APIKey: "sk-x", SlotDiscovery: true},
		"tunnel":   {BaseURL: "http://192.0.2.10:8080", APIFormat: "openai-compat", SlotDiscovery: true},
		"lmstudio": {BaseURL: "http://127.0.0.1:1234", APIFormat: "openai-compat"}, // no flag: ungoverned
		"ollama":   {BaseURL: "http://127.0.0.1:11434"},
	}}
	got, err := slotBackends(cfg)
	if err != nil {
		t.Fatalf("slotBackends: %v", err)
	}
	want := map[string]provider.SlotBackend{
		"lc":     {BaseURL: "http://127.0.0.1:8090", APIKey: "sk-x"},
		"tunnel": {BaseURL: "http://192.0.2.10:8080"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slotBackends = %#v, want %#v", got, want)
	}
}

// slot_discovery on a non-openai-compat provider is a loud config error
// (user config fails loud — ThinkMode precedent).
func TestSlotBackendsRejectsNonOpenAICompat(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434", SlotDiscovery: true},
	}}
	if _, err := slotBackends(cfg); err == nil {
		t.Fatal("want error for slot_discovery on ollama-format provider")
	}
}

// Full New: flagged provider is governed, unflagged is not. The fake
// backend is loopback purely so the test has a live /v1/models to refresh
// against — the flag, not the host, decides.
func TestNewWiresSlotSourceFromConfigFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc":    {BaseURL: srv.URL, APIFormat: "openai-compat", SlotDiscovery: true},
		"plain": {BaseURL: srv.URL, APIFormat: "openai-compat"},
	}}
	b, err := New(context.Background(), Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()

	if n, ok := b.Router.SlotCapacity(provider.ModelKey{Provider: "lc", Model: "m1"}); !ok || n != 1 {
		t.Fatalf("flagged provider SlotCapacity = (%d, %v), want governed (1, true)", n, ok)
	}
	if _, ok := b.Router.SlotCapacity(provider.ModelKey{Provider: "plain", Model: "m1"}); ok {
		t.Fatal("unflagged provider must be ungoverned")
	}
}

// Config validation must run BEFORE provider registration/RefreshModels:
// an invalid config errors without a single network request (and never
// point this test at a real port like 11434 — a live Ollama runs there on
// the dev machine).
func TestNewRejectsSlotDiscoveryOnOllamaBeforeIO(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: srv.URL, SlotDiscovery: true},
	}}
	if _, err := New(context.Background(), Options{Config: cfg}); err == nil {
		t.Fatal("want New error for slot_discovery on ollama provider")
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 0 {
		t.Fatalf("config validation performed %d network requests, want 0", requests)
	}
}

// The flag must survive the real file path, not just struct literals: a
// broken "slot_discovery" JSON tag — or config.Load defaulting/rewriting
// fields in a way the selector does not expect — is invisible to tests
// that construct ProviderConfig directly.
func TestSlotDiscoveryRoundTripsThroughConfigLoad(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(`{
  "providers": {
    "lc":     { "base_url": "http://127.0.0.1:8090", "api_format": "openai-compat", "slot_discovery": true },
    "ollama": { "base_url": "http://127.0.0.1:11434" }
  },
  "models": {
    "default": { "name": "qwen3:8b", "provider": "lc", "type": "dense" }
  },
  "defaults": { "chat": "default" }
}`), 0o600); err != nil {
		t.Fatalf("write valid config: %v", err)
	}
	cfg, err := config.Load(valid)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Providers["lc"].SlotDiscovery {
		t.Fatal("slot_discovery: true did not round-trip through config.Load")
	}
	got, err := slotBackends(cfg)
	if err != nil {
		t.Fatalf("slotBackends on loaded config: %v", err)
	}
	if len(got) != 1 || got["lc"].BaseURL != "http://127.0.0.1:8090" {
		t.Fatalf("slotBackends on loaded config = %#v, want only lc", got)
	}

	// The loud-error path for a file-backed config now fires at Load
	// itself: config.validate enforces the slot policy statically (410
	// spec s1), classified as CodeSlotPolicyInvalid. Bootstrap retains
	// its own check as defense in depth for programmatic Configs that
	// bypass Load — pinned below.
	invalid := filepath.Join(dir, "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{
  "providers": {
    "ollama": { "base_url": "http://127.0.0.1:11434", "slot_discovery": true }
  },
  "models": {
    "default": { "name": "qwen3:8b", "provider": "ollama", "type": "dense" }
  },
  "defaults": { "chat": "default" }
}`), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	_, err = config.Load(invalid)
	if err == nil {
		t.Fatal("want Load error for slot_discovery on ollama-format provider")
	}
	if d, ok := config.DiagnosticOf(err); !ok || d.Code != config.CodeSlotPolicyInvalid {
		t.Fatalf("Load diagnostic = %+v ok=%v, want code %s", d, ok, config.CodeSlotPolicyInvalid)
	}

	// The equivalent invalid Config built programmatically bypasses Load's
	// validation entirely; New must still reject it loudly (the retained
	// defense-in-depth path). No request is made: validation runs before IO.
	badCfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:1", SlotDiscovery: true},
	}}
	if _, err := New(context.Background(), Options{Config: badCfg}); err == nil {
		t.Fatal("want New error for programmatic config with slot_discovery on ollama provider")
	}
}

// bootstrapFakeSlotSource implements provider.SlotSource for the
// caller-override test.
type bootstrapFakeSlotSource struct {
	mu       sync.Mutex
	capacity int
	closed   int
}

func (f *bootstrapFakeSlotSource) Capacity(provider.ModelKey) (int, bool) { return f.capacity, true }
func (f *bootstrapFakeSlotSource) RecordUse(provider.ModelKey)            {}
func (f *bootstrapFakeSlotSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

// Caller-supplied WithSlotSource must override the config-derived source —
// the routerOpts ordering rule (caller options apply last). The
// distinctive capacity 9 can only come from the custom source (the
// config-derived one would report 1 for an unprobed key).
func TestCallerSlotSourceOverridesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"lc": {BaseURL: srv.URL, APIFormat: "openai-compat", SlotDiscovery: true},
	}}
	custom := &bootstrapFakeSlotSource{capacity: 9}
	b, err := New(context.Background(), Options{
		Config:        cfg,
		RouterOptions: []provider.RouterOption{provider.WithSlotSource(custom)},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n, ok := b.Router.SlotCapacity(provider.ModelKey{Provider: "lc", Model: "m1"}); !ok || n != 9 {
		_ = b.Close()
		t.Fatalf("SlotCapacity = (%d, %v), want custom (9, true)", n, ok)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	custom.mu.Lock()
	defer custom.mu.Unlock()
	if custom.closed != 1 {
		t.Fatalf("custom source Close calls = %d, want 1", custom.closed)
	}
}

// ---------------------------------------------------------------------------
// slots override (#400 config override)
// ---------------------------------------------------------------------------

func TestBuildSlotOverridesRequiresGovernedProvider(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: "http://127.0.0.1:8090", APIFormat: "openai-compat"}, // no slot_discovery
		},
		Models: map[string]config.ModelConfig{
			"default": {Name: "m1", Provider: "lc", Type: "dense", Slots: 4},
		},
	}
	if _, err := buildSlotOverrides(cfg, map[string]provider.SlotBackend{}); err == nil {
		t.Fatal("want error: slots override without slot_discovery on the provider")
	}
}

func TestBuildSlotOverridesRejectsNegativeProgrammaticConfig(t *testing.T) {
	// Programmatic Config never passes through config.Load, so bootstrap
	// must recheck (context_window precedent).
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: "http://127.0.0.1:8090", APIFormat: "openai-compat", SlotDiscovery: true},
		},
		Models: map[string]config.ModelConfig{
			"default": {Name: "m1", Provider: "lc", Type: "dense", Slots: -1},
		},
	}
	slotBEs, err := slotBackends(cfg)
	if err != nil {
		t.Fatalf("slotBackends: %v", err)
	}
	if _, err := buildSlotOverrides(cfg, slotBEs); err == nil {
		t.Fatal("want error: negative slots must be rejected at bootstrap")
	}
}

func TestBuildSlotOverridesConflictNamesBothRoles(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: "http://127.0.0.1:8090", APIFormat: "openai-compat", SlotDiscovery: true},
		},
		Models: map[string]config.ModelConfig{
			"chat":  {Name: "m1", Provider: "lc", Type: "dense", Slots: 2},
			"embed": {Name: "m1", Provider: "lc", Type: "dense", Slots: 8},
		},
	}
	slotBEs, err := slotBackends(cfg)
	if err != nil {
		t.Fatalf("slotBackends: %v", err)
	}
	_, err = buildSlotOverrides(cfg, slotBEs)
	if err == nil {
		t.Fatal("want conflict error for differing slots on one ModelKey")
	}
	if got := err.Error(); !strings.Contains(got, "chat") || !strings.Contains(got, "embed") {
		t.Fatalf("conflict error %q must name both roles", got)
	}
}

func TestNewAppliesSlotOverrideWithoutProbing(t *testing.T) {
	var mu sync.Mutex
	propsRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		if r.URL.Path == "/props" {
			mu.Lock()
			propsRequests++
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: srv.URL, APIFormat: "openai-compat", SlotDiscovery: true},
		},
		Models: map[string]config.ModelConfig{
			"default": {Name: "m1", Provider: "lc", Type: "dense", Slots: 3},
		},
	}
	b, err := New(context.Background(), Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()
	key := provider.ModelKey{Provider: "lc", Model: "m1"}
	if n, ok := b.Router.SlotCapacity(key); !ok || n != 3 {
		t.Fatalf("SlotCapacity = (%d, %v), want pinned (3, true)", n, ok)
	}
	// RecordUse on a pinned key must never probe.
	b.Router.RecordSlotUse(key)
	mu.Lock()
	defer mu.Unlock()
	if propsRequests != 0 {
		t.Fatalf("/props requests = %d for a pinned key, want 0", propsRequests)
	}
}

func TestNewRejectsSlotOverrideWhenNoProviderHasDiscovery(t *testing.T) {
	// slots overrides with NO slot-discovery provider at all is the same
	// loud config error, not a silent no-op.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc": {BaseURL: srv.URL, APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"default": {Name: "m1", Provider: "lc", Type: "dense", Slots: 4},
		},
	}
	if _, err := New(context.Background(), Options{Config: cfg}); err == nil {
		t.Fatal("want New error for slots override with zero slot-discovery providers")
	}
}
