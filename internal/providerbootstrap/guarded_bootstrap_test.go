package providerbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// countingOpenAIServer fakes an openai-compat backend, counting requests per
// endpoint. The counters below the guard are the observation points for
// every zero-request claim (I1, I7, I14).
type countingOpenAIServer struct {
	srv        *httptest.Server
	models     atomic.Int64
	chats      atomic.Int64
	props      atomic.Int64
	totalSlots int
}

func newCountingOpenAIServer(t *testing.T) *countingOpenAIServer {
	t.Helper()
	s := &countingOpenAIServer{totalSlots: 4}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			s.models.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/v1/chat/completions":
			s.chats.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
			})
		case "/props":
			s.props.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_slots": 4}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// gateFromPlan materializes, plans, and installs a gate over the plan's
// edges with every destination granted — the composition Task 8's entry
// points will perform.
func gateFromPlan(t *testing.T, cfg *config.Config, routes []PlannedRoute, opts planOpts) (*provider.DestinationGate, *NetworkPlan) {
	t.Helper()
	eff := mustEff(t, cfg)
	plan, err := buildNetworkPlan(eff, routes, opts)
	if err != nil {
		t.Fatal(err)
	}
	m, err := provider.NewDestinationManifest(plan.Edges...)
	if err != nil {
		t.Fatal(err)
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.AllowAllDestinations(), m); err != nil {
		t.Fatal(err)
	}
	return gate, plan
}

// twoProviderConfig: provider "a" carries the chat role; provider "b" is
// configured but reachable from nothing.
func twoProviderConfig(a, b string) *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{
			"a": {APIFormat: "openai-compat", BaseURL: a},
			"b": {APIFormat: "openai-compat", BaseURL: b},
		},
		Models: map[string]config.ModelConfig{
			"chatrole": {Provider: "a", Name: "m1", Type: "dense", ContextWindow: 32768},
		},
		Defaults: map[string]string{"chat": "chatrole"},
	}
}

// I7/M7 at the wire: only ACTIVE providers are refreshed; the configured
// bystander receives zero requests of any kind.
func TestNewGatedRefreshesOnlyActiveProviders(t *testing.T) {
	a := newCountingOpenAIServer(t)
	b := newCountingOpenAIServer(t)
	cfg := twoProviderConfig(a.srv.URL, b.srv.URL)
	pcB := cfg.Providers["b"]
	pcB.SlotDiscovery = true
	cfg.Providers["b"] = pcB

	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, cfg, []PlannedRoute{route}, planOpts{})

	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()

	if got := a.models.Load(); got != 1 {
		t.Errorf("active provider refreshed %d times, want 1", got)
	}
	if got := b.models.Load() + b.chats.Load() + b.props.Load(); got != 0 {
		t.Errorf("inactive provider received %d requests, want 0", got)
	}
	// The inactive slot-discovery backend leaves governance entirely:
	// (0, false), never the governed fail-safe (1, true) — and therefore
	// nothing to probe.
	if n, ok := bundle.Router.SlotCapacity(provider.ModelKey{Provider: "b", Model: "m1"}); ok || n != 0 {
		t.Errorf("inactive slot-discovery provider governed: (%d, %v), want (0, false)", n, ok)
	}
}

// I1: a deny-all gate is fatal at the refresh bind, with zero outbound
// requests — denial is never downgraded to a Bundle warning.
func TestNewGatedDenialIsFatalWithZeroRequests(t *testing.T) {
	a := newCountingOpenAIServer(t)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"a": {APIFormat: "openai-compat", BaseURL: a.srv.URL},
		},
		Models:   map[string]config.ModelConfig{"chatrole": {Provider: "a", Name: "m1", Type: "dense", ContextWindow: 32768}},
		Defaults: map[string]string{"chat": "chatrole"},
	}

	_, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: provider.NewDestinationGate(), // deny-all: nothing installed
		ActiveProviders: []string{"a"},
	})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("New under deny-all = %v, want ErrDestinationDenied", err)
	}
	if got := a.models.Load(); got != 0 {
		t.Errorf("denied bootstrap sent %d requests, want 0", got)
	}
}

