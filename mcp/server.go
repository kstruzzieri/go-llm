// Package mcp exposes go-llm capabilities as a Model Context Protocol server.
// It wraps the ollama, rag, completion, and config packages behind MCP tools,
// prompts, and resources, allowing any MCP-compatible client to use them.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	defaultOllamaURL    = "http://localhost:11434"
	serverName          = "go-llm"
	serverVersion       = "0.1.0"
	ragContextMaxTokens = 4096
)

// Server wraps go-llm functionality as an MCP server.
type Server struct {
	ollamaURL         string
	ollamaURLExplicit bool
	configPath        string
	ragPath           string
	ragDisabled       bool
	tlsCert           string
	tlsKey            string

	client        *ollama.Client
	cfg           *config.Config
	store         rag.VectorStore
	indexer       *rag.Indexer
	retriever     *rag.Retriever
	completer     *completion.Provider
	modelRegistry *provider.ModelRegistry
	ollamaProv    provider.Provider

	ollamaAvailable bool

	mu       sync.RWMutex
	resolved map[string]config.ResolvedModel

	httpServer *http.Server
	mcpServer  *gomcp.Server
}

// Option configures a Server.
type Option func(*Server)

// WithOllamaURL sets the Ollama server URL (default: http://localhost:11434).
func WithOllamaURL(url string) Option {
	return func(s *Server) {
		s.ollamaURL = url
		s.ollamaURLExplicit = true
	}
}

// WithConfig sets the path to models.json.
// If empty, config.Default() auto-discovers the configuration.
func WithConfig(path string) Option {
	return func(s *Server) {
		s.configPath = path
	}
}

// WithRAGPath sets the path to the RAG SQLite database.
// Defaults to ~/.local/share/go-llm/rag.db.
func WithRAGPath(path string) Option {
	return func(s *Server) {
		s.ragPath = path
	}
}

// WithRAGDisabled disables RAG functionality entirely.
func WithRAGDisabled() Option {
	return func(s *Server) {
		s.ragDisabled = true
	}
}

// WithTLS sets the TLS certificate and key file paths for HTTPS.
func WithTLS(cert, key string) Option {
	return func(s *Server) {
		s.tlsCert = cert
		s.tlsKey = key
	}
}

// defaultRAGPath returns the default path for the RAG database.
// It expands to $HOME/.local/share/go-llm/rag.db.
// Falls back to "rag.db" in the working directory if $HOME is unavailable.
func defaultRAGPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "rag.db"
	}
	return filepath.Join(home, ".local", "share", "go-llm", "rag.db")
}

// NewServer creates and initializes an MCP server with the given options.
// Failures during initialization are non-fatal: the server starts in
// degraded mode when Ollama is unavailable or configuration is missing.
func NewServer(ctx context.Context, opts ...Option) (*Server, error) {
	s := &Server{
		ollamaURL: defaultOllamaURL,
		ragPath:   defaultRAGPath(),
		resolved:  make(map[string]config.ResolvedModel),
	}
	for _, opt := range opts {
		opt(s)
	}

	// Step 1: Load configuration.
	// Explicit config path: hard error if it fails (user asked for this file).
	// Auto-discovery: non-fatal if missing.
	if s.configPath != "" {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			return nil, fmt.Errorf("mcp: load config %q: %w", s.configPath, err)
		}
		s.cfg = cfg
	} else {
		cfg, err := config.Default()
		if err != nil {
			if !errors.Is(err, config.ErrConfigNotFound) {
				return nil, fmt.Errorf("mcp: auto-discover config: %w", err)
			}
		} else {
			s.cfg = cfg
		}
	}

	// Step 2: Build the Ollama client, honoring provider settings from config
	// unless the caller explicitly overrode the base URL.
	clientOpts := []ollama.Option{ollama.WithBaseURL(s.ollamaURL)}
	if s.cfg != nil {
		if provider := s.cfg.Provider("ollama"); provider != nil {
			if !s.ollamaURLExplicit && provider.BaseURL != "" {
				s.ollamaURL = provider.BaseURL
				clientOpts[0] = ollama.WithBaseURL(s.ollamaURL)
			}
			if provider.Timeout.Duration > 0 {
				clientOpts = append(clientOpts, ollama.WithTimeout(provider.Timeout.Duration))
			}
		}
	}
	s.client = ollama.NewClient(clientOpts...)

	// Step 3: Check Ollama availability (non-fatal, degraded mode on failure).
	s.ollamaAvailable = s.client.IsAvailable(ctx)

	// Step 3b: Build the provider-level model registry even in degraded mode
	// so explicit completion requests can recover once Ollama comes back.
	_ = s.ensureModelRegistry()

	// Step 4: Open RAG store if not disabled (before model resolution
	// so that rebuildDerivedClients can wire up indexer/retriever).
	if !s.ragDisabled && s.ragPath != "" {
		parentDir := filepath.Dir(s.ragPath)
		if err := os.MkdirAll(parentDir, 0o755); err != nil {
			return nil, fmt.Errorf("mcp: create RAG directory %q: %w", parentDir, err)
		}
		store, err := rag.NewSQLiteStore(s.ragPath)
		if err != nil {
			return nil, fmt.Errorf("mcp: open RAG store: %w", err)
		}
		s.store = store
	}

	// Step 5: Resolve models and rebuild derived clients (non-fatal).
	// Uses refreshResolved which stores partial results and calls rebuildDerivedClients.
	if s.cfg != nil && s.ollamaAvailable {
		_ = s.refreshResolved(ctx) // non-fatal: partial results are kept
	} else {
		// No resolution possible — still build derived clients with defaults.
		s.rebuildDerivedClients(ctx)
	}

	// Step 6: Create MCP SDK server.
	s.mcpServer = gomcp.NewServer(&gomcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	// Step 7: Register tools, prompts, and resources.
	s.registerChatTools()
	s.registerGenerateTools()
	s.registerCompletionTools()
	s.registerEmbedTools()
	s.registerRAGTools()
	s.registerModelTools()
	s.registerAnalysisTools()
	s.registerPrompts()
	s.registerResources()

	return s, nil
}

