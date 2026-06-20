// Package mcp exposes go-llm capabilities as a Model Context Protocol server.
// It wraps the ollama, rag, completion, and config packages behind MCP tools,
// prompts, and resources, allowing any MCP-compatible client to use them.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
	"github.com/kstruzzieri/go-llm/transcript"
)

const (
	defaultOllamaURL    = "http://localhost:11434"
	serverName          = "go-llm"
	serverVersion       = "0.1.0"
	ragContextMaxTokens = 4096
)

// Server wraps go-llm functionality as an MCP server.
// routeEngine is the MCP-local interface to provider.Router. Defining it here
// (rather than depending on the concrete *provider.Router) lets handler tests
// inject a fake without constructing a full provider/registry stack.
type routeEngine interface {
	Route(context.Context, provider.RoutingRequest) (*provider.RoutePlan, error)
	Close() error
	BreakerInfo(string) (provider.BreakerInfo, bool)
	WarmthSnapshot() []provider.WarmModel
	StickyRoutes() map[string]provider.StickyRouteInfo
}

type Server struct {
	ollamaURL         string
	ollamaURLExplicit bool
	configPath        string
	ragPath           string
	ragDisabled       bool
	transcriptDBPath  string
	transcriptStore   *transcript.Store
	fingerprintStore  fingerprint.Store
	tlsCert           string
	tlsKey            string

	client           *ollama.Client
	cfg              *config.Config
	store            rag.VectorStore
	indexer          *rag.Indexer
	retriever        *rag.Retriever
	completer        *completion.Provider
	modelRegistry    *provider.ModelRegistry
	providerRegistry *provider.Registry
	ollamaProv       provider.Provider // default "ollama"-format provider when warmth is wired; set for parity assertions, no production reader

	router       routeEngine
	warmthSource provider.WarmthSource

	ollamaAvailable bool
	closed          bool
	stateVersion    uint64

	fimPriorityCfg      provider.Priority
	fimPriorityExplicit bool

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

// WithTranscriptStore enables opt-in conversation persistence at the given
// SQLite path. Empty (the default) disables persistence. Every successful chat
// call is then recorded as a replayable benchmark trace. The DB is local and
// unredacted; it must not be committed or shared (redaction happens at capture
// export, not at persistence).
func WithTranscriptStore(path string) Option {
	return func(s *Server) {
		s.transcriptDBPath = path
	}
}

// WithFingerprintStore enables model fingerprint persistence and profiling
// through the provider-aware ModelRegistry.
func WithFingerprintStore(store fingerprint.Store) Option {
	return func(s *Server) {
		s.fingerprintStore = store
	}
}

// WithTLS sets the TLS certificate and key file paths for HTTPS.
func WithTLS(cert, key string) Option {
	return func(s *Server) {
		s.tlsCert = cert
		s.tlsKey = key
	}
}

// WithFIMPriority sets the routing priority used when MCP FIM completions
// are dispatched through provider.Router. When this option is not invoked,
// s.fimPriority() returns provider.PriorityHigh.
//
// Per-consumer guidance: IDE consumers (Firn IDE) treating keystroke latency
// as non-droppable may pass provider.PriorityCritical. Non-IDE MCP consumers
// can leave the default, drop to provider.PriorityNormal, or set
// provider.PriorityBackground for batch/background completion. Hard-coding
// either default would bake one product's policy into the seam.
func WithFIMPriority(p provider.Priority) Option {
	return func(s *Server) {
		s.fimPriorityCfg = p
		s.fimPriorityExplicit = true
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
		if cfgProvider := s.legacyOllamaProviderConfig(); cfgProvider != nil {
			if !s.ollamaURLExplicit && cfgProvider.BaseURL != "" {
				s.ollamaURL = cfgProvider.BaseURL
				clientOpts[0] = ollama.WithBaseURL(s.ollamaURL)
			}
			if cfgProvider.Timeout.Duration > 0 {
				clientOpts = append(clientOpts, ollama.WithTimeout(cfgProvider.Timeout.Duration))
			}
		}
	}
	s.client = ollama.NewClient(clientOpts...)

	// Step 3: Check Ollama availability (non-fatal, degraded mode on failure).
	s.ollamaAvailable = s.client.IsAvailable(ctx)

	// Step 3b: Build the provider-level model registry (and Router/warmth) even
	// in degraded mode so explicit completion requests can recover once Ollama
	// comes back. ensureModelRegistry now owns the full bootstrap, including the
	// warmth source and Router, so the old Step 4c block is gone.
	if err := s.ensureModelRegistry(ctx); err != nil {
		return nil, err
	}

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

	// Step 4b: open the optional transcript store (off unless WithTranscriptStore
	// is set). Failure is fatal: the caller explicitly asked to persist here.
	if s.transcriptDBPath != "" {
		cleanupTranscriptStartupFailure := func() {
			if s.store != nil {
				_ = s.store.Close()
				s.store = nil
			}
		}
		if dir := filepath.Dir(s.transcriptDBPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				cleanupTranscriptStartupFailure()
				return nil, fmt.Errorf("mcp: create transcript directory %q: %w", dir, err)
			}
		}
		ts, err := transcript.Open(ctx, s.transcriptDBPath)
		if err != nil {
			cleanupTranscriptStartupFailure()
			return nil, fmt.Errorf("mcp: open transcript store: %w", err)
		}
		s.transcriptStore = ts
	}

	// Step 5: Resolve models and rebuild derived clients (non-fatal).
	// Uses refreshResolved which stores partial results and calls rebuildDerivedClients.
	if s.cfg != nil {
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

func (s *Server) legacyOllamaProviderConfig() *config.ProviderConfig {
	if s.cfg == nil {
		return nil
	}
	if cfgProvider := s.cfg.Provider("ollama"); cfgProvider != nil && providerConfigIsOllama(*cfgProvider) {
		return cfgProvider
	}

	keys := make([]string, 0, len(s.cfg.Providers))
	for key, cfgProvider := range s.cfg.Providers {
		if key == "ollama" {
			continue
		}
		if providerConfigIsOllama(cfgProvider) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) != 1 {
		return nil
	}
	cfgProvider := s.cfg.Providers[keys[0]]
	return &cfgProvider
}

func providerConfigIsOllama(cfgProvider config.ProviderConfig) bool {
	return cfgProvider.APIFormat == "" || cfgProvider.APIFormat == "ollama"
}

// rebuildDerivedClients rebuilds the completer, indexer, and retriever from
// the currently resolved models. Network-backed completion construction runs
// outside the server lock so canceled callers do not block other handlers.
func (s *Server) rebuildDerivedClients(ctx context.Context) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return
	}
	resolved := make(map[string]config.ResolvedModel, len(s.resolved))
	for k, v := range s.resolved {
		resolved[k] = v
	}
	store := s.store
	stateVersion := s.stateVersion
	s.mu.RUnlock()

	// Rebuild completer from resolved "completion" model.
	var completer *completion.Provider
	if rm, ok := resolved["completion"]; ok && rm.Name != "" {
		if c, err := s.newCompletionProvider(ctx, rm.Name, rm.Provider); err == nil {
			completer = c
		}
	}

	// Rebuild indexer and retriever only when the embedding default resolved.
	// If resolution failed or defaults are unavailable, keep RAG clients nil so
	// callers do not discover the outage only on first use.
	embeddingModel := ""
	if rm, ok := resolved["embedding"]; ok && rm.Name != "" {
		embeddingModel = modelSelector(rm.Provider, rm.Name)
	}

	var indexer *rag.Indexer
	var retriever *rag.Retriever
	if store != nil && embeddingModel != "" {
		// Indexing is best-effort batch traffic — yield to user-facing routes.
		if idx, err := rag.NewIndexerWithEmbedder(
			s.ragEmbedder(provider.PriorityBackground),
			store,
			rag.WithEmbeddingModel(embeddingModel),
		); err == nil {
			indexer = idx
		}
		// Retrieval is in the user-facing latency path.
		if ret, err := rag.NewRetrieverWithEmbedder(
			s.ragEmbedder(provider.PriorityNormal),
			store,
			rag.WithRetrieverModel(embeddingModel),
		); err == nil {
			retriever = ret
		}
	}

	s.mu.Lock()
	if s.closed || s.stateVersion != stateVersion {
		s.mu.Unlock()
		return
	}
	s.completer = completer
	s.indexer = indexer
	s.retriever = retriever
	s.mu.Unlock()
}

