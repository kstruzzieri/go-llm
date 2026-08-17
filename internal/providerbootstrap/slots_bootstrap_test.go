package providerbootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
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
