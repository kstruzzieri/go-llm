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
	mu         sync.Mutex
	closed     bool
	closeOnce  sync.Once
}
