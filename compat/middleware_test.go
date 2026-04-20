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
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestCORSMiddleware_SetsHeaders(t *testing.T) {
	h := corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}), "*")

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS origin = %q, want *", got)
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
	}), "*")
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
