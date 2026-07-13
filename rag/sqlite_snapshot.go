package rag

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

type sqliteSnapshot struct {
	chunks    []Chunk
	rowIDs    []int64
	indexedAt []int64
	vectors   []float64
	dimension int
	probe     VectorSpaceProbe
}

type sqliteSnapshotLoad struct {
	done     chan struct{}
	snapshot *sqliteSnapshot
	err      error
}

func (s *SQLiteStore) sqliteSnapshot(ctx context.Context) (*sqliteSnapshot, error) {
	s.snapshotMu.Lock()
	if s.resident != nil {
		snapshot := s.resident
		s.snapshotMu.Unlock()
		return snapshot, nil
	}
	load := s.snapshotLoad
	if load == nil {
		load = &sqliteSnapshotLoad{done: make(chan struct{})}
		s.snapshotLoad = load
		// The shared load is decoupled from the initiating request: it runs
		// on its own goroutine with cancellation stripped, so one caller's
		// cancellation or deadline cannot fail concurrent waiters or abort
		// the one-time warm-up they all depend on.
		go s.runSnapshotLoad(context.WithoutCancel(ctx), load)
	}
	s.snapshotMu.Unlock()

	select {
	case <-load.done:
		return load.snapshot, load.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *SQLiteStore) runSnapshotLoad(ctx context.Context, load *sqliteSnapshotLoad) {
	load.snapshot, load.err = s.loadSQLiteSnapshot(ctx)

	s.snapshotMu.Lock()
	if load.err == nil {
		s.resident = load.snapshot
	}
	s.snapshotLoad = nil
	close(load.done)
	s.snapshotMu.Unlock()
}

func (s *SQLiteStore) loadSQLiteSnapshot(ctx context.Context) (*sqliteSnapshot, error) {
	stopLoad := s.stageTimer("snapshot_load_decode")
	defer stopLoad()

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&count); err != nil {
		return nil, fmt.Errorf("rag: load snapshot count: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, id, source, start_line, end_line, language,
		       stable_key, indexed_at, embedding, vector_space_id
		  FROM chunks
		 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("rag: load snapshot rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	snapshot := &sqliteSnapshot{
		chunks:    make([]Chunk, 0, count),
		rowIDs:    make([]int64, 0, count),
		indexedAt: make([]int64, 0, count),
	}
	var minVectorSpaceID, maxVectorSpaceID string
	dimensionSet := false
	var decoder corpusEmbeddingDecoder
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var (
			chunk            Chunk
			rowID            int64
			indexedAt        int64
			encodedEmbedding []byte
			vectorSpaceID    string
		)
		if err := rows.Scan(&rowID, &chunk.ID, &chunk.Source, &chunk.StartLine,
			&chunk.EndLine, &chunk.Language, &chunk.StableKey, &indexedAt,
			&encodedEmbedding, &vectorSpaceID); err != nil {
			return nil, fmt.Errorf("rag: scan snapshot chunk: %w", err)
		}

		embedding, err := decoder.decode(encodedEmbedding, chunk.ID)
		if err != nil {
			return nil, err
		}
		if !dimensionSet {
			snapshot.dimension = len(embedding)
			snapshot.vectors = make([]float64, 0, count*snapshot.dimension)
			dimensionSet = true
		} else if len(embedding) != snapshot.dimension {
			return nil, fmt.Errorf("rag: load snapshot: embedding dimension mismatch for chunk %q (expected=%d stored=%d)",
				chunk.ID, snapshot.dimension, len(embedding))
		}
		normalizeVector(embedding)

		snapshot.chunks = append(snapshot.chunks, chunk)
		snapshot.rowIDs = append(snapshot.rowIDs, rowID)
		snapshot.indexedAt = append(snapshot.indexedAt, indexedAt)
		snapshot.vectors = append(snapshot.vectors, embedding...)
		if vectorSpaceID == "" {
			snapshot.probe.HasUnknown = true
		} else {
			if minVectorSpaceID == "" || vectorSpaceID < minVectorSpaceID {
				minVectorSpaceID = vectorSpaceID
			}
			if maxVectorSpaceID == "" || vectorSpaceID > maxVectorSpaceID {
				maxVectorSpaceID = vectorSpaceID
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate snapshot chunks: %w", err)
	}
	if minVectorSpaceID != "" {
		snapshot.probe.KnownIDs = []string{minVectorSpaceID}
		if maxVectorSpaceID != minVectorSpaceID {
			snapshot.probe.KnownIDs = append(snapshot.probe.KnownIDs, maxVectorSpaceID)
		}
	}
	return snapshot, nil
}

func normalizeVector(vector []float64) {
	var magnitudeSquared float64
	for _, value := range vector {
		magnitudeSquared += value * value
	}
	if magnitudeSquared == 0 {
		return
	}
	inverseMagnitude := 1 / math.Sqrt(magnitudeSquared)
	for i := range vector {
		vector[i] *= inverseMagnitude
	}
}

func cloneVectorSpaceProbe(probe VectorSpaceProbe) VectorSpaceProbe {
	return VectorSpaceProbe{
		KnownIDs:   append([]string(nil), probe.KnownIDs...),
		HasUnknown: probe.HasUnknown,
	}
}

type snapshotCandidate struct {
	index int
	score float64
}

type snapshotCandidateHeap []snapshotCandidate

func (h snapshotCandidateHeap) Len() int      { return len(h) }
func (h snapshotCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h snapshotCandidateHeap) Less(i, j int) bool {
	return candidateBetter(h[j], h[i])
}
func (h *snapshotCandidateHeap) Push(value any) {
	*h = append(*h, value.(snapshotCandidate))
}
func (h *snapshotCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func candidateBetter(a, b snapshotCandidate) bool {
	return a.score > b.score || (a.score == b.score && a.index < b.index)
}

func selectSnapshotTopK(ctx context.Context, scores []float64, k int) ([]snapshotCandidate, error) {
	if len(scores) == 0 {
		return nil, nil
	}
	if k <= 0 || k > len(scores) {
		k = len(scores)
	}
	candidates := make(snapshotCandidateHeap, 0, k)
	for index, score := range scores {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offerSnapshotCandidate(&candidates, snapshotCandidate{index: index, score: score}, k)
	}
	sortSnapshotCandidates(candidates)
	return candidates, nil
}

func offerSnapshotCandidate(candidates *snapshotCandidateHeap, candidate snapshotCandidate, k int) {
	if len(*candidates) < k {
		heap.Push(candidates, candidate)
	} else if candidateBetter(candidate, (*candidates)[0]) {
		(*candidates)[0] = candidate
		heap.Fix(candidates, 0)
	}
}

func sortSnapshotCandidates(candidates snapshotCandidateHeap) {
	sort.Slice(candidates, func(i, j int) bool {
		return candidateBetter(candidates[i], candidates[j])
	})
}

func (snapshot *sqliteSnapshot) semanticScores(ctx context.Context, queryEmbedding []float64) ([]float64, error) {
	if len(snapshot.chunks) == 0 {
		return nil, nil
	}
	if err := snapshot.validateQueryDimension(queryEmbedding); err != nil {
		return nil, err
	}
	query := snapshot.normalizedQuery(queryEmbedding)
	scores := make([]float64, len(snapshot.chunks))
	for i := range snapshot.chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		scores[i] = snapshot.semanticScore(i, query)
	}
	return scores, nil
}

func (snapshot *sqliteSnapshot) semanticTopK(ctx context.Context, queryEmbedding []float64, k int) ([]snapshotCandidate, error) {
	candidates, err := snapshot.semanticCandidateHeap(ctx, queryEmbedding, k)
	if err != nil {
		return nil, err
	}
	sortSnapshotCandidates(candidates)
	return candidates, nil
}

func (snapshot *sqliteSnapshot) semanticCandidateHeap(ctx context.Context, queryEmbedding []float64, k int) (snapshotCandidateHeap, error) {
	if len(snapshot.chunks) == 0 {
		return nil, nil
	}
	if err := snapshot.validateQueryDimension(queryEmbedding); err != nil {
		return nil, err
	}
	if k <= 0 || k > len(snapshot.chunks) {
		k = len(snapshot.chunks)
	}
	query := snapshot.normalizedQuery(queryEmbedding)
	candidates := make(snapshotCandidateHeap, 0, k)
	for i := range snapshot.chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		offerSnapshotCandidate(&candidates, snapshotCandidate{index: i, score: snapshot.semanticScore(i, query)}, k)
	}
	return candidates, nil
}

func (snapshot *sqliteSnapshot) normalizedQuery(queryEmbedding []float64) []float64 {
	query := append([]float64(nil), queryEmbedding...)
	normalizeVector(query)
	return query
}

func (snapshot *sqliteSnapshot) semanticScore(index int, normalizedQuery []float64) float64 {
	var score float64
	offset := index * snapshot.dimension
	for j, queryValue := range normalizedQuery {
		score += queryValue * snapshot.vectors[offset+j]
	}
	return score
}

func (snapshot *sqliteSnapshot) validateQueryDimension(queryEmbedding []float64) error {
	if len(snapshot.chunks) > 0 && len(queryEmbedding) != snapshot.dimension {
		return fmt.Errorf("rag: search: embedding dimension mismatch for chunk %q (query=%d stored=%d)",
			snapshot.chunks[0].ID, len(queryEmbedding), snapshot.dimension)
	}
	return nil
}

func (snapshot *sqliteSnapshot) temporalScores(ctx context.Context, qCtx QueryContext) ([]float64, error) {
	scores := make([]float64, len(snapshot.chunks))
	var now int64
	if !qCtx.Timestamp.IsZero() {
		now = qCtx.Timestamp.Unix()
	}
	if now <= 0 {
		for _, indexedAt := range snapshot.indexedAt {
			if indexedAt > now {
				now = indexedAt
			}
		}
	}
	for i, indexedAt := range snapshot.indexedAt {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if indexedAt <= 0 {
			continue
		}
		age := float64(now - indexedAt)
		if age <= 0 {
			scores[i] = 1
		} else {
			scores[i] = math.Pow(2, -age/defaultTemporalHalfLifeSeconds)
		}
	}
	return scores, nil
}

func (s *SQLiteStore) searchMultiSnapshot(ctx context.Context, queryEmbedding []float64, query string,
	k int, qCtx QueryContext) ([]ScoredResult, error) {
	snapshot, err := s.sqliteSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	chunks := snapshot.chunks
	if len(chunks) == 0 {
		return nil, nil
	}

	stopValidation := s.stageTimer("vector_dimension_validation")
	if err := snapshot.validateQueryDimension(queryEmbedding); err != nil {
		return nil, err
	}
	stopValidation()

	stopSemantic := s.stageTimer("semantic_scoring")
	semanticScores, err := snapshot.semanticScores(ctx, queryEmbedding)
	if err != nil {
		return nil, fmt.Errorf("rag: semantic scoring: %w", err)
	}
	stopSemantic()

	stopKeyword := s.stageTimer("keyword_scoring")
	keywordScores, err := NewKeywordScorer(s.db).ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: keyword scoring: %w", err)
	}
	stopKeyword()

	stopTemporal := s.stageTimer("temporal_scoring")
	temporalScores, err := snapshot.temporalScores(ctx, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: temporal scoring: %w", err)
	}
	stopTemporal()

	stopStructural := s.stageTimer("structural_scoring")
	structuralScores, err := NewStructuralScorer().ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag: structural scoring: %w", err)
	}
	stopStructural()

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

	stopFusion := s.stageTimer("fusion_ranking_top_k")
	semanticRanks := computeRanks(semanticScores)
	keywordRanks := computeRanks(keywordScores)
	fusedScores := make([]float64, len(chunks))
	for i := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fusedScores[i] = rrfContribution(semanticRanks[i]) + rrfContribution(keywordRanks[i])
		if foldBehavioral {
			fusedScores[i] += rrfContribution(behavioralRanks[i])
		}
		fusedScores[i] += defaultTemporalWeight*safeIndex(temporalScores, i) +
			defaultStructuralWeight*safeIndex(structuralScores, i)
	}
	candidates, err := selectSnapshotTopK(ctx, fusedScores, k)
	if err != nil {
		return nil, err
	}
	stopFusion()

	indices := make([]int, len(candidates))
	for i, candidate := range candidates {
		indices[i] = candidate.index
	}
	hydrated, err := s.hydrateSnapshotChunks(ctx, snapshot, indices)
	if err != nil {
		return nil, err
	}
	results := make([]ScoredResult, len(candidates))
	for i, candidate := range candidates {
		index := candidate.index
		signals := map[string]float64{
			"semantic":   semanticScores[index],
			"keyword":    keywordScores[index],
			"temporal":   safeIndex(temporalScores, index),
			"structural": safeIndex(structuralScores, index),
		}
		if s.behavioral != nil {
			signals["behavioral"] = safeIndex(behavioralScores, index)
		}
		results[i] = ScoredResult{
			SearchResult: SearchResult{
				Chunk:    hydrated[i],
				Score:    semanticScores[index],
				Distance: 1 - semanticScores[index],
			},
			RankScore: candidate.score,
			Signals:   signals,
		}
	}
	return results, nil
}

