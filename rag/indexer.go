package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/ollama"
	"golang.org/x/sync/errgroup"
)

// Indexer coordinates chunking, embedding, and storing documents.
// Store writes are serialized internally via mutex, so multiple goroutines
// may call IndexFile concurrently provided the injected Chunker and
// VectorStore implementations are themselves safe for concurrent use.
type Indexer struct {
	embedder      Embedder
	model         string
	store         VectorStore
	chunker       Chunker
	storeMu       sync.Mutex
	workspaceRoot string // canonicalized absolute path; used for StableKey computation
}

// atomicSourceReplacer is an optional store capability for transactional
// source replacement during re-indexing.
type atomicSourceReplacer interface {
	ReplaceSource(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64) error
}

// atomicSourceReplacerWithHash extends atomicSourceReplacer to include
// the source content hash for incremental indexing change detection.
type atomicSourceReplacerWithHash interface {
	ReplaceSourceWithHash(ctx context.Context, source string, chunks []Chunk, embeddings [][]float64, sourceHash string) error
}

// atomicSourceReplacerWithVectorSpaceID extends atomicSourceReplacerWithHash
// with vector-space identifier persistence. Stores that implement the
// capability support drift detection at retrieval time; stores that only
// implement atomicSourceReplacerWithHash silently drop the vsid. Implementors
// must reject non-empty chunk batches with an empty vectorSpaceID.
type atomicSourceReplacerWithVectorSpaceID interface {
	ReplaceSourceWithHashAndVectorSpaceID(
		ctx context.Context,
		source string,
		chunks []Chunk,
		embeddings [][]float64,
		sourceHash string,
		vectorSpaceID string,
	) error
}

// atomicSourceReplacerWithExpectedHashAndVectorSpaceID is an internal-only
// SQLite-backed capability that extends the vsid-aware replacement path with a
// transactional source-hash compare-and-swap for incremental indexing.
type atomicSourceReplacerWithExpectedHashAndVectorSpaceID interface {
	ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(
		ctx context.Context,
		source string,
		chunks []Chunk,
		embeddings [][]float64,
		sourceHash string,
		vectorSpaceID string,
		expectedSourceHash string,
	) error
}

// IndexerOption configures an Indexer.
type IndexerOption func(*Indexer)

// WithEmbeddingModel sets the embedding model name (default: "nomic-embed-text").
//
// An empty model defers selection to the Embedder. Router-backed embedders
// that allow chain fallback (the MCP server's ragEmbedder does not) can
// substitute embedding models, corrupting the SQLite vector store by mixing
// incompatible vector spaces. Only pass an empty string when the Embedder
// is known not to fall back across embedding-model boundaries.
func WithEmbeddingModel(model string) IndexerOption {
	return func(idx *Indexer) {
		idx.model = model
	}
}

// WithChunker sets the chunker to use for splitting documents.
func WithChunker(c Chunker) IndexerOption {
	return func(idx *Indexer) {
		idx.chunker = c
	}
}

// WithWorkspaceRoot sets the workspace root directory used for StableKey
// computation. The path is canonicalized with filepath.Abs + filepath.Clean.
// This is required when using IndexFile directly; IndexDirectory sets it
// automatically from the directory argument.
func WithWorkspaceRoot(root string) IndexerOption {
	return func(idx *Indexer) {
		abs, err := filepath.Abs(root)
		if err == nil {
			idx.workspaceRoot = filepath.Clean(abs)
		}
	}
}

