package rag

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateProgressiveRequest(t *testing.T) {
	valid := ProgressiveRenderRequest{MaxTokens: 100, MaxBytes: 1000}
	tests := []struct {
		name    string
		mutate  func(*ProgressiveRenderRequest)
		wantErr string
	}{
		{"valid", func(r *ProgressiveRenderRequest) {}, ""},
		{"zero MaxTokens", func(r *ProgressiveRenderRequest) { r.MaxTokens = 0 }, "MaxTokens"},
		{"negative MaxTokens", func(r *ProgressiveRenderRequest) { r.MaxTokens = -1 }, "MaxTokens"},
		{"zero MaxBytes", func(r *ProgressiveRenderRequest) { r.MaxBytes = 0 }, "MaxBytes"},
		{"negative MinFullResults", func(r *ProgressiveRenderRequest) { r.MinFullResults = -1 }, "MinFullResults"},
		{"MaxDepth out of range", func(r *ProgressiveRenderRequest) { r.MaxDepth = Depth(99) }, "MaxDepth"},
		{"negative MaxDepth", func(r *ProgressiveRenderRequest) { r.MaxDepth = Depth(-1) }, "MaxDepth"},
		{"blank pin source", func(r *ProgressiveRenderRequest) {
			r.Pinned = []PinRef{{Source: "", ChunkID: "c1"}}
		}, "pin"},
		{"blank pin chunk id", func(r *ProgressiveRenderRequest) {
			r.Pinned = []PinRef{{Source: "s", ChunkID: ""}}
		}, "pin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid
			tt.mutate(&req)
			err := validateProgressiveRequest(req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tt.wantErr)
			}
		})
	}
}

// allocFixture builds n sources each with one result of the given content
// sizes, no summaries (metadata orientation only).
func allocFixture(contents ...string) []*progressiveSource {
	sources := make([]*progressiveSource, len(contents))
	for i, c := range contents {
		src := fmt.Sprintf("pkg/f%02d.go", i)
		sources[i] = &progressiveSource{
			source:     src,
			firstIndex: i,
			results: []SearchResult{{
				Chunk: Chunk{ID: fmt.Sprintf("c%02d", i), Content: c, Source: src, StartLine: 1, EndLine: 1},
				Score: 0.9,
			}},
			resultIdx: []int{i},
			provFound: true,
			prov:      SourceProvenance{ContentHash: "h", VectorSpaceID: "v"},
			reasons:   []ValidityReason{ReasonMissing},
			decisions: map[string]bool{},
		}
	}
	return sources
}

// freshSummaryFixture builds one source with two results and a fresh summary
// whose overview is the caller's. Overview size is the whole variable under
// test in both callers — huge means the L1 upgrade must be skipped, small
// means it must be taken — so they share everything else.
func freshSummaryFixture(overview string) *progressiveSource {
	return &progressiveSource{
		source:     "pkg/a.go",
		firstIndex: 0,
		results: []SearchResult{
			{Chunk: Chunk{ID: "c1", Content: "alpha alpha", Source: "pkg/a.go", StartLine: 1, EndLine: 1}, Score: 0.9},
			{Chunk: Chunk{ID: "c2", Content: "beta beta", Source: "pkg/a.go", StartLine: 3, EndLine: 3}, Score: 0.8},
		},
		resultIdx: []int{0, 1},
		provFound: true,
		prov:      SourceProvenance{ContentHash: "h", VectorSpaceID: "v"},
		summary: &SourceSummary{
			Source: "pkg/a.go", ContentHash: "h", VectorSpaceID: "v",
			Abstract: "Short.", Overview: overview,
			SummaryModel: "m", FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
		},
		fresh:     true,
		decisions: map[string]bool{},
	}
}

func TestAllocatePinnedOverflowIsError(t *testing.T) {
	sources := allocFixture(strings.Repeat("x", 4000))
	req := ProgressiveRenderRequest{
		MaxTokens: 10, MaxBytes: 1 << 20,
		Pinned: []PinRef{{Source: "pkg/f00.go", ChunkID: "c00"}},
	}
	_, err := allocate(sources, req, defaultEstimate)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("pinned overflow must error, got %v", err)
	}
	// Same behavior under the byte ceiling.
	req2 := ProgressiveRenderRequest{
		MaxTokens: 1 << 20, MaxBytes: 100,
		Pinned: []PinRef{{Source: "pkg/f00.go", ChunkID: "c00"}},
	}
	_, err = allocate(sources, req2, defaultEstimate)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("pinned byte overflow must error, got %v", err)
	}
}

