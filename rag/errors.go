// Package rag — sentinel errors.
//
// These two sentinels are kept distinct because the recovery action diverges:
//
//   - ErrVectorSpaceMismatch: query-side fix — the corpus is consistent but
//     the embedder produced an embedding in a different vector space.
//     Reconfigure the embedder (or its model) to match the corpus.
//
//   - ErrCorpusMixedVectorSpaces: corpus-side fix — chunks from multiple
//     incompatible vector spaces coexist (or legacy unknown-vsid rows
//     coexist with known ones). The only safe recovery is a full re-index.
package rag

import "errors"

var (
	// ErrVectorSpaceMismatch indicates the query embedding's vector-space
	// identifier does not match the (single) vector space found in the
	// corpus, or the query embedder did not produce a VectorSpaceID at all.
	ErrVectorSpaceMismatch = errors.New("rag: vector-space mismatch")

	// ErrCorpusMixedVectorSpaces indicates the corpus contains chunks from
	// more than one vector space, or a partially-migrated corpus where
	// known-vsid rows coexist with legacy unknown-vsid rows.
	ErrCorpusMixedVectorSpaces = errors.New("rag: corpus contains mixed vector spaces")
)
