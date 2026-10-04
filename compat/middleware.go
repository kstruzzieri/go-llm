package compat

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
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

// crossOriginMiddleware refuses state-changing requests that a browser marks
// as coming from another origin (http.CrossOriginProtection). Disabling CORS
// only hides responses: a page can still send a text/plain POST, which needs
// no preflight, and run a model blind. The WithCORS origin is trusted, and
// "*" turns the check off, since it already invites every origin.
func crossOriginMiddleware(next http.Handler, origin string) http.Handler {
	if origin == "*" {
		return next
	}
	protection := http.NewCrossOriginProtection()
	if origin != "" {
		// ListenAndServe refuses an origin no browser sends (checkCORSOrigin);
		// one that reaches here anyway matches no request and admits no one.
		_ = protection.AddTrustedOrigin(origin)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := protection.Check(r); err != nil {
			writeError(w, http.StatusForbidden, "cross_origin_not_allowed", "cross-origin request not allowed (configure compat.WithCORS)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// checkCORSOrigin rejects a WithCORS origin that no browser sends as Origin.
// CORS and the cross-origin guard both compare it with the request's Origin
// byte for byte, so anything but a lower-case scheme://host[:port] without
// the scheme's default port refuses the very client it was meant to admit.
func checkCORSOrigin(origin string) error {
	if origin == "" || origin == "*" {
		return nil
	}
	if err := http.NewCrossOriginProtection().AddTrustedOrigin(origin); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCORSOrigin, err)
	}
	u, _ := url.Parse(origin) // cannot fail: AddTrustedOrigin parsed it
	if origin != strings.ToLower(u.Scheme+"://"+u.Host) {
		return fmt.Errorf("%w: %q is not a lower-case scheme://host[:port]", ErrInvalidCORSOrigin, origin)
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("%w: %q has an empty port", ErrInvalidCORSOrigin, origin)
	}
	if portText := u.Port(); portText != "" {
		// Browsers serialize ports as 16-bit decimal numbers without leading zeros.
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || portText != strconv.FormatUint(port, 10) {
			return fmt.Errorf("%w: %q port must be a canonical decimal number from 0 to 65535", ErrInvalidCORSOrigin, origin)
		}
		if u.Scheme == "https" && port == 443 || u.Scheme == "http" && port == 80 {
			return fmt.Errorf("%w: %q must omit the default port", ErrInvalidCORSOrigin, origin)
		}
	}
	return nil
}

// hostMiddleware refuses requests whose Host header names neither a loopback
// address nor an allowed host. This blocks DNS rebinding: a page on an
// attacker's domain that resolves to 127.0.0.1 is same-origin to the browser,
// so neither CORS nor the cross-origin guard applies, but its requests still
// carry the attacker's Host.
// allowed holds names already normalized by hostname.
func hostMiddleware(next http.Handler, allowed []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostname(r.Host)
		if host != "" && (isLoopbackHost(host) || slices.Contains(allowed, host)) {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("compat: refused %s %q: Host %q not allowed (see WithAllowedHosts)", r.Method, r.URL.Path, r.Host)
		writeError(w, http.StatusForbidden, "host_not_allowed", "Host header not allowed (configure compat.WithAllowedHosts)")
	})
}

// hostname returns the lower-cased host of a Host header or host:port
// address, without the port or IPv6 brackets.
func hostname(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
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

// withSemaphore wraps next with admission control at the handler boundary.
// This helper is for routes that do NOT need body-parse-before-acquire
// (e.g. GET endpoints, or fixed-priority POSTs like /feedback). Body-bearing
// endpoints that resolve priority from `x_priority` MUST acquire the
// semaphore inline AFTER parsing so the resolved priority is honored —
// wrapping them here would acquire before the priority is known.
func withSemaphore(sem *semaphore, p provider.Priority, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, ok := sem.acquire(p)
		if !ok {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "capacity", "server is at capacity")
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}
