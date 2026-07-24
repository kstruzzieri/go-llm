package rageval

import (
	"context"
	"fmt"
	"sort"

	"github.com/kstruzzieri/go-llm/rag"
)

const outlineIndexedAt = int64(1_700_000_000)
const outlineNeutralCurrentFile = "workspace/neutral.go"

// Corpus shape is defined once here and derived everywhere else so the fixture
// and its source pool cannot drift apart. TestBuildOutlineFixtureContract locks
// the resulting totals (chunks, sources, queries, per-category counts).
const (
	// outlineSupportsPerQuery is the number of expected support chunks per query.
	outlineSupportsPerQuery = 2
	// outlineQueriesPerCategory is the number of golden queries per category.
	outlineQueriesPerCategory = 4
	// outlineCorpusChunks is the fixed total chunk count (supports + filler).
	outlineCorpusChunks = 1401
	// outlineCorpusSources is the fixed total distinct source-path count.
	outlineCorpusSources = 138
)

// outlineCategories are the golden-query categories, in fixed order. Each maps
// to a distinct support-source naming scheme in outlineSources.
var outlineCategories = []string{
	"direct_symbol",
	"path_and_pairing",
	"outline_summary",
	"content_only",
	"distributed_support",
}

// outlineTotalQueries is the derived golden-query count.
func outlineTotalQueries() int { return len(outlineCategories) * outlineQueriesPerCategory }

type outlineFixture struct {
	chunks     []rag.Chunk
	embeddings map[string][]float64
	queries    []outlineQuery
}

type outlineQuery struct {
	ID              string
	Category        string
	Query           string
	ExpectedIDs     []string
	ExpectedSources []string
	CurrentFile     string
	Embedding       []float64
}

func buildOutlineFixture(dim int) (outlineFixture, error) {
	if dim <= 0 {
		return outlineFixture{}, fmt.Errorf("rag eval: outline fixture dimension must be positive")
	}

	fixture := outlineFixture{embeddings: make(map[string][]float64, outlineCorpusChunks)}
	sources := outlineSources()
	totalQueries := outlineTotalQueries()
	if need := totalQueries * outlineSupportsPerQuery; len(sources) < need {
		return outlineFixture{}, fmt.Errorf("rag eval: outline source pool %d < required %d support sources", len(sources), need)
	}
	if need := totalQueries * outlineSupportsPerQuery; outlineCorpusChunks < need {
		return outlineFixture{}, fmt.Errorf("rag eval: outline corpus target %d < required %d support chunks", outlineCorpusChunks, need)
	}
	for categoryIndex, category := range outlineCategories {
		for n := 0; n < outlineQueriesPerCategory; n++ {
			queryIndex := categoryIndex*outlineQueriesPerCategory + n
			supports := [outlineSupportsPerQuery]rag.Chunk{
				outlineSupport(queryIndex, 0, category, sources[queryIndex*outlineSupportsPerQuery]),
				outlineSupport(queryIndex, 1, category, sources[queryIndex*outlineSupportsPerQuery+1]),
			}
			vector := xorshiftVector(uint64(queryIndex+1), dim)
			currentFile := supports[0].Source
			if category == "distributed_support" {
				currentFile = outlineNeutralCurrentFile
			}
			query := outlineQuery{
				ID:              fmt.Sprintf("outline-query-%02d", queryIndex),
				Category:        category,
				Query:           outlineQueryText(queryIndex, category),
				CurrentFile:     currentFile,
				Embedding:       append([]float64(nil), vector...),
				ExpectedIDs:     []string{supports[0].ID, supports[1].ID},
				ExpectedSources: []string{supports[0].Source, supports[1].Source},
			}
			for _, chunk := range supports {
				fixture.chunks = append(fixture.chunks, chunk)
				fixture.embeddings[chunk.ID] = append([]float64(nil), vector...)
			}
			fixture.queries = append(fixture.queries, query)
		}
	}

	for i := len(fixture.chunks); i < outlineCorpusChunks; i++ {
		chunk := rag.Chunk{
			ID:        fmt.Sprintf("outline-filler-%04d", i),
			Source:    sources[i%len(sources)],
			StartLine: 1,
			EndLine:   5,
			Language:  "go",
			StableKey: fmt.Sprintf("%s::Filler%04d#0", sources[i%len(sources)], i),
			Content:   fmt.Sprintf("package fixture\n\n// Filler%04d is deterministic corpus noise.\nfunc Filler%04d() int { return %d }\n", i, i, i),
			Metadata:  map[string]string{"package": "fixture", "symbol": fmt.Sprintf("Filler%04d", i)},
		}
		fixture.chunks = append(fixture.chunks, chunk)
		fixture.embeddings[chunk.ID] = xorshiftVector(uint64(1000+i), dim)
	}
	return fixture, nil
}

