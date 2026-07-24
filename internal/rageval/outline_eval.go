package rageval

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/kstruzzieri/go-llm/rag"
)

const OutlineSchemaVersion = "rag-outline-eval/v1"

var outlineModeNames = []string{
	"full_corpus_search_multi",
	"resident_exact",
	"bounded_semantic_keyword_union",
	"outline_then_content",
	"hierarchical",
}

// OutlineOptions controls the eval-only outline retrieval experiment.
// A positive CandidateLimit must be greater than the fixed final K of 10
// because production hierarchical retrieval requires CandidateLimit > K.
type OutlineOptions struct {
	Dimensions     int
	Samples        int
	CandidateLimit int
}

// OutlineReport is the complete five-mode experiment result.
type OutlineReport struct {
	SchemaVersion string              `json:"schema_version"`
	Runtime       OutlineRuntime      `json:"runtime"`
	Corpus        OutlineCorpus       `json:"corpus"`
	Modes         []OutlineModeReport `json:"modes"`
	Conclusion    string              `json:"conclusion"`
}

type OutlineRuntime struct {
	GoVersion    string `json:"go_version"`
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

type OutlineCorpus struct {
	Chunks         int   `json:"chunks"`
	Sources        int   `json:"sources"`
	Queries        int   `json:"queries"`
	Dimensions     int   `json:"dimensions"`
	FinalK         []int `json:"final_k"`
	CandidateLimit int   `json:"candidate_limit"`
	Samples        int   `json:"samples"`
}

type OutlineModeReport struct {
	Name    string               `json:"name"`
	Summary OutlineModeSummary   `json:"summary"`
	Queries []OutlineQueryReport `json:"queries"`
}

type OutlineModeSummary struct {
	RecallAt5                   float64        `json:"recall_at_5"`
	RecallAt10                  float64        `json:"recall_at_10"`
	MRRAt5                      float64        `json:"mrr_at_5"`
	MRRAt10                     float64        `json:"mrr_at_10"`
	ExpectedSupportCoverageAt5  float64        `json:"expected_support_coverage_at_5"`
	ExpectedSupportCoverageAt10 float64        `json:"expected_support_coverage_at_10"`
	CitationSourceAccuracyAt5   float64        `json:"citation_source_accuracy_at_5"`
	CitationSourceAccuracyAt10  float64        `json:"citation_source_accuracy_at_10"`
	FinalContextTokensAt5       float64        `json:"final_context_tokens_at_5"`
	FinalContextTokensAt10      float64        `json:"final_context_tokens_at_10"`
	PlanningTokens              *int           `json:"planning_tokens"`
	LatencyMS                   LatencySummary `json:"latency_ms"`
	AllocatedBytes              float64        `json:"allocated_bytes"`
	AllocationCount             float64        `json:"allocation_count"`
	// CandidatesInspected is the average number of unique lean chunk records
	// consulted before any full-content hydration.
	CandidatesInspected float64 `json:"candidates_inspected"`
	// RankedCandidates is the average final scoring or selection set size.
	RankedCandidates float64 `json:"ranked_candidates"`
	// HydratedContentChunks is the average number of full-content chunks loaded
	// by the adapter, including candidates loaded before later selection.
	HydratedContentChunks float64 `json:"hydrated_content_chunks"`
	DeterministicOrdering bool    `json:"deterministic_ordering"`
}

type OutlineQueryReport struct {
	ID              string            `json:"id"`
	Category        string            `json:"category"`
	Query           string            `json:"query"`
	ExpectedIDs     []string          `json:"expected_ids"`
	ExpectedSources []string          `json:"expected_sources"`
	ResultIDs       []string          `json:"result_ids"`
	ResultSources   []string          `json:"result_sources"`
	Metrics         []OutlineKMetrics `json:"metrics"`
	PlanningTokens  *int              `json:"planning_tokens"`
	LatencyMS       LatencySummary    `json:"latency_ms"`
	AllocatedBytes  float64           `json:"allocated_bytes"`
	AllocationCount float64           `json:"allocation_count"`
	// CandidatesInspected counts unique lean chunk records consulted before
	// full-content hydration for this query.
	CandidatesInspected float64 `json:"candidates_inspected"`
	// RankedCandidates counts the mode's final scoring or selection set.
	RankedCandidates float64 `json:"ranked_candidates"`
	// HydratedContentChunks counts full-content chunks loaded by the adapter.
	HydratedContentChunks float64 `json:"hydrated_content_chunks"`
	DeterministicOrdering bool    `json:"deterministic_ordering"`
}

type OutlineKMetrics struct {
	K                       int     `json:"k"`
	Recall                  float64 `json:"recall"`
	MRR                     float64 `json:"mrr"`
	ExpectedSupportCoverage float64 `json:"expected_support_coverage"`
	CitationSourceAccuracy  float64 `json:"citation_source_accuracy"`
	FinalContextTokens      int     `json:"final_context_tokens"`
}

type outlineCandidate struct {
	Chunk     rag.Chunk
	Embedding []float64
}

type outlineRetrieval struct {
	Results []rag.SearchResult
	// CandidatesInspected counts unique lean records consulted before hydration.
	CandidatesInspected int
	// RankedCandidates counts the mode's final scoring or selection set.
	RankedCandidates int
	// HydratedContentChunks counts all full-content chunks loaded by the adapter.
	HydratedContentChunks int
}

type outlineMode struct {
	name     string
	retrieve func(context.Context, outlineQuery) (outlineRetrieval, error)
}

func defaultOutlineOptions(opts OutlineOptions) OutlineOptions {
	if opts.Dimensions <= 0 {
		opts.Dimensions = 768
	}
	if opts.Samples <= 0 {
		opts.Samples = 5
	}
	if opts.CandidateLimit <= 0 {
		opts.CandidateLimit = 50
	}
	return opts
}

// RunOutlineExperiment runs the five eval-only retrieval modes.
func RunOutlineExperiment(ctx context.Context, opts OutlineOptions) (*OutlineReport, error) {
	opts = defaultOutlineOptions(opts)
	if opts.CandidateLimit <= 10 {
		return nil, fmt.Errorf("rag eval: outline candidate limit must be greater than 10")
	}
	fixture, err := buildOutlineFixture(opts.Dimensions)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "go-llm-outline-eval-")
	if err != nil {
		return nil, fmt.Errorf("rag eval: create outline temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	path := filepath.Join(tempDir, "outline.db")
	if err := seedOutlineStore(ctx, path, fixture); err != nil {
		return nil, err
	}
	full, err := rag.NewSQLiteStore(path)
	if err != nil {
		return nil, fmt.Errorf("rag eval: open mutable outline store: %w", err)
	}
	defer func() { _ = full.Close() }()
	resident, err := rag.OpenSQLiteStoreReadOnly(path)
	if err != nil {
		return nil, fmt.Errorf("rag eval: open resident outline store: %w", err)
	}
	defer func() { _ = resident.Close() }()

	candidates, err := loadOutlineCandidates(ctx, resident)
	if err != nil {
		return nil, err
	}
	retriever, err := rag.NewRetrieverWithEmbedder(newOutlineEmbedder(fixture), resident, rag.WithRetrieverModel("outline-fixture"))
	if err != nil {
		return nil, fmt.Errorf("rag eval: create hierarchical retriever: %w", err)
	}

	modes := []outlineMode{
		{name: outlineModeNames[0], retrieve: func(ctx context.Context, query outlineQuery) (outlineRetrieval, error) {
			results, err := full.SearchMulti(ctx, query.Embedding, query.Query, 10, outlineQueryContext(query))
			return outlineRetrieval{Results: outlineSearchResults(results), CandidatesInspected: len(fixture.chunks), RankedCandidates: len(fixture.chunks), HydratedContentChunks: len(fixture.chunks)}, err
		}},
		{name: outlineModeNames[1], retrieve: func(ctx context.Context, query outlineQuery) (outlineRetrieval, error) {
			results, err := resident.SearchMulti(ctx, query.Embedding, query.Query, 10, outlineQueryContext(query))
			return outlineRetrieval{Results: outlineSearchResults(results), CandidatesInspected: len(candidates), RankedCandidates: len(fixture.chunks), HydratedContentChunks: len(results)}, err
		}},
		{name: outlineModeNames[2], retrieve: func(ctx context.Context, query outlineQuery) (outlineRetrieval, error) {
			semantic, err := topSemanticCandidates(ctx, candidates, query.Embedding, opts.CandidateLimit)
			if err != nil {
				return outlineRetrieval{}, err
			}
			keyword, err := topKeywordCandidates(ctx, resident, candidates, query.Query, opts.CandidateLimit)
			if err != nil {
				return outlineRetrieval{}, err
			}
			union := boundedUnion(semantic, keyword)
			results, err := rankOutlineCandidates(ctx, resident, union, query.Query, query.Embedding, outlineQueryContext(query), 10)
			return outlineRetrieval{Results: outlineSearchResults(results), CandidatesInspected: len(candidates), RankedCandidates: len(union), HydratedContentChunks: len(results)}, err
		}},
		{name: outlineModeNames[3], retrieve: func(ctx context.Context, query outlineQuery) (outlineRetrieval, error) {
			selected := selectOutlineCandidates(candidates, query.Query, opts.CandidateLimit)
			results, err := rankOutlineCandidates(ctx, resident, selected, query.Query, query.Embedding, outlineQueryContext(query), 10)
			return outlineRetrieval{Results: outlineSearchResults(results), CandidatesInspected: len(candidates), RankedCandidates: len(selected), HydratedContentChunks: len(results)}, err
		}},
		{name: outlineModeNames[4], retrieve: func(ctx context.Context, query outlineQuery) (outlineRetrieval, error) {
			response, err := retriever.RetrieveHierarchical(ctx, rag.HierarchicalRetrievalRequest{
				Request: rag.RetrievalRequest{
					Query:        query.Query,
					K:            10,
					QueryContext: outlineQueryContext(query),
				},
				CandidateLimit: opts.CandidateLimit,
				MaxDepth:       2,
				MaxGroups:      1,
				MaxTokens:      1 << 20,
				Timeout:        10 * time.Second,
			})
			return outlineRetrieval{
				Results:               outlineSearchResults(response.Results),
				CandidatesInspected:   len(candidates),
				RankedCandidates:      response.Trace.Budget.InspectedCandidates,
				HydratedContentChunks: response.Policy.CandidateCount,
			}, err
		}},
	}

	report := &OutlineReport{
		SchemaVersion: OutlineSchemaVersion,
		Runtime: OutlineRuntime{
			GoVersion:    runtime.Version(),
			OS:           runtime.GOOS,
			Architecture: runtime.GOARCH,
		},
		Corpus: OutlineCorpus{
			Chunks:         len(fixture.chunks),
			Sources:        len(outlineSources()),
			Queries:        len(fixture.queries),
			Dimensions:     opts.Dimensions,
			FinalK:         []int{5, 10},
			CandidateLimit: opts.CandidateLimit,
			Samples:        opts.Samples,
		},
		Modes: make([]OutlineModeReport, 0, len(modes)),
	}
	for _, mode := range modes {
		modeReport, err := runOutlineMode(ctx, fixture.queries, opts.Samples, mode)
		if err != nil {
			return nil, fmt.Errorf("rag eval: mode %s: %w", mode.name, err)
		}
		report.Modes = append(report.Modes, modeReport)
	}
	report.Conclusion = outlineConclusion(report.Modes)
	return report, nil
}

func outlineQueryContext(query outlineQuery) rag.QueryContext {
	return rag.QueryContext{CurrentFile: query.CurrentFile, Timestamp: replayTimestamp}
}

func newOutlineEmbedder(fixture outlineFixture) rag.Embedder {
	embeddings := make(map[string][]float64, len(fixture.queries))
	for _, query := range fixture.queries {
		embeddings[query.Query] = append([]float64(nil), query.Embedding...)
	}
	return rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		if len(inputs) != 1 {
			return rag.EmbedResult{}, fmt.Errorf("rag eval: outline embedder expects 1 input, got %d", len(inputs))
		}
		embedding, ok := embeddings[inputs[0]]
		if !ok {
			return rag.EmbedResult{}, fmt.Errorf("rag eval: no outline embedding for query %q", inputs[0])
		}
		return rag.EmbedResult{
			Embeddings:    [][]float64{append([]float64(nil), embedding...)},
			Provider:      "fixture",
			Model:         "outline-fixture",
			VectorSpaceID: vectorSpaceID,
		}, nil
	})
}