// buildIndexer is the single private path constructing an Indexer; both the
// legacy ollama-backed shim NewIndexer and the new NewIndexerWithEmbedder
// route through it so option-application order and defaults match exactly.
func buildIndexer(emb Embedder, store VectorStore, opts ...IndexerOption) *Indexer {
	idx := &Indexer{
		embedder: emb,
		model:    "nomic-embed-text",
		store:    store,
		chunker:  NewCodeChunker(),
	}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// NewIndexer is the *ollama.Client-backed compat shim. Existing consumers
// (Firn IDE, Flux ML, Quantum Trader) continue to use this constructor
// unchanged; new code should prefer NewIndexerWithEmbedder.
//
// Nil-passthrough is preserved: passing a nil client yields an Indexer
// whose embedder is nil and which will nil-deref on first embed call,
// matching the pre-Embedder behaviour.
func NewIndexer(client *ollama.Client, store VectorStore, opts ...IndexerOption) *Indexer {
	return buildIndexer(embedderFromOllamaClient(client), store, opts...)
}

// NewIndexerWithEmbedder is the router-aware constructor. The supplied
// Embedder mediates all embedding calls, so callers can route through
// provider.Router (circuit breakers, warmth, token-budget validation) by
// supplying a Router-backed Embedder.
//
// Returns an error if embedder is nil — distinct from NewIndexer's
// no-validation contract, since callers picking the new shape are
// expected to supply a real Embedder.
func NewIndexerWithEmbedder(embedder Embedder, store VectorStore, opts ...IndexerOption) (*Indexer, error) {
	if embedder == nil {
		return nil, fmt.Errorf("rag: NewIndexerWithEmbedder: embedder is required")
	}
	if isNilVectorStore(store) {
		return nil, fmt.Errorf("rag: NewIndexerWithEmbedder: store is required")
	}
	return buildIndexer(embedder, store, opts...), nil
}

func (idx *Indexer) replaceSource(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64) error {
	return idx.replaceSourceWithProvenance(ctx, path, chunks, embeddings, "", "")
}

func (idx *Indexer) replaceSourceWithHash(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, sourceHash string) error {
	return idx.replaceSourceWithProvenance(ctx, path, chunks, embeddings, sourceHash, "")
}

// replaceSourceWithProvenance serializes source replacement writes and
// dispatches capability-descending: a store that supports vector-space-id
// persistence gets the vsid; a store that only supports the source hash gets
// the hash with vsid silently dropped; a store that only supports atomic
// replacement gets neither; legacy stores fall through to the non-atomic
// delete+store fallback. VSID write invariants are enforced by the
// vsid-capable store implementation, not by this dispatcher.
func (idx *Indexer) replaceSourceWithProvenance(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID string) error {
	idx.storeMu.Lock()
	defer idx.storeMu.Unlock()

	return idx.replaceSourceWithProvenanceLocked(ctx, path, chunks, embeddings, sourceHash, vectorSpaceID)
}

func (idx *Indexer) replaceSourceWithProvenanceIfSourceHash(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID, expectedSourceHash string) error {
	idx.storeMu.Lock()
	defer idx.storeMu.Unlock()

	if replacer, ok := idx.store.(atomicSourceReplacerWithExpectedHashAndVectorSpaceID); ok {
		return replacer.ReplaceSourceWithHashAndVectorSpaceIDIfSourceHash(ctx, path, chunks, embeddings, sourceHash, vectorSpaceID, expectedSourceHash)
	}

	return fmt.Errorf("%w: source-hash CAS unsupported by %T", ErrIncrementalRebuildRequired, idx.store)
}

func (idx *Indexer) replaceSourceWithProvenanceLocked(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID string) error {
	if replacer, ok := idx.store.(atomicSourceReplacerWithVectorSpaceID); ok {
		return replacer.ReplaceSourceWithHashAndVectorSpaceID(ctx, path, chunks, embeddings, sourceHash, vectorSpaceID)
	}

	if replacer, ok := idx.store.(atomicSourceReplacerWithHash); ok {
		return replacer.ReplaceSourceWithHash(ctx, path, chunks, embeddings, sourceHash)
	}

	if replacer, ok := idx.store.(atomicSourceReplacer); ok {
		return replacer.ReplaceSource(ctx, path, chunks, embeddings)
	}

	// Best-effort fallback for custom stores that do not implement atomic replace.
	// This preserves backwards compatibility with existing VectorStore
	// implementations but is not transactional across delete + store.
	if err := idx.store.DeleteBySource(ctx, path); err != nil {
		return fmt.Errorf("delete old chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	if err := idx.store.Store(ctx, chunks, embeddings); err != nil {
		return fmt.Errorf("store chunks: %w", err)
	}
	return nil
}

// IndexFile indexes a single file: reads, chunks, embeds, and stores it.
// Existing data is preserved if chunking or embedding fails.
// If the underlying store supports atomic source replacement (SQLiteStore does),
// re-indexing is transactional across delete + store.
func (idx *Indexer) IndexFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("rag: read file %q: %w", path, err)
	}

	content := string(data)
	if content == "" {
		if err := idx.replaceSource(ctx, path, nil, nil); err != nil {
			return fmt.Errorf("rag: clear chunks for empty file %q: %w", path, err)
		}
		return nil
	}

	// Step 1: Chunk the file
	chunks, err := idx.chunker.Chunk(path, content)
	if err != nil {
		return fmt.Errorf("rag: chunk %q: %w", path, err)
	}
	if len(chunks) == 0 {
		if err := idx.replaceSource(ctx, path, nil, nil); err != nil {
			return fmt.Errorf("rag: clear chunks for %q with no chunk output: %w", path, err)
		}
		return nil
	}

	// Step 1.5: Compute stable keys if workspace root is set.
	if idx.workspaceRoot != "" {
		for i := range chunks {
			key, err := ComputeStableKey(chunks[i], idx.workspaceRoot)
			if err != nil {
				// Non-fatal: leave StableKey empty rather than failing the entire index.
				continue
			}
			chunks[i].StableKey = key
		}
	}

	// Step 2: Generate embeddings (most likely to fail — network/model errors)
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	res, err := idx.embedder.Embed(ctx, idx.model, texts)
	if err != nil {
		// Embedding failed — preserve existing indexed data for this file
		return fmt.Errorf("%w: embed chunks for %q: %w", ErrEmbedderFailed, path, err)
	}
	if len(res.Embeddings) != len(texts) {
		return fmt.Errorf("%w: embed chunks for %q: got %d for %d chunks", ErrEmbeddingCountMismatch, path, len(res.Embeddings), len(texts))
	}
	embeddings := res.Embeddings

	// Step 3: Replace old chunks with new ones. Store the source signature so
	// subsequent IndexFileIncremental calls can safely use the fast path.
	// Persist the resolved vector-space identity so retrieval-time drift
	// detection can fail closed across cross-run embedding-model changes.
	vsid := resolveVectorSpaceID(res)
	sourceHash := idx.currentSourceSignature(content).String()
	if err := idx.replaceSourceWithProvenance(ctx, path, chunks, embeddings, sourceHash, vsid); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}

	return nil
}