func outlineSources() []string {
	sources := make([]string, 0, outlineCorpusSources)
	// Each query gets outlineSupportsPerQuery named sources whose naming scheme
	// is chosen by its category (queryIndex / outlineQueriesPerCategory), in the
	// fixed outlineCategories order.
	for queryIndex := 0; queryIndex < outlineTotalQueries(); queryIndex++ {
		switch queryIndex / outlineQueriesPerCategory {
		case 0: // direct_symbol
			sources = append(sources, fmt.Sprintf("internal/direct%02d/resolver.go", queryIndex), fmt.Sprintf("internal/direct%02d/types.go", queryIndex))
		case 1: // path_and_pairing
			sources = append(sources, fmt.Sprintf("internal/pairing%02d/handler.go", queryIndex), fmt.Sprintf("internal/pairing%02d/handler_test.go", queryIndex))
		case 2: // outline_summary
			sources = append(sources, fmt.Sprintf("internal/summary%02d/service.go", queryIndex), fmt.Sprintf("internal/summary%02d/model.go", queryIndex))
		case 3: // content_only
			sources = append(sources, fmt.Sprintf("internal/content%02d/service.go", queryIndex), fmt.Sprintf("internal/content%02d/model.go", queryIndex))
		default: // distributed_support
			sources = append(sources, fmt.Sprintf("cmd/distributed%02d/main.go", queryIndex), fmt.Sprintf("pkg/distributed%02d/support.go", queryIndex))
		}
	}
	for i := len(sources); i < outlineCorpusSources; i++ {
		sources = append(sources, fmt.Sprintf("internal/filler%03d/file.go", i))
	}
	return sources
}

func outlineSupport(queryIndex, supportIndex int, category, source string) rag.Chunk {
	id := fmt.Sprintf("outline-support-%02d-%d", queryIndex, supportIndex)
	symbol := fmt.Sprintf("Support%02dPart%d", queryIndex, supportIndex)
	metadata := map[string]string{"package": "fixture", "symbol": symbol, "heading": "outline support"}
	content := fmt.Sprintf("package fixture\n\nfunc %s() {}\n", symbol)
	switch category {
	case "direct_symbol":
		symbol = fmt.Sprintf("ResolveDirect%02d", queryIndex)
		metadata["symbol"] = symbol
		content = fmt.Sprintf("package fixture\n\nfunc %s() {}\n", symbol)
	case "path_and_pairing":
		metadata["source_test_pair"] = fmt.Sprintf("pairing-%02d", queryIndex)
		content = fmt.Sprintf("package fixture\n\nfunc %s() {}\n", symbol)
	case "outline_summary":
		metadata["doc"] = fmt.Sprintf("summary-cue-%02d", queryIndex)
		content = fmt.Sprintf("package fixture\n\nfunc %s() {}\n", symbol)
	case "content_only":
		content = fmt.Sprintf("package fixture\n\nfunc %s() string { return \"content-cue-%02d\" }\n", symbol, queryIndex)
	case "distributed_support":
		metadata["doc"] = fmt.Sprintf("distributed-cue-%02d", queryIndex)
		content = fmt.Sprintf("package fixture\n\nfunc %s() {}\n", symbol)
	}
	return rag.Chunk{
		ID:        id,
		Source:    source,
		StartLine: 1,
		EndLine:   3,
		Language:  "go",
		StableKey: fmt.Sprintf("%s::%s#0", source, symbol),
		Content:   content,
		Metadata:  metadata,
	}
}

func outlineQueryText(queryIndex int, category string) string {
	switch category {
	case "direct_symbol":
		return fmt.Sprintf("Where is ResolveDirect%02d declared?", queryIndex)
	case "path_and_pairing":
		return fmt.Sprintf("Find the production and test pairing-%02d files.", queryIndex)
	case "outline_summary":
		return fmt.Sprintf("What implements summary-cue-%02d?", queryIndex)
	case "content_only":
		return fmt.Sprintf("Which code returns content-cue-%02d?", queryIndex)
	default:
		return fmt.Sprintf("Find the distributed-cue-%02d support.", queryIndex)
	}
}

func xorshiftVector(seed uint64, dim int) []float64 {
	vector := make([]float64, dim)
	for i := range vector {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		vector[i] = float64(seed&0xffff)/32768 - 1
	}
	return vector
}

func outlineIdentity(chunk rag.Chunk) string {
	if chunk.StableKey != "" {
		return "stable:" + chunk.StableKey
	}
	return "id:" + chunk.ID
}

func seedOutlineStore(ctx context.Context, path string, fixture outlineFixture) error {
	store, err := rag.NewSQLiteStore(path)
	if err != nil {
		return fmt.Errorf("rag eval: create outline store: %w", err)
	}
	grouped := make(map[string][]rag.Chunk)
	for _, chunk := range fixture.chunks {
		grouped[chunk.Source] = append(grouped[chunk.Source], chunk)
	}
	sources := make([]string, 0, len(grouped))
	for source := range grouped {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		chunks := grouped[source]
		embeddings := make([][]float64, len(chunks))
		for i, chunk := range chunks {
			embeddings[i] = fixture.embeddings[chunk.ID]
		}
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, source, chunks, embeddings, "outline-fixture:"+source, vectorSpaceID); err != nil {
			_ = store.Close()
			return fmt.Errorf("rag eval: seed outline source %q: %w", source, err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE chunks SET indexed_at = ?", outlineIndexedAt); err != nil {
		_ = store.Close()
		return fmt.Errorf("rag eval: set outline indexed_at: %w", err)
	}
	if err := store.Close(); err != nil {
		return fmt.Errorf("rag eval: close outline store: %w", err)
	}
	return nil
}