// The full stack through the Router: a routed chat on an admitted edge
// reaches the backend THROUGH the guarded client (arrival proves the
// capability chain, since the guard denies bare requests), and a use case
// with no edge produces a denial and zero chat requests.
func TestNewGatedRoutedChatEndToEnd(t *testing.T) {
	a := newCountingOpenAIServer(t)
	b := newCountingOpenAIServer(t)
	cfg := twoProviderConfig(a.srv.URL, b.srv.URL)

	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, cfg, []PlannedRoute{route}, planOpts{})

	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()

	rp, err := bundle.Router.Route(t.Context(), provider.RoutingRequest{
		UseCase:        "chat",
		Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
		PreferredChain: []string{"a/m1"},
		StrictChain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.ExecuteChat(t.Context()); err != nil {
		t.Fatalf("admitted routed chat: %v", err)
	}
	if got := a.chats.Load(); got != 1 {
		t.Errorf("chat endpoint hit %d times, want 1", got)
	}

	// Same backend, unmanifested purpose: denied before any request.
	rp2, err := bundle.Router.Route(t.Context(), provider.RoutingRequest{
		UseCase:        "summarize",
		Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
		PreferredChain: []string{"a/m1"},
		StrictChain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rp2.ExecuteChat(t.Context())
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("unmanifested purpose = %v, want ErrDestinationDenied", err)
	}
	if got := a.chats.Load(); got != 1 {
		t.Errorf("denied purpose reached the chat endpoint (count %d, want still 1)", got)
	}
}

// T7 structural backstop: a provider call that BYPASSES every binding site —
// direct Resolve + Chat on a bare context — dies in the guarded client
// itself. This is what makes the boundary structural rather than an
// enumeration of call sites: a future caller that forgets to bind fails
// loudly instead of leaking.
func TestNewGatedDirectProviderCallDeniedWithoutCapability(t *testing.T) {
	a := newCountingOpenAIServer(t)
	b := newCountingOpenAIServer(t)
	cfg := twoProviderConfig(a.srv.URL, b.srv.URL)

	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, cfg, []PlannedRoute{route}, planOpts{})
	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()
	chatsBefore := a.chats.Load()

	p, err := bundle.Providers.Resolve(provider.ModelKey{Provider: "a", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), provider.ChatRequest{
		Model:    "m1",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("bare direct provider call = %v, want ErrDestinationDenied", err)
	}
	if got := a.chats.Load(); got != chatsBefore {
		t.Errorf("bare direct call reached the chat endpoint (%d -> %d)", chatsBefore, got)
	}
}

// Ollama-format providers get the same guard: admitted refresh succeeds,
// deny-all is fatal with zero requests.
func TestNewGatedOllamaProvider(t *testing.T) {
	// /api/tags and the per-model /api/show are both part of the ollama
	// provider's model-refresh listing and run under the bound capability;
	// anything else is a violation.
	var tags, shows, others atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			tags.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"m1"}]}`))
		case "/api/show":
			shows.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{}}`))
		default:
			others.Add(1)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"ollama": {APIFormat: "ollama", BaseURL: srv.URL},
		},
		Models:   map[string]config.ModelConfig{"chatrole": {Provider: "ollama", Name: "m1", Type: "dense", ContextWindow: 32768}},
		Defaults: map[string]string{"chat": "chatrole"},
	}
	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, cfg, []PlannedRoute{route}, planOpts{})

	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()
	if got := tags.Load(); got != 1 {
		t.Errorf("ollama refresh hit /api/tags %d times, want 1", got)
	}
	if got := shows.Load(); got != 1 {
		t.Errorf("ollama refresh enriched via /api/show %d times, want 1 (authorized refresh traffic)", got)
	}

	// Structural backstop for the ollama-format client too: a direct call
	// on a bare context dies in the guarded client, zero requests.
	p, err := bundle.Providers.Resolve(provider.ModelKey{Provider: "ollama", Model: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Chat(context.Background(), provider.ChatRequest{
		Model:    "m1",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("bare direct ollama call = %v, want ErrDestinationDenied", err)
	}
	if got := others.Load(); got != 0 {
		t.Errorf("bare direct ollama call reached the server (%d non-tags requests)", got)
	}

	tags.Store(0)
	_, err = New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: provider.NewDestinationGate(),
		ActiveProviders: []string{"ollama"},
	})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("deny-all ollama bootstrap = %v, want ErrDestinationDenied", err)
	}
	if got := tags.Load(); got != 0 {
		t.Errorf("denied ollama bootstrap sent %d requests, want 0", got)
	}
}

