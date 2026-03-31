package prefetch

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/rag"
)

// resourceConfig holds tuning parameters derived from a ResourceProfile.
type resourceConfig struct {
	cacheCapacity int
	concurrency   int
	triggerPolicy string // "every_open", "debounced", "disabled"
}

// configForResources maps a ResourceProfile to cache sizing and concurrency limits.
func configForResources(r fingerprint.ResourceProfile) resourceConfig {
	switch {
	case r.TotalMemoryMB >= 65536: // 64GB+
		return resourceConfig{
			cacheCapacity: 500,
			concurrency:   4,
			triggerPolicy: "every_open",
		}
	case r.TotalMemoryMB >= 16384: // 16GB+
		return resourceConfig{
			cacheCapacity: 100,
			concurrency:   1,
			triggerPolicy: "debounced",
		}
	default:
		return resourceConfig{
			cacheCapacity: 0,
			concurrency:   0,
			triggerPolicy: "disabled",
		}
	}
}

// EngineOption configures an Engine.
type EngineOption func(*Engine)

// WithDebounce sets the debounce interval for the Watch loop.
// Default is 2 seconds.
func WithDebounce(d time.Duration) EngineOption {
	return func(e *Engine) {
		e.debounce = d
	}
}

// WithCacheTTL sets the time-to-live for warm cache entries.
// Default is 5 minutes.
func WithCacheTTL(d time.Duration) EngineOption {
	return func(e *Engine) {
		e.cacheTTL = d
	}
}

// WithResources overrides the resource profile used for cache sizing.
func WithResources(r fingerprint.ResourceProfile) EngineOption {
	return func(e *Engine) {
		e.resources = r
	}
}

// Engine is a prefetch-aware retriever that predictively warms a cache
// with contextually relevant chunks based on IDE state. User-initiated
// Retrieve calls check the warm cache first and fall back to the cold
// retriever on a miss.
type Engine struct {
	retriever Retriever
	store     rag.VectorStore
	cache     *WarmCache
	state     StateProvider
	debounce  time.Duration
	cacheTTL  time.Duration
	resources fingerprint.ResourceProfile
	config    resourceConfig

	// prefetchCancel cancels any in-flight prefetch operation so that
	// user-initiated retrieval always has priority.
	prefetchMu     sync.Mutex
	prefetchCancel context.CancelFunc
}

// NewEngine creates a prefetch Engine backed by the given cold retriever,
// vector store, and state provider. Resource-aware configuration is applied
// from the provided ResourceProfile (or override via WithResources).
func NewEngine(retriever Retriever, store rag.VectorStore, state StateProvider, opts ...EngineOption) *Engine {
	e := &Engine{
		retriever: retriever,
		store:     store,
		state:     state,
		debounce:  2 * time.Second,
		cacheTTL:  5 * time.Minute,
	}

	for _, opt := range opts {
		opt(e)
	}

	// Auto-detect resources when no override was provided.
	if e.resources.TotalMemoryMB == 0 && e.resources.CPUCores == 0 {
		if detected, err := fingerprint.Detect(); err == nil {
			e.resources = detected
		}
	}

	e.config = configForResources(e.resources)

	if e.config.cacheCapacity > 0 {
		e.cache = NewWarmCache(e.config.cacheCapacity, e.cacheTTL)
	}

	return e
}

// Retrieve checks the warm cache first. On a cache miss (or if SkipCache is
// set), it delegates to the underlying cold retriever. Any in-flight prefetch
// operation is cancelled to give priority to user-initiated retrieval.
func (e *Engine) Retrieve(ctx context.Context, query string, k int,
	qCtx rag.QueryContext, opts RetrieveOptions) (*RetrieveResult, error) {

	// Cancel any in-flight prefetch so the cold retriever is not contended.
	e.cancelPrefetch()

	cacheKey := buildCacheKey(query, k, qCtx)

	if !opts.SkipCache && e.cache != nil {
		if results, ok := e.cache.Get(cacheKey); ok {
			// Trim to requested k if cache has more.
			if k > 0 && len(results) > k {
				results = results[:k]
			}
			return &RetrieveResult{
				Chunks:   results,
				CacheHit: true,
			}, nil
		}
	}

	result, err := e.retriever.Retrieve(ctx, query, k, qCtx, RetrieveOptions{SkipCache: true})
	if err != nil {
		return nil, fmt.Errorf("prefetch: cold retrieve: %w", err)
	}

	// Warm the cache with this result for future hits.
	if e.cache != nil {
		e.cache.Put(cacheKey, result.Chunks)
	}

	return result, nil
}

