package rag

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// ChunkWithEmbedding pairs a stored chunk with its embedding vector.
// Used by GetBySource to return chunk data needed for incremental comparison.
type ChunkWithEmbedding struct {
	Chunk     Chunk
	Embedding []float64
	// VectorSpaceID is empty for legacy rows and for embeddings written before
	// the writer could identify the vector space; those cases are intentionally
	// indistinguishable after reading from SQLite's DEFAULT '' column.
	VectorSpaceID string
}

// sourceChunkLoader is an optional store capability for loading existing
// chunks with embeddings by source path. Required for incremental indexing.
// Implementations should return chunks ordered by start_line.
type sourceChunkLoader interface {
	GetBySource(ctx context.Context, source string) ([]ChunkWithEmbedding, error)
}

// sourceHashChecker is an optional store capability for fast file-level
// change detection. Returns the stored source index signature for a
// source, or empty string if not found.
type sourceHashChecker interface {
	GetSourceHash(ctx context.Context, source string) (string, error)
}

// chunkerSignatureProvider lets built-in chunkers describe the configuration
// that affects chunk boundaries. Configuration changes must invalidate the
// source signature even when the concrete chunker type stays the same.
type chunkerSignatureProvider interface {
	sourceSignature() string
}

// sourceSignatureVersion identifies the persisted source signature format.
// Bump this when the index inputs change in a way that requires full re-embed.
const sourceSignatureVersion = 2

// sourceSignature captures the inputs that determine whether persisted
// embeddings can be safely reused for a source file.
type sourceSignature struct {
	Version          int    `json:"version"`
	ContentHash      string `json:"content_hash"`
	EmbeddingModel   string `json:"embedding_model"`
	Chunker          string `json:"chunker"`
	StableKeyVersion string `json:"stable_key_version"`
}

func (s sourceSignature) String() string {
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	return string(data)
}

func (s sourceSignature) compatibleWith(other sourceSignature) bool {
	return s.Version == other.Version &&
		s.EmbeddingModel == other.EmbeddingModel &&
		s.Chunker == other.Chunker &&
		s.StableKeyVersion == other.StableKeyVersion
}

func parseSourceSignature(raw string) (sourceSignature, bool) {
	var sig sourceSignature
	if err := json.Unmarshal([]byte(raw), &sig); err != nil {
		return sourceSignature{}, false
	}
	if sig.Version == 0 || sig.ContentHash == "" {
		return sourceSignature{}, false
	}
	return sig, true
}

func chunkerSignature(chunker Chunker) string {
	if sig, ok := chunker.(chunkerSignatureProvider); ok {
		return sig.sourceSignature()
	}
	return fmt.Sprintf("%T", chunker)
}

func (idx *Indexer) currentSourceSignature(content string) sourceSignature {
	return sourceSignature{
		Version:          sourceSignatureVersion,
		ContentHash:      contentHash(content),
		EmbeddingModel:   idx.model,
		Chunker:          chunkerSignature(idx.chunker),
		StableKeyVersion: stableKeyVersion,
	}
}

func (idx *Indexer) requiresFullReembed(stored string, current sourceSignature) bool {
	storedSig, ok := parseSourceSignature(stored)
	if !ok {
		// Legacy rows only stored a raw content hash, so the embedding model
		// and chunker identity are unknown. Re-embed once to upgrade safely.
		return true
	}
	return !storedSig.compatibleWith(current)
}

// contentHash computes a SHA-256 hex digest of the given content.
// Used to detect chunk-level and file-level content changes.
func contentHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", h)
}
