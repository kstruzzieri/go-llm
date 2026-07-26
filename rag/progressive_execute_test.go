package rag

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func progressiveTestEmbedder() EmbedderFunc {
	return func(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
		vecs := make([][]float64, len(inputs))
		for i := range inputs {
			vecs[i] = []float64{1, 0, 0, 0}
		}
		return EmbedResult{Embeddings: vecs}, nil
	}
}

// newProgressiveTestRetriever returns a Retriever over a real store so
// provenance/summary/digest reads see real rows. newTestStore already
// registers the Close cleanup; do not add a second one.
func newProgressiveTestRetriever(t *testing.T) (*Retriever, *SQLiteStore) {
	t.Helper()
	store := newTestStore(t)
	r, err := NewRetrieverWithEmbedder(progressiveTestEmbedder(), store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}
	return r, store
}

func TestRenderProgressiveEndToEnd(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	content := "func A() {\n\treturn\n}\n"
	storeChunksRaw(t, store, [][]any{
		{"e1", content, "pkg/a.go", 10, 12, "go", `{"symbol_path":"A"}`, emb, int64(1700000000), "k1", sigJSON(t, "hashA"), "vs1"},
	})
	results := []SearchResult{{
		Chunk: Chunk{ID: "e1", Content: content, Source: "pkg/a.go", StartLine: 10, EndLine: 12,
			Language: "go", Metadata: map[string]string{"symbol_path": "A"}, StableKey: "k1"},
		Score: 0.87,
	}}

	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if !strings.Contains(out, "--- pkg/a.go (lines 10-12, similarity: 0.87) ---") {
		t.Fatalf("evidence missing:\n%s", out)
	}
	if !strings.Contains(out, "note: metadata overview (no summary: missing)") {
		t.Fatalf("metadata orientation missing:\n%s", out)
	}
	if trace.SelectedResults != 1 || trace.DistinctSources != 1 || trace.EvidenceBlocks != 1 {
		t.Fatalf("trace counters wrong: %+v", trace)
	}
	st := trace.Sources[0]
	if st.BestRank != 1 || st.EffectiveDepth != DepthL2 || st.ScoreKind != "semantic_similarity" {
		t.Fatalf("source trace wrong: %+v", st)
	}
	if len(st.RenderedEvidence) != 1 || st.RenderedEvidence[0].ChunkID != "e1" ||
		st.RenderedEvidence[0].StartLine != 10 || st.RenderedEvidence[0].StableKey != "k1" {
		t.Fatalf("rendered evidence wrong: %+v", st.RenderedEvidence)
	}
	// A reason set containing `missing` must surface as the summary_missing
	// decision (spec section 10 emission table).
	if !slices.Contains(st.Decisions, DecisionSummaryMissing) {
		t.Fatalf("summary_missing missing from decisions: %v", st.Decisions)
	}
	if slices.Contains(st.Decisions, DecisionSummaryStale) {
		t.Fatalf("a missing summary is not a stale one: %v", st.Decisions)
	}
	// Free is the remainder, not a second copy of used.
	if st.EstimatedTokens <= 0 {
		t.Fatalf("per-source EstimatedTokens must be charged, got %d", st.EstimatedTokens)
	}
	if trace.EstimatedTokensUsed <= 0 ||
		trace.EstimatedTokensUsed+trace.EstimatedTokensFree != trace.MaxTokens {
		t.Fatalf("token accounting wrong: used=%d free=%d max=%d",
			trace.EstimatedTokensUsed, trace.EstimatedTokensFree, trace.MaxTokens)
	}
	if trace.BytesUsed != len(out) {
		t.Fatalf("BytesUsed = %d, output is %d bytes", trace.BytesUsed, len(out))
	}
}

