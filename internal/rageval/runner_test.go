package rageval

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

const fixturePath = "testdata/fixtures.json"
const baselinePath = "testdata/baseline.json"

func TestFixtureValidate(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	if len(fixture.Queries) != 20 {
		t.Fatalf("queries = %d, want 20 (v1 corpus contract)", len(fixture.Queries))
	}
	// Categories are open-set on a per-version basis: every query must land in
	// the allowed set, AND every allowed category must have >= 4 queries.
	// Adding a new category requires updating allowedCategories below so the
	// addition gets a review pass instead of silently appearing.
	allowedCategories := map[string]int{
		"single_hop":       0,
		"cross_file_trace": 0,
		"compare":          0,
		"architecture":     0,
		"code_review":      0,
	}
	for _, query := range fixture.Queries {
		if _, ok := allowedCategories[query.Category]; !ok {
			t.Fatalf("query %q has category %q not in allowed set %v", query.ID, query.Category, sortedKeys(allowedCategories))
		}
		allowedCategories[query.Category]++
	}
	const minPerCategory = 4
	for _, category := range sortedKeys(allowedCategories) {
		if allowedCategories[category] < minPerCategory {
			t.Fatalf("category %q has %d queries, want >= %d", category, allowedCategories[category], minPerCategory)
		}
	}
}

func TestFixtureEmbedderRejectsUnknownQuery(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	emb := newFixtureEmbedder(fixture)
	_, err = emb.Embed(context.Background(), "fixture-embedding", []string{"this query does not exist in the fixture"})
	if err == nil {
		t.Fatal("expected error for unknown query, got nil")
	}
	if !strings.Contains(err.Error(), "no fixture embedding") {
		t.Fatalf("expected error to mention 'no fixture embedding', got: %v", err)
	}
}

