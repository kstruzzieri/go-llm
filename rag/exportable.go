package rag

import (
	"context"
	"iter"
)

// ExportedChunk bundles a chunk with its embedding for export iteration.
type ExportedChunk struct {
	Chunk     Chunk
	Embedding []float64
}

// ExportFilter specifies which chunks to include in an export.
// Zero-value fields mean no filtering on that dimension.
//
// SourcePattern is a glob-style pattern for matching source paths.
// Pattern semantics are implementation-defined per backend:
//   - SQLiteStore uses SQLite GLOB (* matches any chars, ? matches one char)
//   - Other backends may use filepath.Match, regex, or equivalent
//
// Patterns match against the chunk's Source field, which contains paths
// as stored by the indexer. Use patterns appropriate for the backend.
type ExportFilter struct {
	SourcePattern string // glob-style pattern for source path matching (semantics are backend-defined)
	Language      string // exact match (e.g., "go")
}

// Exportable is implemented by vector stores that support bulk export.
// The returned iterator streams chunks one at a time for bounded memory usage.
// Resource cleanup (closing database cursors) happens automatically when
// the consumer stops iterating, via Go 1.23+ range-over-function semantics.
type Exportable interface {
	ExportChunks(ctx context.Context, filter *ExportFilter) (iter.Seq2[ExportedChunk, error], error)
}
