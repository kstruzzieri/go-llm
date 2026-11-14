package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/provider"
)

// boundedTestKey is the API key the bounded-request tests configure; no error
// text may carry it.
const boundedTestKey = "sk-bounded-test-secret"

// probeLimit is the byte limit a probe request carries, taken from the recipe.
func probeLimit(t *testing.T) int64 {
	t.Helper()
	recipe, ok := fingerprint.RecipeFor(fingerprint.MethodChatStreamTools)
	if !ok {
		t.Fatal("no chat-stream-tools recipe")
	}
	return recipe.MaxResponseBytes
}

func boundedCtx(t *testing.T) context.Context {
	return responsebound.WithLimit(context.Background(), probeLimit(t))
}

// completedStream is a complete streamed reply: one content delta, a finish
// reason, usage and the [DONE] sentinel.
const completedStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
	"data: [DONE]\n\n"

const completedChat = `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`

const listing = `{"object":"list","data":[{"id":"m"}]}`

// serveCompleted answers each endpoint with a complete, valid reply.
func serveCompleted(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet:
		_, _ = io.WriteString(w, listing)
	case strings.Contains(readAll(r), `"stream":true`):
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completedStream)
	default:
		_, _ = io.WriteString(w, completedChat)
	}
}

func readAll(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// writeBrokenBody sends status with a Content-Length the body never reaches,
// then either closes the connection (a short body) or resets it.
func writeBrokenBody(t *testing.T, w http.ResponseWriter, status int, reset bool) {
	t.Helper()
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: 4096\r\n\r\n{\"error\":{\"message\":\"cut", status, http.StatusText(status))
	_ = buf.Flush()
	if reset {
		// Let the client take the headers before the reset discards unread data.
		time.Sleep(100 * time.Millisecond)
		_ = conn.(*net.TCPConn).SetLinger(0)
	}
	_ = conn.Close()
}

// call is one way of sending a request through the client.
type call struct {
	name string
	do   func(ctx context.Context, p *Provider) error
}

var (
	streamCall = call{"POST stream", func(ctx context.Context, p *Provider) error {
		return p.ChatStream(ctx, provider.ChatRequest{Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}},
			func(provider.ChatResponse) error { return nil })
	}}
	chatCall = call{"POST chat", func(ctx context.Context, p *Provider) error {
		_, err := p.Chat(ctx, provider.ChatRequest{Model: "m", Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}})
		return err
	}}
	modelsCall = call{"GET models", func(ctx context.Context, p *Provider) error {
		_, err := p.Models(ctx)
		return err
	}}
)

func statusOf(err error) (int, bool) {
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		return hs.HTTPStatusCode(), true
	}
	return 0, false
}

// redirectPair is an origin that redirects every request to a target, each
// counting the requests it receives.
type redirectPair struct {
	origin, target *httptest.Server
	originHits     atomic.Int64
	targetHits     atomic.Int64
}

func newRedirectPair(t *testing.T, code int) *redirectPair {
	t.Helper()
	rp := &redirectPair{}
	rp.target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.targetHits.Add(1)
		serveCompleted(w, r)
	}))
	t.Cleanup(rp.target.Close)
	rp.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.originHits.Add(1)
		http.Redirect(w, r, rp.target.URL+r.URL.Path, code)
	}))
	t.Cleanup(rp.origin.Close)
	return rp
}

var redirectCodes = []int{301, 302, 303, 307, 308}

func TestBoundedRequest_RedirectRefused(t *testing.T) {
	for _, code := range redirectCodes {
		for _, c := range []call{streamCall, chatCall, modelsCall} {
			t.Run(fmt.Sprintf("%d %s", code, c.name), func(t *testing.T) {
				rp := newRedirectPair(t, code)
				p := NewProvider(NewClient(rp.origin.URL))
				err := c.do(boundedCtx(t), p)
				if got, ok := statusOf(err); !ok || got != code {
					t.Fatalf("err = %v (status %d, %v), want a status error for %d", err, got, ok, code)
				}
				if got := rp.originHits.Load(); got != 1 {
					t.Errorf("origin saw %d requests, want 1", got)
				}
				if got := rp.targetHits.Load(); got != 0 {
					t.Errorf("redirect target saw %d requests, want 0", got)
				}
			})
		}
	}
}

