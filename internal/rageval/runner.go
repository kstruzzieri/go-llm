//go:generate go run ../../cmd/rag-eval -fixtures testdata/fixtures.json -out testdata/baseline.json -no-latency

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

// contextFormatter mirrors rag.Retriever.BuildContext for token estimation.
//
// IMPORTANT CONTRACT: BuildContext is currently a pure formatter (no receiver
// state). We rely on that to call it via a zero-value Retriever singleton. If
// BuildContext ever reads receiver fields, contextTokens silently produces
// wrong numbers. Grep for callers of BuildContext before changing it.
var contextFormatter rag.Retriever

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
	// WithVectorOnly keeps the "static" mode a true dense-cosine baseline
	// (SQLiteStore.Search). Without it, Retrieve detects the store's
	// MultiSignalSearcher capability and runs hybrid RRF with an empty
	// QueryContext, mislabeling the baseline (#275).
	retriever, err := rag.NewRetrieverWithEmbedder(embedder, store, rag.WithRetrieverModel("fixture-embedding"), rag.WithVectorOnly())
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
			Timestamp:   replayTimestamp,
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
		Thresholds:    buildThresholds(),
		Modes:         []ModeReport{*static, *hybrid},
	}, nil
}

// buildThresholds returns the v1 regression floors ratified by the owner.
//
// Posture: each non-null floor is set at "best known minus a small buffer" so
// real regressions trip CI without flapping on natural metric noise. Numbers
// are not aspirational — they catch drops, they do not chase improvements.
// Tightening floors after sustained improvement is a #95-and-later activity.
//
// Latency thresholds are intentionally null. The current baseline runs on a
// 12-chunk in-memory SQLite corpus with N=20 cold samples per mode; P95
// derived from that is statistically unstable and bears no relation to
// production workload. Setting a number here would either flap on noise or
// be ignored when #95 raises it against a realistic corpus — either way it
// teaches the team to distrust threshold breaches. Better null than theater.
func buildThresholds() ThresholdSummary {
	return ThresholdSummary{
		Owner:                             OwnerKeith,
		Status:                            StatusThresholdsRatified,
		MinimumStaticRecallAt5:            floatPtr(0.95), // current 0.9583; 1pp floor catches real regressions without flapping.
		MinimumHybridRecallAt5Improvement: floatPtr(0.0),  // hybrid currently matches static; "must not regress" until corpus discriminates.
		MaximumDuplicateRate:              floatPtr(0.05), // current 0; 5% tolerance for SearchMulti's diversification.
		MinimumContextPrecisionProxy:      floatPtr(0.50), // current 0.51 @K=5; below 0.50 means majority of returned chunks are irrelevant.
		MaximumAverageContextTokenGrowth:  floatPtr(0.10), // current both modes match; 10% growth flags context-bloat regression.
		MaximumOptInHybridP95LatencyMS:    nil,            // deferred to #95: synthetic corpus latency is noise.
		MaximumFutureDefaultP95LatencyMS:  nil,            // deferred to #95: synthetic corpus latency is noise.
	}
}

// WriteReport writes a stable, pretty JSON report.
func WriteReport(path string, report *Report) error {
	return writeJSONReport(path, report)
}

// WriteOutlineReport writes a stable, pretty JSON outline experiment report.
func WriteOutlineReport(path string, report *OutlineReport) error {
	return writeJSONReport(path, report)
}

func writeJSONReport(path string, report any) error {
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
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("rag eval: %s cancelled before %q: %w", mode, query.ID, err)
		}
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
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("rag eval: %s cancelled during warm runs of %q: %w", mode, query.ID, err)
			}
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
			"thresholds ratified for v1 regression detection; see runner.buildThresholds for posture rationale",
			"hybrid_search_multi regresses MRR@5 vs static (~0.825 vs 1.0) on this synthetic corpus — captured for #94 scorer-weight discussion, not flagged as a blocker",
			"latency thresholds intentionally null: N=20 cold samples on a 12-chunk in-memory SQLite corpus is statistically unstable; defer to #95 against realistic workload",
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

// computeKMetrics computes per-K metrics over the result set. If the retriever
// returns fewer than k items, the metrics reflect the available results — i.e.
// recall@k's denominator is len(expectedIDs), not k, so a "recall@5" against a
// retriever that only returned 3 results is computed over those 3.
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
	contextText := contextFormatter.BuildContext(results, 0)
	return (len(contextText) + charsPerTokenEstimate - 1) / charsPerTokenEstimate
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

// Embed looks up a precomputed embedding by exact query-text match.
//
// CONTRACT: requires inputs[0] to equal a QueryFixture.Query string exactly as
// it appears in fixtures.json. This works because rag.Retriever.Retrieve passes
// the query through to the embedder unchanged. If Retrieve ever normalizes
// (trim, lowercase, etc.) the query, lookup will fail with the "no fixture
// embedding" error below — diagnose by checking what Retrieve sent vs. what the
// fixture contains. See TestFixtureEmbedderRejectsUnknownQuery for the error
// shape this contract produces on miss.
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
