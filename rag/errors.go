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

	// ErrVectorSpaceDrift indicates an incremental index attempted to mix
	// freshly embedded chunks with cached chunks from a different vector space.
	ErrVectorSpaceDrift = errors.New("rag: vector-space drift")

	// ErrMissingVectorSpaceID indicates an embedder or write path produced a
	// non-empty batch without a VectorSpaceID/Provider/Model identity.
	ErrMissingVectorSpaceID = errors.New("rag: missing vector-space id")

	// ErrIncrementalRebuildRequired indicates the incremental path cannot
	// safely reuse cached embeddings and the caller should do a full re-embed.
	ErrIncrementalRebuildRequired = errors.New("rag: incremental index requires full re-embed")

	// ErrIncrementalStaleSource indicates the source changed in the store
	// between the incremental read/diff and the transactional replace.
	ErrIncrementalStaleSource = errors.New("rag: incremental source changed during indexing")

	// ErrEmbedderFailed indicates an embedding call failed during indexing.
	ErrEmbedderFailed = errors.New("rag: embedder failed")

	// ErrEmbeddingCountMismatch indicates an embedder returned the wrong number
	// of vectors for the requested input batch.
	ErrEmbeddingCountMismatch = errors.New("rag: embedding count mismatch")

	// ErrStoreOperation indicates the vector store failed an operation needed
	// by indexing or retrieval.
	ErrStoreOperation = errors.New("rag: vector store operation failed")
)
