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
	hugeOverview := strings.Repeat("w", 4000)
	src := &progressiveSource{
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
			Abstract: "Short.", Overview: hugeOverview,
			SummaryModel: "m", FormatVersion: SourceSummaryFormatVersion, SummarizedAt: 1700000000,
		},
		fresh:     true,
		decisions: map[string]bool{},
	}
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
