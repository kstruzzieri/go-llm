package providerbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// Every credential here is a synthetic fixture. No real configuration,
// environment variable or service is read.
const guardTestKey = "golem-c-key-7c41d0a95e2b"

// seenRequest is one request the fake backend received.
type seenRequest struct{ host, path, body string }

// contentBackend fakes an openai-compat server and records every request that
// reaches it, whatever host the client addressed.
type contentBackend struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

// newContentBackend starts the fake and points every dial of
// http.DefaultTransport clones at it, so a destination such as
// http://llm.example.invalid is reachable without DNS or a real network. The
// destination guard still sees the real destination, so what the test observes
// is the guard's and the content guard's behavior, not the network's. handle
// runs after the request is recorded and answers it when it returns true.
//
// The swap of the process-wide http.DefaultTransport is undone at cleanup.
// These tests are serial, and parallel tests in the package only start once the
// serial ones have finished, so nothing else reads the transport meanwhile.
func newContentBackend(t *testing.T, handle func(w http.ResponseWriter, r *http.Request) bool) *contentBackend {
	t.Helper()
	b := &contentBackend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.seen = append(b.seen, seenRequest{host: r.Host, path: r.URL.Path, body: string(raw)})
		b.mu.Unlock()
		if handle != nil && handle(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		case "/v1/embeddings":
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(raw, &req)
			data := make([]map[string]any, len(req.Input))
			for i := range data {
				data[i] = map[string]any{"index": i, "embedding": []float64{0.5, 0.25}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)

	addr := b.srv.Listener.Addr().String()
	prev := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		},
	}
	t.Cleanup(func() { http.DefaultTransport = prev })
	return b
}

// total is the number of requests received on any path.
func (b *contentBackend) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.seen)
}

// requests returns the requests received for path, in arrival order.
func (b *contentBackend) requests(path string) []seenRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []seenRequest
	for _, r := range b.seen {
		if r.path == path {
			out = append(out, r)
		}
	}
	return out
}

// guardCfg is a one-provider openai-compat config named "under".
func guardCfg(baseURL, apiKey string) *config.Config {
	return &config.Config{Providers: map[string]config.ProviderConfig{
		"under": {APIFormat: "openai-compat", BaseURL: baseURL, APIKey: apiKey},
	}}
}

