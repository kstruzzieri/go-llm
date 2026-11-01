package openaicompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// TestCacheAffinityResolution pins how a client picks its mode: the base URL's
// host when no explicit mode is given, the explicit mode otherwise. A host
// matches only exactly or as a subdomain, so a look-alike never earns a key.
func TestCacheAffinityResolution(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		opts    []ClientOption // nil: inferred, so InferCacheAffinity must agree
		want    string
	}{
		{name: "openai", baseURL: "https://api.openai.com/v1", want: "openai"},
		{name: "openrouter", baseURL: "https://openrouter.ai/api", want: "openrouter"},
		{name: "openrouter look-alike", baseURL: "https://openrouter.ai.evil.example", want: "none"},
		{name: "loopback llama.cpp", baseURL: "http://127.0.0.1:8090", want: "none"},
		{name: "abacus", baseURL: "https://routellm.abacus.ai", want: "none"},
		{name: "explicit openai on abacus", baseURL: "https://routellm.abacus.ai", opts: []ClientOption{WithCacheAffinity("openai")}, want: "openai"},
		{name: "explicit none on openai", baseURL: "https://api.openai.com/v1", opts: []ClientOption{WithCacheAffinity("none")}, want: "none"},
		{name: "opencode zen", baseURL: "https://opencode.ai/zen/go", want: "opencode"},
		{name: "opencode inference", baseURL: "https://opencode.ai/inference/go/openai", want: "opencode"},
		{name: "host is case-insensitive and ignores port", baseURL: "HTTPS://API.OpenAI.com:443/v1", want: "openai"},
		{name: "subdomain", baseURL: "https://eu.openrouter.ai/api", want: "openrouter"},
		{name: "openai look-alike", baseURL: "https://api.openai.com.evil.example/v1", want: "none"},
		{name: "suffix without a dot", baseURL: "https://notopenai.com", want: "none"},
		{name: "suffix without a dot on a listed name", baseURL: "https://xopenrouter.ai", want: "none"},
		{name: "host in the path", baseURL: "https://evil.example/api.openai.com", want: "none"},
		{name: "unparseable", baseURL: "http://[::1", want: "none"},
		{name: "explicit empty infers", baseURL: "https://api.openai.com", opts: []ClientOption{WithCacheAffinity("")}, want: "openai"},
		{name: "unknown explicit sends nothing", baseURL: "https://api.openai.com", opts: []ClientOption{WithCacheAffinity("bogus")}, want: "none"},
		{name: "explicit opencode on loopback", baseURL: "http://127.0.0.1:8090", opts: []ClientOption{WithCacheAffinity("opencode")}, want: "opencode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.opts == nil {
				if got := InferCacheAffinity(tt.baseURL); got != tt.want {
					t.Errorf("InferCacheAffinity(%q) = %q, want %q", tt.baseURL, got, tt.want)
				}
			}
			if got := NewClient(tt.baseURL, tt.opts...).cacheAffinity; got != tt.want {
				t.Errorf("NewClient(%q).cacheAffinity = %q, want %q", tt.baseURL, got, tt.want)
			}
		})
	}
}

// affinityCapture records the headers and body of every request, answering
// SSE when the body asks to stream so Chat and ChatStream both decode.
type affinityCapture struct {
	srv *httptest.Server

	mu     sync.Mutex
	header []http.Header
	body   []map[string]json.RawMessage
}

func newAffinityCapture(t *testing.T) *affinityCapture {
	t.Helper()
	a := &affinityCapture{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not a JSON object: %v", err)
		}
		a.mu.Lock()
		a.header = append(a.header, r.Header.Clone())
		a.body = append(a.body, body)
		a.mu.Unlock()
		if string(body["stream"]) == "true" {
			writeSSE(t, w, []chatChunk{
				{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "ok"}}}},
				{Model: "m", Choices: []chatChunkChoice{{FinishReason: stringPtr("stop")}},
					Usage: &usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionJSON))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

// last returns the most recent request's three affinity carriers; a value
// that was not sent is reported as absent, not as "".
func (a *affinityCapture) last(t *testing.T) (opencode, sessionHdr, cacheKey string, okOpencode, okSession, okKey bool) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.header) == 0 {
		t.Fatal("no request reached the server")
	}
	h, b := a.header[len(a.header)-1], a.body[len(a.body)-1]
	if v, ok := h[http.CanonicalHeaderKey("x-opencode-session")]; ok {
		opencode, okOpencode = strings.Join(v, ","), true
	}
	if v, ok := h[http.CanonicalHeaderKey("x-session-id")]; ok {
		sessionHdr, okSession = strings.Join(v, ","), true
	}
	if raw, ok := b["prompt_cache_key"]; ok {
		okKey = true
		if err := json.Unmarshal(raw, &cacheKey); err != nil {
			t.Fatalf("prompt_cache_key is not a string: %s", raw)
		}
	}
	return
}

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// affinityChat drives one Chat or ChatStream through p with sessionID.
func affinityChat(t *testing.T, p *Provider, stream bool, sessionID string) {
	t.Helper()
	req := provider.ChatRequest{Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, SessionID: sessionID}
	var err error
	if stream {
		err = p.ChatStream(context.Background(), req, func(provider.ChatResponse) error { return nil })
	} else {
		_, err = p.Chat(context.Background(), req)
	}
	if err != nil {
		t.Fatalf("chat (stream=%v): %v", stream, err)
	}
}

