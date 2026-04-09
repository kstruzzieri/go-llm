package rag

// chunkDiff holds the result of comparing old stored chunks against newly
// chunked content for a single source file. Each chunk is classified as
// unchanged, modified, added, or deleted based on StableKey matching and
// content hash comparison.
type chunkDiff struct {
	// unchanged chunks: StableKey matches and content hash is identical.
	// The stored embedding can be reused without re-embedding.
	unchanged []unchangedChunk
	// modified chunks: StableKey matches but content hash differs.
	// These need re-embedding.
	modified []modifiedChunk
	// added chunks: no StableKey match in old set, need embedding.
	added []Chunk
	// deletedIDs: old chunk IDs whose StableKey was not matched by any
	// new chunk. These should be removed from the store.
	deletedIDs []string
}

// unchangedChunk pairs a new chunk (with current position metadata) with
// the stored embedding that can be reused. The chunk field carries the new
// chunk's metadata (StartLine, EndLine may have shifted) while embedding
// is carried over from the old stored chunk.
type unchangedChunk struct {
	chunk     Chunk     // new chunk (may have different StartLine/EndLine)
	embedding []float64 // embedding from store (content unchanged)
}

// modifiedChunk pairs a new chunk with the old chunk ID it replaces.
// The content has changed so the embedding must be recomputed, but the
// StableKey match tells us this is an update rather than a delete+add.
type modifiedChunk struct {
	chunk Chunk  // new chunk content + StableKey
	oldID string // old chunk ID (for tracking only; ReplaceSource handles cleanup)
}

// diffChunks compares old stored chunks against newly produced chunks,
// matching on StableKey and comparing content hashes to classify each
// chunk as unchanged, modified, added, or deleted.
//
// Algorithm:
//  1. Build a map of old chunks keyed by StableKey (first occurrence wins
//     if duplicates exist).
//  2. For each new chunk, look up its StableKey in the old map:
//     - Empty StableKey -> Added (cannot match).
//     - StableKey found and contentHash matches -> Unchanged (reuse embedding).
//     - StableKey found but contentHash differs -> Modified (re-embed).
//     - StableKey not found -> Added.
//  3. Old chunks whose StableKey was never matched -> Deleted.
//
// This is a pure function with no side effects or I/O.
//
// Precondition: all chunks in newChunks should have StableKeys computed
// (though some may be empty, which means they cannot match).
func diffChunks(oldChunks []ChunkWithEmbedding, newChunks []Chunk) chunkDiff {
	var result chunkDiff

	// Build map: StableKey -> ChunkWithEmbedding (first occurrence wins).
	// Also track which old chunk IDs made it into the map so we can
	// correctly identify duplicates as deleted.
	oldByKey := make(map[string]ChunkWithEmbedding, len(oldChunks))
	for _, old := range oldChunks {
		if old.Chunk.StableKey == "" {
			continue
		}
		if _, exists := oldByKey[old.Chunk.StableKey]; !exists {
			oldByKey[old.Chunk.StableKey] = old
		}
	}

	// Track which old chunk IDs were matched by a new chunk.
	matchedOldIDs := make(map[string]bool, len(oldByKey))

	for _, nc := range newChunks {
		if nc.StableKey == "" {
			result.added = append(result.added, nc)
			continue
		}

		old, found := oldByKey[nc.StableKey]
		if !found {
			result.added = append(result.added, nc)
			continue
		}

		matchedOldIDs[old.Chunk.ID] = true

		if contentHash(old.Chunk.Content) == contentHash(nc.Content) {
			result.unchanged = append(result.unchanged, unchangedChunk{
				chunk:     nc,
				embedding: old.Embedding,
			})
		} else {
			result.modified = append(result.modified, modifiedChunk{
				chunk: nc,
				oldID: old.Chunk.ID,
			})
		}
	}

	// Any old chunk that was not matched is deleted. This includes:
	// - Old chunks with empty StableKeys (never entered the map).
	// - Old chunks whose StableKey was a duplicate (not the first occurrence).
	// - Old chunks whose StableKey had no corresponding new chunk.
	for _, old := range oldChunks {
		if !matchedOldIDs[old.Chunk.ID] {
			result.deletedIDs = append(result.deletedIDs, old.Chunk.ID)
		}
	}

	return result
}
