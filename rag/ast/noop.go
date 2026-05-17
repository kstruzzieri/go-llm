package ast

import (
	"context"
	"fmt"
	"path/filepath"
)

// NoOpExtractor satisfies [Extractor] without doing any work. It is the
// safe choice for callers that have not yet wired a real extractor:
// Languages reports no supported languages, Extract returns an empty graph
// stamped with the requested scope/vsid, and Stale always reports true (so any
// previously-cached state is invalidated rather than served).
type NoOpExtractor struct{}

// Languages implements [Extractor]. Returns nil — claiming no languages
// lets callers short-circuit on the Languages check before invoking Extract.
func (NoOpExtractor) Languages() []string { return nil }

// Extract implements [Extractor]. Always succeeds with an empty graph stamped
// with the requested scope, vsid, and canonicalized root.
func (NoOpExtractor) Extract(_ context.Context, scope string, root string, vectorSpaceID string) (SymbolGraph, error) {
	return SymbolGraph{
		Scope:         scope,
		VectorSpaceID: vectorSpaceID,
		Root:          filepath.ToSlash(filepath.Clean(root)),
	}, nil
}

// Stale implements [Extractor]. Always returns true so a caller relying on
// a no-op extractor never accidentally serves a stale persisted graph; this
// is fail-closed by design and matches the safety stance the package's
// drift-signature scope inherits from the chunk store.
func (NoOpExtractor) Stale(_ context.Context, _ string, _ string, _ string) (bool, error) {
	return true, nil
}

// NoOpStore satisfies [SymbolStore] without persisting anything. Writes
// succeed silently; reads return [ErrSymbolNotFound] (singular) or an empty
// [CallSet] (plural). Useful as the default when AST persistence has not been
// wired in by the consumer.
type NoOpStore struct{}

// UpsertGraph implements [SymbolStore].
func (NoOpStore) UpsertGraph(_ context.Context, graph SymbolGraph) error {
	for i, edge := range graph.Calls {
		if err := edge.Validate(); err != nil {
			return fmt.Errorf("call edge %d: %w", i, err)
		}
	}
	return nil
}

// DeleteGraph implements [SymbolStore].
func (NoOpStore) DeleteGraph(_ context.Context, _ string, _ string) error { return nil }

// ExtractionSignature implements [SymbolStore].
func (NoOpStore) ExtractionSignature(_ context.Context, _ string, _ string) (string, error) {
	return "", ErrSymbolNotFound
}

// GetSymbol implements [SymbolStore].
func (NoOpStore) GetSymbol(_ context.Context, _ string, _ string, _ string) (SymbolNode, error) {
	return SymbolNode{}, ErrSymbolNotFound
}

// SymbolEnclosing implements [SymbolStore].
func (NoOpStore) SymbolEnclosing(_ context.Context, _ string, _ string, _ string, _ int) (SymbolNode, error) {
	return SymbolNode{}, ErrSymbolNotFound
}

// Callers implements [SymbolStore].
func (NoOpStore) Callers(_ context.Context, _ string, _ string, _ string, limit int) (CallSet, error) {
	if limit < 0 {
		return CallSet{}, fmt.Errorf("%w: negative callers limit %d", ErrInvalidArgument, limit)
	}
	return CallSet{}, nil
}

// Callees implements [SymbolStore].
func (NoOpStore) Callees(_ context.Context, _ string, _ string, _ string, limit int) (CallSet, error) {
	if limit < 0 {
		return CallSet{}, fmt.Errorf("%w: negative callees limit %d", ErrInvalidArgument, limit)
	}
	return CallSet{}, nil
}
