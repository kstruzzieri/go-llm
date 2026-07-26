package rag

import (
	"context"
	"testing"
)

func TestChunkContentDigestBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	storeChunksRaw(t, store, [][]any{
		{"d1", "alpha", "pkg/a.go", 1, 1, "go", `{}`, emb, int64(1), "", "", ""},
		{"d2", "beta", "pkg/a.go", 2, 2, "go", `{}`, emb, int64(1), "", "", ""},
	})

	got, err := store.ChunkContentDigestBatch(ctx, []string{"d1", "d2", "gone"})
	if err != nil {
		t.Fatalf("ChunkContentDigestBatch: %v", err)
	}
	if got["d1"] != sha256Hex("alpha") || got["d2"] != sha256Hex("beta") {
		t.Fatalf("digest mismatch: %+v", got)
	}
	if _, ok := got["gone"]; ok {
		t.Fatal("absent chunk id must not appear")
	}
	if len(got) != 2 {
		t.Fatalf("want 2 digests, got %d", len(got))
	}
}