func TestAllocateUnmatchedAndDuplicatePins(t *testing.T) {
	sources := allocFixture("small content")
	req := ProgressiveRenderRequest{
		MaxTokens: 10000, MaxBytes: 1 << 20,
		Pinned: []PinRef{
			{Source: "pkg/f00.go", ChunkID: "c00"},
			{Source: "pkg/f00.go", ChunkID: "c00"},  // duplicate: counted once
			{Source: "pkg/gone.go", ChunkID: "zzz"}, // unmatched: reported
		},
	}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(st.unmatchedPins) != 1 || st.unmatchedPins[0].Source != "pkg/gone.go" {
		t.Fatalf("unmatched pins = %+v", st.unmatchedPins)
	}
	if len(sources[0].evidence) != 1 {
		t.Fatalf("duplicate pin must admit one block, got %d", len(sources[0].evidence))
	}
	if !sources[0].decisions[DecisionCallerPinned] {
		t.Fatal("pinned source must carry caller_pinned")
	}
}

func TestAllocateFloorPreferenceRecordsShortfall(t *testing.T) {
	// Three sources; budget fits floor evidence for only one.
	big := strings.Repeat("y", 2000)
	sources := allocFixture(big, big, big)
	req := ProgressiveRenderRequest{MaxTokens: 600, MaxBytes: 1 << 20, MinFullResults: 3}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if st.floorRequested != 3 {
		t.Fatalf("FloorRequested = %d, want 3", st.floorRequested)
	}
	if st.floorRendered != 1 {
		t.Fatalf("floor should render exactly 1, rendered %d", st.floorRendered)
	}
	// A block rejected once is never reconsidered: exactly one rejection per
	// oversized source, not one per pass (spec section 10 emission table).
	if st.nonFitting != 2 {
		t.Fatalf("nonFitting = %d, want 2 (one per rejected floor block)", st.nonFitting)
	}
	// Budget is never exceeded for the floor.
	if st.tokensUsed > req.MaxTokens {
		t.Fatalf("tokens %d exceed budget %d", st.tokensUsed, req.MaxTokens)
	}
}

func TestAllocateOversizedMiddleBlockSkipped(t *testing.T) {
	// Source 1 huge, sources 0 and 2 small: 0 and 2 render evidence, 1 gets
	// orientation only — no first-non-fit break (spec section 9.4 step 8).
	sources := allocFixture("small a", strings.Repeat("z", 100000), "small b")
	req := ProgressiveRenderRequest{MaxTokens: 300, MaxBytes: 1 << 20, MinFullResults: 1}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(sources[0].evidence) != 1 || len(sources[2].evidence) != 1 {
		t.Fatalf("small blocks must render: %d, %d", len(sources[0].evidence), len(sources[2].evidence))
	}
	if len(sources[1].evidence) != 0 {
		t.Fatal("huge block must be skipped")
	}
	if st.nonFitting == 0 {
		t.Fatal("skipped block must count in nonFitting")
	}
}

func TestAllocateMarginalCostL1NotForcedOverEvidence(t *testing.T) {
	// One source with a fresh summary whose L1 is huge; two results.
	// Evidence-first ordering must admit both evidence blocks before
	// considering the L1 upgrade, and the oversized upgrade is then skipped.
	src := freshSummaryFixture(strings.Repeat("w", 4000))
	req := ProgressiveRenderRequest{MaxTokens: 200, MaxBytes: 1 << 20, MinFullResults: 1}
	if _, err := allocate([]*progressiveSource{src}, req, defaultEstimate); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(src.evidence) != 2 {
		t.Fatalf("both evidence blocks must render before L1 is considered, got %d", len(src.evidence))
	}
	if src.orientation == orientationL0L1 {
		t.Fatal("oversized L1 upgrade must be skipped")
	}
}

func TestAllocateMaxDepthL1NoEvidencePinsUnmatched(t *testing.T) {
	sources := allocFixture("content")
	req := ProgressiveRenderRequest{
		MaxTokens: 10000, MaxBytes: 1 << 20, MaxDepth: DepthL1,
		Pinned: []PinRef{{Source: "pkg/f00.go", ChunkID: "c00"}},
	}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(sources[0].evidence) != 0 {
		t.Fatal("MaxDepth L1 must render no evidence")
	}
	if len(st.unmatchedPins) != 1 {
		t.Fatalf("pins under MaxDepth<L2 must report unmatched, got %+v", st.unmatchedPins)
	}
}

