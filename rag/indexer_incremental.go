package rag

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// canDoIncremental checks whether the preconditions for incremental indexing
// are met: the store supports GetBySource, a workspace root is set, and at
// least half the chunks have non-empty StableKeys.
func (idx *Indexer) canDoIncremental(chunks []Chunk) bool {
	// Store must support GetBySource.
	if _, ok := idx.store.(sourceChunkLoader); !ok {
		return false
	}
	// Workspace root must be set for StableKey computation.
	if idx.workspaceRoot == "" {
		return false
	}
	// Need at least one chunk to evaluate.
	if len(chunks) == 0 {
		return false
	}
	// At least half the chunks should have StableKeys for meaningful matching.
	keyed := 0
	for _, c := range chunks {
		if c.StableKey != "" {
			keyed++
		}
	}
	return keyed > len(chunks)/2
}

// IndexFileIncremental indexes a file using incremental diff-aware logic.
// It re-chunks the entire file, compares against stored chunks via StableKey,
// and only embeds chunks whose content actually changed.
//
// Falls back to full IndexFile behavior when:
//   - The store does not implement sourceChunkLoader
//   - No existing chunks are stored for this source
//   - The workspace root is not set (StableKey computation impossible)
//   - More than 50% of new chunks have empty StableKeys
//   - Any error occurs during the incremental path
//
// On success, the store contains exactly the same chunks that a full
// IndexFile would produce. The only difference is fewer embedding API calls.
func (idx *Indexer) IndexFileIncremental(ctx context.Context, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("rag: read file %q: %w", path, err)
	}

	content := string(data)
	if content == "" {
		if err := idx.replaceSource(ctx, path, nil, nil); err != nil {
			return fmt.Errorf("rag: clear chunks for empty file %q: %w", path, err)
		}
		return nil
	}

	// Fast path: if the full source signature matches the stored signature, skip
	// entirely. This avoids chunking, GetBySource, and all comparison work.
	currentSig := idx.currentSourceSignature(content)
	sourceHash := currentSig.String()
	forceFullReembed := false
	var expectedSourceHash string
	hasExpectedSourceHash := false
	if checker, ok := idx.store.(sourceHashChecker); ok {
		storedHash, hashErr := checker.GetSourceHash(ctx, path)
		if hashErr == nil {
			if storedHash == "" {
				// Rows migrated from earlier schema versions have no persisted
				// provenance, so unchanged chunks cannot be safely reused.
				forceFullReembed = true
			} else {
				if storedHash == sourceHash {
					return nil
				}
				forceFullReembed = idx.requiresFullReembed(storedHash, currentSig)
				if !forceFullReembed {
					expectedSourceHash = storedHash
					hasExpectedSourceHash = true
				}
			}
		}
	}

	// Step 1: Chunk the file (always needed -- chunking is cheap).
	chunks, err := idx.chunker.Chunk(path, content)
	if err != nil {
		return fmt.Errorf("rag: chunk %q: %w", path, err)
	}
	if len(chunks) == 0 {
		if err := idx.replaceSource(ctx, path, nil, nil); err != nil {
			return fmt.Errorf("rag: clear chunks for %q with no chunk output: %w", path, err)
		}
		return nil
	}
	if hasExpectedSourceHash {
		if _, ok := idx.store.(atomicSourceReplacerWithExpectedHashAndVectorSpaceID); !ok {
			forceFullReembed = true
		}
	}

	// Step 1.5: Compute stable keys if workspace root is set.
	if idx.workspaceRoot != "" {
		for i := range chunks {
			key, keyErr := ComputeStableKey(chunks[i], idx.workspaceRoot)
			if keyErr != nil {
				// Non-fatal: leave StableKey empty rather than failing the entire index.
				continue
			}
			chunks[i].StableKey = key
		}
	}

	// Try incremental path.
	if !forceFullReembed && idx.canDoIncremental(chunks) {
		err := idx.indexIncremental(ctx, path, chunks, sourceHash, expectedSourceHash, hasExpectedSourceHash)
		if err == nil {
			return nil
		}
		// Context cancellation should be propagated immediately.
		if ctx.Err() != nil {
			return fmt.Errorf("rag: incremental index %q: %w", path, err)
		}
		if !incrementalShouldFallback(err) {
			return fmt.Errorf("rag: incremental index %q: %w", path, err)
		}
	}

	// Full re-index path (same as IndexFile).
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	res, err := idx.embedder.Embed(ctx, idx.model, texts)
	if err != nil {
		// Embedding failed -- preserve existing indexed data for this file.
		return fmt.Errorf("%w: embed chunks for %q: %w", ErrEmbedderFailed, path, err)
	}
	if len(res.Embeddings) != len(texts) {
		return fmt.Errorf("%w: embed chunks for %q: got %d for %d chunks", ErrEmbeddingCountMismatch, path, len(res.Embeddings), len(texts))
	}
	embeddings := res.Embeddings

	// Mirror IndexFile's vsid resolution so the store can enforce the
	// vsid-aware write contract.
	vsid := resolveVectorSpaceID(res)
	if err := idx.replaceSourceWithProvenance(ctx, path, chunks, embeddings, sourceHash, vsid); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}
	return nil
}

