package ast

import "context"

// Extractor walks a source tree and produces a [SymbolGraph]. The interface
// is language-agnostic so a single [SymbolStore] consumer can be wired to
// language-specific extractors (Go via go/ast, tree-sitter for others)
// without leaking parser types across the seam.
//
// Implementations MUST be safe for concurrent use; a caller indexing many
// roots in parallel will invoke Extract from multiple goroutines.
type Extractor interface {
	// Languages returns the set of language identifiers this extractor can
	// produce graphs for, using rag.Chunk.Language tokens ("go", "python",
	// "typescript", ...). The returned slice MUST be safe for the caller
	// to read concurrently with future Extract calls.
	Languages() []string

	// Extract walks root and produces a graph stamped with vectorSpaceID.
	// An empty graph with nil error is the correct response for a tree
	// containing no sources in any of this extractor's languages — that
	// case is success, not failure. Errors are reserved for IO / parse
	// failures the caller cannot recover from.
	//
	// The returned [SymbolGraph.Root] MUST equal root after the same
	// canonicalization Extract applies to node files (filepath.Clean +
	// forward slashes), so paths join cleanly.
	Extract(ctx context.Context, root string, vectorSpaceID string) (SymbolGraph, error)

	// Stale reports whether a graph previously extracted from root and
	// stamped with prevSignature can still be reused, or whether the
	// caller must re-extract. The signature is an opaque token defined
	// by the extractor itself — it MAY encode the vector-space identity,
	// source-file fingerprints, the extractor's own version, the toolchain
	// version, or any combination. Callers treat it as a string and pass
	// it back unchanged.
	//
	// Returning (true, nil) forces re-extraction. Returning (false, nil)
	// authorizes reuse of the previously persisted graph.
	Stale(ctx context.Context, root string, prevSignature string) (bool, error)
}
