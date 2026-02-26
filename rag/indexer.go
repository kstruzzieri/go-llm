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

// IndexFile indexes a single file: reads, chunks, embeds, and stores it.
func (idx *Indexer) IndexFile(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("rag: read file %q: %w", path, err)
	}

	content := string(data)
	if content == "" {
		return nil
	}

	// Remove old chunks for this file before re-indexing
	if err := idx.store.DeleteBySource(ctx, path); err != nil {
		return fmt.Errorf("rag: delete old chunks for %q: %w", path, err)
	}

	chunks, err := idx.chunker.Chunk(path, content)
	if err != nil {
		return fmt.Errorf("rag: chunk %q: %w", path, err)
	}
	if len(chunks) == 0 {
		return nil
	}

	// Extract texts for batch embedding
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	embeddings, err := idx.client.EmbedBatch(ctx, idx.model, texts)
	if err != nil {
		return fmt.Errorf("rag: embed chunks for %q: %w", path, err)
	}

	if err := idx.store.Store(ctx, chunks, embeddings); err != nil {
		return fmt.Errorf("rag: store chunks for %q: %w", path, err)
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

// WithConcurrency sets the number of concurrent file indexing operations (default: 4).
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

	// Load .gitignore patterns if present
	ignorePatterns := loadGitignore(filepath.Join(dir, ".gitignore"))

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip files we can't read
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

		return idx.IndexFile(ctx, path)
	})
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
