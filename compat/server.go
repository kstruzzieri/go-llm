package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// ErrNonLoopbackRequiresTLS is returned from ListenAndServe when the bind
// address is not a loopback and WithTLS was not configured.
var ErrNonLoopbackRequiresTLS = errors.New("compat: non-loopback address requires TLS (use WithTLS)")

// Server is the OpenAI-compatible HTTP façade over a provider.Router.
//
// After New returns, Server is safe for concurrent use: ListenAndServe runs
// the HTTP server, handler goroutines read Router/Registry/config fields
// concurrently, and Close performs idempotent shutdown from any goroutine.
// Options must be applied at construction time only; applying an Option to
// a Server that is already serving is not safe.
type Server struct {
	router    *provider.Router
	registry  *provider.ModelRegistry
	providers *provider.Registry

	addr              string
	basePath          string
	corsOrigin        string
	tlsCert           string
	tlsKey            string
	aliases           map[string]string
	maxConcurrency    int
	embeddingsEnabled bool
	shutdownTimeout   time.Duration

	httpServer *http.Server
	startedAt  time.Time

	// mu guards closed, which request handlers read to reject work that
	// arrives after shutdown begins. closeOnce is a separate primitive that
	// makes the Close body itself idempotent; it cannot replace closed because
	// handlers need a cheap readable flag, not a run-once action. startedAt is
	// written once in New so handlers invoked via buildHandler (tests,
	// embedded callers) see a meaningful uptime even when ListenAndServe has
	// not run; it is read-only thereafter and does not require mu.
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}

// New constructs a Server with the given router, model registry, and provider
// registry. These may be nil in tests that exercise only the bare HTTP
// lifecycle; real callers must supply all three. Options override defaults.
func New(router *provider.Router, registry *provider.ModelRegistry, providers *provider.Registry, opts ...Option) *Server {
	s := &Server{
		router:            router,
		registry:          registry,
		providers:         providers,
		addr:              "127.0.0.1:18741",
		basePath:          "/v1",
		corsOrigin:        "*",
		aliases:           map[string]string{},
		maxConcurrency:    4,
		embeddingsEnabled: false,
		shutdownTimeout:   30 * time.Second,
		startedAt:         time.Now(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ListenAndServe starts the HTTP server and blocks until ctx is cancelled or
// an unrecoverable error occurs. Non-loopback bind requires TLS.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.tlsCert == "" && !isLoopback(s.addr) {
		return fmt.Errorf("%w: addr=%q", ErrNonLoopbackRequiresTLS, s.addr)
	}

	httpServer := &http.Server{Addr: s.addr, Handler: s.buildHandler()}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	s.httpServer = httpServer
	s.mu.Unlock()

	// The shutdown goroutine must exit when EITHER ctx is canceled OR the
	// serve call returns early (e.g. port already in use). Without the served
	// channel, an early error leaks the goroutine for the lifetime of ctx.
	served := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		case <-served:
		}
	}()

	var err error
	if s.tlsCert != "" {
		err = httpServer.ListenAndServeTLS(s.tlsCert, s.tlsKey)
	} else {
		err = httpServer.ListenAndServe()
	}
	close(served)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close shuts down the server. Idempotent. Subsequent ListenAndServe calls
// after Close return http.ErrServerClosed.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		srv := s.httpServer
		s.mu.Unlock()
		if srv == nil {
			return
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		err = srv.Shutdown(shutdownCtx)
	})
	return err
}

// buildHandler constructs the HTTP handler. Routes are registered on the mux
// in later tasks; the returned handler wraps the mux in (outermost first)
// CORS → request-ID → logging → recovery → mux. Empty corsOrigin disables CORS.
//
// Order matters:
//   - request-ID is outside CORS-preflight short-circuit so OPTIONS responses
//     still carry X-Request-Id for correlation.
//   - logging wraps recovery so panics (converted to 500s by recovery) are
//     still recorded in the access log with the correct status.
//   - recovery is innermost so its statusRecorder-aware writer check (for
//     mid-stream panics) sees the same *statusRecorder that logging installed.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+s.basePath+"/status", s.handleStatus)
	mux.HandleFunc("GET "+s.basePath+"/models", s.handleModels)

	var handler http.Handler = mux
	handler = recoveryMiddleware(handler)
	handler = loggingMiddleware(handler)
	handler = requestIDMiddleware(handler)
	handler = corsMiddleware(handler, s.corsOrigin)
	return handler
}