func TestRenderProgressiveStaleTextNeverRendered(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	const sentinel = "STALE-SENTINEL-TEXT"

	storeChunksRaw(t, store, [][]any{
		{"s1", "current content", "pkg/s.go", 1, 1, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "newhash"), "vs1"},
	})
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/s.go", ContentHash: "oldhash", VectorSpaceID: "vs1",
		Abstract: sentinel, Overview: sentinel, SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	results := []SearchResult{{
		Chunk: Chunk{ID: "s1", Content: "current content", Source: "pkg/s.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if strings.Contains(out, sentinel) {
		t.Fatalf("stale summary text leaked into output:\n%s", out)
	}
	got := trace.Sources[0].ValidityReasons
	if len(got) != 1 || got[0] != ReasonStaleContent {
		t.Fatalf("reasons = %v, want [stale_content]", got)
	}
	// OrientationGenerated reports what was RENDERED, not whether a row
	// existed: a stale row exists and must still report false.
	if trace.Sources[0].OrientationGenerated {
		t.Fatal("stale summary must degrade to a metadata overview")
	}
	if !slices.Contains(trace.Sources[0].Decisions, DecisionSummaryStale) {
		t.Fatalf("summary_stale missing from decisions: %v", trace.Sources[0].Decisions)
	}
	if slices.Contains(trace.Sources[0].Decisions, DecisionSummaryMissing) {
		t.Fatalf("a stale summary is not a missing one: %v", trace.Sources[0].Decisions)
	}
}

func TestRenderProgressiveRace(t *testing.T) {
	// Spec section 14 race test: retrieve from version A, reindex to B with a
	// fresh B summary, render the saved A results — B's text must not appear.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	const bSentinel = "SUMMARY-OF-VERSION-B"

	storeChunksRaw(t, store, [][]any{
		{"a1", "version A content", "pkg/r.go", 1, 1, "go", `{}`, emb, int64(100), "", sigJSON(t, "hashA"), "vs1"},
	})
	savedResults := []SearchResult{{
		Chunk: Chunk{ID: "a1", Content: "version A content", Source: "pkg/r.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}

	// Reindex to version B (different chunk id + content) and summarize B.
	if _, err := store.db.Exec(`DELETE FROM chunks WHERE source = 'pkg/r.go'`); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	storeChunksRaw(t, store, [][]any{
		{"b1", "version B content", "pkg/r.go", 1, 1, "go", `{}`, emb, int64(200), "", sigJSON(t, "hashB"), "vs1"},
	})
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/r.go", ContentHash: "hashB", VectorSpaceID: "vs1",
		Abstract: bSentinel, Overview: bSentinel, SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert B summary: %v", err)
	}

	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: savedResults, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if strings.Contains(out, bSentinel) {
		t.Fatalf("version B summary rendered beside version A evidence:\n%s", out)
	}
	if !slices.Contains(trace.Sources[0].ValidityReasons, ReasonEvidenceMismatch) {
		t.Fatalf("evidence_mismatch missing: %v", trace.Sources[0].ValidityReasons)
	}
	if !trace.Sources[0].MetadataFromSnapshot {
		t.Fatal("race path must flag MetadataFromSnapshot")
	}
}

func TestRenderProgressiveDeterministic(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"x1", "aaa", "pkg/x.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"y1", "bbb", "pkg/y.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "x1", Content: "aaa", Source: "pkg/x.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "y1", Content: "bbb", Source: "pkg/y.go", StartLine: 1, EndLine: 1}, Score: 0.8},
	}
	req := ProgressiveRenderRequest{Results: results, MaxTokens: 10000, MaxBytes: 1 << 20}
	first, _, err := r.RenderProgressive(ctx, req)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	for i := 0; i < 10; i++ {
		out, _, err := r.RenderProgressive(ctx, req)
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
		if out != first {
			t.Fatalf("render %d differs from first over identical input", i)
		}
	}
	// Source order = first-result order, not lexical: pkg/x.go before pkg/y.go
	// here, but assert via index to prove it is rank, not name.
	if !strings.HasPrefix(first, "### pkg/x.go") {
		t.Fatalf("first source must be the first-ranked one:\n%s", first)
	}
	// Exactly one "\n" separator between sources: every block already ends in
	// one newline, so the separator shows up as the blank line before the next
	// header. The allocator charged for exactly this byte (separatorBytes).
	if !strings.Contains(first, "\n\n### pkg/y.go") {
		t.Fatalf("sources must be separated by exactly one \\n:\n%q", first)
	}
	// ...and no separator trails the last source.
	if strings.HasSuffix(first, "\n\n") {
		t.Fatalf("trailing separator emitted after the last source:\n%q", first)
	}
}

func TestRenderProgressiveErrorReturnsZeroTrace(t *testing.T) {
	// An error must never hand back a partially filled trace: one carrying
	// MaxBytes, SelectedResults and DistinctSources with used=0 free=0 reads
	// exactly like a completed render that found sources, spent nothing, and
	// exhausted its budget. Zero trace, or the used+free==MaxTokens contract
	// means nothing.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"z1", "pinned body", "pkg/z.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "z1", Content: "pinned body", Source: "pkg/z.go", StartLine: 1, EndLine: 1}, Score: 0.9},
	}

	// Pinned blocks over budget: the deepest error path, and the one whose
	// partial trace looked most like a real result.
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 1, MaxBytes: 1,
		Pinned: []PinRef{{Source: "pkg/z.go", ChunkID: "z1"}},
	})
	if err == nil {
		t.Fatal("pinned blocks over budget must error")
	}
	if out != "" || !reflect.DeepEqual(trace, ProgressiveTrace{}) {
		t.Fatalf("error path must return the zero trace, got out=%q trace=%+v", out, trace)
	}

	// The validation path must not echo an invalid budget back either.
	out, trace, err = r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 0, MaxBytes: 100,
	})
	if err == nil {
		t.Fatal("MaxTokens of 0 must error")
	}
	if out != "" || !reflect.DeepEqual(trace, ProgressiveTrace{}) {
		t.Fatalf("validation error must return the zero trace, got out=%q trace=%+v", out, trace)
	}
}

