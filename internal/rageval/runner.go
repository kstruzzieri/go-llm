package rageval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/kstruzzieri/go-llm/rag"
)

// Run executes static and hybrid retrieval against the fixture.
func Run(ctx context.Context, fixture *Fixture, opts RunOptions) (*Report, error) {
	opts = defaultRunOptions(opts)
	if err := fixture.Validate(); err != nil {
		return nil, err
	}

	store, err := buildStore(ctx, fixture)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()

	embedder := newFixtureEmbedder(fixture)
	retriever, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel("fixture-embedding"))
	if err != nil {
		return nil, fmt.Errorf("rag eval: create retriever: %w", err)
	}

	static, err := runMode(ctx, "static", fixture, opts, func(ctx context.Context, q QueryFixture, k int) ([]rag.SearchResult, error) {
		return retriever.Retrieve(ctx, q.Query, k)
	})
	if err != nil {
		return nil, err
	}

	hybrid, err := runMode(ctx, "hybrid_search_multi", fixture, opts, func(ctx context.Context, q QueryFixture, k int) ([]rag.SearchResult, error) {
		results, err := store.SearchMulti(ctx, q.Embedding, q.Query, k, rag.QueryContext{
			CurrentFile: q.CurrentFile,
			Timestamp:   time.Unix(1_700_000_000, 0),
		})
		if err != nil {
			return nil, err
		}
		searchResults := make([]rag.SearchResult, len(results))
		for i, result := range results {
			searchResults[i] = result.SearchResult
		}
		return searchResults, nil
	})
	if err != nil {
		return nil, err
	}

	return &Report{
		SchemaVersion: SchemaVersion,
		Corpus:        summarizeFixture(fixture),
		Thresholds: ThresholdSummary{
			Owner:  "Keith",
			Status: "pending_owner_values_before_95",
		},
		Modes: []ModeReport{*static, *hybrid},
	}, nil
}

// WriteReport writes a stable, pretty JSON report.
func WriteReport(path string, report *Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("rag eval: marshal report: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("rag eval: write report: %w", err)
	}
	return nil
}

type retrieveFunc func(context.Context, QueryFixture, int) ([]rag.SearchResult, error)

func runMode(ctx context.Context, mode string, fixture *Fixture, opts RunOptions, retrieve retrieveFunc) (*ModeReport, error) {
	queryReports := make([]QueryReport, 0, len(fixture.Queries))
	var coldLatencies []float64
	var warmLatencies []float64

	for _, query := range fixture.Queries {
		var results []rag.SearchResult
		coldMS, err := measure(opts.MeasureLatency, func() error {
			var retrieveErr error
			results, retrieveErr = retrieve(ctx, query, 10)
			return retrieveErr
		})
		if err != nil {
			return nil, fmt.Errorf("rag eval: %s retrieve %q: %w", mode, query.ID, err)
		}
		coldLatencies = append(coldLatencies, coldMS)

		queryWarm := make([]float64, 0, opts.WarmRuns)
		for i := 0; i < opts.WarmRuns; i++ {
			warmMS, err := measure(opts.MeasureLatency, func() error {
				_, retrieveErr := retrieve(ctx, query, 10)
				return retrieveErr
			})
			if err != nil {
				return nil, fmt.Errorf("rag eval: %s warm retrieve %q: %w", mode, query.ID, err)
			}
			queryWarm = append(queryWarm, warmMS)
			warmLatencies = append(warmLatencies, warmMS)
		}

		queryReports = append(queryReports, QueryReport{
			ID:            query.ID,
			Category:      query.Category,
			ExpectedIDs:   append([]string(nil), query.ExpectedIDs...),
			ResultIDs:     resultIDs(results),
			Metrics:       []KMetrics{computeKMetrics(results, query.ExpectedIDs, 5), computeKMetrics(results, query.ExpectedIDs, 10)},
			ColdLatencyMS: coldMS,
			WarmLatencyMS: summarizeLatencies(queryWarm),
		})
	}

	return &ModeReport{
		Name:    mode,
		Summary: summarizeMode(queryReports, coldLatencies, warmLatencies),
		Queries: queryReports,
	}, nil
}