// bootGuarded bootstraps cfg behind an armed destination gate that admits the
// chat and embedding routes of every provider, with "under" active. A non-empty
// overrideURL replaces the base URL of "under" the way the golem flag does.
func bootGuarded(t *testing.T, cfg *config.Config, overrideURL string, contentGuard bool) (*Bundle, *provider.DestinationGate) {
	t.Helper()
	overrideProvider := ""
	if overrideURL != "" {
		overrideProvider = "under"
	}
	eff, err := Materialize(cfg, "", overrideProvider, overrideURL)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	plan, err := BuildNetworkPlan(eff, []PlannedRoute{
		{UseCase: "chat", Recommend: true},
		{UseCase: "embedding", Recommend: true},
	}, PlanOptions{})
	if err != nil {
		t.Fatalf("BuildNetworkPlan: %v", err)
	}
	m, err := provider.NewDestinationManifest(plan.Edges...)
	if err != nil {
		t.Fatalf("NewDestinationManifest: %v", err)
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.AllowAllDestinations(), m); err != nil {
		t.Fatalf("Install: %v", err)
	}
	b, err := New(t.Context(), Options{
		Config:                          cfg,
		OpenAICompatURLOverrideProvider: overrideProvider,
		OpenAICompatURLOverride:         overrideURL,
		DestinationGate:                 gate,
		ActiveProviders:                 plan.ActiveProviders,
		ContentGuard:                    contentGuard,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, gate
}

// chatVia sends one chat turn through the bundle's "under" provider with a
// destination capability bound, as a routed call would carry.
func chatVia(t *testing.T, b *Bundle, gate *provider.DestinationGate, content string) error {
	t.Helper()
	p, ok := b.Providers.Get("under")
	if !ok {
		t.Fatal("provider \"under\" not registered")
	}
	ctx, err := gate.Bind(t.Context(), "chat", "under")
	if err != nil {
		t.Fatalf("Bind chat: %v", err)
	}
	_, err = p.Chat(ctx, provider.ChatRequest{
		Model:    "m1",
		Messages: []provider.ChatMessage{{Role: "user", Content: content}},
	})
	return err
}

// embedVia sends one embedding request through the bundle's "under" provider.
func embedVia(ctx context.Context, t *testing.T, b *Bundle, gate *provider.DestinationGate, input string) error {
	t.Helper()
	p, ok := b.Providers.Get("under")
	if !ok {
		t.Fatal("provider \"under\" not registered")
	}
	bctx, err := gate.Bind(ctx, "embedding", "under")
	if err != nil {
		t.Fatalf("Bind embedding: %v", err)
	}
	_, err = p.Embed(bctx, provider.EmbedRequest{Model: "m1", Input: []string{input}})
	return err
}

// TestContentGuardInstalledOnlyForNonLoopback (#424): the guard follows the
// MATERIALIZED destination, so a URL override moves a provider across the
// boundary in either direction. Installation is proved by behavior: a guarded
// client refuses a body carrying the key and sends nothing, an exempt one sends
// it, and a clean body passes through either.
func TestContentGuardInstalledOnlyForNonLoopback(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		override string
		guarded  bool
	}{
		{name: "localhost", baseURL: "http://localhost:8080"},
		{name: "127.0.0.1", baseURL: "http://127.0.0.1:8080"},
		{name: "[::1]", baseURL: "http://[::1]:8080"},
		{name: "10.0.0.5", baseURL: "http://10.0.0.5:8080", guarded: true},
		{name: "169.254.1.1", baseURL: "http://169.254.1.1:8080", guarded: true},
		{name: "named host", baseURL: "http://llm.example.invalid", guarded: true},
		{name: "override to remote", baseURL: "http://127.0.0.1:8080", override: "http://llm.example.invalid", guarded: true},
		{name: "override to localhost", baseURL: "http://llm.example.invalid", override: "http://localhost:8080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := newContentBackend(t, nil)
			b, gate := bootGuarded(t, guardCfg(tc.baseURL, guardTestKey), tc.override, true)
			// The startup refresh went through this provider's client: the
			// route to the backend works, so zero chats below is a refusal.
			if n := len(be.requests("/v1/models")); n != 1 {
				t.Fatalf("startup model refresh requests = %d, want 1", n)
			}

			err := chatVia(t, b, gate, "note "+guardTestKey)
			chats := be.requests("/v1/chat/completions")
			if !tc.guarded {
				if err != nil {
					t.Fatalf("exempt client: chat with the key = %v, want it sent", err)
				}
				if len(chats) != 1 || !strings.Contains(chats[0].body, guardTestKey) {
					t.Fatalf("exempt client: backend chats = %+v, want exactly one carrying the key", chats)
				}
				return
			}
			if !errors.Is(err, provider.ErrContentDenied) {
				t.Fatalf("guarded client: chat with the key = %v, want ErrContentDenied", err)
			}
			if len(chats) != 0 {
				t.Fatalf("guarded client: backend chats = %+v, want none", chats)
			}
			// A clean body passes the same client, so the denial was the
			// content check and not a dead route.
			if err := chatVia(t, b, gate, "hello"); err != nil {
				t.Fatalf("guarded client: clean chat = %v, want it sent", err)
			}
			chats = be.requests("/v1/chat/completions")
			if len(chats) != 1 || strings.Contains(chats[0].body, guardTestKey) {
				t.Fatalf("guarded client: backend chats = %+v, want exactly one clean body", chats)
			}
		})
	}
}

// TestContentGuardDefaultOff (#424): without Options.ContentGuard the built
// clients are the ones a default bootstrap always built, remote or not.
func TestContentGuardDefaultOff(t *testing.T) {
	be := newContentBackend(t, nil)
	b, gate := bootGuarded(t, guardCfg("http://llm.example.invalid", guardTestKey), "", false)
	if err := chatVia(t, b, gate, "note "+guardTestKey); err != nil {
		t.Fatalf("chat with the key = %v, want it sent", err)
	}
	chats := be.requests("/v1/chat/completions")
	if len(chats) != 1 || !strings.Contains(chats[0].body, guardTestKey) {
		t.Fatalf("backend chats = %+v, want exactly one carrying the key", chats)
	}
}