func (s *SQLiteStore) searchSnapshot(ctx context.Context, queryEmbedding []float64, k int) ([]SearchResult, error) {
	snapshot, err := s.sqliteSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	if len(snapshot.chunks) == 0 {
		return nil, nil
	}

	stopSemantic := s.stageTimer("semantic_scoring")
	candidates, err := snapshot.semanticCandidateHeap(ctx, queryEmbedding, k)
	if err != nil {
		return nil, err
	}
	stopSemantic()

	stopRanking := s.stageTimer("ranking_top_k")
	sortSnapshotCandidates(candidates)
	stopRanking()

	indices := make([]int, len(candidates))
	for i, candidate := range candidates {
		indices[i] = candidate.index
	}
	chunks, err := s.hydrateSnapshotChunks(ctx, snapshot, indices)
	if err != nil {
		return nil, err
	}
	results := make([]SearchResult, len(candidates))
	for i, candidate := range candidates {
		results[i] = SearchResult{
			Chunk:    chunks[i],
			Score:    candidate.score,
			Distance: 1 - candidate.score,
		}
	}
	return results, nil
}

const snapshotHydrationBatchSize = 500

func (s *SQLiteStore) hydrateSnapshotChunks(ctx context.Context, snapshot *sqliteSnapshot, indices []int) ([]Chunk, error) {
	if len(indices) == 0 {
		return nil, nil
	}
	stopHydration := s.stageTimer("finalist_hydration")
	defer stopHydration()

	hydrated := make(map[int64]Chunk, len(indices))
	for start := 0; start < len(indices); start += snapshotHydrationBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+snapshotHydrationBatchSize, len(indices))
		args := make([]any, end-start)
		for i, index := range indices[start:end] {
			args[i] = snapshot.rowIDs[index]
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		rows, err := s.db.QueryContext(ctx, `
			SELECT rowid, id, content, source, start_line, end_line, language, metadata, stable_key
			  FROM chunks
			 WHERE rowid IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("rag: hydrate snapshot finalists: %w", err)
		}
		for rows.Next() {
			var chunk Chunk
			var rowID int64
			var metadataJSON string
			if err := rows.Scan(&rowID, &chunk.ID, &chunk.Content, &chunk.Source,
				&chunk.StartLine, &chunk.EndLine, &chunk.Language, &metadataJSON,
				&chunk.StableKey); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("rag: scan snapshot finalist: %w", err)
			}
			chunk.Metadata = make(map[string]string)
			if err := json.Unmarshal([]byte(metadataJSON), &chunk.Metadata); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("rag: unmarshal metadata for chunk %q: %w", chunk.ID, err)
			}
			hydrated[rowID] = chunk
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("rag: iterate snapshot finalists: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("rag: close snapshot finalist rows: %w", err)
		}
	}

	chunks := make([]Chunk, len(indices))
	for i, index := range indices {
		rowID := snapshot.rowIDs[index]
		chunk, ok := hydrated[rowID]
		if !ok {
			return nil, fmt.Errorf("rag: hydrate snapshot finalist rowid %d: row missing", rowID)
		}
		chunks[i] = chunk
	}
	return chunks, nil
}
