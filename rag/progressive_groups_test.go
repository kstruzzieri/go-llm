package rag

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// The expected representation descriptors are spelled out with LITERAL
// contextdepth values rather than reusing the implementation's repAbstract /
// repMeta / ... vars on purpose: a test that names the same var the renderer
// names cannot detect a mutation of that var (lesson: an assertion that
// derives from the value under test is blind to it). These are the spec's
// vocabulary, restated.
var (
	wantRepMeta     = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata}
	wantRepAbstract = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationGenerated}
	wantRepOverview = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationGenerated}
	wantRepEvidence = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}
)

// progressiveGroupsFixture seeds a store with the three ladder shapes the
// groups projection must distinguish, in retrieval order:
//
//	pkg/fresh.go   two results, valid stored summary  -> abstract rungs
//	pkg/stale.go   two results, content-hash mismatch -> metadata rung
//	pkg/demote.go  one result, valid but huge summary -> abstract rungs, and
//	               under a tight budget the FLAT render either demotes it to
//	               the metadata overview or omits it entirely — both are
//	               allocation state written after the groups snapshot is taken.
func progressiveGroupsFixture(t *testing.T) (*Retriever, []SearchResult) {
	t.Helper()
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	freshA, freshB := "fresh body one", "fresh body two"
	staleA, staleB := "stale body one", "stale body two"
	demote := "demote body"
	hugeAbstract := strings.Repeat("long stored abstract text. ", 40)

	storeChunksRaw(t, store, [][]any{
		{"fr1", freshA, "pkg/fresh.go", 1, 1, "go", `{"symbol_path":"F1"}`, emb, int64(1700000000), "kf1", sigJSON(t, "hashF"), "vs1"},
		{"fr2", freshB, "pkg/fresh.go", 3, 3, "go", `{}`, emb, int64(1700000000), "kf2", sigJSON(t, "hashF"), "vs1"},
		{"sl1", staleA, "pkg/stale.go", 1, 1, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashS"), "vs1"},
		{"sl2", staleB, "pkg/stale.go", 3, 3, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashS"), "vs1"},
		{"dm1", demote, "pkg/demote.go", 1, 1, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashD"), "vs1"},
	})
	for _, s := range []SourceSummary{
		{Source: "pkg/fresh.go", ContentHash: "hashF", VectorSpaceID: "vs1",
			Abstract: "Fresh purpose.", Overview: "Fresh overview.", SummaryModel: "m",
			FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000},
		// Deliberately hashed against a version the chunks do not carry.
		{Source: "pkg/stale.go", ContentHash: "OLD-hashS", VectorSpaceID: "vs1",
			Abstract: "Stale purpose.", Overview: "Stale overview.", SummaryModel: "m",
			FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000},
		{Source: "pkg/demote.go", ContentHash: "hashD", VectorSpaceID: "vs1",
			Abstract: hugeAbstract, Overview: hugeAbstract, SummaryModel: "m",
			FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000},
	} {
		if err := store.UpsertSourceSummary(ctx, s); err != nil {
			t.Fatalf("upsert %s: %v", s.Source, err)
		}
	}

	results := []SearchResult{
		{Chunk: Chunk{ID: "fr1", Content: freshA, Source: "pkg/fresh.go", StartLine: 1, EndLine: 1,
			Language: "go", Metadata: map[string]string{"symbol_path": "F1"}, StableKey: "kf1"}, Score: 0.9},
		{Chunk: Chunk{ID: "fr2", Content: freshB, Source: "pkg/fresh.go", StartLine: 3, EndLine: 3,
			Language: "go", StableKey: "kf2"}, Score: 0.85},
		{Chunk: Chunk{ID: "sl1", Content: staleA, Source: "pkg/stale.go", StartLine: 1, EndLine: 1}, Score: 0.8},
		{Chunk: Chunk{ID: "sl2", Content: staleB, Source: "pkg/stale.go", StartLine: 3, EndLine: 3}, Score: 0.75},
		{Chunk: Chunk{ID: "dm1", Content: demote, Source: "pkg/demote.go", StartLine: 1, EndLine: 1}, Score: 0.7},
	}
	return r, results
}

func groupForSource(t *testing.T, groups []ProgressiveGroup, source string) ProgressiveGroup {
	t.Helper()
	for _, g := range groups {
		if g.Desc.Subject.ID == source {
			return g
		}
	}
	t.Fatalf("no group for source %q in %d groups", source, len(groups))
	return ProgressiveGroup{}
}

func countVerbatim(desc contextdepth.AlternativeDesc) int {
	n := 0
	for _, rep := range desc.Representations {
		if rep.Kind == contextdepth.RepresentationVerbatim {
			n++
		}
	}
	return n
}

// TestRenderProgressiveWrapperEquivalence pins the legacy entry point to the
// groups-bearing one: same output, same trace, value for value. It cannot
// catch a regression in the SHARED render path (both entry points run it) —
// the existing byte-level tests and the allocator suite are that oracle, and
// they stay unmodified.
func TestRenderProgressiveWrapperEquivalence(t *testing.T) {
	r, results := progressiveGroupsFixture(t)
	ctx := context.Background()

	// Each arm names an allocator state the groups snapshot must be immune to,
	// and then ASSERTS the fixture reaches it: an arm whose name is a lie
	// tests nothing. wantDemoteDecision/wantOmitted describe pkg/demote.go,
	// the third source trace.
	tests := []struct {
		name               string
		maxTokens          int
		maxBytes           int
		wantDemoteDecision string
		wantOmitted        int
	}{
		// Every source renders; pkg/demote.go keeps its stored abstract.
		{"ample", 1 << 20, 1 << 20, "", 0},
		// pkg/demote.go's abstract no longer fits, so allocation writes
		// summaryBudgetOmitted and falls back to the metadata overview.
		{"demoted", 1 << 20, 500, DecisionBudgetDemoted, 0},
		// Tighter still: pkg/demote.go renders nothing at all.
		{"omitted", 1 << 20, 400, DecisionNoFit, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := ProgressiveRenderRequest{Results: results, MaxTokens: tc.maxTokens, MaxBytes: tc.maxBytes}
			wantOut, wantTrace, err := r.RenderProgressive(ctx, req)
			if err != nil {
				t.Fatalf("RenderProgressive: %v", err)
			}
			gotOut, gotTrace, groups, err := r.RenderProgressiveWithGroups(ctx, req)
			if err != nil {
				t.Fatalf("RenderProgressiveWithGroups: %v", err)
			}
			if gotOut != wantOut {
				t.Fatalf("output differs between entry points:\nlegacy:\n%s\ngroups:\n%s", wantOut, gotOut)
			}
			if !reflect.DeepEqual(gotTrace, wantTrace) {
				t.Fatalf("trace differs between entry points:\nlegacy: %+v\ngroups: %+v", wantTrace, gotTrace)
			}
			if len(groups) != wantTrace.DistinctSources {
				t.Fatalf("len(groups) = %d, want DistinctSources %d", len(groups), wantTrace.DistinctSources)
			}
			if wantTrace.DistinctSources != 3 {
				t.Fatalf("fixture must present 3 distinct sources, got %d", wantTrace.DistinctSources)
			}
			// Fixture guards: this arm must really drive the allocator into the
			// state its name claims.
			if wantTrace.OmittedSources != tc.wantOmitted {
				t.Fatalf("arm %q: OmittedSources = %d, want %d", tc.name, wantTrace.OmittedSources, tc.wantOmitted)
			}
			if tc.wantDemoteDecision != "" &&
				!slices.Contains(wantTrace.Sources[2].Decisions, tc.wantDemoteDecision) {
				t.Fatalf("arm %q: pkg/demote.go decisions %v lack %q",
					tc.name, wantTrace.Sources[2].Decisions, tc.wantDemoteDecision)
			}
			// The declaration is budget-blind: even the source that rendered
			// NOTHING still offers its whole ladder.
			demote := groupForSource(t, groups, "pkg/demote.go")
			if len(demote.Alternatives) != 4 {
				t.Fatalf("arm %q: pkg/demote.go offers %d alternatives, want the full 4",
					tc.name, len(demote.Alternatives))
			}
			for i, g := range groups {
				if g.Desc.Subject.Domain != contextdepth.DomainRAG {
					t.Errorf("groups[%d].Domain = %q, want %q", i, g.Desc.Subject.Domain, contextdepth.DomainRAG)
				}
				if g.Desc.Subject.ID != wantTrace.Sources[i].Source {
					t.Errorf("groups[%d].ID = %q, want trace source %q", i, g.Desc.Subject.ID, wantTrace.Sources[i].Source)
				}
				if g.Desc.Rank != wantTrace.Sources[i].BestRank {
					t.Errorf("groups[%d].Rank = %d, want BestRank %d", i, g.Desc.Rank, wantTrace.Sources[i].BestRank)
				}
			}
		})
	}
}

// TestProgressiveGroupsValidityMatrix pins the ladder shape per source class:
// which rungs exist, in which order, and that a FRESH source is never offered
// a metadata rung (whose note would claim "no summary" about a source that has
// one).
func TestProgressiveGroupsValidityMatrix(t *testing.T) {
	r, results := progressiveGroupsFixture(t)
	_, _, groups, err := r.RenderProgressiveWithGroups(context.Background(), ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}

	tests := []struct {
		name   string
		source string
		want   [][]contextdepth.RepresentationDesc
	}{
		{
			// Two orientation rungs, then prefixes k=1,2 ordered by (k, rung).
			name:   "fresh",
			source: "pkg/fresh.go",
			want: [][]contextdepth.RepresentationDesc{
				{wantRepAbstract},
				{wantRepAbstract, wantRepOverview},
				{wantRepAbstract, wantRepEvidence},
				{wantRepAbstract, wantRepOverview, wantRepEvidence},
				{wantRepAbstract, wantRepEvidence, wantRepEvidence},
				{wantRepAbstract, wantRepOverview, wantRepEvidence, wantRepEvidence},
			},
		},
		{
			name:   "stale",
			source: "pkg/stale.go",
			want: [][]contextdepth.RepresentationDesc{
				{wantRepMeta},
				{wantRepMeta, wantRepEvidence},
				{wantRepMeta, wantRepEvidence, wantRepEvidence},
			},
		},
		{
			name:   "fresh_single_result",
			source: "pkg/demote.go",
			want: [][]contextdepth.RepresentationDesc{
				{wantRepAbstract},
				{wantRepAbstract, wantRepOverview},
				{wantRepAbstract, wantRepEvidence},
				{wantRepAbstract, wantRepOverview, wantRepEvidence},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := groupForSource(t, groups, tc.source)
			if len(g.Alternatives) != len(tc.want) {
				t.Fatalf("%s: %d alternatives, want %d", tc.source, len(g.Alternatives), len(tc.want))
			}
			for i, want := range tc.want {
				got := g.Alternatives[i].Desc.Representations
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s alternative %d = %v, want %v", tc.source, i, got, want)
				}
			}
			for i, alt := range g.Alternatives {
				if !alt.Desc.Valid() {
					t.Errorf("%s alternative %d is not a valid descriptor: %v", tc.source, i, alt.Desc)
				}
				// The declared verbatim count, the carried attribution, and the
				// prefix that was actually concatenated must be one number.
				if n := countVerbatim(alt.Desc); n != len(alt.RenderedEvidence) {
					t.Errorf("%s alternative %d declares %d verbatim components but carries %d RenderedEvidence",
						tc.source, i, n, len(alt.RenderedEvidence))
				}
				for j, ev := range alt.RenderedEvidence {
					if ev.Source != tc.source {
						t.Errorf("%s alternative %d evidence %d attributed to %q", tc.source, i, j, ev.Source)
					}
				}
				if alt.Content == "" {
					t.Errorf("%s alternative %d has empty content", tc.source, i)
				}
			}
		})
	}

	// A fresh source may never be offered the metadata rung: its note line
	// would state "no summary" about a source that has a valid one.
	for _, source := range []string{"pkg/fresh.go", "pkg/demote.go"} {
		g := groupForSource(t, groups, source)
		for i, alt := range g.Alternatives {
			for _, rep := range alt.Desc.Representations {
				if rep.Kind == contextdepth.RepresentationMetadata {
					t.Errorf("%s alternative %d offers a metadata rung", source, i)
				}
			}
			if strings.Contains(alt.Content, "(no summary") {
				t.Errorf("%s alternative %d claims no summary:\n%s", source, i, alt.Content)
			}
		}
	}
	// ...and the stale source's metadata rung must carry the honest note, so
	// the assertion above is testing a distinction that exists.
	stale := groupForSource(t, groups, "pkg/stale.go")
	if !strings.Contains(stale.Alternatives[0].Content, "(no summary") {
		t.Fatalf("stale metadata rung lost its note:\n%s", stale.Alternatives[0].Content)
	}
}

// TestProgressiveGroupsDecoupledFromLocalBudget is the reason groups are built
// from the PREPARED snapshot rather than the post-allocation one (#331 spec
// 3.1). The retrieve tool renders under a small tool-local budget; if the
// capability declaration were derived from what that budget admitted, an
// omitted result could never be restored by a global allocator with room to
// spare.
func TestProgressiveGroupsDecoupledFromLocalBudget(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	small := "one"
	big := strings.Repeat("a very long second evidence line\n", 20)

	storeChunksRaw(t, store, [][]any{
		{"lb1", small, "pkg/lb.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "hashL"), "vs1"},
		{"lb2", big, "pkg/lb.go", 3, 22, "go", `{}`, emb, int64(1), "", sigJSON(t, "hashL"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "lb1", Content: small, Source: "pkg/lb.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "lb2", Content: big, Source: "pkg/lb.go", StartLine: 3, EndLine: 22}, Score: 0.8},
	}
	// Room for the orientation and the first evidence block only.
	_, trace, groups, err := r.RenderProgressiveWithGroups(ctx, ProgressiveRenderRequest{
		Results: results, MaxTokens: 60, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}
	// Fixture guard: the flat render must actually omit the second result, or
	// this test proves nothing.
	if trace.EvidenceBlocks != 1 || trace.NonFittingBlocks == 0 {
		t.Fatalf("fixture must admit exactly one of two results: blocks=%d nonFitting=%d",
			trace.EvidenceBlocks, trace.NonFittingBlocks)
	}
	if len(trace.Sources[0].RenderedEvidence) != 1 {
		t.Fatalf("flat render must attribute one block, got %d", len(trace.Sources[0].RenderedEvidence))
	}

	g := groupForSource(t, groups, "pkg/lb.go")
	var maxPrefix int
	var prefix2 *ProgressiveAlternative
	for i := range g.Alternatives {
		n := len(g.Alternatives[i].RenderedEvidence)
		if n > maxPrefix {
			maxPrefix = n
		}
		if n == 2 {
			prefix2 = &g.Alternatives[i]
		}
	}
	if prefix2 == nil {
		t.Fatalf("the tool-local budget pruned the prefix-2 alternative; longest prefix offered was %d", maxPrefix)
	}
	if prefix2.RenderedEvidence[0].ChunkID != "lb1" || prefix2.RenderedEvidence[1].ChunkID != "lb2" {
		t.Fatalf("prefix-2 must cover both in-hand results in retrieval order: %+v", prefix2.RenderedEvidence)
	}
	if !strings.Contains(prefix2.Content, "a very long second evidence line") {
		t.Fatalf("prefix-2 content omits the budget-rejected result:\n%s", prefix2.Content)
	}
}

// TestProgressiveGroupsContentBytes pins the materialized bytes of the deepest
// abstract-rung alternative to the SAME renderers the flat path uses. It is
// what makes an off-by-one in the prefix loop visible: the descriptor and the
// attribution could both stay consistent while the concatenated text lost a
// block.
func TestProgressiveGroupsContentBytes(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	one, two, three := "body one", "body two", "body three"

	storeChunksRaw(t, store, [][]any{
		{"cb1", one, "pkg/cb.go", 1, 1, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashC"), "vs1"},
		{"cb2", two, "pkg/cb.go", 3, 3, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashC"), "vs1"},
		{"cb3", three, "pkg/cb.go", 5, 5, "go", `{}`, emb, int64(1700000000), "", sigJSON(t, "hashC"), "vs1"},
	})
	if err := store.UpsertSourceSummary(ctx, SourceSummary{
		Source: "pkg/cb.go", ContentHash: "hashC", VectorSpaceID: "vs1",
		Abstract: "Content purpose.", Overview: "Content overview.", SummaryModel: "m",
		FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	results := []SearchResult{
		{Chunk: Chunk{ID: "cb1", Content: one, Source: "pkg/cb.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "cb2", Content: two, Source: "pkg/cb.go", StartLine: 3, EndLine: 3}, Score: 0.8},
		{Chunk: Chunk{ID: "cb3", Content: three, Source: "pkg/cb.go", StartLine: 5, EndLine: 5}, Score: 0.7},
	}
	req := ProgressiveRenderRequest{Results: results, MaxTokens: 1 << 20, MaxBytes: 1 << 20}

	// Expectation from the same prepared snapshot the renderer sees, so the
	// only thing under test is the concatenation itself.
	sources, err := r.prepareProgressiveSources(ctx, req)
	if err != nil {
		t.Fatalf("prepareProgressiveSources: %v", err)
	}
	if len(sources) != 1 || !sources[0].fresh {
		t.Fatalf("fixture must be one FRESH source, got %d sources", len(sources))
	}
	want := orientationText(sources[0], orientationL0)
	for _, res := range sources[0].results {
		want += evidenceText(res)
	}

	_, _, groups, err := r.RenderProgressiveWithGroups(ctx, req)
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}
	g := groupForSource(t, groups, "pkg/cb.go")
	wantDesc := []contextdepth.RepresentationDesc{
		wantRepAbstract, wantRepEvidence, wantRepEvidence, wantRepEvidence,
	}
	var found *ProgressiveAlternative
	for i := range g.Alternatives {
		if reflect.DeepEqual(g.Alternatives[i].Desc.Representations, wantDesc) {
			found = &g.Alternatives[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no [abstract + 3 evidence] alternative among %d", len(g.Alternatives))
	}
	if found.Content != want {
		t.Fatalf("alternative content mismatch\n got (%d bytes):\n%s\nwant (%d bytes):\n%s",
			len(found.Content), found.Content, len(want), want)
	}
	if len(found.RenderedEvidence) != 3 {
		t.Fatalf("RenderedEvidence = %d, want 3", len(found.RenderedEvidence))
	}
	if found.Desc.Depth() != contextdepth.DepthL2 {
		t.Fatalf("evidence-bearing alternative depth = %v, want L2", found.Desc.Depth())
	}
}

// TestRenderProgressiveWithGroupsBlankSource: a blank Chunk.Source cannot key
// a SubjectRef, so the groups entry point rejects it. The legacy entry point
// keeps its existing behavior exactly — this validation is additive.
func TestRenderProgressiveWithGroupsBlankSource(t *testing.T) {
	r, store := newProgressiveTestRetriever(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"bs1", "named body", "pkg/bs.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
	})
	results := []SearchResult{
		{Chunk: Chunk{ID: "bs1", Content: "named body", Source: "pkg/bs.go", StartLine: 1, EndLine: 1}, Score: 0.9},
		{Chunk: Chunk{ID: "bs2", Content: "anonymous body", Source: "", StartLine: 1, EndLine: 1}, Score: 0.8},
	}
	req := ProgressiveRenderRequest{Results: results, MaxTokens: 1 << 20, MaxBytes: 1 << 20}

	out, trace, groups, err := r.RenderProgressiveWithGroups(ctx, req)
	if err == nil {
		t.Fatal("blank Chunk.Source must error on the groups entry point")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Fatalf("error must name the offending index: %v", err)
	}
	if out != "" || groups != nil || !reflect.DeepEqual(trace, ProgressiveTrace{}) {
		t.Fatalf("error path must return zero values, got out=%q groups=%v trace=%+v", out, groups, trace)
	}

	// Same request, legacy entry point: unchanged, no new error.
	legacyOut, legacyTrace, err := r.RenderProgressive(ctx, req)
	if err != nil {
		t.Fatalf("legacy entry point must not gain an error: %v", err)
	}
	if legacyTrace.DistinctSources != 2 {
		t.Fatalf("legacy render must still group the blank source: %d distinct", legacyTrace.DistinctSources)
	}
	if !strings.Contains(legacyOut, `### source: ""`) {
		t.Fatalf("legacy render must still emit the blank source:\n%s", legacyOut)
	}
}
