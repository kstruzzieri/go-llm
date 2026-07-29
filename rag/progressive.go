package rag

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// Depth is the rendered fidelity of one source in a progressive render.
//
// Deprecated: use contextdepth.Depth. Depth is a compatibility alias kept
// for source and numeric-encoding compatibility (Firn IDE, Flux ML, Quantum
// Trader consume this package); it will not be removed before the next major
// version. Note one boundary-only legacy behavior documented on
// ProgressiveRenderRequest.MaxDepth: the zero value there means
// "unrestricted (DepthL2)". New APIs reject the invalid zero instead.
type Depth = contextdepth.Depth

const (
	// Deprecated: use contextdepth.DepthInvalid. Retains its historical
	// "unrestricted" meaning only in ProgressiveRenderRequest.MaxDepth and its
	// historical "omitted" meaning only in ProgressiveSourceTrace.EffectiveDepth.
	DepthNone = contextdepth.DepthInvalid
	// Deprecated: use contextdepth.DepthL0.
	DepthL0 = contextdepth.DepthL0
	// Deprecated: use contextdepth.DepthL1.
	DepthL1 = contextdepth.DepthL1
	// Deprecated: use contextdepth.DepthL2.
	DepthL2 = contextdepth.DepthL2
)

// PinRef identifies one caller-required L2 result. ChunkID is the chunks
// primary key and is the only unambiguous coordinate: stable keys are blank
// by default and their index is non-unique (rag/migration.go:134).
type PinRef struct {
	Source  string
	ChunkID string
}

// RenderedEvidence describes one L2 block that was actually emitted. Only
// these coordinates may support source-line citations; orientation text never
// does.
type RenderedEvidence struct {
	Source    string
	ChunkID   string
	StableKey string // may be blank; display only
	StartLine int
	EndLine   int
	Score     float64
}

// ProgressiveRenderRequest configures RenderProgressive. Both budgets are
// hard ceilings. MaxDepth of DepthNone means unrestricted (DepthL2).
type ProgressiveRenderRequest struct {
	Results        []SearchResult
	MaxTokens      int // required, > 0
	MaxBytes       int // required, > 0
	MinFullResults int // L2 floor preference; 0 => 1; negative rejected
	// MaxDepth caps rendered fidelity. LEGACY BOUNDARY BEHAVIOR: the zero
	// value (DepthNone / contextdepth.DepthInvalid) means unrestricted
	// (DepthL2). This survives for compatibility at the rag boundary only;
	// new contextdepth-based APIs reject the invalid zero (spec D2).
	MaxDepth Depth    // DepthNone => DepthL2
	Pinned   []PinRef // caller-required L2
	// Estimate must be pure and deterministic — the same string must always
	// cost the same. The pinned pre-check and the step 6b upgrade delta each
	// call it twice on the same text and assume the two calls agree, so a
	// stateful estimator would let the pre-check pass and leave the charging
	// loop to overspend the ceiling it was supposed to guard.
	Estimate func(string) int // nil, or a non-positive result for non-empty text, => defaultEstimate
}

// defaultEstimate is the token heuristic used when the caller supplies no
// Estimate, or when a supplied one returns a non-positive value for
// non-empty text.
func defaultEstimate(s string) int { return (len(s) + 3) / 4 }

// Decision constants for ProgressiveSourceTrace.Decisions. Unlike
// ValidityReason, these are plain strings rather than a named type: their
// emission order carries no meaning (Decisions is sorted with sort.Strings,
// so alphabetical order is the whole contract, per spec), so there is no
// reasonOrder-style completeness guard to hang a named type on, and the spec
// mandates []string directly.
const (
	DecisionCallerPinned   = "caller_pinned"
	DecisionFloorReserved  = "floor_reserved"
	DecisionRankUpgraded   = "rank_upgraded"
	DecisionBudgetDemoted  = "budget_demoted"
	DecisionSummaryMissing = "summary_missing"
	DecisionSummaryStale   = "summary_stale"
	DecisionNoFit          = "no_fit"
)

// ProgressiveRenderFormatVersion identifies the rendered-block format emitted
// by RenderProgressive, reported on ProgressiveTrace.RenderFormatVersion.
// Version 2 quotes untrusted values (source, managed title) in orientation
// and evidence headers; version 1 (slice 1) interpolated them raw and is not
// kept — there is no legacy render switch. This is NOT
// SourceSummaryFormatVersion: stored summary rows are unchanged.
const ProgressiveRenderFormatVersion = 2

