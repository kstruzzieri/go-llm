package rag

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Indexer coordinates chunking, embedding, and storing documents.
type Indexer struct {
	client  *ollama.Client
	model   string
	store   VectorStore
	chunker Chunker
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

// WithConcurrency is reserved for future use. Indexing is currently sequential.
// This option is accepted but has no effect yet.
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
// It respects .gitignore patterns and can be cancelled via context.
func (idx *Indexer) IndexDirectory(ctx context.Context, dir string, opts ...IndexDirOption) error {
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

	// Load root .gitignore patterns if present.
	// NOTE: This is a simplified implementation that only reads the root .gitignore
	// and supports basic patterns (basename matching, directory suffixes).
	// Not supported: path-scoped rules, negation (!pattern), nested .gitignore
	// files, ** globs, or character classes. For full .gitignore compliance,
	// consider using a dedicated library.
	ignorePatterns := loadGitignore(filepath.Join(dir, ".gitignore"))

	var indexErrors []string

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			indexErrors = append(indexErrors, fmt.Sprintf("cannot access %q: %v", path, err))
			return nil // continue indexing other files
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip excluded directories
		if info.IsDir() {
			name := info.Name()
			for _, excl := range cfg.exclude {
				if name == excl {
					return filepath.SkipDir
				}
			}
			return nil
		}

		// Check file extension
		ext := strings.ToLower(filepath.Ext(path))
		if !cfg.extensions[ext] {
			return nil
		}

		// Check gitignore patterns
		relPath, _ := filepath.Rel(dir, path)
		if isIgnored(relPath, ignorePatterns) {
			return nil
		}

		if err := idx.IndexFile(ctx, path); err != nil {
			indexErrors = append(indexErrors, fmt.Sprintf("index %q: %v", path, err))
		}
		return nil
	})

	if walkErr != nil {
		return fmt.Errorf("rag: walk directory %q: %w", dir, walkErr)
	}
	if len(indexErrors) > 0 {
		return fmt.Errorf("rag: index directory %q completed with %d errors: %s",
			dir, len(indexErrors), strings.Join(indexErrors, "; "))
	}
	return nil
}

// loadGitignore reads patterns from a .gitignore file.
func loadGitignore(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// isIgnored checks if a relative path matches any gitignore patterns.
func isIgnored(relPath string, patterns []string) bool {
	for _, pattern := range patterns {
		// Simple pattern matching: supports basic gitignore patterns
		matched, _ := filepath.Match(pattern, filepath.Base(relPath))
		if matched {
			return true
		}
		// Check directory patterns
		if strings.HasSuffix(pattern, "/") {
			dir := strings.TrimSuffix(pattern, "/")
			if strings.Contains(relPath, dir+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
