package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

// Compile-time interface check: SQLiteStore implements MultiSignalSearcher.
var _ MultiSignalSearcher = (*SQLiteStore)(nil)

// Default RRF constant. Higher values reduce the influence of rank position.
const rrfK = 60

// Default bonus weights for non-ranked signals (conservative starting values).
const (
	defaultTemporalWeight   = 0.1
	defaultStructuralWeight = 0.1
)

// SearchMulti performs multi-signal retrieval combining semantic similarity,
// keyword matching (FTS5 BM25), temporal recency, and structural proximity.
//
// Semantic and keyword signals are fused via Reciprocal Rank Fusion (RRF).
// Temporal and structural signals are added as weighted bonuses.
//
// This method loads all chunks with embeddings from the database, scores them
// across all available signals, and returns the top-k results with per-signal
// score breakdowns.
func (s *SQLiteStore) SearchMulti(ctx context.Context, queryEmbedding []float64, query string,
	k int, qCtx QueryContext) ([]ScoredResult, error) {

	// Load all chunks with embeddings.
	chunks, embeddings, err := s.loadChunksWithEmbeddings(ctx)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}

	// Build embedding map for the semantic scorer.
	stopValidation := s.stageTimer("vector_dimension_validation")
	embMap := make(map[string][]float64, len(chunks))
	for i, chunk := range chunks {
		if err := validateSearchEmbeddingDimension(chunk.ID, queryEmbedding, embeddings[i]); err != nil {
			return nil, err
		}
		embMap[chunk.ID] = embeddings[i]
	}
	stopValidation()

	// Create and run scorers.
	semantic := NewSemanticScorer()
	semantic.SetEmbeddings(embMap)

	keyword := NewKeywordScorer(s.db)
	temporal := NewTemporalScorer(s.db, 0) // default 7-day half-life
	structural := NewStructuralScorer()

	stopSemantic := s.stageTimer("semantic_scoring")
	semanticScores, err := semantic.ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: semantic scoring: %w", err)
	}
	stopSemantic()

	stopKeyword := s.stageTimer("keyword_scoring")
	keywordScores, err := keyword.ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: keyword scoring: %w", err)
	}
	stopKeyword()

	stopTemporal := s.stageTimer("temporal_scoring")
	temporalScores, err := temporal.ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: temporal scoring: %w", err)
	}
	stopTemporal()

	stopStructural := s.stageTimer("structural_scoring")
	structuralScores, err := structural.ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: structural scoring: %w", err)
	}
	stopStructural()

	// Optional behavioral signal as a third RRF list. It is folded only when a
	// weighter is configured AND at least one chunk has a non-zero weight, so
	// cold-start (all-zero) leaves the raw fused score byte-for-byte unchanged.
	// Scored before the fusion window opens so the fusion stage below is one
	// contiguous timing span.
	var behavioralScores []float64
	var behavioralRanks []int
	foldBehavioral := false
	if s.behavioral != nil {
		stopBehavioral := s.stageTimer("behavioral_scoring")
		behavioralScores, err = NewBehavioralScorer(s.behavioral).
			ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
		if err != nil {
			return nil, fmt.Errorf("rag: behavioral scoring: %w", err)
		}
		if anyNonZero(behavioralScores) {
			behavioralRanks = computeRanks(behavioralScores)
			foldBehavioral = true
		}
		stopBehavioral()
	}

	// Compute RRF scores from the semantic, keyword, and (when folded)
	// behavioral ranked lists.
	stopFusion := s.stageTimer("fusion_ranking_top_k")
	semanticRanks := computeRanks(semanticScores)
	keywordRanks := computeRanks(keywordScores)

	// Build final scored results.
	results := make([]ScoredResult, len(chunks))
	for i, chunk := range chunks {
		rrfScore := rrfContribution(semanticRanks[i]) + rrfContribution(keywordRanks[i])
		if foldBehavioral {
			rrfScore += rrfContribution(behavioralRanks[i])
		}

		// Add weighted bonuses from non-ranked signals.
		var temporalBonus, structuralBonus float64
		if temporalScores != nil && i < len(temporalScores) {
			temporalBonus = defaultTemporalWeight * temporalScores[i]
		}
		if i < len(structuralScores) {
			structuralBonus = defaultStructuralWeight * structuralScores[i]
		}

		finalScore := rrfScore + temporalBonus + structuralBonus

		signals := map[string]float64{
			"semantic":   semanticScores[i],
			"keyword":    keywordScores[i],
			"temporal":   safeIndex(temporalScores, i),
			"structural": structuralScores[i],
		}
		// Expose behavioral only when a weighter is configured (0 during
		// cold-start, raw weighted score otherwise). Absent when nil.
		if s.behavioral != nil {
			signals["behavioral"] = safeIndex(behavioralScores, i)
		}

		results[i] = ScoredResult{
			SearchResult: SearchResult{
				Chunk:    chunk,
				Score:    semanticScores[i], // semantic cosine similarity (0-1 contract)
				Distance: 1 - semanticScores[i],
			},
			RankScore: finalScore, // fused RRF + bonus score; the ranking key
			Signals:   signals,
		}
	}

	// Sort by fused ranking score descending.
	sort.Slice(results, func(i, j int) bool {
		return results[i].RankScore > results[j].RankScore
	})

	if k > 0 && len(results) > k {
		results = results[:k]
	}
	stopFusion()
	return results, nil
}