func TestFixtureValidateRejectsDuplicateQueryText(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	fixture.Queries[1].Query = fixture.Queries[0].Query
	err = fixture.Validate()
	if err == nil {
		t.Fatal("expected error for duplicate query text, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate query text") {
		t.Fatalf("expected error to mention duplicate query text, got: %v", err)
	}
}

// TestBuildStoreSeedsParseableProvenance pins that buildStore writes a
// versioned source-signature document, not a bare string. rag's
// parseSourceSignature accepts a signature only when it unmarshals to
// version != 0 AND content_hash != "", so a bare value leaves ContentHash
// blank ("unknown") for every seeded source. Any eval that reuses buildStore
// and then needs provenance — installing fresh summaries via
// UpsertSourceSummary, which must copy ContentHash/VectorSpaceID from
// SourceProvenanceBatch — would either fail "lacks provenance" or silently
// derive stale validity. The sibling outline path already hit this
// (fixtureSourceSignature, added as outlineSourceSignature in f9ab4f6).
func TestBuildStoreSeedsParseableProvenance(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	ctx := context.Background()
	store, err := buildStore(ctx, fixture)
	if err != nil {
		t.Fatalf("buildStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	seen := make(map[string]bool, len(fixture.Corpus))
	sources := make([]string, 0, len(fixture.Corpus))
	for _, chunk := range fixture.Corpus {
		if !seen[chunk.Source] {
			seen[chunk.Source] = true
			sources = append(sources, chunk.Source)
		}
	}
	sort.Strings(sources)

	provenance, err := store.SourceProvenanceBatch(ctx, sources)
	if err != nil {
		t.Fatalf("SourceProvenanceBatch: %v", err)
	}
	if len(provenance) != len(sources) {
		t.Fatalf("provenance rows = %d, want %d", len(provenance), len(sources))
	}
	hashes := make(map[string]string, len(sources))
	for _, source := range sources {
		p, ok := provenance[source]
		if !ok {
			t.Fatalf("source %q missing from provenance batch", source)
		}
		if p.Mixed {
			t.Fatalf("source %q reports mixed provenance; buildStore seeds one signature per source", source)
		}
		if p.ContentHash == "" {
			t.Fatalf("source %q has blank ContentHash: buildStore's stored signature did not parse", source)
		}
		if p.VectorSpaceID != vectorSpaceID {
			t.Fatalf("source %q VectorSpaceID = %q, want %q", source, p.VectorSpaceID, vectorSpaceID)
		}
		if previous, dup := hashes[p.ContentHash]; dup {
			t.Fatalf("sources %q and %q share ContentHash %q: signature does not cover chunk content", previous, source, p.ContentHash)
		}
		hashes[p.ContentHash] = source
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRunFixtureWithoutLiveModel(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	report, err := Run(context.Background(), fixture, RunOptions{MeasureLatency: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Modes) != 2 {
		t.Fatalf("modes = %d, want 2", len(report.Modes))
	}

	// Per-mode minimums. Floors are below the current baseline numbers with
	// enough slack to absorb floating-point ties (MRR) and 1-query rank flips
	// (recall@5 on 20-query corpus = 0.05/query). Tighter floors live in
	// the regression-detection harness (TestBaselineReproducible).
	type modeFloor struct {
		minRecallAt5 float64
		minMRRAt5    float64
	}
	wantFloors := map[string]modeFloor{
		"static":              {minRecallAt5: 0.95, minMRRAt5: 0.95}, // current 0.9583 / 1.0
		"hybrid_search_multi": {minRecallAt5: 0.95, minMRRAt5: 0.80}, // current 0.9583 / 0.825 (MRR regression vs static is known; see baseline notes)
	}
	for _, mode := range report.Modes {
		if len(mode.Queries) != len(fixture.Queries) {
			t.Fatalf("mode %q queries = %d, want %d", mode.Name, len(mode.Queries), len(fixture.Queries))
		}
		floor, ok := wantFloors[mode.Name]
		if !ok {
			t.Fatalf("mode %q has no defined floor; update wantFloors when adding modes", mode.Name)
		}
		if mode.Summary.RecallAt5 < floor.minRecallAt5 {
			t.Fatalf("mode %q recall@5 = %.4f, want >= %.4f", mode.Name, mode.Summary.RecallAt5, floor.minRecallAt5)
		}
		if mode.Summary.MRRAt5 < floor.minMRRAt5 {
			t.Fatalf("mode %q MRR@5 = %.4f, want >= %.4f", mode.Name, mode.Summary.MRRAt5, floor.minMRRAt5)
		}
		if mode.Summary.DuplicateRateAt10 != 0 {
			t.Fatalf("mode %q duplicate_rate@10 = %f, want 0 (no dedup regressions)", mode.Name, mode.Summary.DuplicateRateAt10)
		}
	}
	assertRatifiedThresholdSummary(t, report.Thresholds)
	assertReportMeetsRatifiedThresholds(t, report)
}

// TestStaticModeIsDenseVectorOnly proves the "static" mode runs dense cosine
// retrieval (SQLiteStore.Search), not the hybrid SearchMulti path (#275).
//
// The fixture is built so the two paths provably disagree: the query embedding
// ranks the chunks dense-top > filler > kw-top by cosine, while the query text
// lexically matches only kw-top. Hybrid RRF fuses the keyword list and lifts
// kw-top above filler (RRF: dense-top 1/61+1/62 > kw-top 1/63+1/61 > filler
// 1/62+1/62 — no ties), so:
//
//	dense (static): [dense-top, filler, kw-top]
//	hybrid:         [dense-top, kw-top, filler]
//
// Before the #275 fix the static retriever took the SearchMulti branch and
// returned the hybrid order, making this test fail.
func TestStaticModeIsDenseVectorOnly(t *testing.T) {
	const queryText = "flurbomatic"
	fixture := &Fixture{
		Corpus: []FixtureChunk{
			{ID: "dense-top", Source: "a.go", Language: "go", StableKey: "a.go:1", Content: "alpha beta gamma", Embedding: []float64{1, 0, 0}},
			{ID: "filler", Source: "b.go", Language: "go", StableKey: "b.go:1", Content: "delta epsilon zeta", Embedding: []float64{0.8, 0.6, 0}},
			{ID: "kw-top", Source: "c.go", Language: "go", StableKey: "c.go:1", Content: "flurbomatic reconciler", Embedding: []float64{0.5, 0.866, 0}},
		},
		Queries: []QueryFixture{
			{ID: "q1", Category: "single_hop", Query: queryText, ExpectedIDs: []string{"dense-top"}, Embedding: []float64{1, 0, 0}},
		},
	}

	report, err := Run(context.Background(), fixture, RunOptions{MeasureLatency: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	resultsByMode := map[string][]string{}
	for _, mode := range report.Modes {
		if len(mode.Queries) != 1 {
			t.Fatalf("mode %q queries = %d, want 1", mode.Name, len(mode.Queries))
		}
		resultsByMode[mode.Name] = mode.Queries[0].ResultIDs
	}

	wantStatic := []string{"dense-top", "filler", "kw-top"}
	wantHybrid := []string{"dense-top", "kw-top", "filler"}
	if got := resultsByMode["static"]; !slices.Equal(got, wantStatic) {
		t.Fatalf("static result order = %v, want dense cosine order %v (static mode must not run hybrid SearchMulti)", got, wantStatic)
	}
	if got := resultsByMode["hybrid_search_multi"]; !slices.Equal(got, wantHybrid) {
		t.Fatalf("hybrid result order = %v, want RRF order %v (fixture no longer discriminates dense vs hybrid)", got, wantHybrid)
	}
}

// TestBaselineReproducible re-runs Run against the fixture and asserts the
// deterministic summary fields match the committed baseline.json within
// tolerance. This catches two failure modes:
//
//  1. Retriever/scorer changes that shift numbers without a baseline regen.
//  2. Manual edits to baseline.json that don't match what the code produces.
//
// Latency fields are deliberately not compared — they vary run-to-run. Run
// `go generate ./internal/rageval` to regenerate after intentional changes.
func TestBaselineReproducible(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}
	live, err := Run(context.Background(), fixture, RunOptions{MeasureLatency: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var committed Report
	if err := json.Unmarshal(data, &committed); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}

	if live.SchemaVersion != committed.SchemaVersion {
		t.Fatalf("schema drift: live=%q committed=%q", live.SchemaVersion, committed.SchemaVersion)
	}
	if len(live.Modes) != len(committed.Modes) {
		t.Fatalf("mode count: live=%d committed=%d", len(live.Modes), len(committed.Modes))
	}

	const tol = 1e-9
	committedByName := map[string]ModeSummary{}
	for _, m := range committed.Modes {
		committedByName[m.Name] = m.Summary
	}
	for _, liveMode := range live.Modes {
		want, ok := committedByName[liveMode.Name]
		if !ok {
			t.Fatalf("live mode %q absent from committed baseline; regenerate via `go generate ./internal/rageval`", liveMode.Name)
		}
		assertCloseFloat(t, liveMode.Name, "recall_at_5", liveMode.Summary.RecallAt5, want.RecallAt5, tol)
		assertCloseFloat(t, liveMode.Name, "recall_at_10", liveMode.Summary.RecallAt10, want.RecallAt10, tol)
		assertCloseFloat(t, liveMode.Name, "mrr_at_5", liveMode.Summary.MRRAt5, want.MRRAt5, tol)
		assertCloseFloat(t, liveMode.Name, "mrr_at_10", liveMode.Summary.MRRAt10, want.MRRAt10, tol)
		assertCloseFloat(t, liveMode.Name, "duplicate_rate_at_10", liveMode.Summary.DuplicateRateAt10, want.DuplicateRateAt10, tol)
		assertCloseFloat(t, liveMode.Name, "context_precision_at_5", liveMode.Summary.ContextPrecisionAt5, want.ContextPrecisionAt5, tol)
		assertCloseFloat(t, liveMode.Name, "context_precision_at_10", liveMode.Summary.ContextPrecisionAt10, want.ContextPrecisionAt10, tol)
		assertCloseFloat(t, liveMode.Name, "average_context_tokens_at_5", liveMode.Summary.AverageContextTokensAt5, want.AverageContextTokensAt5, tol)
		assertCloseFloat(t, liveMode.Name, "average_context_tokens_at_10", liveMode.Summary.AverageContextTokensAt10, want.AverageContextTokensAt10, tol)
	}
}

func assertCloseFloat(t *testing.T, mode, field string, got, want, tol float64) {
	t.Helper()
	diff := got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > tol {
		t.Fatalf("baseline drift: mode=%q field=%q got=%g want=%g diff=%g (regenerate via `go generate ./internal/rageval` if intentional)", mode, field, got, want, diff)
	}
}

func TestRunModeHonorsZeroWarmRuns(t *testing.T) {
	fixture, err := LoadFixture(fixturePath)
	if err != nil {
		t.Fatalf("LoadFixture: %v", err)
	}

	calls := 0
	report, err := runMode(context.Background(), "test", fixture, RunOptions{WarmRuns: 0, MeasureLatency: false}, func(_ context.Context, query QueryFixture, _ int) ([]rag.SearchResult, error) {
		calls++
		return []rag.SearchResult{
			{
				Chunk: rag.Chunk{
					ID:      query.ExpectedIDs[0],
					Content: "fixture result",
				},
			},
		}, nil
	})
	if err != nil {
		t.Fatalf("runMode: %v", err)
	}
	if calls != len(fixture.Queries) {
		t.Fatalf("retrieve calls = %d, want %d cold calls only", calls, len(fixture.Queries))
	}
	for _, query := range report.Queries {
		if query.WarmLatencyMS.Count != 0 {
			t.Fatalf("query %q warm latency count = %d, want 0", query.ID, query.WarmLatencyMS.Count)
		}
	}
}

func TestBaselineReportShape(t *testing.T) {
	data, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var report Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Fatalf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if report.Corpus.Queries != 20 {
		t.Fatalf("baseline queries = %d, want 20", report.Corpus.Queries)
	}
	assertRatifiedThresholdSummary(t, report.Thresholds)
	if len(report.Modes) != 2 {
		t.Fatalf("baseline modes = %d, want 2", len(report.Modes))
	}
	assertReportMeetsRatifiedThresholds(t, &report)
}

func assertRatifiedThresholdSummary(t *testing.T, got ThresholdSummary) {
	t.Helper()

	if got.Owner != OwnerKeith {
		t.Fatalf("threshold owner = %q, want %q", got.Owner, OwnerKeith)
	}
	if got.Status != StatusThresholdsRatified {
		t.Fatalf("threshold status = %q, want %q", got.Status, StatusThresholdsRatified)
	}
	assertFloatPtrValue(t, "minimum_static_recall_at_5", got.MinimumStaticRecallAt5, 0.95)
	assertFloatPtrValue(t, "minimum_hybrid_recall_at_5_improvement", got.MinimumHybridRecallAt5Improvement, 0.0)
	assertFloatPtrValue(t, "maximum_duplicate_rate", got.MaximumDuplicateRate, 0.05)
	assertFloatPtrValue(t, "minimum_context_precision_proxy", got.MinimumContextPrecisionProxy, 0.50)
	assertFloatPtrValue(t, "maximum_average_context_token_growth", got.MaximumAverageContextTokenGrowth, 0.10)
	assertNilFloatPtr(t, "maximum_opt_in_hybrid_p95_latency_ms", got.MaximumOptInHybridP95LatencyMS)
	assertNilFloatPtr(t, "maximum_future_default_p95_latency_ms", got.MaximumFutureDefaultP95LatencyMS)
}

func assertReportMeetsRatifiedThresholds(t *testing.T, report *Report) {
	t.Helper()

	thresholds := report.Thresholds
	assertRatifiedThresholdSummary(t, thresholds)

	static := modeSummary(t, report, "static")
	hybrid := modeSummary(t, report, "hybrid_search_multi")

	if threshold := thresholds.MinimumStaticRecallAt5; threshold != nil && static.RecallAt5 < *threshold {
		t.Fatalf("static recall_at_5 = %.4f, want >= %.4f", static.RecallAt5, *threshold)
	}
	if threshold := thresholds.MinimumHybridRecallAt5Improvement; threshold != nil {
		improvement := hybrid.RecallAt5 - static.RecallAt5
		if improvement < *threshold {
			t.Fatalf("hybrid recall_at_5 improvement = %.4f, want >= %.4f (hybrid=%.4f static=%.4f)", improvement, *threshold, hybrid.RecallAt5, static.RecallAt5)
		}
	}
	if threshold := thresholds.MaximumDuplicateRate; threshold != nil {
		for _, mode := range report.Modes {
			if mode.Summary.DuplicateRateAt10 > *threshold {
				t.Fatalf("mode %q duplicate_rate_at_10 = %.4f, want <= %.4f", mode.Name, mode.Summary.DuplicateRateAt10, *threshold)
			}
		}
	}
	if threshold := thresholds.MinimumContextPrecisionProxy; threshold != nil {
		for _, mode := range report.Modes {
			if mode.Summary.ContextPrecisionAt5 < *threshold {
				t.Fatalf("mode %q context_precision_at_5 = %.4f, want >= %.4f", mode.Name, mode.Summary.ContextPrecisionAt5, *threshold)
			}
		}
	}
	if threshold := thresholds.MaximumAverageContextTokenGrowth; threshold != nil {
		assertTokenGrowthWithin(t, "average_context_tokens_at_5", hybrid.AverageContextTokensAt5, static.AverageContextTokensAt5, *threshold)
		assertTokenGrowthWithin(t, "average_context_tokens_at_10", hybrid.AverageContextTokensAt10, static.AverageContextTokensAt10, *threshold)
	}
}

func modeSummary(t *testing.T, report *Report, name string) ModeSummary {
	t.Helper()
	for _, mode := range report.Modes {
		if mode.Name == name {
			return mode.Summary
		}
	}
	t.Fatalf("mode %q missing from report", name)
	return ModeSummary{}
}

func assertFloatPtrValue(t *testing.T, field string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %.4f", field, want)
	}
	diff := *got - want
	if diff < 0 {
		diff = -diff
	}
	if diff > 1e-12 {
		t.Fatalf("%s = %.12f, want %.12f", field, *got, want)
	}
}

func assertNilFloatPtr(t *testing.T, field string, got *float64) {
	t.Helper()
	if got != nil {
		t.Fatalf("%s = %.4f, want nil", field, *got)
	}
}

func assertTokenGrowthWithin(t *testing.T, field string, got, base, maxGrowth float64) {
	t.Helper()
	if base <= 0 {
		t.Fatalf("%s base = %.4f, want > 0", field, base)
	}
	growth := (got - base) / base
	if growth > maxGrowth {
		t.Fatalf("%s growth = %.4f, want <= %.4f (hybrid=%.2f static=%.2f)", field, growth, maxGrowth, got, base)
	}
}
