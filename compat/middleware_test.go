package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestCORSMiddleware_SetsHeaders(t *testing.T) {
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "https://app.example")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("CORS origin = %q, want https://app.example", got)
	}
}

// TestBuildHandler_DefaultDisablesCORS pins the #633 default: a Server built
// without WithCORS sends no CORS headers, so a page on another origin can
// neither read responses nor get a preflight approved.
func TestBuildHandler_DefaultDisablesCORS(t *testing.T) {
	h := New(nil, nil, nil).buildHandler()
	for _, method := range []string{http.MethodGet, http.MethodOptions} {
		req := httptest.NewRequest(method, "/v1/models", nil)
		req.Host = "127.0.0.1:18741"
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want none by default", method, got)
		}
	}
}

// TestBuildHandler_HostGuard pins DNS-rebinding protection (#633): only a
// Host naming a loopback address, the WithAddr host, or a WithAllowedHosts
// entry reaches the routes; anything else is refused with 403. Allowed hosts
// request an unknown route, so a 404 proves the request got past the guard.
func TestBuildHandler_HostGuard(t *testing.T) {
	cases := []struct {
		name    string
		opts    []Option
		host    string
		allowed bool
	}{
		{"loopback v4", nil, "127.0.0.1:18741", true},
		{"loopback v4 without port", nil, "127.0.0.1", true},
		{"loopback v4 range", nil, "127.0.0.2:18741", true},
		{"loopback v6", nil, "[::1]:18741", true},
		{"loopback v6 without port", nil, "[::1]", true},
		{"localhost", nil, "localhost:18741", true},
		{"localhost any case", nil, "LocalHost:18741", true},
		{"rebinding name", nil, "attacker.example:18741", false},
		{"rebinding name without port", nil, "attacker.example", false},
		{"name embedding loopback ip", nil, "127.0.0.1.nip.io:18741", false},
		{"name embedding localhost", nil, "localhost.attacker.example:18741", false},
		{"unconfigured lan address", nil, "192.168.1.5:18741", false},
		{"unspecified address", nil, "0.0.0.0:18741", false},
		{"empty", nil, "", false},
		{"empty despite empty allowlist entry", []Option{WithAllowedHosts("")}, "", false},
		{"listen host", []Option{WithAddr("10.0.0.5:18741")}, "10.0.0.5:18741", true},
		{"listen host name any case", []Option{WithAddr("gateway.internal:443")}, "Gateway.Internal", true},
		{"other lan address with listen host", []Option{WithAddr("10.0.0.5:18741")}, "10.0.0.6:18741", false},
		{"allowlisted name", []Option{WithAllowedHosts("host.docker.internal")}, "host.docker.internal:18741", true},
		{"allowlist ignores entry port and case", []Option{WithAllowedHosts("Host.Docker.Internal:9999")}, "host.docker.internal:18741", true},
		{"allowlist accumulates", []Option{WithAllowedHosts("a.internal"), WithAllowedHosts("b.internal")}, "a.internal:18741", true},
		{"allowlist matches whole names", []Option{WithAllowedHosts("docker.internal")}, "host.docker.internal:18741", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/no-such-route", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			New(nil, nil, nil, tc.opts...).buildHandler().ServeHTTP(rec, req)
			if tc.allowed {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("Host %q: status = %d, want 404 from the mux (allowed)", tc.host, rec.Code)
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Host %q: status = %d, want 403 (refused)", tc.host, rec.Code)
			}
			if env := decodeErrorEnvelope(t, rec); env.Error.Code != "host_not_allowed" {
				t.Errorf("Host %q: error code = %q, want host_not_allowed", tc.host, env.Error.Code)
			}
		})
	}
}

// TestBuildHandler_HostGuardPrecedesCORS pins the guard outside CORS: with
// CORS enabled, a preflight naming a foreign Host is refused instead of
// approved, while one naming a loopback Host is still answered.
func TestBuildHandler_HostGuardPrecedesCORS(t *testing.T) {
	h := New(nil, nil, nil, WithCORS("https://app.example")).buildHandler()
	for _, tc := range []struct {
		host string
		want int
	}{
		{"attacker.example:18741", http.StatusForbidden},
		{"127.0.0.1:18741", http.StatusNoContent},
	} {
		req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		req.Host = tc.host
		req.Header.Set("Origin", "https://app.example")
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("preflight with Host %q: status = %d, want %d", tc.host, rec.Code, tc.want)
		}
	}
}

