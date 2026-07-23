package rag

import (
	"testing"
)

// TestDiffChunksAllUnchanged verifies that when all new chunks have matching
// StableKeys and identical content, everything is classified as unchanged
// with embeddings carried through.
func TestDiffChunksAllUnchanged(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
			Embedding: []float64{1.0, 0.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func World() {}", StableKey: "main.go::World#0"},
			Embedding: []float64{0.0, 1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Hello() {}", StableKey: "main.go::Hello#0", StartLine: 1},
		{ID: "n2", Content: "func World() {}", StableKey: "main.go::World#0", StartLine: 5},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 2 {
		t.Errorf("unchanged = %d, want 2", len(diff.unchanged))
	}
	if len(diff.modified) != 0 {
		t.Errorf("modified = %d, want 0", len(diff.modified))
	}
	if len(diff.added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.added))
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}

	// Verify cached embeddings are carried through.
	if len(diff.unchanged[0].embedding) != 2 {
		t.Error("unchanged[0] embedding not carried through")
	}
}

// TestDiffChunksSingleModified verifies that a chunk with the same StableKey
// but different content is classified as modified with the old ID recorded.
func TestDiffChunksSingleModified(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Hello() { return 1 }", StableKey: "main.go::Hello#0"},
			Embedding: []float64{1.0, 0.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func World() {}", StableKey: "main.go::World#0"},
			Embedding: []float64{0.0, 1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Hello() { return 42 }", StableKey: "main.go::Hello#0"},
		{ID: "n2", Content: "func World() {}", StableKey: "main.go::World#0"},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 1 {
		t.Errorf("unchanged = %d, want 1", len(diff.unchanged))
	}
	if len(diff.modified) != 1 {
		t.Errorf("modified = %d, want 1", len(diff.modified))
	}
	if diff.modified[0].chunk.Content != "func Hello() { return 42 }" {
		t.Errorf("modified content = %q, unexpected", diff.modified[0].chunk.Content)
	}
	if diff.modified[0].oldID != "c1" {
		t.Errorf("modified oldID = %q, want %q", diff.modified[0].oldID, "c1")
	}
	if len(diff.added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.added))
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}
}