func measure(enabled bool, fn func() error) (float64, error) {
	if !enabled {
		return 0, fn()
	}
	start := time.Now()
	err := fn()
	return elapsedMS(time.Since(start)), err
}

func buildStore(ctx context.Context, fixture *Fixture) (*rag.SQLiteStore, error) {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		return nil, fmt.Errorf("rag eval: create sqlite store: %w", err)
	}
	grouped := make(map[string][]int)
	for i, chunk := range fixture.Corpus {
		grouped[chunk.Source] = append(grouped[chunk.Source], i)
	}
	sources := make([]string, 0, len(grouped))
	for source := range grouped {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		indexes := grouped[source]
		chunks := make([]rag.Chunk, 0, len(indexes))
		embeddings := make([][]float64, 0, len(indexes))
		for _, idx := range indexes {
			fixtureChunk := fixture.Corpus[idx]
			chunks = append(chunks, rag.Chunk{
				ID:        fixtureChunk.ID,
				Source:    fixtureChunk.Source,
				StartLine: fixtureChunk.StartLine,
				EndLine:   fixtureChunk.EndLine,
				Language:  fixtureChunk.Language,
				StableKey: fixtureChunk.StableKey,
				Content:   fixtureChunk.Content,
				Metadata:  fixtureChunk.Metadata,
			})
			embeddings = append(embeddings, append([]float64(nil), fixtureChunk.Embedding...))
		}
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, source, chunks, embeddings, "fixture:"+source, vectorSpaceID); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("rag eval: seed source %q: %w", source, err)
		}
	}
	return store, nil
}

func summarizeFixture(fixture *Fixture) CorpusSummary {
	categories := make(map[string]int)
	for _, query := range fixture.Queries {
		categories[query.Category]++
	}
	return CorpusSummary{
		Chunks:     len(fixture.Corpus),
		Queries:    len(fixture.Queries),
		Categories: categories,
		TopK:       []int{5, 10},
		VectorID:   vectorSpaceID,
		Notes: []string{
			"fixtures use deterministic synthetic embeddings; no live Ollama dependency",
			"threshold values are owner-gated before #95",
		},
	}
}

func summarizeMode(queries []QueryReport, cold, warm []float64) ModeSummary {
	return ModeSummary{
		RecallAt5:                averageMetric(queries, 5, func(m KMetrics) float64 { return m.Recall }),
		RecallAt10:               averageMetric(queries, 10, func(m KMetrics) float64 { return m.Recall }),
		MRRAt5:                   averageMetric(queries, 5, func(m KMetrics) float64 { return m.MRR }),
		MRRAt10:                  averageMetric(queries, 10, func(m KMetrics) float64 { return m.MRR }),
		DuplicateRateAt10:        averageMetric(queries, 10, func(m KMetrics) float64 { return m.DuplicateRate }),
		ContextPrecisionAt5:      averageMetric(queries, 5, func(m KMetrics) float64 { return m.ContextPrecisionProxy }),
		ContextPrecisionAt10:     averageMetric(queries, 10, func(m KMetrics) float64 { return m.ContextPrecisionProxy }),
		AverageContextTokensAt5:  averageMetric(queries, 5, func(m KMetrics) float64 { return float64(m.ContextTokens) }),
		AverageContextTokensAt10: averageMetric(queries, 10, func(m KMetrics) float64 { return float64(m.ContextTokens) }),
		ColdLatencyMS:            summarizeLatencies(cold),
		WarmLatencyMS:            summarizeLatencies(warm),
	}
}

