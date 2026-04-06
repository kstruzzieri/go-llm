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
	client        *ollama.Client
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

// IndexerOption configures an Indexer.
type IndexerOption func(*Indexer)

// WithEmbeddingModel sets the embedding model name (default: "nomic-embed-text").
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

// NewIndexer creates an indexer that coordinates chunking, embedding, and storing.
func NewIndexer(client *ollama.Client, store VectorStore, opts ...IndexerOption) *Indexer {
	idx := &Indexer{
		client:  client,
		model:   "nomic-embed-text",
		store:   store,
		chunker: NewCodeChunker(),
	}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

func (idx *Indexer) replaceSource(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64) error {
	idx.storeMu.Lock()
	defer idx.storeMu.Unlock()

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

	embeddings, err := idx.client.EmbedBatch(ctx, idx.model, texts)
	if err != nil {
		// Embedding failed — preserve existing indexed data for this file
		return fmt.Errorf("rag: embed chunks for %q: %w", path, err)
	}

	// Step 3: Replace old chunks with new ones.
	if err := idx.replaceSource(ctx, path, chunks, embeddings); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}

	return nil
}

// IndexDirOption configures IndexDirectory behavior.
type IndexDirOption func(*indexDirConfig)

type indexDirConfig struct {
	extensions  map[string]bool
	exclude     []string
	concurrency int
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

// IndexStatus reports the progress of an indexing operation.
type IndexStatus struct {
	TotalFiles   int
	IndexedFiles int
	SkippedFiles int
	Errors       []string
	InProgress   bool
}

// IndexDirectory indexes all supported files in a directory tree.
// Files are processed concurrently (default: 4 workers).
// It respects .gitignore patterns and can be cancelled via context.
// The cleaned absolute path of dir is used as the workspace root for
// StableKey computation during this indexing run.
func (idx *Indexer) IndexDirectory(ctx context.Context, dir string, opts ...IndexDirOption) error {
	// Set workspace root for StableKey computation.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("rag: resolve directory %q: %w", dir, err)
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

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walkErrors = append(walkErrors, fmt.Sprintf("cannot access %q: %v", path, err))
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
		return fmt.Errorf("rag: walk directory %q: %w", dir, walkErr)
	}

	// Phase 2: Index files concurrently.
	var (
		mu          sync.Mutex
		indexErrors = append([]string{}, walkErrors...)
	)

	var g errgroup.Group
	g.SetLimit(cfg.concurrency)

	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := idx.IndexFile(ctx, path); err != nil {
				mu.Lock()
				indexErrors = append(indexErrors, fmt.Sprintf("index %q: %v", path, err))
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	if len(indexErrors) > 0 {
		return fmt.Errorf("rag: index directory %q completed with %d errors: %s",
			dir, len(indexErrors), strings.Join(indexErrors, "; "))
	}
	if ctx.Err() != nil {
		return fmt.Errorf("rag: index directory %q: %w", dir, ctx.Err())
	}
	return nil
}
