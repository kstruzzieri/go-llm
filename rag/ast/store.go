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

// SymbolStore persists and serves symbol graphs. A single implementation
// handles both halves; the interface is intentionally NOT split into
// reader/writer because no real consumer has yet justified the split.
//
// Every write carries a VectorSpaceID via the [SymbolGraph]; every read
// filters by one explicitly. A vector-space change atomically invalidates
// the graph alongside the parallel chunk store.
type SymbolStore interface {
	// UpsertGraph atomically replaces every row previously written under
	// graph.VectorSpaceID with the contents of graph. Implementations MUST
	// be transactional — a partially-written graph would leave the read
	// path returning nodes with edges pointing at missing endpoints.
	UpsertGraph(ctx context.Context, graph SymbolGraph) error

	// DeleteByVectorSpace removes every row stamped with vectorSpaceID.
	// Used for "drop without replacement" workflows (vsid retirement,
	// migration cleanup) — NOT as a precursor to UpsertGraph, which
	// already replaces atomically.
	DeleteByVectorSpace(ctx context.Context, vectorSpaceID string) error

	// GetSymbol resolves a SymbolID to its node within vectorSpaceID.
	// Returns [ErrSymbolNotFound] when the ID is not present in the
	// store for that vsid.
	GetSymbol(ctx context.Context, vectorSpaceID string, symbolID string) (SymbolNode, error)

	// SymbolEnclosing returns the node whose [StartLine, EndLine] range
	// contains line, within file (canonicalized the same way the indexer
	// canonicalizes SymbolNode.File). Returns [ErrSymbolNotFound] when
	// no enclosing symbol is recorded.
	SymbolEnclosing(ctx context.Context, vectorSpaceID string, file string, line int) (SymbolNode, error)

	// Callers returns up to limit symbols that call symbolID, with the
	// edge that connects each one to symbolID. A limit of 0 means
	// unlimited. Implementations SHOULD enforce a default cap when the
	// caller passes 0 on a known-hot symbol; the interface does not
	// require any particular ordering.
	Callers(ctx context.Context, vectorSpaceID string, symbolID string, limit int) ([]CallSite, error)

	// Callees returns up to limit symbols called by symbolID, with the
	// edge that connects symbolID to each one. Same limit semantics as
	// Callers.
	Callees(ctx context.Context, vectorSpaceID string, symbolID string, limit int) ([]CallSite, error)
}