// ensureModelRegistry lazily assembles the provider/model-registry/router stack
// via internal/providerbootstrap and installs it under the server lock. It is
// ctx-aware so bootstrap's best-effort RefreshModels honors cancellation. The
// install is coherent: on losing the init race, the freshly built bundle is
// discarded by closing it (which stops its warmth source) so fields are never
// spliced across two bundles.
func (s *Server) ensureModelRegistry(ctx context.Context) error {
	s.mu.RLock()
	if s.modelRegistry != nil && s.providerRegistry != nil {
		s.mu.RUnlock()
		return nil
	}
	s.mu.RUnlock()

	override := ""
	if s.ollamaURLExplicit {
		override = s.ollamaURL
	}
	routerOpts := []provider.RouterOption{}
	var warmthSource *provider.OllamaWarmthSource
	// Warmth source is wired only for the default "ollama" provider, matching the
	// prior NewServer Step 4c predicate (s.ollamaProv.Name()=="ollama").
	if providerConfigHasDefaultOllama(s.cfg) {
		warmthSource = provider.NewOllamaWarmthSource(s.ollamaURL, "ollama")
		routerOpts = append(routerOpts, provider.WithWarmthSource(warmthSource))
	}

	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:            s.cfg,
		FingerprintStore:  s.fingerprintStore,
		OllamaURLOverride: override,
		RouterOptions:     routerOpts,
	})
	if err != nil {
		return fmt.Errorf("mcp: model registry unavailable: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Lost the init race: another goroutine already installed a stack. Discard
	// ours coherently — close the unused router (which stops our warmth source)
	// — and keep theirs. Never splice fields across two bundles.
	if s.modelRegistry != nil && s.providerRegistry != nil {
		_ = bundle.Close()
		return nil
	}
	// Won: install the whole stack from THIS bundle.
	s.providerRegistry = bundle.Providers
	s.modelRegistry = bundle.Models
	s.router = bundle.Router
	// Only assign when present: storing a typed-nil *OllamaWarmthSource into the
	// provider.WarmthSource interface field would make s.warmthSource a non-nil
	// interface holding a nil pointer, breaking the "no warmth source" invariant
	// the prior Step 4c code preserved by assigning inside the predicate branch.
	// The same guard mirrors the configuredProviders invariant for ollamaProv:
	// it is the "ollama"-named provider ONLY when that provider is ollama-format
	// (an openai-compat provider named "ollama" leaves both fields nil).
	if warmthSource != nil {
		s.warmthSource = warmthSource
		s.ollamaProv = providerForName(bundle.Providers, "ollama")
	}
	// Surface best-effort bootstrap failures (a provider that failed to register
	// or refresh its model index) rather than silently dropping Bundle.Warnings.
	// Non-fatal: the registry/router were still built. Logged only for the bundle
	// we install (the lost-race bundle's warnings belong to a discarded stack).
	for _, w := range bundle.Warnings {
		log.Printf("mcp: model registry init: %v", w)
	}
	return nil
}

// providerConfigHasDefaultOllama reports whether the effective config exposes an
// "ollama"-named provider speaking the ollama api_format. It reproduces the
// prior NewServer Step 4c predicate (s.ollamaProv != nil &&
// s.ollamaProv.Name() == "ollama"): a nil config synthesizes the default ollama
// provider, and a non-nil config qualifies only when its "ollama" entry is
// ollama-format (an openai-compat provider named "ollama" must NOT match).
func providerConfigHasDefaultOllama(cfg *config.Config) bool {
	if cfg == nil {
		return true
	}
	pCfg, ok := cfg.Providers["ollama"]
	if !ok {
		return false
	}
	return providerConfigIsOllama(pCfg)
}

// providerForName returns the registered provider whose Name() == name, or nil
// if no such provider exists or the registry is nil.
func providerForName(reg *provider.Registry, name string) provider.Provider {
	if reg == nil {
		return nil
	}
	p, ok := reg.Get(name)
	if !ok {
		return nil
	}
	return p
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
func (s *Server) newCompletionProvider(ctx context.Context, model, providerName string) (*completion.Provider, error) {
	if err := s.ensureModelRegistry(ctx); err != nil {
		return nil, err
	}

	s.mu.RLock()
	modelRegistry := s.modelRegistry
	providerRegistry := s.providerRegistry
	s.mu.RUnlock()
	if modelRegistry == nil || providerRegistry == nil {
		return nil, fmt.Errorf("mcp: model registry unavailable")
	}

	key, err := s.modelKeyForCompletion(ctx, model, providerName)
	if err != nil {
		return nil, err
	}
	profile, err := modelRegistry.Lookup(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("mcp: lookup profile %q: %w", model, err)
	}
	cfg, err := completion.ProviderConfigFromProfile(profile)
	if err != nil {
		return nil, err
	}

	// modelRegistry.Lookup proved this model exists for its provider. Seed
	// the providerRegistry's routing index so Router can
	// dispatch to it even when the bulk /api/tags-based RefreshModels at
	// startup failed (partial Ollama outage). Idempotent; failure here is
	// non-fatal — Router's own ProvidersForModel error will surface
	// naturally if the seed didn't take.
	if pReg := s.providerRegistrySnapshot(); pReg != nil {
		_ = pReg.AddModelToIndex(key.Model, key.Provider)
	}

	return completion.NewProviderWithGenerator(s.fimGenerator(s.fimPriority()), key.String(), cfg)
}

func (s *Server) modelKeyForCompletion(ctx context.Context, model, providerName string) (provider.ModelKey, error) {
	if providerName != "" {
		return provider.ModelKey{Provider: providerName, Model: model}, nil
	}
	if key, ok := s.parseKnownModelSelector(model); ok {
		return key, nil
	}

	inferredProvider, err := s.inferProviderForExplicitModel(ctx, model)
	if err != nil {
		return provider.ModelKey{}, err
	}
	if inferredProvider != "" {
		return provider.ModelKey{Provider: inferredProvider, Model: model}, nil
	}
	return provider.ModelKey{}, fmt.Errorf("mcp: provider required for model %q", model)
}

func (s *Server) refreshProviderModelIndexes(ctx context.Context) {
	pReg := s.providerRegistrySnapshot()
	if pReg == nil {
		return
	}
	for _, name := range pReg.Names() {
		_ = pReg.RefreshModels(ctx, name)
	}
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

func (s *Server) routerSnapshot() routeEngine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.router
}

func (s *Server) transcriptStoreSnapshot() *transcript.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.transcriptStore
}

// fimPriority returns the configured FIM routing priority, defaulting to
// provider.PriorityHigh when WithFIMPriority was not invoked.
//
// The fimPriorityExplicit boolean is required because
// provider.PriorityBackground is the zero value of provider.Priority and is
// itself a valid configured value; "did the caller invoke WithFIMPriority?"
// cannot be answered by checking fimPriorityCfg == 0 alone.
//
// Called by newCompletionProvider when wiring completion construction
// through s.fimGenerator.
func (s *Server) fimPriority() provider.Priority {
	if !s.fimPriorityExplicit {
		return provider.PriorityHigh
	}
	return s.fimPriorityCfg
}

func (s *Server) providerRegistrySnapshot() *provider.Registry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providerRegistry
}

