package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// chatCompletionJSON is a minimal well-formed /v1/chat/completions response,
// enough for Chat to decode without error so tests can assert on what the
// client sent rather than on decode behaviour.
const chatCompletionJSON = `{"id":"1","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`

// headerCapture is a minimal OpenAI-compatible server recording the headers
// and body of the last request it served, so tests can assert on what the
// client actually put on the wire.
type headerCapture struct {
	srv *httptest.Server

	mu     sync.Mutex
	header http.Header
	body   string
}

func newHeaderCapture(t *testing.T) *headerCapture {
	t.Helper()
	h := &headerCapture{header: http.Header{}}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h.mu.Lock()
		h.header = r.Header.Clone()
		h.body = string(body)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionJSON))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// get returns the value of a captured header, "" when absent.
func (h *headerCapture) get(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.header.Get(name)
}

// present reports whether the header was sent at all, distinguishing an
// absent header from one sent with an empty value.
func (h *headerCapture) present(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.header[http.CanonicalHeaderKey(name)]
	return ok
}

func (h *headerCapture) requestBody() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.body
}

// chatOnce drives one non-streaming Chat through the capture server.
func chatOnce(t *testing.T, p *Provider, req provider.ChatRequest) {
	t.Helper()
	if req.Model == "" {
		req.Model = "m"
	}
	if req.Messages == nil {
		req.Messages = []provider.ChatMessage{{Role: "user", Content: "hi"}}
	}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

func TestUserAgentHeader(t *testing.T) {
	tests := []struct {
		name string
		opts []ClientOption
		want string // exact match; empty means "expect the go-llm/ default"
	}{
		{name: "defaults to a go-llm identity"},
		{name: "WithUserAgent overrides the default", opts: []ClientOption{WithUserAgent("firn/1.2.3")}, want: "firn/1.2.3"},
		{name: "empty WithUserAgent leaves the default intact", opts: []ClientOption{WithUserAgent("")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHeaderCapture(t)
			chatOnce(t, NewProvider(NewClient(h.srv.URL, tt.opts...)), provider.ChatRequest{})

			got := h.get("User-Agent")
			if tt.want != "" {
				if got != tt.want {
					t.Errorf("User-Agent = %q, want %q", got, tt.want)
				}
				return
			}
			if !strings.HasPrefix(got, "go-llm/") {
				t.Errorf("User-Agent = %q, want prefix %q", got, "go-llm/")
			}
			if strings.HasPrefix(got, "Go-http-client/") {
				t.Errorf("User-Agent = %q, want a client-specific agent, not the net/http default", got)
			}
		})
	}
}

// TestDefaultUserAgentReportsOnlyThisModule guards the version-resolution
// rule: info.Main describes the consumer when go-llm is imported, so its
// version must never be reported as go-llm's.
func TestDefaultUserAgentReportsOnlyThisModule(t *testing.T) {
	got := defaultUserAgent()
	if !strings.HasPrefix(got, "go-llm/") {
		t.Fatalf("defaultUserAgent() = %q, want prefix %q", got, "go-llm/")
	}
	if strings.Contains(got, "(") || strings.Contains(got, ")") {
		t.Errorf("defaultUserAgent() = %q, must not contain parentheses (RFC 9110 comment syntax)", got)
	}
	if strings.TrimPrefix(got, "go-llm/") == "" {
		t.Errorf("defaultUserAgent() = %q, want a non-empty version segment", got)
	}
}

const sessionHeader = "x-opencode-session"

func TestOpencodeSessionHeader(t *testing.T) {
	tests := []struct {
		name        string
		sessionID   string
		wantPresent bool
		wantErr     bool
	}{
		{name: "sent when SessionID is set", sessionID: "thread-abc", wantPresent: true},
		{name: "omitted entirely when SessionID is empty"},
		{name: "leading space is rejected", sessionID: " thread-abc", wantErr: true},
		{name: "trailing space is rejected", sessionID: "thread-abc ", wantErr: true},
		{name: "leading tab is rejected", sessionID: "\tthread-abc", wantErr: true},
		{name: "trailing tab is rejected", sessionID: "thread-abc\t", wantErr: true},
		{name: "whitespace only is rejected", sessionID: " \t", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHeaderCapture(t)
			_, err := NewProvider(NewClient(h.srv.URL)).Chat(context.Background(), provider.ChatRequest{
				Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}, SessionID: tt.sessionID,
			})
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "session ID") {
					t.Errorf("Chat error = %v, want session ID validation error", err)
				}
				if h.requestBody() != "" {
					t.Error("invalid session ID reached the server")
				}
				return
			}
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}

			if got := h.present(sessionHeader); got != tt.wantPresent {
				t.Fatalf("%s present = %v, want %v (value %q)", sessionHeader, got, tt.wantPresent, h.get(sessionHeader))
			}
			if tt.wantPresent {
				if got := h.get(sessionHeader); got != tt.sessionID {
					t.Errorf("%s = %q, want %q", sessionHeader, got, tt.sessionID)
				}
			}
		})
	}
}

// TestSessionIDStaysOffTheWireBody pins the json:"-" tag: the id is request
// metadata, and leaking it into the JSON body would change the payload every
// existing caller sends.
func TestSessionIDStaysOffTheWireBody(t *testing.T) {
	req := provider.ChatRequest{SessionID: "thread-abc"}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "thread-abc") {
		t.Errorf("marshaled ChatRequest contains SessionID: %s", raw)
	}
	h := newHeaderCapture(t)
	chatOnce(t, NewProvider(NewClient(h.srv.URL)), req)

	body := h.requestBody()
	if body == "" {
		t.Fatal("captured request body is empty; the assertion below would be vacuous")
	}
	if strings.Contains(body, "thread-abc") {
		t.Errorf("request body contains SessionID: %s", body)
	}
}

// TestSessionHeaderOnStreamingRequests covers ChatStream, which reaches the
// transport through postSSE rather than postJSON.
func TestSessionHeaderOnStreamingRequests(t *testing.T) {
	var (
		mu   sync.Mutex
		hdr  http.Header
		body string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hdr, body = r.Header.Clone(), string(b)
		mu.Unlock()
		writeSSE(t, w, []chatChunk{
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{Content: "ok"}}}},
			{Model: "m", Choices: []chatChunkChoice{{Delta: chatMessage{}, FinishReason: stringPtr("stop")}},
				Usage: &usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}},
		})
	}))
	t.Cleanup(srv.Close)

	p := NewProvider(NewClient(srv.URL))
	err := p.ChatStream(context.Background(), provider.ChatRequest{
		Model:     "m",
		Messages:  []provider.ChatMessage{{Role: "user", Content: "hi"}},
		SessionID: "thread-stream",
	}, func(provider.ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got, want := hdr.Get(sessionHeader), "thread-stream"; got != want {
		t.Errorf("%s = %q, want %q", sessionHeader, got, want)
	}
	if got := hdr.Get("User-Agent"); !strings.HasPrefix(got, "go-llm/") {
		t.Errorf("User-Agent = %q, want prefix %q", got, "go-llm/")
	}
	if strings.Contains(body, "thread-stream") {
		t.Errorf("streaming request body contains SessionID: %s", body)
	}
}
