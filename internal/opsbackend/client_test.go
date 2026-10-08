package opsbackend

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestRootDestinationRefusesPrefixesBeforeIO(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:8090/upstream/gemma4:31b",
		"http://127.0.0.1:8090/v1",
		"http://user@127.0.0.1:8090",
		"http://127.0.0.1:8090/?x=1",
		"ftp://127.0.0.1:8090",
		"",
	} {
		if _, err := rootDestination("p", base); !isCode(err, CodeInvalidConfiguration) {
			t.Fatalf("rootDestination(%q) = %v, want invalid_configuration", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:8090", "http://127.0.0.1:8090/", "http://localhost:11434"} {
		if _, err := rootDestination("p", base); err != nil {
			t.Fatalf("rootDestination(%q) = %v", base, err)
		}
	}
}

func TestClientGetsAllowedPathWithBearer(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, err := rootDestination("ls", f.URL())
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int64
	c, err := newClient(d, opsfixture.SentinelAPIKey, provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.get(context.Background(), "version", "/api/version", versionLimit)
	if err != nil || string(body) != opsfixture.DefaultVersion {
		t.Fatalf("get = %q, %v", body, err)
	}
	if got := f.Auth(); len(got) != 1 || got[0] != "Bearer "+opsfixture.SentinelAPIKey {
		t.Fatalf("Authorization = %v", got)
	}
	if _, err := c.get(context.Background(), "props", "/props", versionLimit); !isCode(err, CodeRefused) {
		t.Fatalf("get /props = %v, want refused", err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1 (refusal must not reach it)", n)
	}
}

func TestClientStatusAndCap(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, _ := rootDestination("ls", f.URL())
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	f.SetStatus("/running", http.StatusUnauthorized)
	if _, err := c.get(context.Background(), "running", "/running", runningLimit); !isCode(err, CodeUnauthorized) {
		t.Fatalf("401 = %v", err)
	}
	if got := f.Auth(); len(got) != 1 || got[0] != "" {
		t.Fatalf("keyless Authorization = %q, want none sent", got)
	}
	f.SetStatus("/v1/models", http.StatusForbidden)
	if _, err := c.get(context.Background(), "models", "/v1/models", modelsLimit); !isCode(err, CodeUnauthorized) {
		t.Fatalf("403 = %v", err)
	}
	f.SetStatus("/running", http.StatusMultipleChoices) // Location-less 3xx: a plain response, not a redirect
	if _, err := c.get(context.Background(), "running", "/running", runningLimit); !isCode(err, CodeHTTPStatus) {
		t.Fatalf("300 = %v", err)
	}
	f.SetStatus("/api/metrics", http.StatusInternalServerError)
	_, err = c.get(context.Background(), "metrics", "/api/metrics", metricsLimit)
	if !isCode(err, CodeHTTPStatus) {
		t.Fatalf("500 = %v", err)
	}
	if containsAny(err.Error(), opsfixture.AllSentinels) {
		t.Fatalf("error carried body text: %q", err)
	}
	if _, err := c.get(context.Background(), "version", "/api/version", 8); !isCode(err, CodeTooLarge) {
		t.Fatalf("over cap = %v", err)
	}
	exact := int64(len(opsfixture.DefaultVersion))
	if body, err := c.get(context.Background(), "version", "/api/version", exact); err != nil || string(body) != opsfixture.DefaultVersion {
		t.Fatalf("body of exactly the cap = %q, %v; want it returned whole", body, err)
	}
}

func TestClientRequestTimeout(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	f.HoldFor(t, "/running")
	d, _ := rootDestination("ls", f.URL())
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	// The caller's deadline is longer than the 2 s request timeout, so only
	// the client's own timeout can end the request in time.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = c.get(ctx, "running", "/running", runningLimit)
	if elapsed := time.Since(start); !isCode(err, CodeTimeout) || elapsed > 3*time.Second {
		t.Fatalf("held request: err=%v after %v; want timeout within ~2s", err, elapsed)
	}
}

// rawPeer serves one canned response on loopback after reading the request
// head. With hangUp it closes at once; otherwise it waits for the client to
// close. done closes when the connection is finished.
func rawPeer(t *testing.T, response string, hangUp bool) (base string, done <-chan struct{}) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
			return
		}
		_, _ = io.WriteString(conn, response)
		if !hangUp {
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	return "http://" + ln.Addr().String(), ch
}

func TestClientTruncatedBodyIsUnreachable(t *testing.T) {
	base, _ := rawPeer(t, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\n0123456789", true)
	d, _ := rootDestination("ls", base)
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.get(context.Background(), "running", "/running", runningLimit)
	if !isCode(err, CodeUnreachable) || body != nil {
		t.Fatalf("truncated body = %q, %v; want nil, unreachable", body, err)
	}
}

// lockedLog is a log destination the transport's goroutines can write while
// the test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Not parallel: it swaps the process-wide log output.
func TestClientBytesPastContentLengthNeverLogged(t *testing.T) {
	logged := &lockedLog{}
	prev := log.Writer()
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(prev) })
	base, done := rawPeer(t, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello"+opsfixture.SentinelError, false)
	d, _ := rootDestination("ls", base)
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	if body, err := c.get(context.Background(), "running", "/running", runningLimit); err != nil || string(body) != "hello" {
		t.Fatalf("get = %q, %v; want the Content-Length body", body, err)
	}
	select {
	case <-done: // the client closed the connection; any log line is written
	case <-time.After(5 * time.Second):
		t.Fatal("client never closed the connection")
	}
	if s := logged.String(); opsfixture.ContainsSentinel(s) {
		t.Fatalf("process log carried body text: %q", s)
	}
}

func TestClientRefusesHeaderBreakingAPIKey(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, _ := rootDestination("ls", f.URL())
	var refused atomic.Int64
	_, err := newClient(d, opsfixture.SentinelAPIKey+"\r\nX-Evil: 1", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if !isCode(err, CodeInvalidConfiguration) {
		t.Fatalf("CR/LF api_key = %v, want invalid_configuration", err)
	}
	if opsfixture.ContainsSentinel(err.Error()) {
		t.Fatalf("error carried the key: %q", err)
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestClientRefusesRealRedirect(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/running", http.StatusFound) // allowed -> allowed, same origin
	}))
	t.Cleanup(srv.Close)
	d, _ := rootDestination("ls", srv.URL)
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.get(context.Background(), "version", "/api/version", versionLimit)
	if !isCode(err, CodeDenied) || hits.Load() != 1 {
		t.Fatalf("redirect: err=%v hits=%d; want denied and exactly one request", err, hits.Load())
	}
}

func TestRemoteDestinationCannotBuildClient(t *testing.T) {
	// A hosted base with a path (opencode's https://host/zen/go shape) is
	// remote, not invalid_configuration (spec §4.1): never contacted either way.
	for _, base := range []string{"https://api.example.com", "https://api.example.com/zen/go"} {
		d, err := rootDestination("remote", base)
		if err != nil || d.IsLocal() {
			t.Fatalf("rootDestination(%q) = local=%v, %v; want a remote destination", base, d.IsLocal(), err)
		}
		var refused atomic.Int64
		if _, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused); !isCode(err, CodeDenied) {
			t.Fatalf("remote client for %q = %v, want denied", base, err)
		}
	}
}

// TestAllowlistWrapsDestinationGuard proves the composition order of spec
// §4.2: the allowlist delegates to GuardHTTPClient's transport, not to the
// stock one beneath it. An allowed request on a context the gate never bound
// is denied by the guard and never sent.
func TestAllowlistWrapsDestinationGuard(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, err := rootDestination("ls", f.URL())
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, d.BaseURL()+"/api/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.hc.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("unbound request reached the backend (status %d): allowlist is not layered over GuardHTTPClient", resp.StatusCode)
	}
	if !errors.Is(err, provider.ErrDestinationDenied) || refused.Load() != 0 {
		t.Fatalf("unbound request = %v (refused %d), want the guard's denial, not an allowlist refusal", err, refused.Load())
	}
	if n := len(f.Requests()); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func isCode(err error, want Code) bool {
	code, ok := CodeOf(err)
	return ok && code == want && !errors.Is(err, context.Canceled)
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