// TestBuildHandler_CrossOriginGuard pins CSRF protection (#633): a POST that a
// browser marks as coming from another origin is refused unless that origin
// is the WithCORS origin, while non-browser clients are unaffected. Allowed
// requests hit an unknown route, so a 404 proves they got past the guard.
func TestBuildHandler_CrossOriginGuard(t *testing.T) {
	cases := []struct {
		name      string
		opts      []Option
		origin    string // Origin header; "" sends none
		fetchSite string // Sec-Fetch-Site header; "" sends none
		allowed   bool
	}{
		{"non-browser client", nil, "", "", true},
		{"cross-site page", nil, "https://evil.example", "cross-site", false},
		{"same-site page on another port", nil, "http://localhost:3000", "same-site", false},
		{"old browser without Sec-Fetch-Site", nil, "https://evil.example", "", false},
		{"WithCORS origin is trusted", []Option{WithCORS("https://app.example")}, "https://app.example", "cross-site", true},
		{"other origin despite WithCORS", []Option{WithCORS("https://app.example")}, "https://evil.example", "cross-site", false},
		{"WithCORS star turns the check off", []Option{WithCORS("*")}, "https://evil.example", "cross-site", true},
		{"malformed WithCORS origin trusts nothing", []Option{WithCORS("https://app.example/")}, "https://app.example", "cross-site", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/no-such-route", strings.NewReader("{}"))
			req.Host = "127.0.0.1:18741"
			req.Header.Set("Content-Type", "text/plain")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			rec := httptest.NewRecorder()
			New(nil, nil, nil, tc.opts...).buildHandler().ServeHTTP(rec, req)
			if tc.allowed {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404 from the mux (allowed)", rec.Code)
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (refused)", rec.Code)
			}
			if env := decodeErrorEnvelope(t, rec); env.Error.Code != "cross_origin_not_allowed" {
				t.Errorf("error code = %q, want cross_origin_not_allowed", env.Error.Code)
			}
		})
	}
}

// TestChat_CrossSiteSimplePOSTDoesNotRunModel is the #633 regression: with
// CORS disabled a page can still send a text/plain POST, which needs no
// preflight. It must be refused before the model runs, not merely hidden.
func TestChat_CrossSiteSimplePOSTDoesNotRunModel(t *testing.T) {
	srv, _, calls, teardown := newChatFixture(t, "hi")
	defer teardown()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qwen3:8b","messages":[{"role":"user","content":"x"}]}`))
	req.Host = "127.0.0.1:18741"
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Errorf("model ran %d time(s) for a cross-site POST, want 0", n)
	}
}

func TestCORSMiddleware_EmptyOriginDisables(t *testing.T) {
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("want no CORS header, got %q", got)
	}
}

func TestCORSMiddleware_OptionsReturns204(t *testing.T) {
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next should not run for OPTIONS")
	}), "https://app.example")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, "/v1/models", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("OPTIONS status = %d, want 204", rec.Code)
	}
}

func TestRecoveryMiddleware_ConvertsPanicTo500(t *testing.T) {
	h := recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Type != "api_error" {
		t.Errorf("error type = %q, want api_error", env.Error.Type)
	}
}

func TestRecoveryMiddleware_PanicAfterHeadersAborts(t *testing.T) {
	// Regression guard: a handler panic after headers are committed (i.e.
	// mid-stream in SSE) must NOT try to write a fresh 500 header on top of
	// the already-sent 200. Writing twice would corrupt the stream and
	// trigger Go's "superfluous response.WriteHeader" warning. The correct
	// response is panic(http.ErrAbortHandler), which drops the connection.
	h := loggingMiddleware(recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: chunk 1\n\n"))
		panic("boom mid-stream")
	})))
	rec := httptest.NewRecorder()
	defer func() {
		p := recover()
		if p != http.ErrAbortHandler {
			t.Fatalf("expected panic with http.ErrAbortHandler, got %v", p)
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	t.Fatal("ServeHTTP returned without re-panicking on mid-stream panic")
}

func TestLoggingMiddleware_RecordsPanicStatus(t *testing.T) {
	// Regression guard for middleware order: when recoveryMiddleware is
	// INSIDE loggingMiddleware, a handler panic is converted to a 500 by
	// recovery and then observed as status=500 by logging. If the order is
	// inverted, logging's next.ServeHTTP re-panics before the log line runs,
	// and the 500 goes unrecorded.
	chain := loggingMiddleware(recoveryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})))
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRequestIDMiddleware_ReusesIncoming(t *testing.T) {
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestIDFrom(r.Context()) != "caller-id" {
			t.Errorf("context rid = %q, want caller-id", requestIDFrom(r.Context()))
		}
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-Id", "caller-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("X-Request-Id") != "caller-id" {
		t.Errorf("response rid = %q, want caller-id", rec.Header().Get("X-Request-Id"))
	}
}