// I14/M14: the slot probe runs through the gate. With the slot-probe edge
// deliberately absent, the probe binder denies and /props receives ZERO
// requests — an independent unguarded slot client would dial it.
func TestNewGatedSlotProbeDeniedWithoutEdge(t *testing.T) {
	a := newCountingOpenAIServer(t)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"a": {APIFormat: "openai-compat", BaseURL: a.srv.URL, SlotDiscovery: true},
		},
		Models:   map[string]config.ModelConfig{"chatrole": {Provider: "a", Name: "m1", Type: "dense", ContextWindow: 32768}},
		Defaults: map[string]string{"chat": "chatrole"},
	}
	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	// Manifest WITHOUT the slot-probe edge: keep only chat + model-refresh.
	eff := mustEff(t, cfg)
	plan, err := buildNetworkPlan(eff, []PlannedRoute{route}, planOpts{})
	if err != nil {
		t.Fatal(err)
	}
	var kept []provider.DestinationEdge
	for _, e := range plan.Edges {
		if e.Purpose != provider.DestinationPurposeSlotProbe {
			kept = append(kept, e)
		}
	}
	m, err := provider.NewDestinationManifest(kept...)
	if err != nil {
		t.Fatal(err)
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.AllowAllDestinations(), m); err != nil {
		t.Fatal(err)
	}

	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()

	key := provider.ModelKey{Provider: "a", Model: "m1"}
	bundle.Router.RecordSlotUse(key)

	// The fail-safe capacity entry is the deterministic completion signal:
	// the probe goroutine records it after the binder denies.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, ok := bundle.Router.SlotCapacity(key); ok && n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("slot probe never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.props.Load(); got != 0 {
		t.Errorf("/props received %d requests without a slot-probe edge, want 0", got)
	}
}

// The admitted slot probe works end to end through its guarded per-backend
// client.
func TestNewGatedSlotProbeAdmitted(t *testing.T) {
	a := newCountingOpenAIServer(t)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"a": {APIFormat: "openai-compat", BaseURL: a.srv.URL, SlotDiscovery: true},
		},
		Models:   map[string]config.ModelConfig{"chatrole": {Provider: "a", Name: "m1", Type: "dense", ContextWindow: 32768}},
		Defaults: map[string]string{"chat": "chatrole"},
	}
	route, err := planRoleRoute(cfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, cfg, []PlannedRoute{route}, planOpts{})

	bundle, err := New(t.Context(), Options{
		Config:          cfg,
		DestinationGate: gate,
		ActiveProviders: plan.ActiveProviders,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()

	key := provider.ModelKey{Provider: "a", Model: "m1"}
	bundle.Router.RecordSlotUse(key)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, ok := bundle.Router.SlotCapacity(key); ok && n == 4 {
			break
		}
		if time.Now().After(deadline) {
			n, ok := bundle.Router.SlotCapacity(key)
			t.Fatalf("probed capacity never arrived; last = (%d, %v), props hits = %d", n, ok, a.props.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := a.props.Load(); got != 1 {
		t.Errorf("/props hit %d times, want 1", got)
	}
}

// Ungated New (nil gate) stays byte-for-byte the pre-#477 behavior: every
// configured provider refreshes.
func TestNewUngatedRefreshesEveryProvider(t *testing.T) {
	a := newCountingOpenAIServer(t)
	b := newCountingOpenAIServer(t)
	cfg := twoProviderConfig(a.srv.URL, b.srv.URL)

	bundle, err := New(t.Context(), Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bundle.Close() }()
	if a.models.Load() != 1 || b.models.Load() != 1 {
		t.Errorf("ungated refresh counts = (%d, %d), want (1, 1)", a.models.Load(), b.models.Load())
	}
}

// A gate without ActiveProviders is a wiring bug, not a silent
// nothing-refreshed startup — and so is an active name that matches no
// configured provider, which would skip every refresh while the consent
// receipt claims coverage.
func TestNewGateRequiresActiveProviders(t *testing.T) {
	cfg := twoProviderConfig("http://127.0.0.1:1", "http://127.0.0.1:2")
	_, err := New(context.Background(), Options{
		Config:          cfg,
		DestinationGate: provider.NewDestinationGate(),
	})
	if err == nil {
		t.Fatal("gated New with no ActiveProviders accepted")
	}

	// The unknown-name check must discriminate on its own, so the gate here
	// ADMITS everything the plan reaches — with a deny-all gate the known
	// provider's refresh denial would mask a dropped check.
	a := newCountingOpenAIServer(t)
	b := newCountingOpenAIServer(t)
	okCfg := twoProviderConfig(a.srv.URL, b.srv.URL)
	route, err := planRoleRoute(okCfg, "chatrole", "chat")
	if err != nil {
		t.Fatal(err)
	}
	gate, plan := gateFromPlan(t, okCfg, []PlannedRoute{route}, planOpts{})
	_, err = New(context.Background(), Options{
		Config:          okCfg,
		DestinationGate: gate,
		ActiveProviders: append([]string{"no-such-provider"}, plan.ActiveProviders...),
	})
	if err == nil {
		t.Fatal("gated New with an unknown active provider accepted")
	}
}