// loadChunksWithEmbeddings loads all chunks and their embeddings from the database.
func (s *SQLiteStore) loadChunksWithEmbeddings(ctx context.Context) ([]Chunk, [][]float64, error) {
	stopLoad := s.stageTimer("corpus_load_decode_candidate_hydration")
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, content, source, start_line, end_line, language, metadata, embedding, stable_key FROM chunks`)
	if err != nil {
		return nil, nil, fmt.Errorf("rag: query chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var chunks []Chunk
	var embeddings [][]float64
	for rows.Next() {
		var chunk Chunk
		var metaJSON, embJSON string
		if err := rows.Scan(&chunk.ID, &chunk.Content, &chunk.Source,
			&chunk.StartLine, &chunk.EndLine, &chunk.Language, &metaJSON, &embJSON, &chunk.StableKey); err != nil {
			return nil, nil, fmt.Errorf("rag: scan chunk: %w", err)
		}

		chunk.Metadata = make(map[string]string)
		if err := json.Unmarshal([]byte(metaJSON), &chunk.Metadata); err != nil {
			return nil, nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
		}

		var embedding []float64
		if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
			return nil, nil, fmt.Errorf("rag: unmarshal embedding: %w", err)
		}

		chunks = append(chunks, chunk)
		embeddings = append(embeddings, embedding)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("rag: iterate chunks: %w", err)
	}
	stopLoad()
	return chunks, embeddings, nil
}

func validateSearchEmbeddingDimension(chunkID string, queryEmbedding, storedEmbedding []float64) error {
	if len(storedEmbedding) != len(queryEmbedding) {
		return fmt.Errorf("rag: search: embedding dimension mismatch for chunk %q (query=%d stored=%d)",
			chunkID, len(queryEmbedding), len(storedEmbedding))
	}
	return nil
}

// computeRanks returns 1-based ranks for a score slice (highest score = rank 1).
// Ties receive the same rank: if two items share the highest score, both get rank 1.
func computeRanks(scores []float64) []int {
	type indexed struct {
		index int
		score float64
	}
	sorted := make([]indexed, len(scores))
	for i, s := range scores {
		sorted[i] = indexed{i, s}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	ranks := make([]int, len(scores))
	for i, entry := range sorted {
		if i > 0 && entry.score == sorted[i-1].score {
			// Tie: same rank as the previous entry.
			ranks[entry.index] = ranks[sorted[i-1].index]
		} else {
			ranks[entry.index] = i + 1 // 1-based
		}
	}
	return ranks
}

// rrfContribution computes the RRF score contribution for a given rank.
func rrfContribution(rank int) float64 {
	return 1.0 / float64(rrfK+rank)
}

// anyNonZero reports whether xs contains any non-zero value. Used to skip the
// behavioral RRF list during cold-start so raw fused scores stay identical.
func anyNonZero(xs []float64) bool {
	for _, x := range xs {
		if x != 0 {
			return true
		}
	}
	return false
}

// safeIndex returns scores[i] if in bounds, or 0.
func safeIndex(scores []float64, i int) float64 {
	if scores != nil && i < len(scores) {
		return scores[i]
	}
	return 0
}