// resolveVectorSpaceID picks the vsid the indexer will persist for a batch.
// It prefers the embedder's explicit VectorSpaceID, falls back to
// Provider/Model synthesis when only those two are populated, and returns
// empty when neither path yields a value. Capability-aware dispatch in
// replaceSourceWithProvenance is responsible for failing closed on empty
// vsid against a vsid-capable store (see spec §5.6 / Test 4a).
func resolveVectorSpaceID(res EmbedResult) string {
	if res.VectorSpaceID != "" {
		return res.VectorSpaceID
	}
	if res.Provider != "" && res.Model != "" {
		return res.Provider + "/" + res.Model
	}
	return ""
}

// IndexDirOption configures IndexDirectory behavior.
type IndexDirOption func(*indexDirConfig)

type indexDirConfig struct {
	extensions   map[string]bool
	exclude      []string
	concurrency  int
	incremental  bool
	pruneDeleted bool
}

// WithExtensions sets which file extensions to index (default: .go, .py, .ts, .tsx, .js, .md).
func WithExtensions(exts ...string) IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.extensions = make(map[string]bool)
		for _, ext := range exts {
			if !strings.HasPrefix(ext, ".") {
				ext = "." + ext
			}
			cfg.extensions[ext] = true
		}
	}
}

// WithExclude sets directory patterns to exclude (default: node_modules, .git, dist, vendor).
func WithExclude(patterns ...string) IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.exclude = patterns
	}
}

// WithConcurrency sets the maximum number of files to index in parallel.
// Defaults to 4. Values less than 1 are clamped to 1.
func WithConcurrency(n int) IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.concurrency = n
	}
}

// WithIncremental enables incremental indexing for IndexDirectory.
// When enabled, each file uses IndexFileIncremental which diffs against
// stored chunks and only embeds changed content. Files indexed for the
// first time or whose store lacks incremental support transparently
// fall back to full indexing.
func WithIncremental() IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.incremental = true
	}
}