func boundedUnion(semantic, keyword []outlineCandidate) []outlineCandidate {
	union := make([]outlineCandidate, 0, len(semantic)+len(keyword))
	seen := make(map[string]struct{}, len(semantic)+len(keyword))
	for _, list := range [][]outlineCandidate{semantic, keyword} {
		for _, candidate := range list {
			identity := outlineIdentity(candidate.Chunk)
			if _, exists := seen[identity]; exists {
				continue
			}
			seen[identity] = struct{}{}
			union = append(union, candidate)
		}
	}
	return union
}

func topSemanticCandidates(ctx context.Context, candidates []outlineCandidate, queryEmbedding []float64, limit int) ([]outlineCandidate, error) {
	scorer := rag.NewSemanticScorer()
	scorer.SetEmbeddings(outlineEmbeddingMap(candidates))
	chunks := outlineChunks(candidates)
	scores, err := scorer.ScoreBatch(ctx, chunks, "", queryEmbedding, rag.QueryContext{})
	if err != nil {
		return nil, fmt.Errorf("semantic candidate selection: %w", err)
	}
	return selectCandidateScores(candidates, scores, limit, false), nil
}

func topKeywordCandidates(ctx context.Context, store *rag.SQLiteStore, candidates []outlineCandidate, query string, limit int) ([]outlineCandidate, error) {
	scores, err := rag.NewKeywordScorer(store.DB()).ScoreBatch(ctx, outlineChunks(candidates), query, nil, rag.QueryContext{})
	if err != nil {
		return nil, fmt.Errorf("keyword candidate selection: %w", err)
	}
	return selectCandidateScores(candidates, scores, limit, true), nil
}

