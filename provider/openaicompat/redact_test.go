package openaicompat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// echoServer answers 401 with the received Authorization value (the bare
// key and the full header) inside the body, as JSON or text/plain.
func echoServer(t *testing.T, asJSON bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		if asJSON {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad credentials " + auth + " / " + strings.TrimPrefix(auth, "Bearer ")}})
			return
		}
		_, _ = w.Write([]byte("denied: " + auth + " and " + strings.TrimPrefix(auth, "Bearer ")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func canaryKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "CANARY-776-" + hex.EncodeToString(b)
}

// assertRedacted checks the three error-producing paths without ever
// printing the key.
func assertRedacted(t *testing.T, srv *httptest.Server, key string) {
	t.Helper()
	p := NewProvider(NewClient(srv.URL, WithAPIKey(key)))
	req := provider.ChatRequest{Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}}}
	_, chatErr := p.Chat(context.Background(), req)
	streamErr := p.ChatStream(context.Background(), req, func(provider.ChatResponse) error { return nil })
	_, modelsErr := p.Models(context.Background())
	for name, err := range map[string]error{"Chat": chatErr, "ChatStream": streamErr, "Models": modelsErr} {
		if err == nil {
			t.Errorf("%s: want error", name)
			continue
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("%s: error contains the key (len %d)", name, len(err.Error()))
		}
		if !strings.Contains(err.Error(), "[redacted]") {
			t.Errorf("%s: error lacks [redacted] marker", name)
		}
	}
}

func TestErrorEnvelopeRedactsOwnKey(t *testing.T) {
	assertRedacted(t, echoServer(t, true), canaryKey(t))
}

func TestPlainErrorBodyRedactsOwnKey(t *testing.T) {
	assertRedacted(t, echoServer(t, false), canaryKey(t))
}

// TestStatusLineRedactsOwnKey: the reason phrase after the status code is
// server-chosen, so a raw listener can echo the Authorization header there.
// The body never carries the key, so "[redacted]" can only come from the
// status; each body shape reaches a different readErrorEnvelope return.
func TestStatusLineRedactsOwnKey(t *testing.T) {
	for name, body := range map[string]string{
		"envelope": `{"error":{"message":"denied"}}`,
		"plain":    "denied",
		"empty":    "",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, buf, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, _ = fmt.Fprintf(buf, "HTTP/1.1 401 echo %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", r.Header.Get("Authorization"), len(body), body)
				_ = buf.Flush()
			}))
			t.Cleanup(srv.Close)
			assertRedacted(t, srv, canaryKey(t))
		})
	}
}

func TestErrorTextUnchangedWithoutKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("upstream down"))
	}))
	t.Cleanup(srv.Close)
	_, err := NewProvider(NewClient(srv.URL)).Models(context.Background())
	if err == nil || !strings.HasSuffix(err.Error(), ": upstream down") || strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("keyless error changed: %v", err)
	}
}