// BreakerInfo returns the circuit breaker state for the named provider.
// Returns false if no breaker exists yet or the router is unavailable.
func (s *Server) BreakerInfo(name string) (provider.BreakerInfo, bool) {
	router := s.routerSnapshot()
	if router == nil {
		return provider.BreakerInfo{}, false
	}
	return router.BreakerInfo(name)
}

// WarmthSnapshot returns all currently warm models, or nil if no warmth
// source is configured.
func (s *Server) WarmthSnapshot() []provider.WarmModel {
	router := s.routerSnapshot()
	if router == nil {
		return nil
	}
	return router.WarmthSnapshot()
}

// StickyRoutes returns a snapshot of all active sticky routing entries.
// Will be empty in steady state for chain-routed requests (sticky is
// suppressed at the Router boundary when PreferredChain is set).
func (s *Server) StickyRoutes() map[string]provider.StickyRouteInfo {
	router := s.routerSnapshot()
	if router == nil {
		return nil
	}
	return router.StickyRoutes()
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
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.stateVersion++
	store := s.store
	router := s.router
	tstore := s.transcriptStore
	s.store = nil
	s.indexer = nil
	s.retriever = nil
	s.completer = nil
	s.router = nil
	s.warmthSource = nil
	s.transcriptStore = nil
	s.mu.Unlock()

	var routerErr, storeErr error
	if router != nil {
		routerErr = router.Close()
	}
	if store != nil {
		storeErr = store.Close()
	}
	var transcriptErr error
	if tstore != nil {
		transcriptErr = tstore.Close()
	}
	return errors.Join(routerErr, storeErr, transcriptErr)
}
