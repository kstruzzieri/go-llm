package compat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota + 1
)

// requestIDFrom returns the request ID attached by requestIDMiddleware, or "".
func requestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// corsMiddleware applies a simple CORS policy. An empty origin disables CORS.
func corsMiddleware(next http.Handler, origin string) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoveryMiddleware turns handler panics into 500 JSON errors. If the handler
// panics after response headers have already been committed (e.g. mid-stream
// in SSE), writing a fresh 500 would corrupt the body and produce Go's
// "superfluous response.WriteHeader" warning. In that case we re-panic with
// http.ErrAbortHandler, which is the stdlib's signal to drop the connection
// without logging.
func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			log.Printf("compat: panic serving %s %s: %v", r.Method, r.URL.Path, p)
			if rec, ok := w.(*statusRecorder); ok && rec.wroteHeader {
				panic(http.ErrAbortHandler)
			}
			writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware ensures every request has a correlation ID. If the
// caller sent X-Request-Id, it is reused; otherwise a fresh 128-bit hex ID
// is generated. The ID is reflected on the response header.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the status code for logging and tracks whether
// headers have been committed so recoveryMiddleware knows when a panic is
// mid-stream. It also forwards http.Flusher so SSE handlers wrapped by
// loggingMiddleware can still flush per chunk — without this, w.(http.Flusher)
// would fail against the recorder and the outbound stream would be silently
// buffered.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write marks headers as committed — stdlib auto-calls WriteHeader(200) on the
// first Write, and recoveryMiddleware needs to see that commit to avoid
// double-writing headers after a mid-stream panic.
func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// loggingMiddleware writes one line per request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("compat: %s %s %d %s rid=%s",
			r.Method, r.URL.Path, rec.status,
			time.Since(start).Round(time.Millisecond),
			requestIDFrom(r.Context()))
	})
}
