package rag

import (
	"context"
	"fmt"
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
//	pkg/fresh.go   two results, valid stored summary  -> metadata + abstract rungs
//	pkg/stale.go   two results, content-hash mismatch -> metadata rung only
//	pkg/demote.go  one result, valid but huge summary -> metadata + abstract rungs,
//	               and under a tight budget the FLAT render either demotes it to
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
			if len(demote.Alternatives) != 6 {
				t.Fatalf("arm %q: pkg/demote.go offers %d alternatives, want the full 6",
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
// which rungs exist, in which order, and that EVERY source — fresh included —
// is offered the metadata rung as its cheapest alternative, with a note line
// that tells the truth about whether a summary exists.
func TestProgressiveGroupsValidityMatrix(t *testing.T) {
	r, results := progressiveGroupsFixture(t)
	out, _, groups, err := r.RenderProgressiveWithGroups(context.Background(), ProgressiveRenderRequest{
		Results: results, MaxTokens: 1 << 20, MaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}

	// The FLAT render is unperturbed by the projection above it. Under this
	// ample budget exactly one source (pkg/stale.go) is at the metadata level
	// and no source is budget-demoted, so a builder that wrote the allocator's
	// summaryBudgetOmitted flag — or that swapped a fresh source's rendered
	// orientation — moves one of these three counts.
	if got := strings.Count(out, "note: metadata overview"); got != 1 {
		t.Errorf("flat output carries %d metadata notes, want 1 (pkg/stale.go only):\n%s", got, out)
	}
	if strings.Contains(out, "summary omitted: budget") {
		t.Errorf("flat output claims a budget demotion under an ample budget:\n%s", out)
	}
	if got := strings.Count(out, "\npurpose: "); got != 2 {
		t.Errorf("flat output carries %d stored abstracts, want 2 (fresh + demote):\n%s", got, out)
	}

	tests := []struct {
		name   string
		source string
		fresh  bool
		want   [][]contextdepth.RepresentationDesc
	}{
		{
			// Three orientation rungs, then prefixes k=1,2 ordered by (k, rung).
			name:   "fresh",
			source: "pkg/fresh.go",
			fresh:  true,
			want: [][]contextdepth.RepresentationDesc{
				{wantRepMeta},
				{wantRepAbstract},
				{wantRepAbstract, wantRepOverview},
				{wantRepMeta, wantRepEvidence},
				{wantRepAbstract, wantRepEvidence},
				{wantRepAbstract, wantRepOverview, wantRepEvidence},
				{wantRepMeta, wantRepEvidence, wantRepEvidence},
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
			fresh:  true,
			want: [][]contextdepth.RepresentationDesc{
				{wantRepMeta},
				{wantRepAbstract},
				{wantRepAbstract, wantRepOverview},
				{wantRepMeta, wantRepEvidence},
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
				// Descriptor <-> content, per alternative. The evidence-bearing
				// alternatives are the ones with no other coverage: a builder
				// that rendered every prefix at a single hard-coded orientation
				// level would keep all the counts above consistent while
				// shipping abstract-only bytes under an [abstract, overview]
				// descriptor, and stale summary text under a metadata-only one.
				hasKind := func(k contextdepth.RepresentationKind) bool {
					for _, rep := range alt.Desc.Representations {
						if rep.Kind == k {
							return true
						}
					}
					return false
				}
				metadataOnly := !hasKind(contextdepth.RepresentationGenerated)
				if metadataOnly && strings.Contains(alt.Content, "\npurpose: ") {
					t.Errorf("%s alternative %d declares no generated component but renders stored summary text:\n%s",
						tc.source, i, alt.Content)
				}
				// The note is the provenance claim the model reads to price the
				// block, so it is pinned as EXACT text and per source class:
				// "no summary" about a source that has one is the lie this rung
				// used to be withheld to avoid.
				const noteNone, noteBudget = "(no summary", "(summary omitted: budget)"
				wantNote, otherNote := noteNone, noteBudget
				if tc.fresh {
					wantNote, otherNote = noteBudget, noteNone
				}
				if got := strings.Contains(alt.Content, wantNote); got != metadataOnly {
					t.Errorf("%s alternative %d: metadata-only=%v but %q presence=%v:\n%s",
						tc.source, i, metadataOnly, wantNote, got, alt.Content)
				}
				if strings.Contains(alt.Content, otherNote) {
					t.Errorf("%s alternative %d carries the wrong note variant %q:\n%s",
						tc.source, i, otherNote, alt.Content)
				}
				wantOverview := slices.Contains(alt.Desc.Representations, wantRepOverview)
				if got := strings.Contains(alt.Content, "\noverview: "); got != wantOverview {
					t.Errorf("%s alternative %d: declares overview=%v but renders overview=%v:\n%s",
						tc.source, i, wantOverview, got, alt.Content)
				}
			}
		})
	}

	// A fresh source's CHEAPEST alternative is the metadata rung. Without it
	// mixed assembly is strictly less capable than the flat allocator, which
	// falls back to exactly this block when a stored abstract does not fit
	// (progressive_alloc.go step 5): the mixed allocator would omit the source
	// where flat renders a short one. Cheapest is asserted by bytes, not by
	// position alone — a rung declared first that is not actually smaller buys
	// the allocator nothing.
	for _, source := range []string{"pkg/fresh.go", "pkg/demote.go"} {
		g := groupForSource(t, groups, source)
		meta := g.Alternatives[0]
		if len(meta.Desc.Representations) != 1 ||
			meta.Desc.Representations[0].Kind != contextdepth.RepresentationMetadata {
			t.Fatalf("%s alternative 0 is not the metadata rung: %v", source, meta.Desc.Representations)
		}
		if !strings.Contains(meta.Content, "note: metadata overview (summary omitted: budget)\n") {
			t.Errorf("%s metadata rung lacks the truthful budget note:\n%s", source, meta.Content)
		}
		if strings.Contains(meta.Content, "(no summary") {
			t.Errorf("%s metadata rung claims no summary about a source that has one:\n%s", source, meta.Content)
		}
		for i, alt := range g.Alternatives[1:] {
			if len(alt.Content) <= len(meta.Content) {
				t.Errorf("%s alternative %d is %d bytes, not larger than the %d-byte metadata rung",
					source, i+1, len(alt.Content), len(meta.Content))
			}
		}
	}
	// ...and the stale source's metadata rung must carry the OTHER note, so
	// the assertions above are testing a distinction that exists.
	stale := groupForSource(t, groups, "pkg/stale.go")
	if !strings.Contains(stale.Alternatives[0].Content, "note: metadata overview (no summary") {
		t.Fatalf("stale metadata rung lost its note:\n%s", stale.Alternatives[0].Content)
	}
}

func TestRenderProgressiveWithGroupsRespectsMaxDepth(t *testing.T) {
	tests := []struct {
		name       string
		maxDepth   Depth
		wantDepth  contextdepth.Depth
		wantCounts []int
	}{
		{"L0", DepthL0, contextdepth.DepthL0, []int{2, 1, 2}},
		{"L1", DepthL1, contextdepth.DepthL1, []int{3, 1, 3}},
		{"default_L2", DepthNone, contextdepth.DepthL2, []int{9, 3, 6}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, results := progressiveGroupsFixture(t)
			_, _, groups, err := r.RenderProgressiveWithGroups(context.Background(), ProgressiveRenderRequest{
				Results: results, MaxTokens: 1 << 20, MaxBytes: 1 << 20, MaxDepth: tc.maxDepth,
			})
			if err != nil {
				t.Fatalf("RenderProgressiveWithGroups: %v", err)
			}
			if len(groups) != len(tc.wantCounts) {
				t.Fatalf("len(groups) = %d, want %d", len(groups), len(tc.wantCounts))
			}
			for i, g := range groups {
				if len(g.Alternatives) != tc.wantCounts[i] {
					t.Errorf("group %q has %d alternatives, want %d",
						g.Desc.Subject.ID, len(g.Alternatives), tc.wantCounts[i])
				}
				for j, alt := range g.Alternatives {
					if got := alt.Desc.Depth(); got > tc.wantDepth {
						t.Errorf("group %q alternative %d depth = %v, exceeds %v",
							g.Desc.Subject.ID, j, got, tc.wantDepth)
					}
				}
			}
		})
	}
}

// TestProgressiveGroupsProjectionBytes measures what ONE call's projection
// costs in retained content bytes, because agent/tools/retrieve.go documents
// those figures on Retrieve.Progressive and a documented cost nobody measures
// drifts silently. In mixed mode the bytes are not transient: dispatch clones
// the set onto the anchor message and only an EVICTED chain gives it back.
//
// Content is quadratic in k — rungs x k(k+1)/2 evidence-block copies plus the
// orientation blocks — so the totals below are exact, not bands: a change to
// the rung set or to either block template must move them, and then the numbers
// in retrieve.go's comment have to move with them.
func TestProgressiveGroupsProjectionBytes(t *testing.T) {
	// k is agent/tools' maxRetrieveMaxK, the worst case that ceiling permits:
	// every retrieved result landing on ONE fresh source.
	const k = 20
	for _, tc := range []struct {
		name       string
		chunkBytes int
		want       int
	}{
		{"2KB chunks", 2048, 1410381},
		{"8KB chunks", 8192, 5541291},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := strings.Repeat("x", 63) + "\n"
			body := strings.Repeat(line, tc.chunkBytes/len(line))
			src := &progressiveSource{
				source: "pkg/big.go",
				fresh:  true,
				summary: &SourceSummary{
					Source: "pkg/big.go", Abstract: "Handles the big thing.",
					Overview:     "A slightly longer overview of the big thing.",
					SummaryModel: "m", FormatVersion: SourceSummaryFormatVersion,
					SummarizedAt: 1700000000,
				},
				decisions: map[string]bool{},
			}
			for i := 0; i < k; i++ {
				src.results = append(src.results, SearchResult{
					Chunk: Chunk{ID: fmt.Sprintf("c%d", i), Source: "pkg/big.go", Content: body,
						StartLine: 1, EndLine: 32, Language: "go"},
					Score: 0.5,
				})
			}
			groups := buildProgressiveGroups([]*progressiveSource{src}, DepthL2)
			if len(groups) != 1 {
				t.Fatalf("fixture must be one source, got %d groups", len(groups))
			}
			total := 0
			for _, a := range groups[0].Alternatives {
				total += len(a.Content)
			}
			if total != tc.want {
				t.Fatalf("projection retains %d content bytes (%.2f MB) for k=%d, want %d (%.2f MB); "+
					"update the figures on Retrieve.Progressive in agent/tools/retrieve.go",
					total, float64(total)/(1<<20), k, tc.want, float64(tc.want)/(1<<20))
			}
		})
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
	evidence := ""
	for _, res := range sources[0].results {
		evidence += evidenceText(res)
	}

	_, _, groups, err := r.RenderProgressiveWithGroups(ctx, req)
	if err != nil {
		t.Fatalf("RenderProgressiveWithGroups: %v", err)
	}
	g := groupForSource(t, groups, "pkg/cb.go")

	// BOTH rungs' deepest prefix, byte for byte. Pinning only the abstract rung
	// leaves a builder free to render every prefix at orientationL0 while the
	// overview rung's descriptor still promises [abstract, overview, ...].
	for _, tc := range []struct {
		name  string
		level orientationLevel
		desc  []contextdepth.RepresentationDesc
	}{
		{"abstract", orientationL0, []contextdepth.RepresentationDesc{
			wantRepAbstract, wantRepEvidence, wantRepEvidence, wantRepEvidence}},
		{"abstract_overview", orientationL0L1, []contextdepth.RepresentationDesc{
			wantRepAbstract, wantRepOverview, wantRepEvidence, wantRepEvidence, wantRepEvidence}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := orientationText(sources[0], tc.level) + evidence
			var found *ProgressiveAlternative
			for i := range g.Alternatives {
				if reflect.DeepEqual(g.Alternatives[i].Desc.Representations, tc.desc) {
					found = &g.Alternatives[i]
					break
				}
			}
			if found == nil {
				t.Fatalf("no %s + 3 evidence alternative among %d", tc.name, len(g.Alternatives))
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
		})
	}
}

// TestRenderProgressiveWithGroupsBlankSource: a blank Chunk.Source cannot key a
// SubjectRef, so it degrades the PROJECTION and nothing else. The render, the
// trace and the error are value-identical to the legacy entry point's — which is
// what a Progressive-but-not-Mixed consumer depends on, since agent/tools.Retrieve
// calls the WithGroups entry point unconditionally and cannot see the assembly
// mode. Two arms because the blank result must poison the projection from either
// position: dropping only the alternatives it would have contributed would leave
// its blocks in the flat content that mixed assembly then REPLACES.
func TestRenderProgressiveWithGroupsBlankSource(t *testing.T) {
	named := SearchResult{
		Chunk: Chunk{ID: "bs1", Content: "named body", Source: "pkg/bs.go", StartLine: 1, EndLine: 1}, Score: 0.9}
	blank := SearchResult{
		Chunk: Chunk{ID: "bs2", Content: "anonymous body", Source: "", StartLine: 1, EndLine: 1}, Score: 0.8}

	tests := []struct {
		name    string
		results []SearchResult
	}{
		{"blank_first", []SearchResult{blank, named}},
		{"blank_second", []SearchResult{named, blank}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, store := newProgressiveTestRetriever(t)
			ctx := context.Background()
			emb := []byte{0, 0, 0, 0}
			storeChunksRaw(t, store, [][]any{
				{"bs1", "named body", "pkg/bs.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "h"), "vs1"},
			})
			req := ProgressiveRenderRequest{Results: tc.results, MaxTokens: 1 << 20, MaxBytes: 1 << 20}

			out, trace, groups, err := r.RenderProgressiveWithGroups(ctx, req)
			if err != nil {
				t.Fatalf("a blank source must not fail the render: %v", err)
			}
			if groups != nil {
				t.Fatalf("blank source must yield NO groups, got %d", len(groups))
			}

			// Value-identical to the legacy entry point, output and trace both.
			legacyOut, legacyTrace, err := r.RenderProgressive(ctx, req)
			if err != nil {
				t.Fatalf("legacy entry point must not gain an error: %v", err)
			}
			if out != legacyOut {
				t.Fatalf("groups entry point changed the render:\nGOT:\n%s\nWANT:\n%s", out, legacyOut)
			}
			if !reflect.DeepEqual(trace, legacyTrace) {
				t.Fatalf("groups entry point changed the trace:\ngot  %+v\nwant %+v", trace, legacyTrace)
			}
			// Content pins, not shape pins: an empty render would satisfy both
			// equalities above.
			if legacyTrace.DistinctSources != 2 {
				t.Fatalf("render must still group the blank source: %d distinct", legacyTrace.DistinctSources)
			}
			if !strings.Contains(out, `### source: ""`) {
				t.Fatalf("render must still emit the blank source:\n%s", out)
			}
			if !strings.Contains(out, "anonymous body") || !strings.Contains(out, "named body") {
				t.Fatalf("both bodies must survive; mixed assembly has no groups to replace them with:\n%s", out)
			}
		})
	}
}

// TestRenderProgressiveWithGroupsNoResults is the one path where
// len(groups) == DistinctSources holds by accident rather than by the builder
// loop: both are zero, and the early return never reaches the builder at all.
func TestRenderProgressiveWithGroupsNoResults(t *testing.T) {
	r, _ := newProgressiveTestRetriever(t)
	out, trace, groups, err := r.RenderProgressiveWithGroups(context.Background(), ProgressiveRenderRequest{
		Results: nil, MaxTokens: 100, MaxBytes: 1000,
	})
	if err != nil {
		t.Fatalf("empty render: %v", err)
	}
	if groups != nil {
		t.Fatalf("no results means no groups, got %d", len(groups))
	}
	if out != "" || trace.DistinctSources != 0 || trace.SelectedResults != 0 {
		t.Fatalf("empty render wrong: out=%q trace=%+v", out, trace)
	}
	// The trace itself is NOT zero on this path — it still reports the budget.
	if trace.MaxTokens != 100 || trace.EstimatedTokensFree != 100 {
		t.Fatalf("empty render must report the whole budget free: %+v", trace)
	}
}