func TestAllocateNothingFits(t *testing.T) {
	sources := allocFixture(strings.Repeat("q", 10000))
	// Budget too small for even the metadata orientation block.
	req := ProgressiveRenderRequest{MaxTokens: 1, MaxBytes: 4, MinFullResults: 0}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("nothing-fits must not error: %v", err)
	}
	if sources[0].orientation != orientationNone {
		t.Fatal("source must be omitted")
	}
	if !sources[0].decisions[DecisionNoFit] {
		t.Fatal("omitted source must carry no_fit")
	}
	if st.omitted != 1 {
		t.Fatalf("omitted = %d, want 1", st.omitted)
	}
}

func TestAllocateNegativeEstimatorFallsBackToHeuristic(t *testing.T) {
	sources := allocFixture(strings.Repeat("e", 4000))
	bad := func(string) int { return -1 }
	// With the heuristic fallback (~1000 tokens for the evidence block), this
	// budget cannot fit evidence; a zero-clamp would wrongly admit it.
	req := ProgressiveRenderRequest{MaxTokens: 50, MaxBytes: 1 << 20, MinFullResults: 1}
	if _, err := allocate(sources, req, bad); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(sources[0].evidence) != 0 {
		t.Fatal("negative estimate must fall back to heuristic, not zero")
	}
}

func TestAllocateAdmitsBlockExactlyOnTheCeiling(t *testing.T) {
	// Under MaxDepth L1 the orientation block is the only candidate, so both
	// budgets can be set to exactly its cost. admit uses ">", so a block that
	// exactly fills the budget is admitted; ">=" would leave the last token
	// and the last byte permanently unusable.
	sources := allocFixture("content")
	o := orientationText(sources[0], orientationMeta)
	req := ProgressiveRenderRequest{
		MaxTokens: defaultEstimate(o), MaxBytes: len(o), MaxDepth: DepthL1,
	}
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if sources[0].orientation != orientationMeta {
		t.Fatalf("a block exactly on the ceiling must be admitted, orientation = %v", sources[0].orientation)
	}
	if st.tokensUsed != req.MaxTokens || st.bytesUsed != req.MaxBytes {
		t.Fatalf("used %d tokens / %d bytes, want the budget exactly (%d / %d)",
			st.tokensUsed, st.bytesUsed, req.MaxTokens, req.MaxBytes)
	}
	if st.nonFitting != 0 || st.omitted != 0 {
		t.Fatalf("nonFitting = %d, omitted = %d, want 0/0", st.nonFitting, st.omitted)
	}
}

func TestAllocatePinnedPreCheckChargesSourceSeparators(t *testing.T) {
	// The step 3 pre-check errors out, but the charging loop writes straight
	// to allocState without going through admit, so the two must agree to the
	// token. Two pinned sources is the smallest case where they can disagree:
	// only then does an inter-source separator exist to forget.
	pins := []PinRef{{Source: "pkg/f00.go", ChunkID: "c00"}, {Source: "pkg/f01.go", ChunkID: "c01"}}
	want := 0
	for i, src := range allocFixture("alpha alpha", "beta beta") {
		want += defaultEstimate(orientationText(src, orientationMeta))
		want += defaultEstimate(evidenceText(src.results[0]))
		if i > 0 {
			want += defaultEstimate("\n")
		}
	}

	// One token short of the true cost. A pre-check that omits the separator
	// would total want-1, pass, and then let the charging loop spend want.
	sources := allocFixture("alpha alpha", "beta beta")
	req := ProgressiveRenderRequest{MaxTokens: want - 1, MaxBytes: 1 << 20, Pinned: pins}
	_, err := allocate(sources, req, defaultEstimate)
	if err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("pre-check must charge the inter-source separator, got %v", err)
	}

	// At exactly the true cost it admits and spends exactly that, which pins
	// the pre-check and the charging loop to the same total.
	sources = allocFixture("alpha alpha", "beta beta")
	req.MaxTokens = want
	st, err := allocate(sources, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate at exact pinned cost: %v", err)
	}
	if st.tokensUsed != want {
		t.Fatalf("charged %d tokens, pre-check reserved %d", st.tokensUsed, want)
	}
}