func selectCandidateScores(candidates []outlineCandidate, scores []float64, limit int, nonZeroOnly bool) []outlineCandidate {
	indices := make([]int, 0, len(candidates))
	for i, score := range scores {
		if !nonZeroOnly || score != 0 {
			indices = append(indices, i)
		}
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return scores[indices[i]] > scores[indices[j]]
	})
	if limit > 0 && len(indices) > limit {
		indices = indices[:limit]
	}
	selected := make([]outlineCandidate, len(indices))
	for i, index := range indices {
		selected[i] = candidates[index]
	}
	return selected
}

func selectOutlineCandidates(candidates []outlineCandidate, query string, limit int) []outlineCandidate {
	queryTokens := outlineTokens(query)
	type scored struct {
		candidate outlineCandidate
		overlap   int
	}
	ranked := make([]scored, len(candidates))
	for i, candidate := range candidates {
		outline := []string{
			candidate.Chunk.Source,
			candidate.Chunk.Language,
			candidate.Chunk.StableKey,
		}
		for _, key := range []string{
			"package", "module", "language", "symbol", "symbol_path", "type", "function",
			"source_test_pair", "heading", "comment", "doc", "docstring",
		} {
			outline = append(outline, candidate.Chunk.Metadata[key])
		}
		tokens := outlineTokens(strings.Join(outline, " "))
		overlap := 0
		for token := range queryTokens {
			if _, ok := tokens[token]; ok {
				overlap++
			}
		}
		ranked[i] = scored{candidate: candidate, overlap: overlap}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].overlap != ranked[j].overlap {
			return ranked[i].overlap > ranked[j].overlap
		}
		return outlineIdentity(ranked[i].candidate.Chunk) < outlineIdentity(ranked[j].candidate.Chunk)
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	selected := make([]outlineCandidate, len(ranked))
	for i := range ranked {
		selected[i] = ranked[i].candidate
	}
	return selected
}

