package ast

import "context"

// CallSite pairs a related symbol with the edge that connects it to the
// symbol the caller asked about. Returned by [SymbolStore.Callers] and
// [SymbolStore.Callees] so call-site File/Line are not lost in the lookup.
//
// For Callers(target): Symbol is the caller, Edge.CalleeID == target.
// For Callees(target): Symbol is the callee, Edge.CallerID == target.
type CallSite struct {
	Symbol SymbolNode
	Edge   CallEdge
}

// CallSet is a bounded callers/callees result. Truncated reports whether more
// matching edges existed than the store returned.
type CallSet struct {
	Sites     []CallSite
	Truncated bool
}

// SymbolStore persists and serves symbol graphs. A single implementation
// handles both halves; the interface is intentionally NOT split into
// reader/writer because no real consumer has yet justified the split.
//
// Every write carries Scope and VectorSpaceID via the [SymbolGraph]; every
// read filters by both explicitly. Scope isolates corpora/roots that share an
// embedder; vector-space changes invalidate the graph alongside the parallel
// chunk store within that scope.
type SymbolStore interface {
	// UpsertGraph atomically replaces every row previously written under
	// (graph.Scope, graph.VectorSpaceID) with the contents of graph, including
	// graph.ExtractionSignature. Implementations MUST be transactional and MUST
	// reject invalid call edges with an error wrapping [ErrInvalidGraph].
	UpsertGraph(ctx context.Context, graph SymbolGraph) error

	// DeleteGraph removes every row stamped with (scope, vectorSpaceID). Used
	// for "drop without replacement" workflows (vsid retirement, migration
	// cleanup) — NOT as a precursor to UpsertGraph, which already replaces
	// atomically.
	DeleteGraph(ctx context.Context, scope string, vectorSpaceID string) error

	// ExtractionSignature returns the opaque signature persisted with the
	// current graph for (scope, vectorSpaceID). Callers pass this value back to
	// Extractor.Stale. Returns [ErrSymbolNotFound] when no graph is recorded.
	ExtractionSignature(ctx context.Context, scope string, vectorSpaceID string) (string, error)

	// GetSymbol resolves a SymbolID to its node within (scope, vectorSpaceID).
	// Returns [ErrSymbolNotFound] when the ID is not present in the
	// store for that scope/vsid.
	GetSymbol(ctx context.Context, scope string, vectorSpaceID string, symbolID string) (SymbolNode, error)

	// SymbolEnclosing returns the node whose [StartLine, EndLine] range
	// contains line, within file (canonicalized the same way the indexer
	// canonicalizes SymbolNode.File). Returns [ErrSymbolNotFound] when
	// no enclosing symbol is recorded.
	SymbolEnclosing(ctx context.Context, scope string, vectorSpaceID string, file string, line int) (SymbolNode, error)

	// Callers returns up to limit symbols that call symbolID, with the
	// edge that connects each one to symbolID. A limit of 0 lets the store use
	// its defensive default cap. Negative limits are invalid and MUST return
	// an error. CallSet.Truncated reports whether more matches existed than
	// were returned.
	Callers(ctx context.Context, scope string, vectorSpaceID string, symbolID string, limit int) (CallSet, error)

	// Callees returns up to limit symbols called by symbolID, with the
	// edge that connects symbolID to each one. Same limit semantics as
	// Callers.
	Callees(ctx context.Context, scope string, vectorSpaceID string, symbolID string, limit int) (CallSet, error)
}