// ProgressiveTrace explains a whole progressive render (spec section 10).
type ProgressiveTrace struct {
	MaxTokens int
	MaxBytes  int
	MaxDepth  Depth
	// EstimatedTokensUsed is the configured estimator applied to every
	// FINALLY-emitted block and separator — recomputed from surviving blocks
	// after the defensive trim, so it always describes the returned output.
	// Estimator arithmetic, never provider-tokenizer truth.
	EstimatedTokensUsed int
	// EstimatedTokensFree == max(0, MaxTokens - EstimatedTokensUsed), computed
	// after final rendering and the defensive trim. For a conforming Estimate
	// (pure and deterministic, per its documented contract), every non-error
	// render satisfies EstimatedTokensUsed + EstimatedTokensFree == MaxTokens;
	// the max(0, ...) floor engages only when a contract-violating estimator
	// pushes the recompute past MaxTokens. An empty successful render reports
	// the full budget free; error returns keep the zero trace.
	EstimatedTokensFree int
	BytesUsed           int
	SelectedResults     int // input results
	DistinctSources     int
	SourcesAtL0         int // orientation-only, abstract or metadata overview
	SourcesAtL1         int // orientation-only, stored overview
	SourcesWithEvidence int
	EvidenceBlocks      int // results rendered, not sources
	OmittedSources      int
	NonFittingBlocks    int
	OutputTruncated     bool
	// TrimmedBlocks counts admitted blocks dropped by the defensive
	// whole-block byte trim. OutputTruncated == (TrimmedBlocks > 0).
	// NonFittingBlocks, by contrast, keeps its admission-time meaning: blocks
	// rejected on cost that never entered the output.
	TrimmedBlocks int
	// RenderFormatVersion is ProgressiveRenderFormatVersion for the format
	// this trace's render emitted.
	RenderFormatVersion int
	FloorRequested      int // results
	FloorRendered       int // results, excluding pinned
	UnmatchedPins       []PinRef
	Sources             []ProgressiveSourceTrace
}

// ProgressiveSourceTrace explains one source's rendering decision.
type ProgressiveSourceTrace struct {
	Source    string
	Managed   bool
	BestRank  int     // 1-based index in Results of this source's first result
	BestScore float64 // score of that first result
	ScoreKind string  // "semantic_similarity"
	// Omitted is true when nothing for this source was emitted. Invariants:
	// Omitted => EffectiveDepth == DepthNone (contextdepth.DepthInvalid),
	// EstimatedTokens == 0, RenderedEvidence empty. !Omitted =>
	// EffectiveDepth is a valid depth (L0/L1/L2).
	Omitted        bool
	EffectiveDepth Depth
	// OrientationGenerated: true => stored summary text rendered; false =>
	// deterministic metadata overview. Meaningful iff !Omitted.
	OrientationGenerated bool
	MetadataFromSnapshot bool // metadata built from retrieval-snapshot chunk fields (race path)
	ValidityReasons      []ValidityReason
	Decisions            []string // sorted for byte-stable traces
	EstimatedTokens      int
	RenderedEvidence     []RenderedEvidence
}

// validateProgressiveRequest enforces the public boundary contract
// (DEVELOPMENT_PRINCIPLES.md section 3; mirrors validateHierarchicalRequest).
func validateProgressiveRequest(req ProgressiveRenderRequest) error {
	switch {
	case req.MaxTokens <= 0:
		return fmt.Errorf("rag: progressive render: MaxTokens must be > 0, got %d", req.MaxTokens)
	case req.MaxBytes <= 0:
		return fmt.Errorf("rag: progressive render: MaxBytes must be > 0, got %d", req.MaxBytes)
	case req.MinFullResults < 0:
		return fmt.Errorf("rag: progressive render: MinFullResults must be >= 0, got %d", req.MinFullResults)
	case req.MaxDepth > DepthL2:
		return fmt.Errorf("rag: progressive render: MaxDepth out of range: %d", int(req.MaxDepth))
	}
	for i, pin := range req.Pinned {
		if pin.Source == "" || pin.ChunkID == "" {
			return fmt.Errorf("rag: progressive render: pin %d has blank Source or ChunkID", i)
		}
	}
	return nil
}

// progressiveStoreReader is the optional store capability RenderProgressive
// needs. *SQLiteStore implements it; any other VectorStore degrades to
// summary-missing metadata rendering without error (spec section 6).
type progressiveStoreReader interface {
	SourceProvenanceBatch(ctx context.Context, sources []string) (map[string]SourceProvenance, error)
	SourceSummaryBatch(ctx context.Context, sources []string) (map[string]SourceSummary, error)
	ChunkContentDigestBatch(ctx context.Context, chunkIDs []string) (map[string]string, error)
}

var _ progressiveStoreReader = (*SQLiteStore)(nil)