// TestDiffChunksNewChunkAdded verifies that a new chunk with a StableKey
// not present in the old set is classified as added.
func TestDiffChunksNewChunkAdded(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
			Embedding: []float64{1.0, 0.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
		{ID: "n2", Content: "func NewFunc() {}", StableKey: "main.go::NewFunc#0"},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 1 {
		t.Errorf("unchanged = %d, want 1", len(diff.unchanged))
	}
	if len(diff.added) != 1 {
		t.Errorf("added = %d, want 1", len(diff.added))
	}
	if diff.added[0].StableKey != "main.go::NewFunc#0" {
		t.Errorf("added StableKey = %q, want %q", diff.added[0].StableKey, "main.go::NewFunc#0")
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}
}

// TestDiffChunksChunkDeleted verifies that an old chunk with a StableKey
// not present in the new set is classified as deleted by ID.
func TestDiffChunksChunkDeleted(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
			Embedding: []float64{1.0, 0.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func Removed() {}", StableKey: "main.go::Removed#0"},
			Embedding: []float64{0.0, 1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 1 {
		t.Errorf("unchanged = %d, want 1", len(diff.unchanged))
	}
	if len(diff.deletedIDs) != 1 {
		t.Errorf("deletedIDs = %d, want 1", len(diff.deletedIDs))
	}
	if diff.deletedIDs[0] != "c2" {
		t.Errorf("deletedIDs[0] = %q, want %q", diff.deletedIDs[0], "c2")
	}
}

// TestDiffChunksFullRewrite verifies that when all StableKeys change,
// everything old is deleted and everything new is added.
func TestDiffChunksFullRewrite(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Old1() {}", StableKey: "main.go::Old1#0"},
			Embedding: []float64{1.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func Old2() {}", StableKey: "main.go::Old2#0"},
			Embedding: []float64{0.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func New1() {}", StableKey: "main.go::New1#0"},
		{ID: "n2", Content: "func New2() {}", StableKey: "main.go::New2#0"},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 0 {
		t.Errorf("unchanged = %d, want 0", len(diff.unchanged))
	}
	if len(diff.modified) != 0 {
		t.Errorf("modified = %d, want 0", len(diff.modified))
	}
	if len(diff.added) != 2 {
		t.Errorf("added = %d, want 2", len(diff.added))
	}
	if len(diff.deletedIDs) != 2 {
		t.Errorf("deletedIDs = %d, want 2", len(diff.deletedIDs))
	}
}

// TestDiffChunksEmptyStableKey verifies that new chunks with empty StableKeys
// are always classified as added (they can never match).
func TestDiffChunksEmptyStableKey(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "old content", StableKey: "main.go::Foo#0"},
			Embedding: []float64{1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "new content", StableKey: ""}, // no StableKey
	}

	diff := diffChunks(old, newChunks)

	// Empty StableKey chunks are always "added" -- they can never match.
	if len(diff.added) != 1 {
		t.Errorf("added = %d, want 1", len(diff.added))
	}
	// Old chunk with no match in new set is "deleted".
	if len(diff.deletedIDs) != 1 {
		t.Errorf("deletedIDs = %d, want 1", len(diff.deletedIDs))
	}
}

// TestDiffChunksNoOldChunks verifies that when there are no old chunks
// (first-time index), all new chunks are classified as added.
func TestDiffChunksNoOldChunks(t *testing.T) {
	newChunks := []Chunk{
		{ID: "n1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
		{ID: "n2", Content: "func World() {}", StableKey: "main.go::World#0"},
	}

	diff := diffChunks(nil, newChunks)

	if len(diff.added) != 2 {
		t.Errorf("added = %d, want 2", len(diff.added))
	}
	if len(diff.unchanged) != 0 {
		t.Errorf("unchanged = %d, want 0", len(diff.unchanged))
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}
}

// TestDiffChunksNoNewChunks verifies that when there are no new chunks,
// all old chunks are classified as deleted.
func TestDiffChunksNoNewChunks(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Hello() {}", StableKey: "main.go::Hello#0"},
			Embedding: []float64{1.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func World() {}", StableKey: "main.go::World#0"},
			Embedding: []float64{0.0},
		},
	}

	diff := diffChunks(old, nil)

	if len(diff.deletedIDs) != 2 {
		t.Errorf("deletedIDs = %d, want 2", len(diff.deletedIDs))
	}
	if len(diff.added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.added))
	}
	if len(diff.unchanged) != 0 {
		t.Errorf("unchanged = %d, want 0", len(diff.unchanged))
	}
	if len(diff.modified) != 0 {
		t.Errorf("modified = %d, want 0", len(diff.modified))
	}
}

// TestDiffChunksMixed verifies correct classification when there is a mix
// of unchanged, modified, added, and deleted chunks.
func TestDiffChunksMixed(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Unchanged() {}", StableKey: "main.go::Unchanged#0"},
			Embedding: []float64{1.0, 0.0, 0.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "func Modified() { v1 }", StableKey: "main.go::Modified#0"},
			Embedding: []float64{0.0, 1.0, 0.0},
		},
		{
			Chunk:     Chunk{ID: "c3", Content: "func Deleted() {}", StableKey: "main.go::Deleted#0"},
			Embedding: []float64{0.0, 0.0, 1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Unchanged() {}", StableKey: "main.go::Unchanged#0"},
		{ID: "n2", Content: "func Modified() { v2 }", StableKey: "main.go::Modified#0"},
		{ID: "n3", Content: "func Added() {}", StableKey: "main.go::Added#0"},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 1 {
		t.Errorf("unchanged = %d, want 1", len(diff.unchanged))
	}
	if diff.unchanged[0].chunk.StableKey != "main.go::Unchanged#0" {
		t.Errorf("unchanged StableKey = %q, want %q", diff.unchanged[0].chunk.StableKey, "main.go::Unchanged#0")
	}

	if len(diff.modified) != 1 {
		t.Errorf("modified = %d, want 1", len(diff.modified))
	}
	if diff.modified[0].chunk.StableKey != "main.go::Modified#0" {
		t.Errorf("modified StableKey = %q, want %q", diff.modified[0].chunk.StableKey, "main.go::Modified#0")
	}
	if diff.modified[0].oldID != "c2" {
		t.Errorf("modified oldID = %q, want %q", diff.modified[0].oldID, "c2")
	}

	if len(diff.added) != 1 {
		t.Errorf("added = %d, want 1", len(diff.added))
	}
	if diff.added[0].StableKey != "main.go::Added#0" {
		t.Errorf("added StableKey = %q, want %q", diff.added[0].StableKey, "main.go::Added#0")
	}

	if len(diff.deletedIDs) != 1 {
		t.Errorf("deletedIDs = %d, want 1", len(diff.deletedIDs))
	}
	if diff.deletedIDs[0] != "c3" {
		t.Errorf("deletedIDs[0] = %q, want %q", diff.deletedIDs[0], "c3")
	}
}

// TestDiffChunksDuplicateStableKeys verifies that when old chunks have
// duplicate StableKeys (shouldn't happen in practice), the first occurrence
// wins in the map and subsequent duplicates are treated as deleted.
func TestDiffChunksDuplicateStableKeys(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "first", StableKey: "main.go::Dup#0"},
			Embedding: []float64{1.0},
		},
		{
			Chunk:     Chunk{ID: "c2", Content: "second", StableKey: "main.go::Dup#0"},
			Embedding: []float64{0.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "first", StableKey: "main.go::Dup#0"},
	}

	diff := diffChunks(old, newChunks)

	// Should match the first occurrence in old (c1).
	if len(diff.unchanged) != 1 {
		t.Errorf("unchanged = %d, want 1", len(diff.unchanged))
	}
	// c2 is not matched (duplicate key, only first is used).
	if len(diff.deletedIDs) != 1 {
		t.Errorf("deletedIDs = %d, want 1", len(diff.deletedIDs))
	}
	if len(diff.deletedIDs) == 1 && diff.deletedIDs[0] != "c2" {
		t.Errorf("deletedIDs[0] = %q, want %q", diff.deletedIDs[0], "c2")
	}
}