func TestBoundedRequest_GuardedClientKeepsItsRefusal(t *testing.T) {
	rp := newRedirectPair(t, http.StatusTemporaryRedirect)
	dest, err := provider.NewDestination("llamacpp", rp.origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := provider.NewDestinationManifest(provider.DestinationEdge{Purpose: "agent", Destination: dest})
	if err != nil {
		t.Fatal(err)
	}
	gate := provider.NewDestinationGate()
	if err := gate.Install(provider.NewDestinationPolicy(dest), manifest); err != nil {
		t.Fatal(err)
	}
	hc, err := provider.GuardHTTPClient(gate, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProvider(NewClient(rp.origin.URL, WithHTTPClient(hc)))
	for _, c := range []call{streamCall, modelsCall} {
		t.Run(c.name, func(t *testing.T) {
			before := rp.originHits.Load()
			err := c.do(responsebound.WithLimit(bound, probeLimit(t)), p)
			if !errors.Is(err, provider.ErrDestinationDenied) {
				t.Fatalf("err = %v, want ErrDestinationDenied", err)
			}
			if got := rp.originHits.Load() - before; got != 1 {
				t.Errorf("origin saw %d requests, want 1", got)
			}
			if got := rp.targetHits.Load(); got != 0 {
				t.Errorf("redirect target saw %d requests, want 0", got)
			}
		})
	}
}

func TestBoundedRequest_PermissiveHostPolicyCannotAllow(t *testing.T) {
	var policyCalls atomic.Int64
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		policyCalls.Add(1)
		return nil
	}}
	for _, c := range []call{streamCall, modelsCall} {
		t.Run(c.name, func(t *testing.T) {
			rp := newRedirectPair(t, http.StatusFound)
			p := NewProvider(NewClient(rp.origin.URL, WithHTTPClient(hc)))
			err := c.do(boundedCtx(t), p)
			if got, ok := statusOf(err); !ok || got != http.StatusFound {
				t.Fatalf("err = %v, want a status error for 302", err)
			}
			if got := rp.targetHits.Load(); got != 0 {
				t.Errorf("redirect target saw %d requests, want 0", got)
			}
		})
	}
	if policyCalls.Load() == 0 {
		t.Error("the host's own policy was never consulted")
	}
	// The host's client is unchanged: an ordinary request still follows.
	rp := newRedirectPair(t, http.StatusFound)
	if err := modelsCall.do(context.Background(), NewProvider(NewClient(rp.origin.URL, WithHTTPClient(hc)))); err != nil {
		t.Fatalf("ordinary request through the host client: %v", err)
	}
	if got := rp.targetHits.Load(); got != 1 {
		t.Errorf("ordinary request: target saw %d requests, want 1", got)
	}
}

// keyEchoTransport delivers a rejection whose body read fails with an error
// quoting the API key, so redaction of the cause is observable.
type keyEchoTransport struct{ status int }

type keyEchoBody struct{ sent bool }

var errKeyEcho = errors.New("read failed near " + boundedTestKey)

