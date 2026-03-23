package rag

import (
	"context"
	"database/sql"
	"fmt"
)

// KeywordScorer computes keyword relevance using FTS5 BM25 ranking.
// It requires a *sql.DB handle to query the chunks_fts virtual table,
// which must have been created via the v2 schema migration.
type KeywordScorer struct {
	db *sql.DB
}

// NewKeywordScorer creates a keyword scorer backed by the given database.
// The database must have the chunks_fts FTS5 virtual table (see the v2 schema migration).
func NewKeywordScorer(db *sql.DB) *KeywordScorer {
	return &KeywordScorer{db: db}
}

// Name returns "keyword".
func (s *KeywordScorer) Name() string { return "keyword" }

// ScoreBatch computes BM25-based keyword relevance for each chunk.
// Scores are normalized to the [0, 1] range using max-normalization.
//
// FTS5 MATCH queries can fail on malformed input (e.g., unbalanced quotes
// or special FTS5 syntax characters). In such cases, ScoreBatch returns
// zero scores rather than propagating the error, allowing other signals
// (like semantic similarity) to still contribute to ranking.
func (s *KeywordScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {
	if len(chunks) == 0 || query == "" {
		return make([]float64, len(chunks)), nil
	}

	// Query FTS5 for BM25 scores. bm25() returns negative values where
	// more negative = more relevant. We negate to get positive scores.
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, -bm25(chunks_fts) as score
		 FROM chunks_fts
		 JOIN chunks c ON c.rowid = chunks_fts.rowid
		 WHERE chunks_fts MATCH ?`, query)
	if err != nil {
		// FTS5 MATCH can fail on malformed queries (e.g., special chars).
		// Return zero scores rather than failing the entire search.
		return make([]float64, len(chunks)), nil
	}
	defer rows.Close()

	// Build map of chunk ID -> raw BM25 score.
	bm25Scores := make(map[string]float64)
	var maxScore float64
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("rag: scan BM25 score: %w", err)
		}
		bm25Scores[id] = score
		if score > maxScore {
			maxScore = score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate BM25 scores: %w", err)
	}

	// Normalize scores to [0, 1] range using max-normalization.
	scores := make([]float64, len(chunks))
	if maxScore > 0 {
		for i, chunk := range chunks {
			if raw, ok := bm25Scores[chunk.ID]; ok {
				scores[i] = raw / maxScore
			}
		}
	}

	return scores, nil
}