// rebuildDerivedClients rebuilds the completer, indexer, and retriever from
// the currently resolved models. Network-backed completion construction runs
// outside the server lock so canceled callers do not block other handlers.
func (s *Server) rebuildDerivedClients(ctx context.Context) {
	s.mu.RLock()
	resolved := make(map[string]config.ResolvedModel, len(s.resolved))
	for k, v := range s.resolved {
		resolved[k] = v
	}
	store := s.store
	s.mu.RUnlock()

	// Rebuild completer from resolved "completion" model.
	var completer *completion.Provider
	if rm, ok := resolved["completion"]; ok && rm.Name != "" {
		if c, err := s.newCompletionProvider(ctx, rm.Name); err == nil {
			completer = c
		}
	}

	// Rebuild indexer and retriever only when the embedding default resolved.
	// If resolution failed or defaults are unavailable, keep RAG clients nil so
	// callers do not discover the outage only on first use.
	embeddingModel := ""
	if rm, ok := resolved["embedding"]; ok && rm.Name != "" {
		embeddingModel = rm.Name
	}

	var indexer *rag.Indexer
	var retriever *rag.Retriever
	if store != nil && embeddingModel != "" {
		indexer = rag.NewIndexer(s.client, store, rag.WithEmbeddingModel(embeddingModel))
		retriever = rag.NewRetriever(s.client, store, rag.WithRetrieverModel(embeddingModel))
	}

	s.mu.Lock()
	s.completer = completer
	s.indexer = indexer
	s.retriever = retriever
	s.mu.Unlock()
}

func (s *Server) ensureModelRegistry() error {
	s.mu.RLock()
	if s.modelRegistry != nil && s.ollamaProv != nil {
		s.mu.RUnlock()
		return nil
	}
	client := s.client
	s.mu.RUnlock()

	if client == nil {
		return fmt.Errorf("mcp: model registry unavailable")
	}

	ollamaProv := provider.NewOllamaProvider(client)
	pReg := provider.NewRegistry()
	if err := pReg.Register(ollamaProv); err != nil {
		return fmt.Errorf("mcp: model registry unavailable: %w", err)
	}
	mr, err := provider.NewModelRegistry(pReg, nil)
	if err != nil {
		return fmt.Errorf("mcp: model registry unavailable: %w", err)
	}

	s.mu.Lock()
	if s.ollamaProv == nil {
		s.ollamaProv = ollamaProv
	}
	if s.modelRegistry == nil {
		s.modelRegistry = mr
	}
	s.mu.Unlock()
	return nil
}

// Completer returns the current FIM completion provider (may be nil).
// Safe for concurrent use.
func (s *Server) Completer() *completion.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.completer
}

// newCompletionProvider builds a completion.Provider for the given model by
// looking up its merged ModelProfile via the model registry and deriving a
// ProviderConfig. Returns an error when the registry is unavailable, the
// lookup fails, or the model does not support native FIM at runtime.
func (s *Server) newCompletionProvider(ctx context.Context, model string) (*completion.Provider, error) {
	if err := s.ensureModelRegistry(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	modelRegistry := s.modelRegistry
	ollamaProv := s.ollamaProv
	s.mu.RUnlock()
	if modelRegistry == nil || ollamaProv == nil {
		return nil, fmt.Errorf("mcp: model registry unavailable")
	}

	key := provider.ModelKey{Provider: ollamaProv.Name(), Model: model}
	profile, err := modelRegistry.Lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("mcp: lookup profile %q: %w", model, err)
	}
	cfg, err := completion.ProviderConfigFromProfile(profile)
	if err != nil {
		return nil, err
	}
	return completion.NewProvider(s.client, model, cfg)
}

// Indexer returns the current RAG indexer (nil if RAG disabled).
// Safe for concurrent use.
func (s *Server) Indexer() *rag.Indexer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.indexer
}

// Retriever returns the current RAG retriever (nil if RAG disabled).
// Safe for concurrent use.
func (s *Server) Retriever() *rag.Retriever {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.retriever
}

// Shutdown gracefully stops the server: shuts down the HTTP server (if running),
// then releases all resources (RAG store, derived clients).
// The ctx deadline bounds how long the HTTP server waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	hs := s.httpServer
	s.mu.RUnlock()

	var httpErr error
	if hs != nil {
		httpErr = hs.Shutdown(ctx)
		if errors.Is(httpErr, http.ErrServerClosed) {
			httpErr = nil
		}
	}

	closeErr := s.Close()
	return errors.Join(httpErr, closeErr)
}

// Close releases all resources held by the server.
// Safe to call multiple times; serialized with handler reads via s.mu.
func (s *Server) Close() error {
	s.mu.Lock()
	store := s.store
	s.store = nil
	s.indexer = nil
	s.retriever = nil
	s.completer = nil
	s.mu.Unlock()

	if store != nil {
		return store.Close()
	}
	return nil
}