// TestContentGuardRequiresGate (#424): the guard relies on the destination
// guard's redirect refusal and origin binding, so asking for it without a gate
// is a wiring error raised before anything is built or contacted.
func TestContentGuardRequiresGate(t *testing.T) {
	const want = "providerbootstrap: ContentGuard requires DestinationGate"
	t.Run("valid config", func(t *testing.T) {
		be := newContentBackend(t, nil)
		_, err := New(t.Context(), Options{Config: guardCfg("http://llm.example.invalid", guardTestKey), ContentGuard: true})
		if err == nil || err.Error() != want {
			t.Fatalf("New error = %v, want %q", err, want)
		}
		if n := be.total(); n != 0 {
			t.Fatalf("backend requests = %d, want 0", n)
		}
	})
	t.Run("raised before materialization", func(t *testing.T) {
		// A config with no providers fails Materialize with its own error; the
		// gate check must come first.
		_, err := New(t.Context(), Options{Config: &config.Config{}, ContentGuard: true})
		if err == nil || err.Error() != want {
			t.Fatalf("New error = %v, want %q", err, want)
		}
	})
}

// TestContentGuardKeepsRedirectRefusal (#424): the content guard sits inside
// the destination guard's client and must not weaken its refusal to follow a
// redirect. The loopback row matters most: a loopback client carries no content
// guard, so only the redirect refusal keeps its unchecked body on the machine.
func TestContentGuardKeepsRedirectRefusal(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		status  int
	}{
		{name: "guarded remote 302", baseURL: "http://llm.example.invalid", status: http.StatusFound},
		{name: "exempt loopback 307", baseURL: "http://127.0.0.1:8080", status: http.StatusTemporaryRedirect},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			be := newContentBackend(t, func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/v1/chat/completions" {
					return false
				}
				w.Header().Set("Location", "http://other.example.invalid/v1/chat/completions")
				w.WriteHeader(tc.status)
				return true
			})
			b, gate := bootGuarded(t, guardCfg(tc.baseURL, guardTestKey), "", true)

			err := chatVia(t, b, gate, "hello")
			if !errors.Is(err, provider.ErrDestinationDenied) {
				t.Fatalf("chat answered with a redirect = %v, want ErrDestinationDenied", err)
			}
			if errors.Is(err, provider.ErrContentDenied) {
				t.Fatalf("redirect refusal reported as a content denial: %v", err)
			}
			// Every dial reaches this one backend, so a followed redirect would
			// show as a second chat request addressed to the other host.
			chats := be.requests("/v1/chat/completions")
			if len(chats) != 1 {
				t.Fatalf("backend chats = %+v, want exactly the original request", chats)
			}
		})
	}
}

// TestContentGuardUsesBundleSnapshot (#424): the guard on the built client is
// the bundle's matcher, template components included. Only the resolved
// component is a secret; the authored template, its name and its static prefix
// are not.
func TestContentGuardUsesBundleSnapshot(t *testing.T) {
	const component = "golem-t-component-0001"
	doc, err := config.ParseDocument([]byte(`{
  "providers": {"under": {"base_url": "http://llm.example.invalid", "api_format": "openai-compat", "api_key": "static-prefix-0001-${GOLEM_T_KEY}"}},
  "models": {"chat": {"name": "m", "provider": "under", "type": "dense"}}
}`), config.Origin{Source: config.OriginExplicit}, config.DocumentOptions{LookupEnv: func(name string) (string, bool) {
		if name == "GOLEM_T_KEY" {
			return component, true
		}
		return "", false
	}})
	if err != nil {
		t.Fatalf("ParseDocument: %v", err)
	}
	tests := []struct {
		text   string
		denied bool
	}{
		{component, true},
		{"static-prefix-0001-" + component, true},
		{"static-prefix-0001-", false},
		{"${GOLEM_T_KEY}", false},
		{"golem-t-component-0002", false},
	}
	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			be := newContentBackend(t, nil)
			b, gate := bootGuarded(t, doc.Config(), "", true)
			err := chatVia(t, b, gate, tc.text)
			chats := be.requests("/v1/chat/completions")
			if tc.denied {
				if !errors.Is(err, provider.ErrContentDenied) || len(chats) != 0 {
					t.Fatalf("chat(%q) = %v with %d backend chats, want ErrContentDenied and none", tc.text, err, len(chats))
				}
				return
			}
			if err != nil || len(chats) != 1 {
				t.Fatalf("chat(%q) = %v with %d backend chats, want it sent once", tc.text, err, len(chats))
			}
		})
	}
}