func outlineTokens(text string) map[string]struct{} {
	tokens := make(map[string]struct{})
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		tokens[token] = struct{}{}
	}
	return tokens
}

func loadOutlineCandidates(ctx context.Context, store *rag.SQLiteStore) ([]outlineCandidate, error) {
	rows, err := store.DB().QueryContext(ctx, `
		SELECT id, source, start_line, end_line, language, metadata, embedding, stable_key
		  FROM chunks
		 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("rag eval: load outline candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []outlineCandidate
	for rows.Next() {
		var candidate outlineCandidate
		var metadataJSON string
		var encoded []byte
		if err := rows.Scan(
			&candidate.Chunk.ID,
			&candidate.Chunk.Source,
			&candidate.Chunk.StartLine,
			&candidate.Chunk.EndLine,
			&candidate.Chunk.Language,
			&metadataJSON,
			&encoded,
			&candidate.Chunk.StableKey,
		); err != nil {
			return nil, fmt.Errorf("rag eval: scan outline candidate: %w", err)
		}
		if err := json.Unmarshal([]byte(metadataJSON), &candidate.Chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag eval: decode metadata for chunk %q: %w", candidate.Chunk.ID, err)
		}
		candidate.Embedding, err = decodeOutlineEmbedding(encoded)
		if err != nil {
			return nil, fmt.Errorf("rag eval: decode embedding for chunk %q: %w", candidate.Chunk.ID, err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag eval: iterate outline candidates: %w", err)
	}
	return candidates, nil
}

func decodeOutlineEmbedding(encoded []byte) ([]float64, error) {
	if len(encoded) < 16 || string(encoded[:4]) != "GLLV" || encoded[4] != 1 || encoded[5] != 1 || encoded[6] != 1 {
		return nil, fmt.Errorf("unsupported packed embedding")
	}
	dimension := int(binary.LittleEndian.Uint32(encoded[8:12]))
	payload := int(binary.LittleEndian.Uint32(encoded[12:16]))
	if dimension <= 0 || payload != dimension*4 || len(encoded) != 16+payload {
		return nil, fmt.Errorf("invalid packed embedding shape")
	}
	vector := make([]float64, dimension)
	for i := range vector {
		vector[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(encoded[16+4*i:])))
	}
	return vector, nil
}

func rankOutlineCandidates(
	ctx context.Context,
	store *rag.SQLiteStore,
	candidates []outlineCandidate,
	query string,
	queryEmbedding []float64,
	qCtx rag.QueryContext,
	k int,
) ([]rag.ScoredResult, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	for _, candidate := range candidates {
		if len(candidate.Embedding) != len(queryEmbedding) {
			return nil, fmt.Errorf(
				"rag eval: embedding dimension mismatch for chunk %q (query=%d stored=%d)",
				candidate.Chunk.ID, len(queryEmbedding), len(candidate.Embedding),
			)
		}
	}
	chunks := outlineChunks(candidates)
	semantic := rag.NewSemanticScorer()
	semantic.SetEmbeddings(outlineEmbeddingMap(candidates))
	semanticScores, err := semantic.ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag eval: semantic scoring: %w", err)
	}
	keywordScores, err := rag.NewKeywordScorer(store.DB()).ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag eval: keyword scoring: %w", err)
	}
	temporalScores, err := rag.NewTemporalScorer(store.DB(), 0).ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag eval: temporal scoring: %w", err)
	}
	structuralScores, err := rag.NewStructuralScorer().ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag eval: structural scoring: %w", err)
	}
	_, err = rag.NewBehavioralScorer(nil).ScoreBatch(ctx, chunks, query, queryEmbedding, qCtx)
	if err != nil {
		return nil, fmt.Errorf("rag eval: behavioral scoring: %w", err)
	}

	semanticRanks := outlineRanks(semanticScores)
	keywordRanks := outlineRanks(keywordScores)
	results := make([]rag.ScoredResult, len(candidates))
	for i, candidate := range candidates {
		rankScore := 1/float64(60+semanticRanks[i]) + 1/float64(60+keywordRanks[i])
		rankScore += 0.1*temporalScores[i] + 0.1*structuralScores[i]
		results[i] = rag.ScoredResult{
			SearchResult: rag.SearchResult{
				Chunk:    candidate.Chunk,
				Score:    semanticScores[i],
				Distance: 1 - semanticScores[i],
			},
			RankScore: rankScore,
			Signals: map[string]float64{
				"semantic":   semanticScores[i],
				"keyword":    keywordScores[i],
				"temporal":   temporalScores[i],
				"structural": structuralScores[i],
			},
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].RankScore > results[j].RankScore
	})
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	hydrated, err := hydrateOutlineChunks(ctx, store, resultScoredIDs(results))
	if err != nil {
		return nil, err
	}
	for i := range results {
		results[i].Chunk = hydrated[i]
	}
	return results, nil
}

func outlineChunks(candidates []outlineCandidate) []rag.Chunk {
	chunks := make([]rag.Chunk, len(candidates))
	for i := range candidates {
		chunks[i] = candidates[i].Chunk
	}
	return chunks
}

func outlineEmbeddingMap(candidates []outlineCandidate) map[string][]float64 {
	embeddings := make(map[string][]float64, len(candidates))
	for _, candidate := range candidates {
		embeddings[candidate.Chunk.ID] = candidate.Embedding
	}
	return embeddings
}

func outlineRanks(scores []float64) []int {
	indices := make([]int, len(scores))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(i, j int) bool {
		return scores[indices[i]] > scores[indices[j]]
	})
	ranks := make([]int, len(scores))
	for i, index := range indices {
		if i > 0 && scores[index] == scores[indices[i-1]] {
			ranks[index] = ranks[indices[i-1]]
		} else {
			ranks[index] = i + 1
		}
	}
	return ranks
}

func hydrateOutlineChunks(ctx context.Context, store *rag.SQLiteStore, ids []string) ([]rag.Chunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i := range ids {
		args[i] = ids[i]
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	rows, err := store.DB().QueryContext(ctx, `
		SELECT id, content, source, start_line, end_line, language, metadata, stable_key
		  FROM chunks
		 WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("rag eval: hydrate outline chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	byID := make(map[string]rag.Chunk, len(ids))
	for rows.Next() {
		var chunk rag.Chunk
		var metadataJSON string
		if err := rows.Scan(
			&chunk.ID,
			&chunk.Content,
			&chunk.Source,
			&chunk.StartLine,
			&chunk.EndLine,
			&chunk.Language,
			&metadataJSON,
			&chunk.StableKey,
		); err != nil {
			return nil, fmt.Errorf("rag eval: scan hydrated outline chunk: %w", err)
		}
		if err := json.Unmarshal([]byte(metadataJSON), &chunk.Metadata); err != nil {
			return nil, fmt.Errorf("rag eval: decode hydrated metadata for chunk %q: %w", chunk.ID, err)
		}
		byID[chunk.ID] = chunk
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag eval: iterate hydrated outline chunks: %w", err)
	}
	hydrated := make([]rag.Chunk, len(ids))
	for i, id := range ids {
		chunk, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("rag eval: hydrate outline chunk %q: row missing", id)
		}
		hydrated[i] = chunk
	}
	return hydrated, nil
}

func resultScoredIDs(results []rag.ScoredResult) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Chunk.ID
	}
	return ids
}

