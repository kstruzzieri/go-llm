package openaicompat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	key = strings.TrimRight(key, " \t") // HTTP trims trailing header whitespace on the wire.
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

func TestMalformedResponseRedactsOwnKey(t *testing.T) {
	for name, response := range map[string]string{
		"status line": "HTTP/1.1 %s\r\nContent-Length: 0\r\n\r\n",
		"header":      "HTTP/1.1 401 Unauthorized\r\n%s\r\nContent-Length: 0\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			for name, key := range map[string]string{
				"plain":               canaryKey(t),
				"quoted":              "CANARY-776-quoted-\"\\-end",
				"trailing whitespace": canaryKey(t) + " \t",
			} {
				t.Run(name, func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, buf, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						defer func() { _ = conn.Close() }()
						key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
						_, _ = fmt.Fprintf(buf, response, key)
						_ = buf.Flush()
					}))
					t.Cleanup(srv.Close)
					assertRedacted(t, srv, key)
				})
			}
		})
	}
}

func TestTransportErrorRedactionPreservesCause(t *testing.T) {
	key := canaryKey(t)
	cause := errors.New("redirect refused for " + key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirect", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL, WithAPIKey(key), WithHTTPClient(&http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return cause },
	}))
	for name, call := range map[string]func() error{
		"GET": func() error { return c.getJSON(t.Context(), "/v1/models", nil) },
		"POST": func() error {
			_, err := c.post(t.Context(), "/v1/chat/completions", nil, "application/json")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("redirect unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "[redacted]") {
				t.Error("transport error did not redact the key")
			}
			if !errors.Is(err, cause) {
				t.Error("transport error lost its cause")
			}
			var urlErr *url.Error
			if !errors.As(err, &urlErr) {
				t.Error("transport error lost its URL error type")
			}
		})
	}
}

func TestTransportErrorUnchangedWithoutKey(t *testing.T) {
	for _, key := range []string{"", " \t"} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/redirect", http.StatusFound)
			}))
			t.Cleanup(srv.Close)
			c := NewClient(srv.URL, WithAPIKey(key), WithHTTPClient(&http.Client{
				CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("upstream down") },
			}))
			err := c.getJSON(t.Context(), "/v1/models", nil)
			if err == nil || !strings.HasSuffix(err.Error(), ": upstream down") || strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("keyless transport error changed: %v", err)
			}
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
