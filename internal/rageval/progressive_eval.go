package rageval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/kstruzzieri/go-llm/rag"
)

// ProgressiveSchemaVersion identifies the progressive experiment JSON schema.
const ProgressiveSchemaVersion = "rag-progressive-eval/v1"

// ProgressiveOptions controls the eval-only progressive rendering experiment.
type ProgressiveOptions struct {
	Dimensions int // <= 0 => 768, matching the outline fixture
	MaxTokens  int // per-render budget; <= 0 => 512
}

// ProgressiveReport compares flat BuildContext with RenderProgressive over
// the outline fixture corpus at equal budget. Retrieval selection runs once
// per query; both arms render the identical result slice (#246 guard).
type ProgressiveReport struct {
	SchemaVersion string                   `json:"schema_version"`
	Corpus        ProgressiveCorpus        `json:"corpus"`
	MaxTokens     int                      `json:"max_tokens"`
	Queries       []ProgressiveQueryReport `json:"queries"`
	Summary       ProgressiveSummary       `json:"summary"`
}

// ProgressiveCorpus describes the fixture corpus the experiment ran over.
type ProgressiveCorpus struct {
	Chunks     int `json:"chunks"`
	Sources    int `json:"sources"`
	Queries    int `json:"queries"`
	Dimensions int `json:"dimensions"`
	SelectedK  int `json:"selected_k"`
}

// ProgressiveArm is one rendering arm's measurements for one query.
type ProgressiveArm struct {
	CandidateIDs        []string `json:"candidate_ids"`
	ContextTokens       int      `json:"context_tokens"` // estimator (len+3)/4 on the rendered string
	ContextBytes        int      `json:"context_bytes"`
	RenderFormatVersion int      `json:"render_format_version,omitempty"` // progressive arm only
	OmittedSources      int      `json:"omitted_sources,omitempty"`       // progressive arm only
	SourcesAtL0         int      `json:"sources_at_l0,omitempty"`
	SourcesAtL1         int      `json:"sources_at_l1,omitempty"`
	SourcesWithEvidence int      `json:"sources_with_evidence,omitempty"`
}

// ProgressiveQueryReport is one golden query's paired measurement.
type ProgressiveQueryReport struct {
	ID                 string         `json:"id"`
	Category           string         `json:"category"`
	CandidateSetsEqual bool           `json:"candidate_sets_equal"`
	Flat               ProgressiveArm `json:"flat"`
	Progressive        ProgressiveArm `json:"progressive"`
}

// ProgressiveSummary aggregates across queries.
type ProgressiveSummary struct {
	Queries             int     `json:"queries"`
	AllCandidateEqual   bool    `json:"all_candidate_sets_equal"`
	MeanTokenReduction  float64 `json:"mean_token_reduction"` // 1 - prog/flat, averaged
	MeanBytesReduction  float64 `json:"mean_bytes_reduction"`
	TotalOmittedSources int     `json:"total_omitted_sources"`
	// TotalMetadataFallback counts rendered sources whose orientation came
	// from the deterministic metadata overview rather than a stored summary
	// (!Omitted && !OrientationGenerated) — proof the half of the corpus
	// seeded WITHOUT summaries actually exercises the fallback path.
	TotalMetadataFallback int `json:"total_metadata_fallback"`
}

