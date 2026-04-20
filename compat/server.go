package compat

import (
	"net/http"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Server is the OpenAI-compatible HTTP façade over a provider.Router.
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

	// mu guards closed. closeOnce enforces single-shot Close semantics;
	// startedAt is written once at server start and is read-only thereafter,
	// so it does not require mu.
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}