// TestCacheAffinityWire captures what each mode puts on the wire, on both chat
// transports: openai a body key, openrouter a header key, opencode the raw
// session id, none nothing. An empty session sends nothing in any mode.
func TestCacheAffinityWire(t *testing.T) {
	const session = "thread-abc"
	for _, mode := range []string{"openai", "openrouter", "opencode", "none"} {
		for _, stream := range []bool{false, true} {
			name := mode
			if stream {
				name += "/stream"
			}
			t.Run(name, func(t *testing.T) {
				a := newAffinityCapture(t)
				p := NewProvider(NewClient(a.srv.URL, WithCacheAffinity(mode)))

				affinityChat(t, p, stream, session)
				oc, sh, key, okOC, okSH, okKey := a.last(t)
				switch mode {
				case "openai":
					if !okKey || !hex32.MatchString(key) {
						t.Errorf("prompt_cache_key = %q (present %v), want 32 hex", key, okKey)
					}
					if okOC || okSH {
						t.Errorf("openai mode sent x-opencode-session=%q (%v) x-session-id=%q (%v)", oc, okOC, sh, okSH)
					}
				case "openrouter":
					if !okSH || !hex32.MatchString(sh) {
						t.Errorf("x-session-id = %q (present %v), want 32 hex", sh, okSH)
					}
					if okOC || okKey {
						t.Errorf("openrouter mode sent x-opencode-session=%q (%v) prompt_cache_key=%q (%v)", oc, okOC, key, okKey)
					}
				case "opencode":
					if !okOC || oc != session {
						t.Errorf("x-opencode-session = %q (present %v), want the raw session id %q", oc, okOC, session)
					}
					if okSH || okKey {
						t.Errorf("opencode mode sent x-session-id=%q (%v) prompt_cache_key=%q (%v)", sh, okSH, key, okKey)
					}
				case "none":
					if okOC || okSH || okKey {
						t.Errorf("none mode sent x-opencode-session=%q (%v) x-session-id=%q (%v) prompt_cache_key=%q (%v)", oc, okOC, sh, okSH, key, okKey)
					}
				}

				affinityChat(t, p, stream, "")
				if oc, sh, key, okOC, okSH, okKey := a.last(t); okOC || okSH || okKey {
					t.Errorf("empty session sent x-opencode-session=%q (%v) x-session-id=%q (%v) prompt_cache_key=%q (%v)", oc, okOC, sh, okSH, key, okKey)
				}
			})
		}
	}
}

// TestCacheAffinityKeyStability: one client keys a session the same way every
// time, so the provider's cache can hit; two clients (two processes) key it
// differently, so the key is not a stable identifier across restarts.
func TestCacheAffinityKeyStability(t *testing.T) {
	const session = "thread-abc"
	for _, mode := range []string{"openai", "openrouter"} {
		t.Run(mode, func(t *testing.T) {
			a := newAffinityCapture(t)
			keyOf := func(p *Provider) string {
				affinityChat(t, p, false, session)
				_, sh, key, _, _, _ := a.last(t)
				if mode == "openai" {
					return key
				}
				return sh
			}
			p1 := NewProvider(NewClient(a.srv.URL, WithCacheAffinity(mode)))
			first, again := keyOf(p1), keyOf(p1)
			if first == "" || first != again {
				t.Errorf("same client keyed %q then %q, want one non-empty key", first, again)
			}
			if other := keyOf(NewProvider(NewClient(a.srv.URL, WithCacheAffinity(mode)))); other == first {
				t.Errorf("two clients both keyed the session %q, want per-client secrets", first)
			}
			if p1.client.affinityKey("thread-other") == first {
				t.Error("two sessions on one client share a key")
			}
		})
	}
}

// TestAffinityKeyNotRecomputableFromWorkspace: the key is keyed by a secret,
// so nobody holding the session id or the workspace root can derive it — not
// as the raw id, an unkeyed hash of it, or Golem's workspace id form
// ("workspace:" + hex(sha256(root))[:16], cmd/golem/memory.go).
func TestAffinityKeyNotRecomputableFromWorkspace(t *testing.T) {
	const root = "/Users/someone/project"
	sum := sha256.Sum256([]byte(root))
	workspace := "workspace:" + hex.EncodeToString(sum[:])[:16]
	for _, session := range []string{"thread-abc", workspace} {
		key := NewClient("https://api.openai.com").affinityKey(session)
		if !hex32.MatchString(key) {
			t.Fatalf("affinityKey(%q) = %q, want 32 hex", session, key)
		}
		unkeyed := sha256.Sum256([]byte(session))
		for name, derivable := range map[string]string{
			"raw session id":        session,
			"unkeyed sha256":        hex.EncodeToString(unkeyed[:])[:32],
			"workspace id":          workspace,
			"workspace id hex tail": strings.TrimPrefix(workspace, "workspace:"),
		} {
			if strings.Contains(key, derivable) {
				t.Errorf("affinityKey(%q) = %q is derivable as the %s %q", session, key, name, derivable)
			}
		}
	}
}

// TestAffinityKeyFailsClosedWithoutSecret: a client with no secret (a struct
// literal, or a crypto/rand failure) must send no key rather than an HMAC
// under an empty, publicly known key.
func TestAffinityKeyFailsClosedWithoutSecret(t *testing.T) {
	c := &Client{cacheAffinity: "openai"}
	if got := c.affinityKey("thread-abc"); got != "" {
		t.Errorf("affinityKey without a secret = %q, want \"\"", got)
	}
}