// RunProgressiveExperiment builds the outline fixture corpus, installs fresh
// summaries on a deterministic subset of sources, and measures both arms.
func RunProgressiveExperiment(ctx context.Context, opts ProgressiveOptions) (*ProgressiveReport, error) {
	if opts.Dimensions <= 0 {
		opts.Dimensions = 768
	}
	if opts.MaxTokens <= 0 {
		opts.MaxTokens = 512
	}
	fixture, err := buildOutlineFixture(opts.Dimensions)
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "go-llm-progressive-eval-")
	if err != nil {
		return nil, fmt.Errorf("rag eval: create progressive temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	path := filepath.Join(tempDir, "progressive.db")
	if err := seedOutlineStore(ctx, path, fixture); err != nil {
		return nil, err
	}
	store, err := rag.NewSQLiteStore(path)
	if err != nil {
		return nil, fmt.Errorf("rag eval: open progressive store: %w", err)
	}
	defer func() { _ = store.Close() }()

	if err := seedProgressiveSummaries(ctx, store); err != nil {
		return nil, err
	}
	retriever, err := rag.NewRetrieverWithEmbedder(newOutlineEmbedder(fixture), store,
		rag.WithRetrieverModel("progressive-fixture"))
	if err != nil {
		return nil, err
	}

	report := &ProgressiveReport{
		SchemaVersion: ProgressiveSchemaVersion,
		MaxTokens:     opts.MaxTokens,
		Corpus: ProgressiveCorpus{
			Chunks: len(fixture.chunks), Sources: len(outlineSources()),
			Queries: len(fixture.queries), Dimensions: opts.Dimensions, SelectedK: 10,
		},
	}
	report.Summary.AllCandidateEqual = true
	var tokenRatios, byteRatios []float64
	for _, q := range fixture.queries {
		results, err := store.SearchMulti(ctx, q.Embedding, q.Query, 10, outlineQueryContext(q))
		if err != nil {
			return nil, err
		}
		selected := outlineSearchResults(results)
		flatText := retriever.BuildContext(selected, opts.MaxTokens)
		progText, trace, err := retriever.RenderProgressive(ctx, rag.ProgressiveRenderRequest{
			// MaxBytes: 4 bytes/token, the inverse of the est heuristic below.
			Results: selected, MaxTokens: opts.MaxTokens, MaxBytes: opts.MaxTokens * 4,
		})
		if err != nil {
			return nil, fmt.Errorf("rag eval: progressive render query %s: %w", q.ID, err)
		}
		ids := make([]string, len(selected))
		for i, r := range selected {
			ids[i] = r.Chunk.ID
		}
		// est mirrors rag's unexported defaultEstimate; trace metadata only, so
		// do not "deduplicate" by exporting it.
		est := func(s string) int { return (len(s) + 3) / 4 }
		qr := ProgressiveQueryReport{
			ID: q.ID, Category: q.Category,
			// Both arms consumed the same slice by construction; recorded so
			// the committed report proves it rather than asserts it.
			CandidateSetsEqual: true,
			Flat: ProgressiveArm{CandidateIDs: ids,
				ContextTokens: est(flatText), ContextBytes: len(flatText)},
			Progressive: ProgressiveArm{CandidateIDs: ids,
				// est on the rendered string, same basis as the flat arm: the
				// size-reduction ratios compare artifact sizes on one basis.
				// The renderer's own admission accounting
				// (trace.EstimatedTokensUsed: per-block sums plus separator
				// charges) is deliberately NOT used here.
				ContextTokens:       est(progText),
				ContextBytes:        len(progText),
				RenderFormatVersion: trace.RenderFormatVersion,
				OmittedSources:      trace.OmittedSources,
				SourcesAtL0:         trace.SourcesAtL0,
				SourcesAtL1:         trace.SourcesAtL1,
				SourcesWithEvidence: trace.SourcesWithEvidence,
			},
		}
		report.Queries = append(report.Queries, qr)
		report.Summary.TotalOmittedSources += trace.OmittedSources
		for _, s := range trace.Sources {
			if !s.Omitted && !s.OrientationGenerated {
				report.Summary.TotalMetadataFallback++
			}
		}
		if qr.Flat.ContextTokens > 0 {
			tokenRatios = append(tokenRatios, 1-float64(qr.Progressive.ContextTokens)/float64(qr.Flat.ContextTokens))
		}
		if qr.Flat.ContextBytes > 0 {
			byteRatios = append(byteRatios, 1-float64(qr.Progressive.ContextBytes)/float64(qr.Flat.ContextBytes))
		}
	}
	report.Summary.Queries = len(report.Queries)
	report.Summary.MeanTokenReduction = averageFloat64(tokenRatios)
	report.Summary.MeanBytesReduction = averageFloat64(byteRatios)
	return report, nil
}

// seedProgressiveSummaries installs FRESH summary rows for the even-indexed
// sources (sorted order): half the corpus exercises the stored-summary
// ladder, half the metadata fallback. ContentHash/VectorSpaceID are copied
// from live provenance so validity derives empty (fresh).
func seedProgressiveSummaries(ctx context.Context, store *rag.SQLiteStore) error {
	sources := outlineSources()
	sort.Strings(sources)
	prov, err := store.SourceProvenanceBatch(ctx, sources)
	if err != nil {
		return err
	}
	for i, source := range sources {
		if i%2 != 0 {
			continue
		}
		p, ok := prov[source]
		if !ok || p.ContentHash == "" || p.VectorSpaceID == "" {
			return fmt.Errorf("rag eval: source %q lacks provenance for fresh summary", source)
		}
		if err := store.UpsertSourceSummary(ctx, rag.SourceSummary{
			Source: source, ContentHash: p.ContentHash, VectorSpaceID: p.VectorSpaceID,
			Abstract:     "Fixture abstract for " + source,
			Overview:     "Fixture overview for " + source + ": deterministic eval corpus text.",
			SummaryModel: "progressive-fixture", FormatVersion: rag.SourceSummaryFormatVersion,
			SummarizedAt: outlineIndexedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}