func TestRenderProgressiveEmptyResults(t *testing.T) {
	r, _ := newProgressiveTestRetriever(t)
	out, trace, err := r.RenderProgressive(context.Background(), ProgressiveRenderRequest{
		Results: nil, MaxTokens: 100, MaxBytes: 1000,
	})
	if err != nil {
		t.Fatalf("empty render: %v", err)
	}
	if out != "" || trace.SelectedResults != 0 {
		t.Fatalf("empty input must give empty output, got %q", out)
	}
	// The used + free == MaxTokens invariant holds on the early-return path
	// too: nothing was charged, so the whole ceiling is free.
	if trace.EstimatedTokensUsed != 0 || trace.EstimatedTokensFree != trace.MaxTokens {
		t.Fatalf("empty render must leave the whole budget free: used=%d free=%d max=%d",
			trace.EstimatedTokensUsed, trace.EstimatedTokensFree, trace.MaxTokens)
	}
}

func TestRenderProgressiveRaceReusedChunkID(t *testing.T) {
	// The harder race: a non-canonical chunk ID is REUSED for changed content.
	// ID membership would pass; only the digest comparison catches it.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	const bSentinel = "SUMMARY-OF-REUSED-B"

	storeChunksRaw(t, store, [][]any{
		{"reused", "version A content", "pkg/ru.go", 1, 1, "go", `{}`, emb, int64(100), "", sigJSON(t, "hashA"), "vs1"},
	})
	savedResults := []SearchResult{{
		Chunk: Chunk{ID: "reused", Content: "version A content", Source: "pkg/ru.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}
	// Same ID, different content — a custom chunker is allowed to do this.
	if _, err := store.db.Exec(`UPDATE chunks SET content = 'version B content', source_content_hash = ? WHERE id = 'reused'`, sigJSON(t, "hashB")); err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/ru.go", ContentHash: "hashB", VectorSpaceID: "vs1",
		Abstract: bSentinel, Overview: bSentinel, SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert B summary: %v", err)
	}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: savedResults, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if strings.Contains(out, bSentinel) {
		t.Fatalf("reused-ID race leaked B summary beside A evidence:\n%s", out)
	}
	if !slices.Contains(trace.Sources[0].ValidityReasons, ReasonEvidenceMismatch) {
		t.Fatalf("digest comparison must flag evidence_mismatch: %v", trace.Sources[0].ValidityReasons)
	}
}

func TestRenderProgressiveBlankChunkIDFailsClosed(t *testing.T) {
	// A blank Chunk.ID cannot be looked up, so it is left out of the digest
	// query — and must therefore FAIL CLOSED in the comparison. Skipping it
	// instead would let a custom Chunker's unidentified chunk pass race
	// detection, rendering a fresh stored summary beside evidence nothing
	// verified. Built-in chunkers always populate the ID; this guard is what
	// stands between a custom one and unverified evidence.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	const sentinel = "SUMMARY-BESIDE-UNVERIFIED-EVIDENCE"

	// The stored row makes the summary genuinely FRESH: matching content hash
	// and vector space, so nothing but the blank ID can degrade this source.
	storeChunksRaw(t, store, [][]any{
		{"bk1", "verified content", "pkg/bk.go", 1, 1, "go", `{}`, emb, int64(100), "", sigJSON(t, "hashBK"), "vs1"},
	})
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/bk.go", ContentHash: "hashBK", VectorSpaceID: "vs1",
		Abstract: sentinel, Overview: sentinel, SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	results := []SearchResult{{
		Chunk: Chunk{ID: "", Content: "verified content", Source: "pkg/bk.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	src := trace.Sources[0]
	if !slices.Contains(src.ValidityReasons, ReasonEvidenceMismatch) {
		t.Fatalf("an unverifiable chunk must degrade the source: %v", src.ValidityReasons)
	}
	if src.OrientationGenerated {
		t.Fatal("no stored summary text may render beside unverified evidence")
	}
	if strings.Contains(out, sentinel) {
		t.Fatalf("summary text leaked beside unverified evidence:\n%s", out)
	}
	if !src.MetadataFromSnapshot {
		t.Fatal("fail-closed path must flag MetadataFromSnapshot")
	}
}

func TestRenderProgressiveIdenticalReindexStaysValid(t *testing.T) {
	// A canonical reindex that produces identical chunks must NOT trip the
	// race check: same IDs, same content, same digests.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	row := [][]any{{"same1", "stable content", "pkg/st.go", 1, 1, "go", `{}`, emb, int64(100), "", sigJSON(t, "hashS"), "vs1"}}
	storeChunksRaw(t, store, row)
	results := []SearchResult{{
		Chunk: Chunk{ID: "same1", Content: "stable content", Source: "pkg/st.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}
	// Simulate the reindex: delete and reinsert byte-identical rows.
	if _, err := store.db.Exec(`DELETE FROM chunks WHERE source = 'pkg/st.go'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	storeChunksRaw(t, store, row)
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/st.go", ContentHash: "hashS", VectorSpaceID: "vs1",
		Abstract: "Stable purpose.", Overview: "Stable overview.", SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if len(trace.Sources[0].ValidityReasons) != 0 {
		t.Fatalf("identical reindex must stay fresh, got %v", trace.Sources[0].ValidityReasons)
	}
	if !strings.Contains(out, "purpose: Stable purpose.") {
		t.Fatalf("fresh summary must render through the full DB path:\n%s", out)
	}
	if !trace.Sources[0].OrientationGenerated {
		t.Fatal("fresh summary orientation must be flagged generated")
	}
	// floor_reserved / rank_upgraded are expected here; the summary decisions
	// are not — an empty reason set must yield neither.
	if slices.Contains(trace.Sources[0].Decisions, DecisionSummaryMissing) ||
		slices.Contains(trace.Sources[0].Decisions, DecisionSummaryStale) {
		t.Fatalf("a fresh summary yields no summary decisions, got %v", trace.Sources[0].Decisions)
	}
}

func TestRenderProgressiveFreshSummaryDemotedOnBudget(t *testing.T) {
	// DEV-18: a FRESH source whose stored abstract will not fit falls back to
	// the metadata overview. OrientationGenerated must then report false —
	// deriving it from "a summary row existed" would report true for a block
	// that contains no summary text at all.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	abstract := strings.Repeat("long stored abstract text. ", 40)

	storeChunksRaw(t, store, [][]any{
		{"f1", "tiny", "pkg/f.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "hashF"), "vs1"},
	})
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/f.go", ContentHash: "hashF", VectorSpaceID: "vs1",
		Abstract: abstract, Overview: abstract, SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	results := []SearchResult{{
		Chunk: Chunk{ID: "f1", Content: "tiny", Source: "pkg/f.go", StartLine: 1, EndLine: 1},
		Score: 0.9,
	}}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 200,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	src := trace.Sources[0]
	if len(src.ValidityReasons) != 0 {
		t.Fatalf("the summary is fresh; reasons must stay empty, got %v", src.ValidityReasons)
	}
	if src.OrientationGenerated {
		t.Fatalf("budget-omitted summary means no stored text rendered:\n%s", out)
	}
	if !strings.Contains(out, "note: metadata overview (summary omitted: budget)") {
		t.Fatalf("budget fallback note missing:\n%s", out)
	}
	if !slices.Contains(src.Decisions, DecisionBudgetDemoted) {
		t.Fatalf("budget_demoted missing: %v", src.Decisions)
	}
	// The allocator owns budget_demoted; nothing may add it a second time or
	// pair it with no_fit on a source that did render.
	if slices.Contains(src.Decisions, DecisionNoFit) {
		t.Fatalf("a demoted source rendered, so no_fit is wrong: %v", src.Decisions)
	}
}

func TestRenderProgressiveOmittedSourceIsNotBudgetDemoted(t *testing.T) {
	// DEV-17's guard, end to end. pkg/big.go has its floor bundle rejected on
	// cost (costRejected) and then cannot fit even a metadata overview, so it
	// renders NOTHING. budget_demoted requires "a cheaper alternative was
	// rendered" (spec section 10), so no_fit must stand alone — deriving
	// budget_demoted from costRejected without the orientation guard emits
	// both for the same source, which is incoherent.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	big := strings.Repeat("a very long line of dominant content\n", 20)
	storeChunksRaw(t, store, [][]any{
		{"g1", big, "pkg/big.go", 1, 20, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"s1", "tiny", "pkg/sm.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "g1", Content: big, Source: "pkg/big.go", StartLine: 1, EndLine: 20}, Score: 0.9},
		{Chunk: Chunk{ID: "s1", Content: "tiny", Source: "pkg/sm.go", StartLine: 1, EndLine: 1}, Score: 0.8},
	}
	// Room for pkg/sm.go's whole bundle, but not for pkg/big.go's evidence and
	// not for its orientation on top of what pkg/sm.go spent.
	_, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 200,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	omitted := trace.Sources[0]
	if omitted.Source != "pkg/big.go" || omitted.EffectiveDepth != DepthNone {
		t.Fatalf("fixture must omit pkg/big.go entirely, got %+v", omitted)
	}
	if trace.OmittedSources != 1 {
		t.Fatalf("OmittedSources = %d, want 1", trace.OmittedSources)
	}
	if !slices.Contains(omitted.Decisions, DecisionNoFit) {
		t.Fatalf("no_fit missing: %v", omitted.Decisions)
	}
	if slices.Contains(omitted.Decisions, DecisionBudgetDemoted) {
		t.Fatalf("nothing rendered, so nothing was demoted: %v", omitted.Decisions)
	}
}

func TestRenderProgressivePublicPins(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"p1", "pin me", "pkg/p.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"p2", "also here", "pkg/p.go", 3, 3, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "p1", Content: "pin me", Source: "pkg/p.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "p2", Content: "also here", Source: "pkg/p.go", StartLine: 3, EndLine: 3}, Score: 0.8},
	}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
		Pinned: []PinRef{{Source: "pkg/p.go", ChunkID: "p2"}},
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if !strings.Contains(out, "3| also here") {
		t.Fatalf("pinned chunk must render:\n%s", out)
	}
	src := trace.Sources[0]
	if !slices.Contains(src.Decisions, DecisionCallerPinned) {
		t.Fatalf("caller_pinned missing from decisions: %v", src.Decisions)
	}
	if len(trace.UnmatchedPins) != 0 {
		t.Fatalf("matched pin reported unmatched: %+v", trace.UnmatchedPins)
	}
}

func TestRenderProgressiveMaxDepthL1IsVacuousFloor(t *testing.T) {
	// Under MaxDepth < DepthL2 the L2 floor and every pin are vacuous. That
	// makes FloorRequested 0 even though the caller asked for 5 — deliberate,
	// not an accident of the allocator skipping step 4.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"d1", "body one", "pkg/d.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"d2", "body two", "pkg/d.go", 3, 3, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "d1", Content: "body one", Source: "pkg/d.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "d2", Content: "body two", Source: "pkg/d.go", StartLine: 3, EndLine: 3}, Score: 0.8},
	}
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
		MinFullResults: 5, MaxDepth: DepthL1,
		Pinned: []PinRef{{Source: "pkg/d.go", ChunkID: "d1"}},
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if trace.FloorRequested != 0 || trace.FloorRendered != 0 {
		t.Fatalf("floor must be vacuous below DepthL2: requested=%d rendered=%d",
			trace.FloorRequested, trace.FloorRendered)
	}
	if trace.EvidenceBlocks != 0 || trace.SourcesWithEvidence != 0 || strings.Contains(out, "--- ") {
		t.Fatalf("MaxDepth L1 must render no evidence:\n%s", out)
	}
	if trace.SourcesAtL0 != 1 || trace.Sources[0].EffectiveDepth != DepthL0 {
		t.Fatalf("orientation-only source wrong: atL0=%d depth=%v", trace.SourcesAtL0, trace.Sources[0].EffectiveDepth)
	}
	if len(trace.UnmatchedPins) != 1 || trace.UnmatchedPins[0].ChunkID != "d1" {
		t.Fatalf("vacuous pin must be reported unmatched: %+v", trace.UnmatchedPins)
	}
	if trace.MaxDepth != DepthL1 {
		t.Fatalf("MaxDepth echo wrong: %v", trace.MaxDepth)
	}
	if trace.EstimatedTokensUsed <= 0 ||
		trace.EstimatedTokensUsed+trace.EstimatedTokensFree != trace.MaxTokens {
		t.Fatalf("EstimatedTokensFree must be the remainder: used=%d free=%d max=%d",
			trace.EstimatedTokensUsed, trace.EstimatedTokensFree, trace.MaxTokens)
	}
}

func TestRenderProgressiveOneSourceDominates(t *testing.T) {
	// 8 of 10 results from one source: the trace must still show every
	// distinct source rendered at least at L0, and evidence concentration is
	// visible in EvidenceBlocks vs SourcesWithEvidence.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	var results []SearchResult
	var raw [][]any
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("dom%d", i)
		content := fmt.Sprintf("line %d of dominant", i)
		raw = append(raw, []any{id, content, "pkg/dom.go", i*2 + 1, i*2 + 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"})
		results = append(results, SearchResult{
			Chunk: Chunk{ID: id, Content: content, Source: "pkg/dom.go", StartLine: i*2 + 1, EndLine: i*2 + 1},
			Score: 0.9 - float64(i)*0.01,
		})
	}
	raw = append(raw,
		[]any{"m1", "minor one", "pkg/m1.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		[]any{"m2", "minor two", "pkg/m2.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"})
	storeChunksRaw(t, store, raw)
	results = append(results,
		SearchResult{Chunk: Chunk{ID: "m1", Content: "minor one", Source: "pkg/m1.go", StartLine: 1, EndLine: 1}, Score: 0.5},
		SearchResult{Chunk: Chunk{ID: "m2", Content: "minor two", Source: "pkg/m2.go", StartLine: 1, EndLine: 1}, Score: 0.4})

	_, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 10000, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if trace.DistinctSources != 3 {
		t.Fatalf("DistinctSources = %d, want 3", trace.DistinctSources)
	}
	if trace.OmittedSources != 0 {
		t.Fatalf("ample budget must render every source, omitted %d", trace.OmittedSources)
	}
	if trace.EvidenceBlocks != 10 || trace.SourcesWithEvidence != 3 {
		t.Fatalf("concentration wrong: blocks=%d sourcesWithEvidence=%d", trace.EvidenceBlocks, trace.SourcesWithEvidence)
	}
}

func TestRenderProgressiveMaxBytesWholeBlocksUTF8(t *testing.T) {
	// Byte ceiling drops whole blocks and never splits a multi-byte rune.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	multi := strings.Repeat("héllo wörld ", 40) // multi-byte runes throughout
	storeChunksRaw(t, store, [][]any{
		{"u1", "tiny", "pkg/u1.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"u2", multi, "pkg/u2.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "u1", Content: "tiny", Source: "pkg/u1.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "u2", Content: multi, Source: "pkg/u2.go", StartLine: 1, EndLine: 1}, Score: 0.8},
	}
	// Bytes fit source 1 fully but not source 2's evidence.
	out, trace, err := r.RenderProgressive(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 250,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if len(out) > 250 {
		t.Fatalf("output %d bytes exceeds MaxBytes", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatal("output split a multi-byte rune")
	}
	if strings.Contains(out, "wörld") {
		t.Fatal("oversized evidence must be dropped whole, not partially rendered")
	}
	// Attribution invariant: no partially-rendered block may be attributed.
	for _, src := range trace.Sources {
		for _, ev := range src.RenderedEvidence {
			if !strings.Contains(out, fmt.Sprintf("--- %s (lines %d-", ev.Source, ev.StartLine)) {
				t.Fatalf("attributed evidence %+v not present in output", ev)
			}
		}
	}
}

// orderRecordingStore records the progressive read sequence and the chunk IDs
// the digest read was asked for. It embeds *SQLiteStore, so real rows are
// still read; only the bookkeeping is added.
type orderRecordingStore struct {
	*SQLiteStore
	calls    []string
	digested []string
}

func (s *orderRecordingStore) SourceProvenanceBatch(ctx context.Context, sources []string) (map[string]SourceProvenance, error) {
	s.calls = append(s.calls, "provenance")
	return s.SQLiteStore.SourceProvenanceBatch(ctx, sources)
}

func (s *orderRecordingStore) SourceSummaryBatch(ctx context.Context, sources []string) (map[string]SourceSummary, error) {
	s.calls = append(s.calls, "summary")
	return s.SQLiteStore.SourceSummaryBatch(ctx, sources)
}

func (s *orderRecordingStore) ChunkContentDigestBatch(ctx context.Context, chunkIDs []string) (map[string]string, error) {
	s.calls = append(s.calls, "digest")
	s.digested = append(s.digested, chunkIDs...)
	return s.SQLiteStore.ChunkContentDigestBatch(ctx, chunkIDs)
}

func TestRenderProgressiveReadOrderContract(t *testing.T) {
	// Spec section 8: provenance and summary FIRST, digests LAST, over EVERY
	// retrieved chunk. The digest read is the ground truth that catches a
	// reindex racing the earlier non-transactional reads; running it first
	// reopens the window, and digesting only the chunks that survive
	// allocation silently drops the detector for every chunk it skipped.
	store := newTestStore(t)
	rec := &orderRecordingStore{SQLiteStore: store}
	r, err := NewRetrieverWithEmbedder(progressiveTestEmbedder(), rec)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}
	emb := []byte{0, 0, 0, 0}
	bulky := strings.Repeat("padding line\n", 40)
	storeChunksRaw(t, store, [][]any{
		{"o1", "tiny", "pkg/o1.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"o2", bulky, "pkg/o2.go", 1, 40, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
		{"o3", bulky, "pkg/o2.go", 41, 80, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "o1", Content: "tiny", Source: "pkg/o1.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "o2", Content: bulky, Source: "pkg/o2.go", StartLine: 1, EndLine: 40}, Score: 0.8},
		{Chunk: Chunk{ID: "o3", Content: bulky, Source: "pkg/o2.go", StartLine: 41, EndLine: 80}, Score: 0.7},
	}
	// Deliberately too small for o2/o3, so "all retrieved" and "all rendered"
	// are different sets and the assertion below can tell them apart.
	_, trace, err := r.RenderProgressive(context.Background(), ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 300,
	})
	if err != nil {
		t.Fatalf("RenderProgressive: %v", err)
	}
	if got := strings.Join(rec.calls, ","); got != "provenance,summary,digest" {
		t.Fatalf("read order = %q, want provenance,summary,digest", got)
	}
	if got := strings.Join(rec.digested, ","); got != "o1,o2,o3" {
		t.Fatalf("digest read covered %q, want every retrieved chunk o1,o2,o3", got)
	}
	if trace.EvidenceBlocks >= len(results) {
		t.Fatalf("fixture must leave some chunks unrendered, got %d of %d blocks",
			trace.EvidenceBlocks, len(results))
	}
}

func TestProgressiveByteAccountingMatchesAssembly(t *testing.T) {
	// The hard byte ceiling rests entirely on the allocator charging exactly
	// what assembly emits — orientation, evidence, and the one "\n" between
	// sources. If these two ever disagree, MaxBytes only holds by accident.
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"b1", "alpha body", "pkg/b1.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "hash1"), "vs1"},
		{"b2", "beta body", "pkg/b2.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "hash2"), "vs1"},
		{"b3", "gamma body", "pkg/b3.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "hash3"), "vs1"},
	})
	// A fresh summary on one source so the step 6b A1 -> A2 delta charge is
	// part of the accounting under test.
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/b2.go", ContentHash: "hash2", VectorSpaceID: "vs1",
		Abstract: "Beta purpose.", Overview: "Beta overview.", SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	results := []SearchResult{
		{Chunk: Chunk{ID: "b1", Content: "alpha body", Source: "pkg/b1.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "b2", Content: "beta body", Source: "pkg/b2.go", StartLine: 1, EndLine: 1}, Score: 0.8},
		{Chunk: Chunk{ID: "b3", Content: "gamma body", Source: "pkg/b3.go", StartLine: 1, EndLine: 1}, Score: 0.7},
	}
	req := ProgressiveRenderRequest{Results: results, MaxTokens: 10000, MaxBytes: 1 << 20}
	sources, err := r.prepareProgressiveSources(ctx, req)
	if err != nil {
		t.Fatalf("prepareProgressiveSources: %v", err)
	}
	st, err := allocate(sources, req, req.Estimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	out, truncated := assembleProgressive(sources, req.MaxBytes)
	if truncated {
		t.Fatal("ample budget must not reach the defensive trim")
	}
	if st.bytesUsed != len(out) {
		t.Fatalf("allocator charged %d bytes, assembly emitted %d:\n%q", st.bytesUsed, len(out), out)
	}
}

func TestAssembleProgressiveDropsWholeBlocks(t *testing.T) {
	// Spec section 11's defensive trim. Correct admission never reaches it, so
	// drive it directly: blocks go whole, evidence before orientation, and a
	// multi-byte rune is never split.
	multi := strings.Repeat("héllo wörld ", 6)
	src := &progressiveSource{
		source:      "pkg/t.go",
		results:     []SearchResult{{Chunk: Chunk{ID: "t1", Content: multi, Source: "pkg/t.go", StartLine: 1, EndLine: 1}, Score: 0.9}},
		reasons:     []ValidityReason{ReasonMissing},
		orientation: orientationMeta,
		evidence:    []int{0},
		decisions:   map[string]bool{},
	}
	orientationOnly := orientationText(src, orientationMeta)
	full := orientationOnly + evidenceText(src.results[0])

	out, truncated := assembleProgressive([]*progressiveSource{src}, len(full)-1)
	if !truncated {
		t.Fatal("over-budget assembly must report truncation")
	}
	if out != orientationOnly {
		t.Fatalf("evidence must be dropped whole, got %q", out)
	}
	if !utf8.ValidString(out) {
		t.Fatal("trim split a multi-byte rune")
	}
	if len(src.evidence) != 0 {
		t.Fatalf("dropped evidence must leave attribution state, got %v", src.evidence)
	}

	out, truncated = assembleProgressive([]*progressiveSource{src}, len(orientationOnly)-1)
	if !truncated || out != "" {
		t.Fatalf("orientation must be dropped whole too, got %q truncated=%v", out, truncated)
	}
	if src.orientation != orientationNone || !src.decisions[DecisionNoFit] {
		t.Fatalf("dropping the last block must omit the source: orientation=%v decisions=%v",
			src.orientation, src.decisions)
	}
}