func TestRequestIDMiddleware_GeneratesWhenAbsent(t *testing.T) {
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	id := rec.Header().Get("X-Request-Id")
	if len(id) != 32 || strings.TrimLeft(id, "0123456789abcdef") != "" {
		t.Errorf("generated rid = %q, want 32 hex chars", id)
	}
}

// flushableRecorder is httptest.ResponseRecorder plus an http.Flusher. The
// stdlib recorder does not implement Flusher, so without this stub the
// regression test below could not distinguish "Flush passthrough works" from
// "no Flusher on the chain at all".
type flushableRecorder struct {
	*httptest.ResponseRecorder
	flushed int
}

func (f *flushableRecorder) Flush() { f.flushed++ }

func TestLoggingMiddleware_ForwardsFlusher(t *testing.T) {
	// Regression guard for SSE: statusRecorder must forward Flush to the
	// underlying writer, otherwise streaming handlers (chat/completions)
	// silently buffer their chunks when wrapped by loggingMiddleware.
	var reachedHandler bool
	h := loggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reachedHandler = true
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped ResponseWriter does not implement http.Flusher")
		}
		f.Flush()
	}))
	rec := &flushableRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil))
	if !reachedHandler {
		t.Fatal("handler did not run")
	}
	if rec.flushed != 1 {
		t.Errorf("underlying Flush calls = %d, want 1", rec.flushed)
	}
}

func TestWriteError_OpenAIShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, http.StatusBadRequest, "bad_model", "unknown model")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Type != "invalid_request_error" ||
		env.Error.Code != "bad_model" ||
		env.Error.Message != "unknown model" {
		t.Errorf("bad error body: %+v", env)
	}
}

func TestStatusForCompatError_KnownSentinels(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"no_viable_candidate", provider.ErrNoViableCandidate, 400, "no_viable_candidate"},
		{"budget_exceeded", provider.ErrBudgetExceeded, 400, "budget_exceeded"},
		{"budget_adaptation_required", provider.ErrBudgetAdaptationRequired, 400, "budget_adaptation_required"},
		{"all_breakers_open", provider.ErrAllBreakersOpen, 503, "all_breakers_open"},
		{"deadline", context.DeadlineExceeded, 504, "timeout"},
		{"canceled", context.Canceled, 499, "canceled"},
		{"http_400", &provider.HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}, 400, "upstream_client_error"},
		{"http_404", &provider.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}, 404, "upstream_client_error"},
		{"http_429", &provider.HTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"}, 429, "rate_limited"},
		{"http_500", &provider.HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}, 502, "upstream_error"},
		{"http_502", &provider.HTTPStatusError{StatusCode: 502, Status: "502 Bad Gateway"}, 502, "upstream_error"},
		{"http_503", &provider.HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}, 502, "upstream_error"},
		{"unknown", errors.New("some random failure"), 502, "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, code, _ := statusForCompatError(tc.err)
			if status != tc.wantStatus || code != tc.wantCode {
				t.Errorf("statusForCompatError(%v) = (%d, %q), want (%d, %q)",
					tc.err, status, code, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

func TestStatusForCompatError_WrappedErrorsStillClassified(t *testing.T) {
	// Regression guard: handlers call fmt.Errorf("route: %w", err). The mapper
	// must unwrap and still classify correctly.
	wrapped := fmt.Errorf("route: %w", provider.ErrNoViableCandidate)
	status, code, _ := statusForCompatError(wrapped)
	if status != 400 || code != "no_viable_candidate" {
		t.Errorf("wrapped sentinel = (%d, %q), want (400, no_viable_candidate)", status, code)
	}

	wrappedHTTP := fmt.Errorf("provider: %w", &provider.HTTPStatusError{StatusCode: 429, Status: "429"})
	status, code, _ = statusForCompatError(wrappedHTTP)
	if status != 429 || code != "rate_limited" {
		t.Errorf("wrapped HTTP 429 = (%d, %q), want (429, rate_limited)", status, code)
	}
}

func TestConcurrencyMiddleware_429WhenFull(t *testing.T) {
	sem := newSemaphore(1)
	blocker := make(chan struct{})
	h := withSemaphore(sem, provider.PriorityNormal, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocker
	}))

	// First request takes the only slot.
	done := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status=%d want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header")
	}

	close(blocker)
	<-done
}

func TestWriteCompatError_EndToEnd(t *testing.T) {
	// Verify writeCompatError glues statusForCompatError + writeError correctly.
	rec := httptest.NewRecorder()
	writeCompatError(rec, provider.ErrAllBreakersOpen)
	if rec.Code != 503 {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	var env errorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "all_breakers_open" {
		t.Errorf("code = %q, want all_breakers_open", env.Error.Code)
	}
	if env.Error.Type != "api_error" {
		t.Errorf("type = %q, want api_error", env.Error.Type)
	}
}
