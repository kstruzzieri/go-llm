// Package mcp exposes go-llm capabilities as a Model Context Protocol server.
// It wraps the ollama, rag, completion, and config packages behind MCP tools,
// prompts, and resources, allowing any MCP-compatible client to use them.
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	serverName       = "go-llm"
	serverVersion    = "0.1.0"
)

// Server wraps go-llm functionality as an MCP server.
type Server struct {
	ollamaURL  string
	configPath string
	ragPath    string
	ragDisabled bool
	tlsCert    string
	tlsKey     string

	client    *ollama.Client
	cfg       *config.Config
	store     rag.VectorStore
	indexer   *rag.Indexer
	retriever *rag.Retriever
	completer *completion.Provider

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

	// Step 1: Create Ollama client.
	s.client = ollama.NewClient(ollama.WithBaseURL(s.ollamaURL))

	// Step 2: Load configuration.
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
			// No config found — continue without config.
			_ = err
		} else {
			s.cfg = cfg
		}
	}

	// Step 3: Check Ollama availability (non-fatal — degraded mode).
	s.ollamaAvailable = s.client.IsAvailable(ctx)

	// Step 4: Resolve models if config and Ollama are available (non-fatal).
	if s.cfg != nil && s.ollamaAvailable {
		resolved, err := s.cfg.ResolveAll(ctx, s.client)
		if err != nil {
			// Partial results may be available even on error.
			_ = err
		}
		if resolved != nil {
			s.mu.Lock()
			s.resolved = resolved
			s.mu.Unlock()
		}
	}

	// Step 5: Open RAG store if not disabled.
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

	// Step 6: Build derived clients (completer, indexer, retriever).
	s.rebuildDerivedClients()

	// Step 7: Create MCP SDK server.
	s.mcpServer = gomcp.NewServer(&gomcp.Implementation{
		Name:    serverName,
		Version: serverVersion,
	}, nil)

	// Step 8: Register tools, prompts, and resources (stubs for now).
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

// rebuildDerivedClients rebuilds the completer, indexer, and retriever
// from the currently resolved models.
func (s *Server) rebuildDerivedClients() {
	s.mu.RLock()
	resolved := s.resolved
	s.mu.RUnlock()

	// Rebuild completer from resolved "completion" model.
	if rm, ok := resolved["completion"]; ok && rm.Name != "" {
		s.completer = completion.NewProvider(s.client, rm.Name)
	}

	// Rebuild indexer and retriever from resolved "embedding" model.
	embeddingModel := ""
	if rm, ok := resolved["embedding"]; ok && rm.Name != "" {
		embeddingModel = rm.Name
	} else if s.cfg != nil {
		embeddingModel = s.cfg.ModelFor("embedding")
	}

	if s.store != nil {
		if embeddingModel != "" {
			s.indexer = rag.NewIndexer(s.client, s.store, rag.WithEmbeddingModel(embeddingModel))
			s.retriever = rag.NewRetriever(s.client, s.store, rag.WithRetrieverModel(embeddingModel))
		} else {
			// Use package defaults (nomic-embed-text).
			s.indexer = rag.NewIndexer(s.client, s.store)
			s.retriever = rag.NewRetriever(s.client, s.store)
		}
	}
}

// Close releases all resources held by the server.
func (s *Server) Close() error {
	if s.store != nil {
		return s.store.Close()
	}
	return nil
}

// Stub registration methods — implementations will be added in later tasks.

func (s *Server) registerChatTools()       {}
func (s *Server) registerGenerateTools()   {}
func (s *Server) registerCompletionTools() {}
func (s *Server) registerEmbedTools()      {}
func (s *Server) registerRAGTools()        {}
func (s *Server) registerModelTools()      {}
func (s *Server) registerAnalysisTools()   {}
func (s *Server) registerPrompts()         {}
func (s *Server) registerResources()       {}