func incrementalShouldFallback(err error) bool {
	return errors.Is(err, ErrIncrementalRebuildRequired) ||
		errors.Is(err, ErrIncrementalStaleSource)
}

// indexIncremental is the internal incremental indexing path.
// It assumes preconditions have been checked (store supports GetBySource,
// workspace root is set, StableKeys are computed). The sourceHash is the
// pre-computed source signature that will be stored alongside the new chunks.
//
// Returns a non-nil error on any failure. The caller only falls back to full
// re-indexing for typed repairable errors.
func (idx *Indexer) indexIncremental(ctx context.Context, path string, newChunks []Chunk, sourceHash, expectedSourceHash string, hasExpectedSourceHash bool) error {
	loader, ok := idx.store.(sourceChunkLoader)
	if !ok {
		return fmt.Errorf("%w: store does not support GetBySource", ErrIncrementalRebuildRequired)
	}

	oldChunks, err := loader.GetBySource(ctx, path)
	if err != nil {
		return fmt.Errorf("%w: load existing chunks for %q: %w", ErrStoreOperation, path, err)
	}
	if len(oldChunks) == 0 {
		// First time indexing this file -- fall back to full.
		return fmt.Errorf("%w: no existing chunks for %q", ErrIncrementalRebuildRequired, path)
	}

	// Diff old vs. new.
	diff := diffChunks(oldChunks, newChunks)

	// Short circuit: nothing changed content-wise.
	// Even though chunk content is unchanged, metadata (StartLine/EndLine) may
	// have shifted if lines were added/removed between functions. We still need
	// to write the updated chunks to keep line references accurate.
	if len(diff.modified) == 0 && len(diff.added) == 0 && len(diff.deletedIDs) == 0 {
		// Reuse all existing embeddings — no embed calls needed.
		finalEmbeddings := make([][]float64, len(newChunks))
		for i, nc := range newChunks {
			for _, uc := range diff.unchanged {
				if uc.chunk.StableKey == nc.StableKey {
					finalEmbeddings[i] = uc.embedding
					break
				}
			}
		}
		finalVSID := ""
		if _, capable := idx.store.(atomicSourceReplacerWithVectorSpaceID); capable {
			reusedVSID, status := reusedCachedVSID(diff.unchanged)
			if status != cachedVSIDUniform {
				return reusedCachedVSIDError(path, status)
			}
			finalVSID = reusedVSID
		}
		return idx.replaceIncrementalSource(ctx, path, newChunks, finalEmbeddings, sourceHash, finalVSID, expectedSourceHash, hasExpectedSourceHash)
	}

	// Build a map from StableKey -> cached embedding for unchanged chunks.
	cachedEmbeddings := make(map[string][]float64, len(diff.unchanged))
	for _, uc := range diff.unchanged {
		cachedEmbeddings[uc.chunk.StableKey] = uc.embedding
	}

	// Identify which newChunks need fresh embeddings (in order).
	var textsToEmbed []string
	for _, nc := range newChunks {
		if _, cached := cachedEmbeddings[nc.StableKey]; cached {
			continue // unchanged, will use cached embedding
		}
		textsToEmbed = append(textsToEmbed, nc.Content)
	}

	reusedVSID := ""
	reusedStatus := cachedVSIDNone
	if _, capable := idx.store.(atomicSourceReplacerWithVectorSpaceID); capable && len(diff.unchanged) > 0 {
		reusedVSID, reusedStatus = reusedCachedVSID(diff.unchanged)
		if reusedStatus != cachedVSIDUniform {
			return reusedCachedVSIDError(path, reusedStatus)
		}
	}

	// Embed only changed chunks.
	var freshEmbeddings [][]float64
	freshVSID := ""
	if len(textsToEmbed) > 0 {
		res, embedErr := idx.embedder.Embed(ctx, idx.model, textsToEmbed)
		if embedErr != nil {
			return fmt.Errorf("%w: embed changed chunks for %q: %w", ErrEmbedderFailed, path, embedErr)
		}
		if len(res.Embeddings) != len(textsToEmbed) {
			return fmt.Errorf("%w: embed changed chunks for %q: got %d for %d chunks", ErrEmbeddingCountMismatch, path, len(res.Embeddings), len(textsToEmbed))
		}
		freshEmbeddings = res.Embeddings
		freshVSID = resolveVectorSpaceID(res)
	}

	finalVSID := freshVSID
	if _, capable := idx.store.(atomicSourceReplacerWithVectorSpaceID); capable {
		if len(textsToEmbed) > 0 {
			if len(diff.unchanged) > 0 {
				if freshVSID != "" && reusedVSID != freshVSID {
					return fmt.Errorf("%w: incremental index %q: reused cached VectorSpaceID %q differs from fresh VectorSpaceID %q", ErrVectorSpaceDrift, path, reusedVSID, freshVSID)
				}
			}
			finalVSID = freshVSID
		} else {
			if reusedStatus != cachedVSIDUniform {
				return reusedCachedVSIDError(path, reusedStatus)
			}
			finalVSID = reusedVSID
		}
	}

	// Assemble final embeddings aligned 1:1 with newChunks.
	finalEmbeddings := make([][]float64, len(newChunks))
	freshIdx := 0
	for i, nc := range newChunks {
		if emb, cached := cachedEmbeddings[nc.StableKey]; cached {
			finalEmbeddings[i] = emb
		} else {
			if freshIdx >= len(freshEmbeddings) {
				return fmt.Errorf("%w: incremental index %q: expected %d fresh embeddings, got %d", ErrEmbeddingCountMismatch, path, len(textsToEmbed), len(freshEmbeddings))
			}
			finalEmbeddings[i] = freshEmbeddings[freshIdx]
			freshIdx++
		}
	}

	// Atomically replace source with the full new chunk set.
	if err := idx.replaceIncrementalSource(ctx, path, newChunks, finalEmbeddings, sourceHash, finalVSID, expectedSourceHash, hasExpectedSourceHash); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}
	return nil
}