func outlineSearchResults(results []rag.ScoredResult) []rag.SearchResult {
	if results == nil {
		return nil
	}
	searchResults := make([]rag.SearchResult, len(results))
	for i := range results {
		searchResults[i] = results[i].SearchResult
	}
	return searchResults
}

func runOutlineMode(ctx context.Context, queries []outlineQuery, samples int, mode outlineMode) (OutlineModeReport, error) {
	report := OutlineModeReport{
		Name:    mode.name,
		Queries: make([]OutlineQueryReport, 0, len(queries)),
	}
	if len(queries) > 0 {
		if _, err := mode.retrieve(ctx, queries[0]); err != nil {
			return OutlineModeReport{}, fmt.Errorf("warm mode: %w", err)
		}
	}
	var latencies []float64
	var allocatedBytes, allocationCounts, candidatesInspected, rankedCandidates, hydratedChunks []float64
	for _, query := range queries {
		var first outlineRetrieval
		var queryLatencies, queryBytes, queryAllocations, queryInspected, queryRanked, queryHydrated []float64
		deterministic := true
		for sample := 0; sample < samples; sample++ {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			result, err := mode.retrieve(ctx, query)
			elapsed := elapsedMS(time.Since(start))
			runtime.ReadMemStats(&after)
			if err != nil {
				return OutlineModeReport{}, err
			}
			if sample == 0 {
				first = result
			} else if !equalResultOrdering(first.Results, result.Results) {
				deterministic = false
			}
			queryLatencies = append(queryLatencies, elapsed)
			queryBytes = append(queryBytes, float64(after.TotalAlloc-before.TotalAlloc))
			queryAllocations = append(queryAllocations, float64(after.Mallocs-before.Mallocs))
			queryInspected = append(queryInspected, float64(result.CandidatesInspected))
			queryRanked = append(queryRanked, float64(result.RankedCandidates))
			queryHydrated = append(queryHydrated, float64(result.HydratedContentChunks))
		}
		check, err := mode.retrieve(ctx, query)
		if err != nil {
			return OutlineModeReport{}, err
		}
		deterministic = deterministic && equalResultOrdering(first.Results, check.Results)
		queryReport := OutlineQueryReport{
			ID:                    query.ID,
			Category:              query.Category,
			Query:                 query.Query,
			ExpectedIDs:           append([]string(nil), query.ExpectedIDs...),
			ExpectedSources:       append([]string(nil), query.ExpectedSources...),
			ResultIDs:             resultIDs(first.Results),
			ResultSources:         outlineResultSources(first.Results),
			Metrics:               []OutlineKMetrics{outlineKMetrics(first.Results, query, 5), outlineKMetrics(first.Results, query, 10)},
			LatencyMS:             summarizeLatencies(queryLatencies),
			AllocatedBytes:        averageFloat64(queryBytes),
			AllocationCount:       averageFloat64(queryAllocations),
			CandidatesInspected:   averageFloat64(queryInspected),
			RankedCandidates:      averageFloat64(queryRanked),
			HydratedContentChunks: averageFloat64(queryHydrated),
			DeterministicOrdering: deterministic,
		}
		report.Queries = append(report.Queries, queryReport)
		latencies = append(latencies, queryLatencies...)
		allocatedBytes = append(allocatedBytes, queryBytes...)
		allocationCounts = append(allocationCounts, queryAllocations...)
		candidatesInspected = append(candidatesInspected, queryInspected...)
		rankedCandidates = append(rankedCandidates, queryRanked...)
		hydratedChunks = append(hydratedChunks, queryHydrated...)
	}
	report.Summary = summarizeOutlineMode(report.Queries, latencies, allocatedBytes, allocationCounts, candidatesInspected, rankedCandidates, hydratedChunks)
	return report, nil
}