// TestDiffChunksOrdinalShift verifies that when a function grows from
// 1 chunk to 2 chunks, the original ordinal is modified and the new
// ordinal is added.
func TestDiffChunksOrdinalShift(t *testing.T) {
	// Function grows from 1 chunk to 2 chunks.
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "func Foo() { small }", StableKey: "main.go::Foo#0"},
			Embedding: []float64{1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "func Foo() { much bigger now }", StableKey: "main.go::Foo#0"},
		{ID: "n2", Content: "// Foo continued", StableKey: "main.go::Foo#1"},
	}

	diff := diffChunks(old, newChunks)

	// Foo#0 content changed -> modified.
	if len(diff.modified) != 1 {
		t.Errorf("modified = %d, want 1", len(diff.modified))
	}
	// Foo#1 is new.
	if len(diff.added) != 1 {
		t.Errorf("added = %d, want 1", len(diff.added))
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}
}

// TestDiffChunksPreservesNewChunkMetadata verifies that modified chunks
// use the new chunk's metadata (StartLine, EndLine, etc.) not the old.
func TestDiffChunksPreservesNewChunkMetadata(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk: Chunk{
				ID: "c1", Content: "func Hello() { return 1 }",
				StableKey: "main.go::Hello#0", StartLine: 5, EndLine: 7,
			},
			Embedding: []float64{1.0},
		},
	}

	newChunks := []Chunk{
		{
			ID: "n1", Content: "func Hello() { return 42 }",
			StableKey: "main.go::Hello#0", StartLine: 10, EndLine: 12,
		},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.modified) != 1 {
		t.Fatalf("modified = %d, want 1", len(diff.modified))
	}
	// Modified chunk should use the NEW chunk's metadata (StartLine, etc.)
	if diff.modified[0].chunk.StartLine != 10 {
		t.Errorf("modified chunk StartLine = %d, want 10", diff.modified[0].chunk.StartLine)
	}
	if diff.modified[0].chunk.EndLine != 12 {
		t.Errorf("modified chunk EndLine = %d, want 12", diff.modified[0].chunk.EndLine)
	}
}

