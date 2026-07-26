package rag

import (
	"context"
	"fmt"
)

// Depth is the rendered fidelity of one source in a progressive render.
// Slice 3 of #189 supersedes this with a cross-domain contextdepth package;
// it lives in rag until the mixed assembler exists.
type Depth int

const (
	DepthNone Depth = iota // omitted entirely
	DepthL0                // one-line abstract, or a deterministic metadata overview
	DepthL1                // structured overview
	DepthL2                // full chunk evidence
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
	MaxTokens      int      // required, > 0
	MaxBytes       int      // required, > 0
	MinFullResults int      // L2 floor preference; 0 => 1; negative rejected
	MaxDepth       Depth    // DepthNone => DepthL2
	Pinned         []PinRef // caller-required L2
	// Estimate must be pure and deterministic — the same string must always
	// cost the same. The pinned pre-check and the step 6b upgrade delta each
	// call it twice on the same text and assume the two calls agree, so a
	// stateful estimator would let the pre-check pass and leave the charging
	// loop to overspend the ceiling it was supposed to guard.
	Estimate func(string) int // nil, or a negative result, => defaultEstimate
}

// defaultEstimate is the token heuristic used when the caller supplies no
// Estimate, or when a supplied one returns a negative value.
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

// ProgressiveTrace explains a whole progressive render (spec section 10).
type ProgressiveTrace struct {
	MaxTokens           int
	MaxBytes            int
	MaxDepth            Depth
	EstimatedTokensUsed int
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
	FloorRequested      int // results
	FloorRendered       int // results, excluding pinned
	UnmatchedPins       []PinRef
	Sources             []ProgressiveSourceTrace
}

// ProgressiveSourceTrace explains one source's rendering decision.
type ProgressiveSourceTrace struct {
	Source               string
	Managed              bool
	BestRank             int     // 1-based index in Results of this source's first result
	BestScore            float64 // score of that first result
	ScoreKind            string  // "semantic_similarity"
	EffectiveDepth       Depth   // DepthNone here means omitted entirely — unlike ProgressiveRenderRequest.MaxDepth, where DepthNone means unrestricted
	OrientationGenerated bool    // true => stored summary text; false => metadata overview. Meaningless when EffectiveDepth == DepthNone.
	MetadataFromSnapshot bool    // metadata built from retrieval-snapshot chunk fields (race path)
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
	case req.MaxDepth < DepthNone || req.MaxDepth > DepthL2:
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
