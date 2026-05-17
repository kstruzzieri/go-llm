package ast

import "context"

// NoOpExtractor satisfies [Extractor] without doing any work. It is the
// safe choice for callers that have not yet wired a real extractor:
// Languages reports no supported languages, Extract returns an empty
// graph stamped with the requested vsid, and Stale always reports true
// (so any previously-cached state is invalidated rather than served).
type NoOpExtractor struct{}

// Languages implements [Extractor]. Returns nil — claiming no languages
// lets callers short-circuit on the Languages check before invoking Extract.
func (NoOpExtractor) Languages() []string { return nil }

// Extract implements [Extractor]. Always succeeds with an empty graph
// stamped with the requested vsid and root.
func (NoOpExtractor) Extract(_ context.Context, root string, vectorSpaceID string) (SymbolGraph, error) {
	return SymbolGraph{VectorSpaceID: vectorSpaceID, Root: root}, nil
}

// Stale implements [Extractor]. Always returns true so a caller relying on
// a no-op extractor never accidentally serves a stale persisted graph; this
// is fail-closed by design and matches the safety stance the package's
// drift-signature scope inherits from the chunk store.
func (NoOpExtractor) Stale(_ context.Context, _ string, _ string) (bool, error) {
	return true, nil
}

// NoOpStore satisfies [SymbolStore] without persisting anything. Writes
// succeed silently; reads return [ErrSymbolNotFound] (singular) or empty
// slices (plural). Useful as the default when AST persistence has not
// been wired in by the consumer.
type NoOpStore struct{}

// UpsertGraph implements [SymbolStore].
func (NoOpStore) UpsertGraph(_ context.Context, _ SymbolGraph) error { return nil }

// DeleteByVectorSpace implements [SymbolStore].
func (NoOpStore) DeleteByVectorSpace(_ context.Context, _ string) error { return nil }

// GetSymbol implements [SymbolStore].
func (NoOpStore) GetSymbol(_ context.Context, _ string, _ string) (SymbolNode, error) {
	return SymbolNode{}, ErrSymbolNotFound
}

// SymbolEnclosing implements [SymbolStore].
func (NoOpStore) SymbolEnclosing(_ context.Context, _ string, _ string, _ int) (SymbolNode, error) {
	return SymbolNode{}, ErrSymbolNotFound
}

// Callers implements [SymbolStore].
func (NoOpStore) Callers(_ context.Context, _ string, _ string, _ int) ([]CallSite, error) {
	return nil, nil
}

// Callees implements [SymbolStore].
func (NoOpStore) Callees(_ context.Context, _ string, _ string, _ int) ([]CallSite, error) {
	return nil, nil
}