// WithPruneDeleted enables deleted-source pruning for IndexDirectory.
// After the walk and file indexing complete, stored sources under the
// workspace root that are absent from the eligible file set (deleted,
// newly excluded, or newly gitignored) are removed via DeleteBySource.
// Sources outside the workspace root are never touched. Requires a store
// that supports ListSources (SQLiteStore does); otherwise pruning is
// silently skipped.
func WithPruneDeleted() IndexDirOption {
	return func(cfg *indexDirConfig) {
		cfg.pruneDeleted = true
	}
}

// IndexStatus reports the progress of an indexing operation.
type IndexStatus struct {
	TotalFiles   int
	IndexedFiles int
	SkippedFiles int
	Errors       []string
	InProgress   bool
}

// IndexDirectory indexes all supported files in a directory tree, returning
// only an aggregate error. It delegates to IndexDirectoryWithStatus; existing
// consumers keep the error-only contract.
func (idx *Indexer) IndexDirectory(ctx context.Context, dir string, opts ...IndexDirOption) error {
	_, err := idx.IndexDirectoryWithStatus(ctx, dir, opts...)
	return err
}

// IndexDirectoryWithStatus indexes all supported files in a directory tree and
// reports an IndexStatus snapshot. Files are processed concurrently. It respects
// WithExclude, .gitignore (root + nested), and the configured extension set, and
// can be cancelled via context. The cleaned absolute path of dir is used as the
// workspace root for StableKey computation during this run.
//
// Status semantics (spec §7.1):
//   - TotalFiles:   eligible queued files after extension/exclude/.gitignore filtering.
//   - IndexedFiles: files whose indexing attempt completed without error
//     (including incremental unchanged no-ops).
//   - SkippedFiles: eligible files not attempted because ctx was already
//     cancelled before they could be queued/started.
//   - Errors:       per-file indexing errors, non-fatal walk/.gitignore read
//     errors, and prune errors when WithPruneDeleted is enabled.
//   - InProgress:   always false in the returned final snapshot.
func (idx *Indexer) IndexDirectoryWithStatus(ctx context.Context, dir string, opts ...IndexDirOption) (IndexStatus, error) {
	var status IndexStatus

	// Set workspace root for StableKey computation.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return status, fmt.Errorf("rag: resolve directory %q: %w", dir, err)
	}
	idx.workspaceRoot = filepath.Clean(absDir)

	cfg := &indexDirConfig{
		extensions: map[string]bool{
			".go": true, ".py": true, ".ts": true, ".tsx": true,
			".js": true, ".md": true,
		},
		exclude:     []string{"node_modules", ".git", "dist", "vendor", "__pycache__"},
		concurrency: 4,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}

	// Load .gitignore patterns. Nested .gitignore files are loaded during walk.
	// Read errors are collected as walk errors (best-effort), matching the
	// behavior for nested .gitignore files.
	ignore := newGitignoreMatcher()
	var walkErrors []string
	if err := ignore.addFromFile(filepath.Join(dir, ".gitignore"), "."); err != nil {
		walkErrors = append(walkErrors, fmt.Sprintf("read root .gitignore in %q: %v", dir, err))
	}

	// Phase 1: Walk and collect eligible file paths.
	var files []string
	// walkIncomplete records non-fatal access errors: an unreadable subtree
	// silently drops out of the eligible set, so pruning against it would
	// wrongly delete its stored sources. Missed .gitignore reads are excluded
	// on purpose — they only grow the eligible set, which is prune-safe.
	var walkIncomplete bool

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walkErrors = append(walkErrors, fmt.Sprintf("cannot access %q: %v", path, err))
			walkIncomplete = true
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if info.IsDir() {
			name := info.Name()
			// First-pass: WithExclude filter (cannot be overridden by .gitignore)
			for _, excl := range cfg.exclude {
				if name == excl {
					return filepath.SkipDir
				}
			}

			// Second-pass: gitignore check
			// filepath.Rel cannot fail here: both paths originate from the same filepath.Walk root.
			relDir, _ := filepath.Rel(dir, path)
			relDir = filepath.ToSlash(relDir)
			if relDir != "." && ignore.isIgnored(relDir, true) {
				return filepath.SkipDir
			}

			// Load nested .gitignore if present (skip root — already loaded pre-walk)
			if relDir != "." {
				nestedGitignore := filepath.Join(path, ".gitignore")
				if err := ignore.addFromFile(nestedGitignore, relDir); err != nil {
					walkErrors = append(walkErrors, fmt.Sprintf("read .gitignore in %q: %v", path, err))
				}
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !cfg.extensions[ext] {
			return nil
		}

		// Gitignore check (after cheap extension filter)
		// filepath.Rel cannot fail here: both paths originate from the same filepath.Walk root.
		relPath, _ := filepath.Rel(dir, path)
		relPath = filepath.ToSlash(relPath)
		if ignore.isIgnored(relPath, false) {
			return nil
		}

		files = append(files, path)
		return nil
	})

	if walkErr != nil {
		return status, fmt.Errorf("rag: walk directory %q: %w", dir, walkErr)
	}

	status.TotalFiles = len(files)

	// Phase 2: Index files concurrently.
	var (
		mu          sync.Mutex
		indexErrors = append([]string{}, walkErrors...)
		indexed     int
		skipped     int
	)

	var g errgroup.Group
	g.SetLimit(cfg.concurrency)

	for _, path := range files {
		if ctx.Err() != nil {
			skipped++
			continue
		}
		g.Go(func() error {
			var indexErr error
			if cfg.incremental {
				indexErr = idx.IndexFileIncremental(ctx, path)
			} else {
				indexErr = idx.IndexFile(ctx, path)
			}
			mu.Lock()
			if indexErr != nil {
				indexErrors = append(indexErrors, fmt.Sprintf("index %q: %v", path, indexErr))
			} else {
				indexed++
			}
			mu.Unlock()
			return nil
		})
	}
	_ = g.Wait()

	// Phase 3: Prune stored sources no longer in the eligible set. Requires a
	// COMPLETE eligible set: a fatal walk error returns above, and non-fatal
	// access errors set walkIncomplete, which disables pruning for this run
	// (failure mode is "not pruned", never wrong deletion). Per-file index
	// errors are fine: eligible-but-failed files stay in the keep set.
	if cfg.pruneDeleted && ctx.Err() == nil {
		if walkIncomplete {
			indexErrors = append(indexErrors, "prune skipped: directory walk was incomplete")
		} else {
			indexErrors = append(indexErrors, idx.pruneDeletedSources(ctx, files)...)
		}
	}

	status.IndexedFiles = indexed
	status.SkippedFiles = skipped
	status.Errors = indexErrors

	if ctx.Err() != nil {
		return status, fmt.Errorf("rag: index directory %q: %w", dir, ctx.Err())
	}
	if len(indexErrors) > 0 {
		return status, fmt.Errorf("rag: index directory %q completed with %d errors: %s",
			dir, len(indexErrors), strings.Join(indexErrors, "; "))
	}
	return status, nil
}