// TestNewBundleSharesNoFlightOrMatcher (#424): two bootstraps share neither an
// embedding flight nor a matcher. An embed held in flight on the first bundle
// does not serve an identical call on the second, and each bundle's matcher
// denies only its own key. The held flight is not drained: it finishes under
// the matcher it started with (approval decision 3).
func TestNewBundleSharesNoFlightOrMatcher(t *testing.T) {
	const (
		key1  = "golem-c-key-one-3b7e91d4"
		key2  = "golem-c-key-two-8a52f0c6"
		host1 = "one.example.invalid"
		host2 = "two.example.invalid"
		held  = "shared-input-0001"
	)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseFlight := func() { releaseOnce.Do(func() { close(release) }) }

	be := newContentBackend(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/embeddings" && r.Host == host1 {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		return false // answered by the default handler once released
	})
	// Registered after the backend, so it runs first: the server's Close waits
	// for outstanding handlers, and a failed test must not leave one blocked.
	t.Cleanup(releaseFlight)

	b1, gate1 := bootGuarded(t, guardCfg("http://"+host1, key1), "", true)
	b2, gate2 := bootGuarded(t, guardCfg("http://"+host2, key2), "", true)

	firstDone := make(chan error, 1)
	go func() { firstDone <- embedVia(t.Context(), t, b1, gate1, held) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first bundle's embed never reached the backend")
	}

	// Same model, same input: one shared flight would leave this call waiting
	// on the held one until its deadline.
	ctx2, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := embedVia(ctx2, t, b2, gate2, held); err != nil {
		t.Fatalf("second bundle's embed = %v, want it served independently of the held flight", err)
	}
	embeds := be.requests("/v1/embeddings")
	var toOne, toTwo int
	for _, r := range embeds {
		switch r.host {
		case host1:
			toOne++
		case host2:
			toTwo++
		}
	}
	if toOne != 1 || toTwo != 1 {
		t.Fatalf("embedding requests by host = one:%d two:%d, want 1 each (%+v)", toOne, toTwo, embeds)
	}

	// Each matcher knows only its own key, as a function and as a built guard.
	for _, tc := range []struct {
		name  string
		match func(string) bool
		own   string
		other string
	}{
		{"bundle one", b1.SecretMatcher, key1, key2},
		{"bundle two", b2.SecretMatcher, key2, key1},
	} {
		if !tc.match(tc.own) || tc.match(tc.other) {
			t.Errorf("%s matcher: own=%v other=%v, want true/false", tc.name, tc.match(tc.own), tc.match(tc.other))
		}
	}
	if err := embedVia(t.Context(), t, b2, gate2, key2); !errors.Is(err, provider.ErrContentDenied) {
		t.Errorf("second bundle embedding its own key = %v, want ErrContentDenied", err)
	}
	if err := embedVia(t.Context(), t, b2, gate2, key1); err != nil {
		t.Errorf("second bundle embedding the first bundle's key = %v, want it sent", err)
	}

	releaseFlight()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Errorf("held embed on the first bundle = %v, want it to finish", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held embed did not finish after release")
	}
	if err := embedVia(t.Context(), t, b1, gate1, key1); !errors.Is(err, provider.ErrContentDenied) {
		t.Errorf("first bundle embedding its own key = %v, want ErrContentDenied", err)
	}
	if err := embedVia(t.Context(), t, b1, gate1, key2); err != nil {
		t.Errorf("first bundle embedding the second bundle's key = %v, want it sent", err)
	}
}