func averageMetric(queries []QueryReport, k int, pick func(KMetrics) float64) float64 {
	if len(queries) == 0 {
		return 0
	}
	var total float64
	for _, query := range queries {
		for _, metric := range query.Metrics {
			if metric.K == k {
				total += pick(metric)
				break
			}
		}
	}
	return total / float64(len(queries))
}

func computeKMetrics(results []rag.SearchResult, expectedIDs []string, k int) KMetrics {
	limited := limitResults(results, k)
	return KMetrics{
		K:                     k,
		Recall:                recall(limited, expectedIDs),
		MRR:                   reciprocalRank(limited, expectedIDs),
		DuplicateRate:         duplicateRate(limited),
		ContextPrecisionProxy: contextPrecision(limited, expectedIDs),
		ContextTokens:         contextTokens(limited),
	}
}

func limitResults(results []rag.SearchResult, k int) []rag.SearchResult {
	if k <= 0 || len(results) <= k {
		return results
	}
	return results[:k]
}

func resultIDs(results []rag.SearchResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.Chunk.ID
	}
	return ids
}

func recall(results []rag.SearchResult, expectedIDs []string) float64 {
	if len(expectedIDs) == 0 {
		return 0
	}
	expected := set(expectedIDs)
	hits := 0
	for _, result := range results {
		if expected[result.Chunk.ID] {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

func reciprocalRank(results []rag.SearchResult, expectedIDs []string) float64 {
	expected := set(expectedIDs)
	for i, result := range results {
		if expected[result.Chunk.ID] {
			return 1 / float64(i+1)
		}
	}
	return 0
}

func duplicateRate(results []rag.SearchResult) float64 {
	if len(results) == 0 {
		return 0
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		seen[result.Chunk.ID] = struct{}{}
	}
	return float64(len(results)-len(seen)) / float64(len(results))
}

func contextPrecision(results []rag.SearchResult, expectedIDs []string) float64 {
	if len(results) == 0 {
		return 0
	}
	expected := set(expectedIDs)
	hits := 0
	for _, result := range results {
		if expected[result.Chunk.ID] {
			hits++
		}
	}
	return float64(hits) / float64(len(results))
}

func contextTokens(results []rag.SearchResult) int {
	var retriever rag.Retriever
	contextText := retriever.BuildContext(results, 0)
	return (len(contextText) + 3) / 4
}

func set(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func summarizeLatencies(values []float64) LatencySummary {
	if len(values) == 0 {
		return LatencySummary{}
	}
	cp := append([]float64(nil), values...)
	sort.Float64s(cp)
	return LatencySummary{
		Count: len(cp),
		P50:   percentile(cp, 0.50),
		P95:   percentile(cp, 0.95),
	}
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	idx := int(pos)
	if idx >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := pos - float64(idx)
	return sorted[idx] + (sorted[idx+1]-sorted[idx])*frac
}

type fixtureEmbedder struct {
	byQuery map[string][]float64
}

func newFixtureEmbedder(fixture *Fixture) *fixtureEmbedder {
	byQuery := make(map[string][]float64, len(fixture.Queries))
	for _, query := range fixture.Queries {
		byQuery[query.Query] = append([]float64(nil), query.Embedding...)
	}
	return &fixtureEmbedder{byQuery: byQuery}
}

func (e *fixtureEmbedder) Embed(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
	if len(inputs) != 1 {
		return rag.EmbedResult{}, fmt.Errorf("rag eval: fixture embedder expects 1 input, got %d", len(inputs))
	}
	embedding, ok := e.byQuery[inputs[0]]
	if !ok {
		return rag.EmbedResult{}, fmt.Errorf("rag eval: no fixture embedding for query %q", inputs[0])
	}
	return rag.EmbedResult{
		Embeddings:    [][]float64{append([]float64(nil), embedding...)},
		Provider:      "fixture",
		Model:         "rag-eval-v1",
		VectorSpaceID: vectorSpaceID,
	}, nil
}