// sourceLister is implemented by stores that can enumerate stored source
// paths (SQLiteStore does). Pruning is silently skipped for stores without it.
type sourceLister interface {
	ListSources(ctx context.Context) ([]string, error)
}

// pruneDeletedSources deletes stored sources under the workspace root that
// are not in the eligible file set from the current walk. Sources outside
// the workspace root (or that fail to resolve to an absolute path) are
// ignored defensively. Failures are returned as error strings in the same
// per-item format IndexDirectoryWithStatus collects; a ListSources failure
// yields a single entry and skips pruning entirely.
func (idx *Indexer) pruneDeletedSources(ctx context.Context, files []string) []string {
	lister, ok := idx.store.(sourceLister)
	if !ok {
		return nil
	}

	keep := make(map[string]bool, len(files))
	for _, f := range files {
		if abs, err := filepath.Abs(f); err == nil {
			keep[abs] = true
		}
	}

	sources, err := lister.ListSources(ctx)
	if err != nil {
		return []string{fmt.Sprintf("list sources for prune: %v", err)}
	}

	rootPrefix := idx.workspaceRoot + string(filepath.Separator)
	var errs []string
	for _, src := range sources {
		abs, err := filepath.Abs(src)
		if err != nil || !strings.HasPrefix(abs, rootPrefix) {
			continue
		}
		if keep[abs] {
			continue
		}
		if err := idx.store.DeleteBySource(ctx, src); err != nil {
			errs = append(errs, fmt.Sprintf("prune %q: %v", src, err))
		}
	}
	return errs
}
