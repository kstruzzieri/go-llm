package rag

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// TemporalScorer scores chunks by recency using the indexed_at timestamp.
// Recently indexed chunks score higher using exponential decay:
//
//	score = 2^(-age / halfLife)
//
// where age is the elapsed time in seconds since the chunk was indexed.
// The half-life controls how quickly the recency signal decays.
type TemporalScorer struct {
	db       *sql.DB
	halfLife float64 // decay half-life in seconds
}

// NewTemporalScorer creates a temporal scorer backed by the given database.
// halfLifeSeconds controls how quickly the recency signal decays.
// Use 0 for the default (7 days = 604800 seconds).
// The database must have the indexed_at column (see the v2 schema migration).
func NewTemporalScorer(db *sql.DB, halfLifeSeconds float64) *TemporalScorer {
	if halfLifeSeconds <= 0 {
		halfLifeSeconds = 604800 // 7 days
	}
	return &TemporalScorer{db: db, halfLife: halfLifeSeconds}
}

// Name returns "temporal".
func (s *TemporalScorer) Name() string { return "temporal" }

// ScoreBatch computes exponential-decay recency scores for each chunk.
// The reference time is taken from qCtx.Timestamp if set, otherwise
// the maximum indexed_at value across all chunks is used.
func (s *TemporalScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {
	if len(chunks) == 0 {
		return nil, nil
	}

	// Get indexed_at timestamps for all chunks.
	rows, err := s.db.QueryContext(ctx, `SELECT id, indexed_at FROM chunks`)
	if err != nil {
		return nil, fmt.Errorf("rag: query indexed_at: %w", err)
	}
	defer rows.Close()

	timestamps := make(map[string]int64)
	for rows.Next() {
		var id string
		var ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			return nil, fmt.Errorf("rag: scan indexed_at: %w", err)
		}
		timestamps[id] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate indexed_at: %w", err)
	}

	// Use query timestamp as "now" if provided, otherwise use max indexed_at.
	var now int64
	if !qCtx.Timestamp.IsZero() {
		now = qCtx.Timestamp.Unix()
	}
	if now <= 0 {
		// Find max timestamp as reference.
		for _, ts := range timestamps {
			if ts > now {
				now = ts
			}
		}
	}

	// Compute exponential decay scores: score = 2^(-age/halfLife)
	scores := make([]float64, len(chunks))
	for i, chunk := range chunks {
		if ts, ok := timestamps[chunk.ID]; ok && ts > 0 {
			age := float64(now - ts)
			if age <= 0 {
				scores[i] = 1.0
			} else {
				scores[i] = math.Pow(2, -age/s.halfLife)
			}
		}
	}

	return scores, nil
}