// TestDiffChunksUnchangedPreservesNewMetadata verifies that unchanged chunks
// carry the new chunk's position metadata even though content is the same.
// This is important when lines above a function are inserted, shifting its
// StartLine without changing its content.
func TestDiffChunksUnchangedPreservesNewMetadata(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk: Chunk{
				ID: "c1", Content: "func Hello() {}",
				StableKey: "main.go::Hello#0", StartLine: 5, EndLine: 7,
			},
			Embedding: []float64{1.0, 2.0},
		},
	}

	newChunks := []Chunk{
		{
			ID: "n1", Content: "func Hello() {}",
			StableKey: "main.go::Hello#0", StartLine: 15, EndLine: 17,
		},
	}

	diff := diffChunks(old, newChunks)

	if len(diff.unchanged) != 1 {
		t.Fatalf("unchanged = %d, want 1", len(diff.unchanged))
	}
	// Should use the new chunk's metadata (shifted StartLine).
	if diff.unchanged[0].chunk.StartLine != 15 {
		t.Errorf("unchanged chunk StartLine = %d, want 15", diff.unchanged[0].chunk.StartLine)
	}
	// Should carry through the old embedding.
	if len(diff.unchanged[0].embedding) != 2 {
		t.Errorf("unchanged embedding length = %d, want 2", len(diff.unchanged[0].embedding))
	}
	if diff.unchanged[0].embedding[0] != 1.0 || diff.unchanged[0].embedding[1] != 2.0 {
		t.Errorf("unchanged embedding = %v, want [1.0 2.0]", diff.unchanged[0].embedding)
	}
}

// TestDiffChunksBothEmpty verifies that diffing two empty sets produces
// an empty result with no panics.
func TestDiffChunksBothEmpty(t *testing.T) {
	diff := diffChunks(nil, nil)

	if len(diff.unchanged) != 0 {
		t.Errorf("unchanged = %d, want 0", len(diff.unchanged))
	}
	if len(diff.modified) != 0 {
		t.Errorf("modified = %d, want 0", len(diff.modified))
	}
	if len(diff.added) != 0 {
		t.Errorf("added = %d, want 0", len(diff.added))
	}
	if len(diff.deletedIDs) != 0 {
		t.Errorf("deletedIDs = %d, want 0", len(diff.deletedIDs))
	}
}

// TestDiffChunksOldEmptyStableKeysDeleted verifies that old chunks with
// empty StableKeys are classified as deleted (they can never be matched).
func TestDiffChunksOldEmptyStableKeysDeleted(t *testing.T) {
	old := []ChunkWithEmbedding{
		{
			Chunk:     Chunk{ID: "c1", Content: "legacy chunk", StableKey: ""},
			Embedding: []float64{1.0},
		},
	}

	newChunks := []Chunk{
		{ID: "n1", Content: "new chunk", StableKey: "main.go::Func#0"},
	}

	diff := diffChunks(old, newChunks)

	// Old chunk with empty StableKey can never match -> deleted.
	if len(diff.deletedIDs) != 1 {
		t.Errorf("deletedIDs = %d, want 1", len(diff.deletedIDs))
	}
	if len(diff.deletedIDs) == 1 && diff.deletedIDs[0] != "c1" {
		t.Errorf("deletedIDs[0] = %q, want %q", diff.deletedIDs[0], "c1")
	}
	// New chunk has no match -> added.
	if len(diff.added) != 1 {
		t.Errorf("added = %d, want 1", len(diff.added))
	}
}
