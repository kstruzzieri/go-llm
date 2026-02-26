package rag

import "context"

// VectorStore persists and searches embeddings.
type VectorStore interface {
	// Store saves chunks with their embeddings.
	Store(ctx context.Context, chunks []Chunk, embeddings [][]float64) error

	// Search finds the top-k most similar chunks to the query embedding.
	Search(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error)

	// DeleteBySource removes chunks by source path (for re-indexing).
	DeleteBySource(ctx context.Context, source string) error

	// Stats returns index statistics.
	Stats(ctx context.Context) (StoreStats, error)

	// Close releases store resources.
	Close() error
}

// SearchResult holds a chunk and its similarity score.
type SearchResult struct {
	Chunk    Chunk
	Score    float64 // cosine similarity, 0-1
	Distance float64 // 1 - score
}

// StoreStats holds vector store statistics.
type StoreStats struct {
	TotalChunks  int
	TotalSources int
	EmbeddingDim int
	StorageBytes int64
}