// Watch is a blocking loop that polls the StateProvider at the configured
// debounce interval and prefetches contextually relevant chunks. It returns
// when ctx is cancelled.
//
// Watch is a no-op if the resource profile disables prefetching.
func (e *Engine) Watch(ctx context.Context) error {
	if e.config.triggerPolicy == "disabled" {
		<-ctx.Done()
		return ctx.Err()
	}

	var lastActiveFile string
	var lastOpenFiles []string

	ticker := time.NewTicker(e.debounce)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			activeFile := e.state.ActiveFile()
			openFiles := e.state.OpenFiles()

			if !stateChanged(activeFile, openFiles, lastActiveFile, lastOpenFiles) {
				continue
			}

			lastActiveFile = activeFile
			lastOpenFiles = append([]string(nil), openFiles...)

			e.prefetchForState(ctx, activeFile, openFiles)
		}
	}
}

// prefetchForState identifies related files and warms the cache.
func (e *Engine) prefetchForState(parentCtx context.Context, activeFile string, openFiles []string) {
	if activeFile == "" {
		return
	}

	// Create a cancellable context for this prefetch round.
	ctx, cancel := context.WithCancel(parentCtx)

	e.prefetchMu.Lock()
	if e.prefetchCancel != nil {
		e.prefetchCancel()
	}
	e.prefetchCancel = cancel
	e.prefetchMu.Unlock()

	// Build prefetch queries from related file heuristics.
	queries := e.buildPrefetchQueries(ctx, activeFile, openFiles)

	for _, q := range queries {
		if ctx.Err() != nil {
			break
		}
		qCtx := rag.QueryContext{
			CurrentFile: activeFile,
			OpenFiles:   openFiles,
			Timestamp:   time.Now(),
		}
		const prefetchK = 10
		result, err := e.retriever.Retrieve(ctx, q, prefetchK, qCtx, RetrieveOptions{})
		if err != nil {
			// Prefetch errors are non-fatal; we just skip this query.
			continue
		}
		if e.cache != nil {
			e.cache.Put(buildCacheKey(q, prefetchK, qCtx), result.Chunks)
		}
	}
}

// cancelPrefetch stops any in-flight prefetch operation.
func (e *Engine) cancelPrefetch() {
	e.prefetchMu.Lock()
	if e.prefetchCancel != nil {
		e.prefetchCancel()
		e.prefetchCancel = nil
	}
	e.prefetchMu.Unlock()
}

// buildPrefetchQueries generates query strings from heuristics about
// related files. It uses path-based heuristics (same directory, test file
// pairs, common prefix) and recent edit events.
func (e *Engine) buildPrefetchQueries(ctx context.Context, activeFile string, openFiles []string) []string {
	seen := make(map[string]bool)
	var queries []string

	addQuery := func(q string) {
		if q != "" && !seen[q] {
			seen[q] = true
			queries = append(queries, q)
		}
	}

	// 1. Active file itself.
	addQuery(activeFile)

	// 2. Test file pair.
	addQuery(testFilePair(activeFile))

	// 3. Files in same directory.
	dir := filepath.Dir(activeFile)
	sources := e.indexedSourcesByPattern(ctx, dir+"/*")
	for _, s := range sources {
		addQuery(s)
	}

	// 4. Recently co-edited files (within 30-second window of each other).
	recentEdits := e.state.RecentEdits()
	coEdited := coEditedFiles(recentEdits, 30*time.Second)
	for _, f := range coEdited {
		addQuery(f)
	}

	// 5. Files sharing a common source path prefix with active file.
	prefix := commonPathPrefix(activeFile)
	if prefix != "" {
		prefixSources := e.indexedSourcesByPattern(ctx, prefix+"*")
		for _, s := range prefixSources {
			addQuery(s)
		}
	}

	// Limit the number of prefetch queries to avoid excessive work.
	const maxQueries = 20
	if len(queries) > maxQueries {
		queries = queries[:maxQueries]
	}

	return queries
}

