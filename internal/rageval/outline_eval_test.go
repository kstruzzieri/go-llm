package rageval

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestBuildOutlineFixtureContract(t *testing.T) {
	const dimension = 768

	fixture, err := buildOutlineFixture(dimension)
	if err != nil {
		t.Fatalf("buildOutlineFixture: %v", err)
	}
	if len(fixture.chunks) != 1401 {
		t.Fatalf("chunks = %d, want 1401", len(fixture.chunks))
	}
	if len(fixture.embeddings) != len(fixture.chunks) {
		t.Fatalf("embeddings = %d, want %d", len(fixture.embeddings), len(fixture.chunks))
	}

	ids := make(map[string]rag.Chunk, len(fixture.chunks))
	sources := make(map[string]struct{})
	for _, chunk := range fixture.chunks {
		if chunk.ID == "" || chunk.Source == "" {
			t.Fatalf("chunk missing id or source: %#v", chunk)
		}
		if outlineIdentity(chunk) == "" {
			t.Fatalf("chunk %q has empty identity", chunk.ID)
		}
		if got := len(fixture.embeddings[chunk.ID]); got != dimension {
			t.Fatalf("chunk %q embedding dimension = %d, want %d", chunk.ID, got, dimension)
		}
		ids[chunk.ID] = chunk
		sources[chunk.Source] = struct{}{}
	}
	if len(sources) != 138 {
		t.Fatalf("sources = %d, want 138", len(sources))
	}
	if len(fixture.queries) != 20 {
		t.Fatalf("queries = %d, want 20", len(fixture.queries))
	}

	queries := make(map[string]struct{}, len(fixture.queries))
	categories := make(map[string]int)
	for _, query := range fixture.queries {
		if query.Query == "" {
			t.Fatal("query text is empty")
		}
		if query.CurrentFile == "" {
			t.Fatalf("query %q has no current-file context", query.Query)
		}
		if _, exists := queries[query.Query]; exists {
			t.Fatalf("duplicate query text %q", query.Query)
		}
		queries[query.Query] = struct{}{}
		categories[query.Category]++
		if len(query.Embedding) != dimension {
			t.Fatalf("query %q embedding dimension = %d, want %d", query.Query, len(query.Embedding), dimension)
		}
		if len(query.ExpectedIDs) != 2 || len(query.ExpectedSources) != 2 {
			t.Fatalf("query %q expected supports = (%d ids, %d sources), want 2 each", query.Query, len(query.ExpectedIDs), len(query.ExpectedSources))
		}
		for i, id := range query.ExpectedIDs {
			chunk, ok := ids[id]
			if !ok {
				t.Fatalf("query %q references unknown chunk %q", query.Query, id)
			}
			if chunk.Source != query.ExpectedSources[i] {
				t.Fatalf("query %q source for %q = %q, want %q", query.Query, id, query.ExpectedSources[i], chunk.Source)
			}
		}
		if query.Category == "distributed_support" {
			if _, exists := sources[query.CurrentFile]; exists {
				t.Fatalf("distributed query %q current file %q exists in corpus, want neutral path", query.Query, query.CurrentFile)
			}
			for _, source := range query.ExpectedSources {
				if query.CurrentFile == source {
					t.Fatalf("distributed query %q current file = support source %q, want neutral path", query.Query, source)
				}
			}
		} else if query.CurrentFile != query.ExpectedSources[0] {
			t.Fatalf("query %q current file = %q, want %q", query.Query, query.CurrentFile, query.ExpectedSources[0])
		}
	}
	for _, category := range []string{"direct_symbol", "path_and_pairing", "outline_summary", "content_only", "distributed_support"} {
		if got := categories[category]; got != 4 {
			t.Fatalf("category %q queries = %d, want 4", category, got)
		}
	}
}

