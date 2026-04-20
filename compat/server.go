package compat

import (
	"net/http"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

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
	// written once at server start and is read-only thereafter, so it does not
	// require mu.
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}