// indexedSourcesByPattern uses the Exportable interface (if available) to
// enumerate indexed sources matching a glob pattern. Returns nil if the
// store does not implement Exportable.
func (e *Engine) indexedSourcesByPattern(ctx context.Context, pattern string) []string {
	exp, ok := e.store.(rag.Exportable)
	if !ok {
		return nil
	}

	iter, err := exp.ExportChunks(ctx, &rag.ExportFilter{SourcePattern: pattern})
	if err != nil {
		return nil
	}

	seen := make(map[string]bool)
	var sources []string
	for chunk, err := range iter {
		if err != nil {
			break
		}
		if !seen[chunk.Chunk.Source] {
			seen[chunk.Chunk.Source] = true
			sources = append(sources, chunk.Chunk.Source)
		}
		// Limit to avoid scanning the entire store.
		if len(sources) >= 50 {
			break
		}
	}
	return sources
}

// testFilePair returns the test file counterpart of the given path.
// For "foo.go" it returns "foo_test.go" and vice versa.
// Returns empty string for non-Go files or if no pair exists.
func testFilePair(path string) string {
	if path == "" {
		return ""
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)

	if strings.HasSuffix(base, "_test") {
		// This is a test file; return the implementation file.
		return strings.TrimSuffix(base, "_test") + ext
	}
	// This is an implementation file; return the test file.
	return base + "_test" + ext
}

// coEditedFiles returns unique file paths from edit events that occurred
// within the given time window of each other.
func coEditedFiles(edits []EditEvent, window time.Duration) []string {
	if len(edits) < 2 {
		return nil
	}

	seen := make(map[string]bool)
	var files []string

	for i := 0; i < len(edits); i++ {
		for j := i + 1; j < len(edits); j++ {
			diff := edits[i].Timestamp.Sub(edits[j].Timestamp)
			if diff < 0 {
				diff = -diff
			}
			if diff <= window && edits[i].Source != edits[j].Source {
				if !seen[edits[i].Source] {
					seen[edits[i].Source] = true
					files = append(files, edits[i].Source)
				}
				if !seen[edits[j].Source] {
					seen[edits[j].Source] = true
					files = append(files, edits[j].Source)
				}
			}
		}
	}
	return files
}

// commonPathPrefix returns the parent directory of the file's directory.
// This enables discovering sibling packages or related directories.
// Returns empty string if the path has insufficient depth.
func commonPathPrefix(path string) string {
	dir := filepath.Dir(path)
	parent := filepath.Dir(dir)
	if parent == "." || parent == "/" || parent == dir {
		return ""
	}
	return parent + "/"
}

// buildCacheKey constructs a composite cache key that includes the query
// context dimensions affecting multi-signal scoring, so that different editor
// contexts do not share cached results.
func buildCacheKey(query string, k int, qCtx rag.QueryContext) string {
	return fmt.Sprintf("%s|k=%d|file=%s", query, k, qCtx.CurrentFile)
}

// stateChanged returns true if the active file or open files set has changed.
func stateChanged(activeFile string, openFiles []string, lastActive string, lastOpen []string) bool {
	if activeFile != lastActive {
		return true
	}
	if len(openFiles) != len(lastOpen) {
		return true
	}
	for i := range openFiles {
		if openFiles[i] != lastOpen[i] {
			return true
		}
	}
	return false
}