func TestAllocateFreshSummaryUpgradesToL1(t *testing.T) {
	// The A3b -> A3c success path: a fresh summary with a small overview and
	// ample budget must actually reach orientationL0L1.
	src := freshSummaryFixture("A short overview of pkg/a.go.")
	req := ProgressiveRenderRequest{MaxTokens: 10000, MaxBytes: 1 << 20, MinFullResults: 1}
	st, err := allocate([]*progressiveSource{src}, req, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if src.orientation != orientationL0L1 {
		t.Fatalf("orientation = %v, want orientationL0L1", src.orientation)
	}
	if !src.decisions[DecisionRankUpgraded] {
		t.Fatal("an upgraded source must carry rank_upgraded")
	}
	if src.costRejected || src.decisions[DecisionBudgetDemoted] {
		t.Fatal("nothing was rejected on cost: budget_demoted must not be set")
	}
	// Step 6b charges a signed delta that REPLACES the L0 cost rather than
	// adding to it, so the total is the L0L1 block plus the evidence, never
	// L0 + L0L1 + evidence.
	want := defaultEstimate(orientationText(src, orientationL0L1))
	for i := range src.results {
		want += defaultEstimate(evidenceText(src.results[i]))
	}
	if st.tokensUsed != want {
		t.Fatalf("tokensUsed = %d, want %d (L0L1 orientation replacing L0, plus both evidence blocks)",
			st.tokensUsed, want)
	}
}

func TestAllocateEvidenceRendersInRetrievalOrder(t *testing.T) {
	// Pin the SECOND result so step 3 admits it first, then let the floor
	// admit the first in step 4: the evidence list must still come out in
	// retrieval order, because assembly renders it in slice order.
	src := freshSummaryFixture("A short overview of pkg/a.go.")
	req := ProgressiveRenderRequest{
		MaxTokens: 10000, MaxBytes: 1 << 20, MinFullResults: 1,
		Pinned: []PinRef{{Source: "pkg/a.go", ChunkID: "c2"}},
	}
	if _, err := allocate([]*progressiveSource{src}, req, defaultEstimate); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if len(src.evidence) != 2 || src.evidence[0] != 0 || src.evidence[1] != 1 {
		t.Fatalf("evidence = %v, want [0 1] (admitted 1 then 0)", src.evidence)
	}
}

func TestAllocateSortsSourcesIntoSourceOrder(t *testing.T) {
	// Steps 5 and 6b iterate the slice directly, so allocate sorts it rather
	// than trusting the caller. With a budget that fits exactly one more
	// orientation after the floor, source order decides which source gets it:
	// hand the slice over reversed and the earlier source must still win.
	big := strings.Repeat("y", 2000)
	built := allocFixture(big, big, big)
	first, second, third := built[0], built[1], built[2]
	reversed := []*progressiveSource{third, second, first}

	oCost := defaultEstimate(orientationText(first, orientationMeta))
	budget := oCost + defaultEstimate(evidenceText(first.results[0])) + oCost + defaultEstimate("\n")
	st, err := allocate(reversed, ProgressiveRenderRequest{MaxTokens: budget, MaxBytes: 1 << 20}, defaultEstimate)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if second.orientation == orientationNone {
		t.Fatal("the remaining orientation belongs to the earlier source, not whichever came first in the slice")
	}
	if third.orientation != orientationNone {
		t.Fatalf("last source in source order must be the omitted one, got %v", third.orientation)
	}
	if st.omitted != 1 {
		t.Fatalf("omitted = %d, want 1", st.omitted)
	}
	// The sort is in place, and assembly reads the same slice afterwards.
	if reversed[0] != first || reversed[1] != second || reversed[2] != third {
		t.Fatal("allocate must leave the caller's slice in source order")
	}
}

func TestAllocateBudgetDemotedRequiresSomethingRendered(t *testing.T) {
	// Sources 1 and 2 lose their floor evidence on cost but still render an
	// orientation: demoted, not omitted.
	big := strings.Repeat("y", 2000)
	sources := allocFixture(big, big, big)
	req := ProgressiveRenderRequest{MaxTokens: 600, MaxBytes: 1 << 20, MinFullResults: 3}
	if _, err := allocate(sources, req, defaultEstimate); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	for _, i := range []int{1, 2} {
		if !sources[i].decisions[DecisionBudgetDemoted] {
			t.Fatalf("source %d lost evidence on cost but rendered an orientation, want budget_demoted, got %v",
				i, sources[i].decisions)
		}
	}
	if sources[0].decisions[DecisionBudgetDemoted] {
		t.Fatal("source 0 rendered its floor evidence: nothing was demoted")
	}

	// An omitted source must not claim a demotion: costRejected is set in
	// step 4, before step 5 decides whether anything renders at all.
	omitted := allocFixture(strings.Repeat("q", 10000))
	if _, err := allocate(omitted, ProgressiveRenderRequest{MaxTokens: 1, MaxBytes: 4}, defaultEstimate); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if !omitted[0].costRejected {
		t.Fatal("fixture no longer exercises the guard: nothing was rejected on cost")
	}
	if omitted[0].decisions[DecisionBudgetDemoted] {
		t.Fatal("omitted source must not carry budget_demoted alongside no_fit")
	}
}