type cachedVSIDStatus int

const (
	cachedVSIDNone cachedVSIDStatus = iota
	cachedVSIDUniform
	cachedVSIDLegacyUnknown
	cachedVSIDMixed
)

// reusedCachedVSID returns the unique non-empty VectorSpaceID for reused
// cached embeddings plus a status that distinguishes legacy empty-vsid rows
// from genuinely mixed vector spaces.
func reusedCachedVSID(unchanged []unchangedChunk) (id string, status cachedVSIDStatus) {
	if len(unchanged) == 0 {
		return "", cachedVSIDNone
	}
	for _, uc := range unchanged {
		if uc.vectorSpaceID == "" {
			return "", cachedVSIDLegacyUnknown
		}
		if id == "" {
			id = uc.vectorSpaceID
			continue
		}
		if id != uc.vectorSpaceID {
			return "", cachedVSIDMixed
		}
	}
	return id, cachedVSIDUniform
}

func reusedCachedVSIDError(path string, status cachedVSIDStatus) error {
	switch status {
	case cachedVSIDLegacyUnknown:
		// Legacy unknown rows are repairable by a full re-embed, so expose both
		// the fallback sentinel and the missing-vsid domain reason.
		return fmt.Errorf("%w: %w: incremental index %q: reused cached embeddings have legacy empty VectorSpaceID", ErrIncrementalRebuildRequired, ErrMissingVectorSpaceID, path)
	case cachedVSIDMixed:
		// Mixed known vector spaces are not repairable by silent fallback; they
		// must fail closed so operators can choose an explicit rebuild.
		return fmt.Errorf("%w: incremental index %q: reused cached embeddings have mixed VectorSpaceID values", ErrCorpusMixedVectorSpaces, path)
	default:
		return fmt.Errorf("%w: incremental index %q: no cached VectorSpaceID available", ErrIncrementalRebuildRequired, path)
	}
}

func (idx *Indexer) replaceIncrementalSource(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64, sourceHash, vectorSpaceID, expectedSourceHash string, hasExpectedSourceHash bool) error {
	if hasExpectedSourceHash {
		return idx.replaceSourceWithProvenanceIfSourceHash(ctx, path, chunks, embeddings, sourceHash, vectorSpaceID, expectedSourceHash)
	}
	return idx.replaceSourceWithProvenance(ctx, path, chunks, embeddings, sourceHash, vectorSpaceID)
}

// updateSourceHash updates the source_content_hash for all chunks of a source
// without re-inserting them. Used when chunks are unchanged but the signature
// was not previously stored (e.g. databases migrated from V3).
func (idx *Indexer) updateSourceHash(ctx context.Context, source, hash string) {
	type hashUpdater interface {
		UpdateSourceHash(ctx context.Context, source, hash string) error
	}
	if u, ok := idx.store.(hashUpdater); ok {
		_ = u.UpdateSourceHash(ctx, source, hash)
	}
}