func TestOutlineIdentityUsesStableKeyThenID(t *testing.T) {
	stableA := outlineIdentity(rag.Chunk{ID: "first", StableKey: "shared"})
	stableB := outlineIdentity(rag.Chunk{ID: "second", StableKey: "shared"})
	if stableA == "" || stableA != stableB {
		t.Fatalf("equal stable keys produced identities %q and %q", stableA, stableB)
	}
	idA := outlineIdentity(rag.Chunk{ID: "first"})
	idB := outlineIdentity(rag.Chunk{ID: "second"})
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("empty-key IDs produced identities %q and %q", idA, idB)
	}
	if outlineIdentity(rag.Chunk{ID: "shared"}) == stableA {
		t.Fatal("ID identity collides with stable-key identity")
	}
}

func TestSeedOutlineStoreCreatesFileBackedCorpus(t *testing.T) {
	fixture, err := buildOutlineFixture(768)
	if err != nil {
		t.Fatalf("buildOutlineFixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "outline.db")
	if err := seedOutlineStore(context.Background(), path, fixture); err != nil {
		t.Fatalf("seedOutlineStore: %v", err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(path)
	if err != nil {
		t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
	}
	defer func() { _ = store.Close() }()
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalChunks != 1401 || stats.TotalSources != 138 || stats.EmbeddingDim != 768 {
		t.Fatalf("stats = %#v, want 1401 chunks, 138 sources, dim 768", stats)
	}
}

func TestBoundedUnionDeduplicatesByStableIdentity(t *testing.T) {
	semantic := []outlineCandidate{
		{Chunk: rag.Chunk{ID: "semantic-shared", StableKey: "shared"}},
		{Chunk: rag.Chunk{ID: "fallback-a"}},
	}
	keyword := []outlineCandidate{
		{Chunk: rag.Chunk{ID: "keyword-shared", StableKey: "shared"}},
		{Chunk: rag.Chunk{ID: "fallback-b"}},
	}

	got := boundedUnion(semantic, keyword)
	gotIDs := make([]string, len(got))
	for i := range got {
		gotIDs[i] = got[i].Chunk.ID
	}
	want := []string{"semantic-shared", "fallback-a", "fallback-b"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("boundedUnion IDs = %v, want %v", gotIDs, want)
	}
}

func TestOutlineSelectionUsesMetadataNotContent(t *testing.T) {
	contentOnly := rag.Chunk{ID: "content-only", StableKey: "z", Content: "contentmagic"}
	noCue := rag.Chunk{ID: "no-cue", StableKey: "m"}
	metadataCue := rag.Chunk{ID: "metadata-cue", StableKey: "a", Metadata: map[string]string{"doc": "metadatamagic"}}
	candidates := []outlineCandidate{
		{Chunk: contentOnly, OutlineTokens: outlineCandidateTokens(contentOnly)},
		{Chunk: noCue, OutlineTokens: outlineCandidateTokens(noCue)},
		{Chunk: metadataCue, OutlineTokens: outlineCandidateTokens(metadataCue)},
	}

	got := selectOutlineCandidates(candidates, "contentmagic metadatamagic", len(candidates))
	gotIDs := make([]string, len(got))
	for i := range got {
		gotIDs[i] = got[i].Chunk.ID
	}
	want := []string{"metadata-cue", "no-cue", "content-only"}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("outline selection IDs = %v, want %v", gotIDs, want)
	}
}

func TestOutlineSelectionUsesPrecomputedCandidateTokens(t *testing.T) {
	storedCue := rag.Chunk{ID: "stored-cue", StableKey: "z", Metadata: map[string]string{"doc": "storedmagic"}}
	noCue := rag.Chunk{ID: "no-cue", StableKey: "a"}
	candidates := []outlineCandidate{
		{Chunk: storedCue, OutlineTokens: outlineCandidateTokens(storedCue)},
		{Chunk: noCue, OutlineTokens: outlineCandidateTokens(noCue)},
	}
	candidates[0].Chunk.Metadata = map[string]string{"doc": "changed"}
	candidates[0].Chunk.Content = "changed"

	got := selectOutlineCandidates(candidates, "storedmagic", len(candidates))
	if got[0].Chunk.ID != "stored-cue" {
		t.Fatalf("first candidate = %q, want stored-cue from precomputed outline", got[0].Chunk.ID)
	}
}

func TestEvalCandidateRankingMatchesSearchMultiForWholeCorpus(t *testing.T) {
	ctx := context.Background()
	store, err := rag.NewSQLiteStore(filepath.Join(t.TempDir(), "differential.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	chunks := []rag.Chunk{
		{ID: "target", Source: "pkg/target.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "pkg/target.go::TargetSymbol#0", Content: "func TargetSymbol() {}", Metadata: map[string]string{"symbol": "TargetSymbol"}},
		{ID: "near", Source: "pkg/near.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "pkg/near.go::Near#0", Content: "func Near() {}", Metadata: map[string]string{"symbol": "Near"}},
		{ID: "far", Source: "other/far.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "other/far.go::Far#0", Content: "func Far() {}", Metadata: map[string]string{"symbol": "Far"}},
		{ID: "tie-a", Source: "tie/a.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "tie/a.go::TieA#0", Content: "func TieA() {}", Metadata: map[string]string{"symbol": "TieA"}},
		{ID: "tie-b", Source: "tie/b.go", StartLine: 1, EndLine: 2, Language: "go", StableKey: "tie/b.go::TieB#0", Content: "func TieB() {}", Metadata: map[string]string{"symbol": "TieB"}},
	}
	embeddings := [][]float64{{1, 0}, {0.8, 0.2}, {0, 1}, {-1, 0}, {-1, 0}}
	for i := range chunks {
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, chunks[i].Source, chunks[i:i+1], embeddings[i:i+1], fixtureSourceSignature(chunks[i:i+1]), vectorSpaceID); err != nil {
			t.Fatalf("ReplaceSourceWithHashAndVectorSpaceID: %v", err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, "UPDATE chunks SET indexed_at = ?", outlineIndexedAt); err != nil {
		t.Fatalf("set indexed_at: %v", err)
	}

	candidates, err := loadOutlineCandidates(ctx, store)
	if err != nil {
		t.Fatalf("loadOutlineCandidates: %v", err)
	}
	queryEmbedding := []float64{1, 0}
	qCtx := rag.QueryContext{CurrentFile: "pkg/target.go", Timestamp: replayTimestamp}
	got, err := rankOutlineCandidates(ctx, store, candidates, "TargetSymbol", queryEmbedding, qCtx, len(candidates))
	if err != nil {
		t.Fatalf("rankOutlineCandidates: %v", err)
	}
	want, err := store.SearchMulti(ctx, queryEmbedding, "TargetSymbol", len(candidates), qCtx)
	if err != nil {
		t.Fatalf("SearchMulti: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("results = %d, want %d", len(got), len(want))
	}
	var tieScores []float64
	for i := range want {
		if got[i].Chunk.ID != want[i].Chunk.ID ||
			got[i].Score != want[i].Score ||
			got[i].Distance != want[i].Distance ||
			got[i].RankScore != want[i].RankScore ||
			!reflect.DeepEqual(got[i].Signals, want[i].Signals) {
			t.Fatalf("result[%d] = %#v, want %#v", i, got[i], want[i])
		}
		if want[i].Chunk.ID == "tie-a" || want[i].Chunk.ID == "tie-b" {
			tieScores = append(tieScores, want[i].RankScore)
		}
	}
	if len(tieScores) != 2 || tieScores[0] != tieScores[1] {
		t.Fatalf("production tie scores = %v, want two equal final-rank scores", tieScores)
	}
}

func TestRunOutlineExperimentModesAndDeterminism(t *testing.T) {
	opts := OutlineOptions{
		Dimensions:     128,
		Samples:        1,
		CandidateLimit: 50,
	}
	report, err := RunOutlineExperiment(context.Background(), opts)
	if err != nil {
		t.Fatalf("RunOutlineExperiment: %v", err)
	}
	if report.SchemaVersion != "rag-outline-eval/v1" {
		t.Fatalf("schema = %q, want rag-outline-eval/v1", report.SchemaVersion)
	}
	wantModes := []string{
		"full_corpus_search_multi",
		"resident_exact",
		"bounded_semantic_keyword_union",
		"outline_then_content",
		"hierarchical",
	}
	wantHydrated := map[string]float64{
		"full_corpus_search_multi":       1401,
		"resident_exact":                 10,
		"bounded_semantic_keyword_union": 10,
		"outline_then_content":           10,
		"hierarchical":                   float64(opts.CandidateLimit),
	}
	wantPostRetrieval := map[string]float64{
		"hierarchical": float64(opts.CandidateLimit),
	}
	gotModes := make([]string, len(report.Modes))
	for i, mode := range report.Modes {
		gotModes[i] = mode.Name
		if len(mode.Queries) != 20 {
			t.Fatalf("mode %q queries = %d, want 20", mode.Name, len(mode.Queries))
		}
		if mode.Summary.RankedCandidates == 0 || mode.Summary.HydratedContentChunks == 0 {
			t.Fatalf("mode %q candidate/hydration metrics = (%v, %v), want non-zero", mode.Name, mode.Summary.RankedCandidates, mode.Summary.HydratedContentChunks)
		}
		if mode.Summary.CandidatesInspected != 1401 {
			t.Fatalf("mode %q candidates inspected = %v, want 1401", mode.Name, mode.Summary.CandidatesInspected)
		}
		if mode.Summary.HydratedContentChunks != wantHydrated[mode.Name] {
			t.Fatalf("mode %q hydrated chunks = %v, want %v", mode.Name, mode.Summary.HydratedContentChunks, wantHydrated[mode.Name])
		}
		if mode.Summary.PostRetrievalCandidatesInspected != wantPostRetrieval[mode.Name] {
			t.Fatalf("mode %q post-retrieval candidates = %v, want %v", mode.Name, mode.Summary.PostRetrievalCandidatesInspected, wantPostRetrieval[mode.Name])
		}
		if mode.Name == "hierarchical" && mode.Summary.RankedCandidates != 1401 {
			t.Fatalf("hierarchical ranked candidates = %v, want 1401", mode.Summary.RankedCandidates)
		}
		if mode.Summary.PlanningTokens != nil {
			t.Fatalf("mode %q planning tokens = %v, want nil", mode.Name, *mode.Summary.PlanningTokens)
		}
		if !mode.Summary.DeterministicOrdering {
			t.Fatalf("mode %q ordering is not deterministic", mode.Name)
		}
		for _, query := range mode.Queries {
			if query.CandidatesInspected != 1401 {
				t.Fatalf("mode %q query %q candidates inspected = %v, want 1401", mode.Name, query.ID, query.CandidatesInspected)
			}
			if query.HydratedContentChunks != wantHydrated[mode.Name] {
				t.Fatalf("mode %q query %q hydrated chunks = %v, want %v", mode.Name, query.ID, query.HydratedContentChunks, wantHydrated[mode.Name])
			}
			if query.PostRetrievalCandidatesInspected != wantPostRetrieval[mode.Name] {
				t.Fatalf("mode %q query %q post-retrieval candidates = %v, want %v", mode.Name, query.ID, query.PostRetrievalCandidatesInspected, wantPostRetrieval[mode.Name])
			}
			if query.PlanningTokens != nil {
				t.Fatalf("mode %q query %q planning tokens = %v, want nil", mode.Name, query.ID, *query.PlanningTokens)
			}
			if !query.DeterministicOrdering {
				t.Fatalf("mode %q query %q ordering is not deterministic", mode.Name, query.ID)
			}
		}
	}
	if !reflect.DeepEqual(gotModes, wantModes) {
		t.Fatalf("modes = %v, want %v", gotModes, wantModes)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	if strings.Contains(string(data), `"conclusion"`) || strings.Contains(string(data), `"citation`) {
		t.Fatalf("report contains removed policy or citation fields: %s", data)
	}
	if !strings.Contains(string(data), `"source_path_precision_at_10"`) {
		t.Fatalf("report missing source_path_precision metric: %s", data)
	}

	residentDistributed := outlineQueriesByCategory(report.Modes[1], "distributed_support")
	hierarchicalDistributed := outlineQueriesByCategory(report.Modes[4], "distributed_support")
	for id, resident := range residentDistributed {
		hierarchical := hierarchicalDistributed[id]
		residentMetrics := outlineMetricsAtK(resident, 10)
		hierarchicalMetrics := outlineMetricsAtK(hierarchical, 10)
		if residentMetrics.Recall != 1 || residentMetrics.ExpectedSupportCoverage != 1 {
			t.Fatalf("resident distributed query %q metrics = %+v, want recall/coverage 1", id, residentMetrics)
		}
		if hierarchicalMetrics.Recall != 0.5 || hierarchicalMetrics.ExpectedSupportCoverage != 0.5 {
			t.Fatalf("hierarchical distributed query %q metrics = %+v, want recall/coverage .5", id, hierarchicalMetrics)
		}
		if reflect.DeepEqual(resident.ResultIDs, hierarchical.ResultIDs) {
			t.Fatalf("distributed query %q resident and hierarchy ordering unexpectedly match", id)
		}
	}

	second, err := RunOutlineExperiment(context.Background(), opts)
	if err != nil {
		t.Fatalf("second RunOutlineExperiment: %v", err)
	}
	for i := range report.Modes {
		if report.Modes[i].Name != second.Modes[i].Name {
			t.Fatalf("fresh run mode[%d] = %q, want %q", i, second.Modes[i].Name, report.Modes[i].Name)
		}
		for j := range report.Modes[i].Queries {
			if !reflect.DeepEqual(report.Modes[i].Queries[j].ResultIDs, second.Modes[i].Queries[j].ResultIDs) {
				t.Fatalf("mode %q query %q fresh ordering = %v, want %v",
					report.Modes[i].Name,
					report.Modes[i].Queries[j].ID,
					second.Modes[i].Queries[j].ResultIDs,
					report.Modes[i].Queries[j].ResultIDs,
				)
			}
		}
	}
}

func TestRunOutlineModeWarmsBeforeMeasurements(t *testing.T) {
	calls := 0
	mode := outlineMode{
		name: "warmup",
		retrieve: func(context.Context, outlineQuery) (outlineRetrieval, error) {
			calls++
			return outlineRetrieval{}, nil
		},
	}
	if _, err := runOutlineMode(context.Background(), []outlineQuery{{ID: "query"}}, 1, mode); err != nil {
		t.Fatalf("runOutlineMode: %v", err)
	}
	if calls != 3 {
		t.Fatalf("retrieve calls = %d, want 3 (warmup, measured sample, ordering check)", calls)
	}
}

func TestRunOutlineExperimentRejectsCandidateLimitAtFinalK(t *testing.T) {
	_, err := RunOutlineExperiment(context.Background(), OutlineOptions{
		Dimensions:     2,
		Samples:        1,
		CandidateLimit: 10,
	})
	if err == nil || !strings.Contains(err.Error(), "candidate limit must be greater than 10") {
		t.Fatalf("error = %v, want candidate limit > final K validation", err)
	}
}

func outlineQueriesByCategory(mode OutlineModeReport, category string) map[string]OutlineQueryReport {
	queries := make(map[string]OutlineQueryReport)
	for _, query := range mode.Queries {
		if query.Category == category {
			queries[query.ID] = query
		}
	}
	return queries
}

func outlineMetricsAtK(query OutlineQueryReport, k int) OutlineKMetrics {
	for _, metrics := range query.Metrics {
		if metrics.K == k {
			return metrics
		}
	}
	return OutlineKMetrics{}
}
