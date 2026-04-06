package rag

import (
	"context"
	"crypto/sha256"
	"fmt"
)

// ChunkWithEmbedding pairs a stored chunk with its embedding vector.
// Used by GetBySource to return chunk data needed for incremental comparison.
type ChunkWithEmbedding struct {
	Chunk     Chunk
	Embedding []float64
}

// sourceChunkLoader is an optional store capability for loading existing
// chunks with embeddings by source path. Required for incremental indexing.
// Implementations should return chunks ordered by start_line.
type sourceChunkLoader interface {
	GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error)
}

// sourceHashChecker is an optional store capability for fast file-level
// change detection. Returns the stored content hash for a source, or
// empty string if not found.
type sourceHashChecker interface {
	GetSourceHash(ctx context.Context, source string) (string, error)
}

// contentHash computes a SHA-256 hex digest of the given content.
// Used to detect chunk-level and file-level content changes.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}
