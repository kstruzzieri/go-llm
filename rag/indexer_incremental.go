package rag

import (
	"context"
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
	if checker, ok := idx.store.(sourceHashChecker); ok {
		storedHash, hashErr := checker.GetSourceHash(ctx, path)
		if hashErr == nil && storedHash != "" {
			if storedHash == sourceHash {
				return nil
			}
			forceFullReembed = idx.requiresFullReembed(storedHash, currentSig)
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
		err := idx.indexIncremental(ctx, path, chunks, sourceHash)
		if err == nil {
			return nil
		}
		// Incremental failed -- fall through to full path.
		// Context cancellation should be propagated immediately.
		if ctx.Err() != nil {
			return fmt.Errorf("rag: incremental index %q: %w", path, err)
		}
	}

	// Full re-index path (same as IndexFile).
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	embeddings, err := idx.client.EmbedBatch(ctx, idx.model, texts)
	if err != nil {
		// Embedding failed -- preserve existing indexed data for this file.
		return fmt.Errorf("rag: embed chunks for %q: %w", path, err)
	}

	if err := idx.replaceSourceWithHash(ctx, path, chunks, embeddings, sourceHash); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}
	return nil
}

// indexIncremental is the internal incremental indexing path.
// It assumes preconditions have been checked (store supports GetBySource,
// workspace root is set, StableKeys are computed). The sourceHash is the
// pre-computed source signature that will be stored alongside the new chunks.
//
// Returns a non-nil error on any failure. The caller falls back to full
// re-indexing when this returns an error.
func (idx *Indexer) indexIncremental(ctx context.Context, path string, newChunks []Chunk, sourceHash string) error {
	loader, ok := idx.store.(sourceChunkLoader)
	if !ok {
		return fmt.Errorf("rag: store does not support GetBySource")
	}

	oldChunks, err := loader.GetBySource(ctx, path)
	if err != nil {
		return fmt.Errorf("rag: load existing chunks for %q: %w", path, err)
	}
	if len(oldChunks) == 0 {
		// First time indexing this file -- fall back to full.
		return fmt.Errorf("rag: no existing chunks for %q", path)
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
		return idx.replaceSourceWithHash(ctx, path, newChunks, finalEmbeddings, sourceHash)
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

	// Embed only changed chunks.
	var freshEmbeddings [][]float64
	if len(textsToEmbed) > 0 {
		freshEmbeddings, err = idx.client.EmbedBatch(ctx, idx.model, textsToEmbed)
		if err != nil {
			return fmt.Errorf("rag: embed changed chunks for %q: %w", path, err)
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
				return fmt.Errorf("rag: incremental index %q: embedding count mismatch (expected %d, got %d)", path, len(textsToEmbed), len(freshEmbeddings))
			}
			finalEmbeddings[i] = freshEmbeddings[freshIdx]
			freshIdx++
		}
	}

	// Atomically replace source with the full new chunk set.
	if err := idx.replaceSourceWithHash(ctx, path, newChunks, finalEmbeddings, sourceHash); err != nil {
		return fmt.Errorf("rag: replace chunks for %q: %w", path, err)
	}
	return nil
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
