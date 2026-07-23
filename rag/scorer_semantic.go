package rag

import "context"

// SemanticScorer computes cosine similarity between the query embedding
// and each chunk's embedding. Embeddings are provided via SetEmbeddings
// before calling ScoreBatch, allowing the caller (e.g. SearchMulti) to
// load embeddings once from the vector store and share them across scorers.
type SemanticScorer struct {
	embeddings map[string][]float64 // chunk ID -> embedding vector
}

// Compile-time interface check.
var _ SignalScorer = (*SemanticScorer)(nil)

// NewSemanticScorer creates a SemanticScorer with an empty embedding map.
func NewSemanticScorer() *SemanticScorer {
	return &SemanticScorer{embeddings: make(map[string][]float64)}
}

// SetEmbeddings provides chunk embeddings keyed by chunk ID. This must be
// called before ScoreBatch so the scorer has vectors to compare against.
func (s *SemanticScorer) SetEmbeddings(embeddings map[string][]float64) {
	s.embeddings = embeddings
}

// Name returns the scorer identifier.
func (s *SemanticScorer) Name() string { return "semantic" }

// ScoreBatch computes cosine similarity between queryEmbedding and each
// chunk's embedding (looked up by chunk ID in the pre-loaded embedding map).
// If a chunk's embedding is not found, its score is 0.
func (s *SemanticScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {

	scores := make([]float64, len(chunks))
	for i, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if emb, ok := s.embeddings[chunk.ID]; ok {
			scores[i] = cosineSimilarity(queryEmbedding, emb)
		}
	}
	return scores, nil
}
