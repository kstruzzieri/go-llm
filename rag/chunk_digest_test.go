package rag

import (
	"context"
	"testing"
)

// TestSHA256Hex_Deterministic pins sha256Hex against a NIST test vector,
// mirroring transcript/canonical_test.go's identically-named test for the
// sibling copy of this function (DEV-6): every digest assertion in
// TestChunkContentDigestBatch computes sha256Hex on both sides of the
// comparison, so it cannot detect an internally-consistent change to
// sha256Hex itself (uppercasing, truncating, or salting the hash). This test
// is what catches those.
func TestSHA256Hex_Deterministic(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // SHA-256("abc"), NIST vector
	got := sha256Hex("abc")
	if got != want {
		t.Errorf("sha256Hex(%q) = %q, want %q", "abc", got, want)
	}
	if len(got) != 64 {
		t.Errorf("sha256Hex length = %d, want 64", len(got))
	}
}

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