func outlineKMetrics(results []rag.SearchResult, query outlineQuery, k int) OutlineKMetrics {
	limited := limitResults(results, k)
	return OutlineKMetrics{
		K:                       k,
		Recall:                  recall(limited, query.ExpectedIDs),
		MRR:                     reciprocalRank(limited, query.ExpectedIDs),
		ExpectedSupportCoverage: outlineSupportCoverage(limited, query.ExpectedSources),
		CitationSourceAccuracy:  outlineCitationAccuracy(limited, query.ExpectedSources),
		FinalContextTokens:      contextTokens(limited),
	}
}

func outlineSupportCoverage(results []rag.SearchResult, expectedSources []string) float64 {
	if len(expectedSources) == 0 {
		return 0
	}
	found := make(map[string]struct{}, len(expectedSources))
	expected := make(map[string]struct{}, len(expectedSources))
	for _, source := range expectedSources {
		expected[source] = struct{}{}
	}
	for _, result := range results {
		if _, ok := expected[result.Chunk.Source]; ok {
			found[result.Chunk.Source] = struct{}{}
		}
	}
	return float64(len(found)) / float64(len(expected))
}

func outlineCitationAccuracy(results []rag.SearchResult, expectedSources []string) float64 {
	if len(results) == 0 {
		return 0
	}
	expected := make(map[string]struct{}, len(expectedSources))
	for _, source := range expectedSources {
		expected[source] = struct{}{}
	}
	hits := 0
	for _, result := range results {
		if _, ok := expected[result.Chunk.Source]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(results))
}