func (b *keyEchoBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"error":`), nil
	}
	return 0, errKeyEcho
}
func (b *keyEchoBody) Close() error { return nil }

func (tr keyEchoTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: tr.status, Status: fmt.Sprintf("%d %s", tr.status, http.StatusText(tr.status)),
		Header: http.Header{}, Body: &keyEchoBody{}, Request: r,
	}, nil
}

func TestBoundedRequest_ErrorBodyReadFailureKeepsStatusAndCause(t *testing.T) {
	for _, status := range []int{400, 403, 404, 422} {
		for _, mode := range []struct {
			name  string
			reset bool
			cause error
		}{
			{"short body", false, io.ErrUnexpectedEOF},
			{"reset", true, syscall.ECONNRESET},
		} {
			for _, c := range []call{streamCall, chatCall} {
				t.Run(fmt.Sprintf("%d %s %s", status, mode.name, c.name), func(t *testing.T) {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						writeBrokenBody(t, w, status, mode.reset)
					}))
					defer srv.Close()
					err := c.do(boundedCtx(t), NewProvider(NewClient(srv.URL, WithAPIKey(boundedTestKey))))
					assertRejectionRead(t, err, status, mode.cause)
				})
			}
		}
		t.Run(fmt.Sprintf("%d cause quoting the key", status), func(t *testing.T) {
			hc := &http.Client{Transport: keyEchoTransport{status: status}}
			err := streamCall.do(boundedCtx(t), NewProvider(NewClient("http://127.0.0.1:1", WithAPIKey(boundedTestKey), WithHTTPClient(hc))))
			assertRejectionRead(t, err, status, errKeyEcho)
		})
	}
}

func assertRejectionRead(t *testing.T, err error, status int, cause error) {
	t.Helper()
	var rre *responsebound.RejectionReadError
	if !errors.As(err, &rre) {
		t.Fatalf("err = %v (%T), want a *RejectionReadError", err, err)
	}
	if rre.StatusCode != status {
		t.Errorf("StatusCode = %d, want %d", rre.StatusCode, status)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v, want errors.Is %v", err, cause)
	}
	if code, ok := statusOf(err); ok {
		t.Errorf("err exposes HTTPStatusCode() = %d; a status-keyed rule would match it", code)
	}
	if strings.Contains(err.Error(), boundedTestKey) {
		t.Errorf("error text carries the API key: %q", err)
	}
}

func TestBoundedRequest_EmptyAndCappedErrorBodiesAreStatusErrors(t *testing.T) {
	bodies := map[string]string{
		"empty":         "",
		"over the cap":  `{"error":{"message":"` + strings.Repeat("x", 100*1024) + `"}}`,
		"plain message": `{"error":{"message":"tool_choice not supported"}}`,
	}
	for name, body := range bodies {
		for _, c := range []call{streamCall, chatCall} {
			t.Run(name+" "+c.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				err := c.do(boundedCtx(t), NewProvider(NewClient(srv.URL)))
				if got, ok := statusOf(err); !ok || got != http.StatusBadRequest {
					t.Fatalf("err = %v, want HTTPStatusCode() 400", err)
				}
				var rre *responsebound.RejectionReadError
				if errors.As(err, &rre) {
					t.Fatalf("a clean or capped body read as a read failure: %v", err)
				}
			})
		}
	}
}

func TestOrdinaryRequest_Unchanged(t *testing.T) {
	t.Run("a plain client follows a 307", func(t *testing.T) {
		for _, c := range []call{streamCall, chatCall, modelsCall} {
			rp := newRedirectPair(t, http.StatusTemporaryRedirect)
			if err := c.do(context.Background(), NewProvider(NewClient(rp.origin.URL))); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := rp.targetHits.Load(); got != 1 {
				t.Errorf("%s: target saw %d requests, want 1", c.name, got)
			}
		}
	})
	for _, status := range []int{400, 429, 500} {
		for _, reset := range []bool{false, true} {
			for _, c := range []call{streamCall, chatCall} {
				t.Run(fmt.Sprintf("%d reset=%v %s", status, reset, c.name), func(t *testing.T) {
					broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						writeBrokenBody(t, w, status, reset)
					}))
					defer broken.Close()
					clean := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(status)
						_, _ = io.WriteString(w, `{"error":{"message":"cut"}}`)
					}))
					defer clean.Close()

					brokenErr := c.do(context.Background(), NewProvider(NewClient(broken.URL)))
					cleanErr := c.do(context.Background(), NewProvider(NewClient(clean.URL)))
					if got, ok := statusOf(brokenErr); !ok || got != status {
						t.Fatalf("broken body: err = %v, want a status error for %d", brokenErr, status)
					}
					var rre *responsebound.RejectionReadError
					if errors.As(brokenErr, &rre) {
						t.Fatalf("an ordinary request reported a read failure: %v", brokenErr)
					}
					if bi, ci := provider.IsInfrastructureError(brokenErr), provider.IsInfrastructureError(cleanErr); bi != ci {
						t.Errorf("IsInfrastructureError: broken body %v, clean body %v", bi, ci)
					}
				})
			}
		}
	}
}

func TestProbeResponseLimit_DoesNotChangeOrdinaryRequests(t *testing.T) {
	limit := probeLimit(t)
	const chunk = 1000
	frame := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("a", chunk) + "\"}}]}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for content := int64(0); content <= limit; content += chunk {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, completedStream)
	}))
	defer srv.Close()
	p := NewProvider(NewClient(srv.URL))

	var content int
	err := p.ChatStream(context.Background(), provider.ChatRequest{Model: "m"}, func(r provider.ChatResponse) error {
		content += len(r.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("ordinary stream over the probe limit: %v", err)
	}
	if int64(content) <= limit {
		t.Fatalf("ordinary stream delivered %d content bytes, want more than the probe limit %d", content, limit)
	}

	err = streamCall.do(boundedCtx(t), p)
	if !errors.Is(err, responsebound.ErrExceeded) {
		t.Fatalf("the same stream as a bounded request: err = %v, want ErrExceeded", err)
	}
}