func outlineResultSources(results []rag.SearchResult) []string {
	sources := make([]string, len(results))
	for i := range results {
		sources[i] = results[i].Chunk.Source
	}
	return sources
}

func equalResultOrdering(left, right []rag.SearchResult) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Chunk.ID != right[i].Chunk.ID {
			return false
		}
	}
	return true
}

func summarizeOutlineMode(
	queries []OutlineQueryReport,
	latencies, allocatedBytes, allocationCounts, candidatesInspected, rankedCandidates, hydratedChunks []float64,
) OutlineModeSummary {
	summary := OutlineModeSummary{
		LatencyMS:             summarizeLatencies(latencies),
		AllocatedBytes:        averageFloat64(allocatedBytes),
		AllocationCount:       averageFloat64(allocationCounts),
		CandidatesInspected:   averageFloat64(candidatesInspected),
		RankedCandidates:      averageFloat64(rankedCandidates),
		HydratedContentChunks: averageFloat64(hydratedChunks),
		DeterministicOrdering: true,
	}
	for _, query := range queries {
		summary.DeterministicOrdering = summary.DeterministicOrdering && query.DeterministicOrdering
		for _, metric := range query.Metrics {
			switch metric.K {
			case 5:
				summary.RecallAt5 += metric.Recall
				summary.MRRAt5 += metric.MRR
				summary.ExpectedSupportCoverageAt5 += metric.ExpectedSupportCoverage
				summary.CitationSourceAccuracyAt5 += metric.CitationSourceAccuracy
				summary.FinalContextTokensAt5 += float64(metric.FinalContextTokens)
			case 10:
				summary.RecallAt10 += metric.Recall
				summary.MRRAt10 += metric.MRR
				summary.ExpectedSupportCoverageAt10 += metric.ExpectedSupportCoverage
				summary.CitationSourceAccuracyAt10 += metric.CitationSourceAccuracy
				summary.FinalContextTokensAt10 += float64(metric.FinalContextTokens)
			}
		}
	}
	if len(queries) > 0 {
		count := float64(len(queries))
		summary.RecallAt5 /= count
		summary.RecallAt10 /= count
		summary.MRRAt5 /= count
		summary.MRRAt10 /= count
		summary.ExpectedSupportCoverageAt5 /= count
		summary.ExpectedSupportCoverageAt10 /= count
		summary.CitationSourceAccuracyAt5 /= count
		summary.CitationSourceAccuracyAt10 /= count
		summary.FinalContextTokensAt5 /= count
		summary.FinalContextTokensAt10 /= count
	}
	return summary
}

func averageFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func outlineConclusion(modes []OutlineModeReport) string {
	var outline, resident *OutlineModeSummary
	for i := range modes {
		switch modes[i].Name {
		case "outline_then_content":
			outline = &modes[i].Summary
		case "resident_exact":
			resident = &modes[i].Summary
		}
	}
	if outline == nil || resident == nil {
		return "iterate"
	}
	if outlineDominates(*outline, *resident) && (outline.RecallAt5 > 0 || outline.RecallAt10 > 0) {
		return "keep"
	}
	if outlineDominates(*resident, *outline) {
		return "abandon"
	}
	return "iterate"
}

func outlineDominates(candidate, baseline OutlineModeSummary) bool {
	strict := false
	if !candidate.DeterministicOrdering && baseline.DeterministicOrdering {
		return false
	}
	if candidate.DeterministicOrdering && !baseline.DeterministicOrdering {
		strict = true
	}
	higherIsBetter := [8][2]float64{
		{candidate.RecallAt5, baseline.RecallAt5},
		{candidate.RecallAt10, baseline.RecallAt10},
		{candidate.MRRAt5, baseline.MRRAt5},
		{candidate.MRRAt10, baseline.MRRAt10},
		{candidate.ExpectedSupportCoverageAt5, baseline.ExpectedSupportCoverageAt5},
		{candidate.ExpectedSupportCoverageAt10, baseline.ExpectedSupportCoverageAt10},
		{candidate.CitationSourceAccuracyAt5, baseline.CitationSourceAccuracyAt5},
		{candidate.CitationSourceAccuracyAt10, baseline.CitationSourceAccuracyAt10},
	}
	for _, values := range higherIsBetter {
		if values[0] < values[1] {
			return false
		}
		strict = strict || values[0] > values[1]
	}
	lowerIsBetter := [9][2]float64{
		{candidate.FinalContextTokensAt5, baseline.FinalContextTokensAt5},
		{candidate.FinalContextTokensAt10, baseline.FinalContextTokensAt10},
		{candidate.LatencyMS.P50, baseline.LatencyMS.P50},
		{candidate.LatencyMS.P95, baseline.LatencyMS.P95},
		{candidate.AllocatedBytes, baseline.AllocatedBytes},
		{candidate.AllocationCount, baseline.AllocationCount},
		{candidate.CandidatesInspected, baseline.CandidatesInspected},
		{candidate.RankedCandidates, baseline.RankedCandidates},
		{candidate.HydratedContentChunks, baseline.HydratedContentChunks},
	}
	for _, values := range lowerIsBetter {
		if values[0] > values[1] {
			return false
		}
		strict = strict || values[0] < values[1]
	}
	return strict
}
