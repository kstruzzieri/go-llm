package rag

import (
	"context"
	"fmt"
	"testing"
)

// storeChunksRaw inserts chunk rows directly so tests control every column.
func storeChunksRaw(t *testing.T, store *SQLiteStore, rows [][]any) {
	t.Helper()
	for _, r := range rows {
		_, err := store.db.Exec(`
			INSERT INTO chunks (id, content, source, start_line, end_line, language,
			                    metadata, embedding, indexed_at, stable_key,
			                    source_content_hash, vector_space_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r...)
		if err != nil {
			t.Fatalf("insert chunk: %v", err)
		}
	}
}

func sigJSON(t *testing.T, contentHash string) string {
	t.Helper()
	return fmt.Sprintf(
		`{"version":2,"content_hash":%q,"embedding_model":"m","chunker":"c","stable_key_version":"v1"}`,
		contentHash)
}

func TestSourceProvenanceBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	storeChunksRaw(t, store, [][]any{
		// Uniform source: one hash, one vector space.
		{"id1", "func A() {}", "pkg/a.go", 1, 1, "go", `{}`, emb, int64(100), "k1", sigJSON(t, "hashA"), "vs1"},
		{"id2", "func B() {}", "pkg/a.go", 3, 3, "go", `{}`, emb, int64(200), "k2", sigJSON(t, "hashA"), "vs1"},
		// Mixed content hash within one source.
		{"id3", "x", "pkg/mixed.go", 1, 1, "go", `{}`, emb, int64(300), "k3", sigJSON(t, "h1"), "vs1"},
		{"id4", "y", "pkg/mixed.go", 2, 2, "go", `{}`, emb, int64(400), "k4", sigJSON(t, "h2"), "vs1"},
		// Blank vector space and unparseable signature: plain Store() write,
		// not only legacy — Store() (rag/sqlite_store.go:403) calls
		// insertChunksTx with literal empty strings for both.
		{"id5", "z", "pkg/legacy.go", 1, 1, "go", `{}`, emb, int64(500), "", "", ""},
		// Inverse mixing: uniform hash, mixed vector space.
		{"id6", "p", "pkg/vsmix.go", 1, 1, "go", `{}`, emb, int64(600), "k6", sigJSON(t, "hv"), "vs1"},
		{"id7", "q", "pkg/vsmix.go", 2, 2, "go", `{}`, emb, int64(650), "k7", sigJSON(t, "hv"), "vs2"},
	})

	got, err := store.SourceProvenanceBatch(ctx, []string{"pkg/a.go", "pkg/mixed.go", "pkg/legacy.go", "pkg/vsmix.go", "pkg/absent.go"})
	if err != nil {
		t.Fatalf("SourceProvenanceBatch: %v", err)
	}

	a := got["pkg/a.go"]
	if a.ContentHash != "hashA" || a.VectorSpaceID != "vs1" || a.Mixed || a.Managed {
		t.Fatalf("pkg/a.go provenance wrong: %+v", a)
	}
	if a.IndexedAt != 200 {
		t.Fatalf("IndexedAt should be MAX (200), got %d", a.IndexedAt)
	}

	m := got["pkg/mixed.go"]
	if !m.Mixed {
		t.Fatalf("pkg/mixed.go must be Mixed: %+v", m)
	}
	if m.ContentHash != "" {
		t.Fatalf("mixed content hash must be blanked, got %q", m.ContentHash)
	}
	if m.VectorSpaceID != "vs1" {
		t.Fatalf("unmixed vector space must survive, got %q", m.VectorSpaceID)
	}
	if m.IndexedAt != 400 {
		t.Fatalf("mixed source IndexedAt must still be MAX (400), got %d", m.IndexedAt)
	}

	l := got["pkg/legacy.go"]
	if l.ContentHash != "" || l.VectorSpaceID != "" {
		t.Fatalf("legacy blanks must stay blank (never guessed): %+v", l)
	}

	v := got["pkg/vsmix.go"]
	if !v.Mixed || v.VectorSpaceID != "" {
		t.Fatalf("mixed vector space must blank only that field: %+v", v)
	}
	if v.ContentHash != "hv" {
		t.Fatalf("uniform content hash must survive vector-space mixing, got %q", v.ContentHash)
	}

	if _, ok := got["pkg/absent.go"]; ok {
		t.Fatal("absent source must not appear in the map")
	}
}

func TestSourceProvenanceBatchManaged(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	source := "managed:0123456789abcdef0123456789abcdef.md"
	storeChunksRaw(t, store, [][]any{
		{"mid1", "# Doc", source, 1, 1, "markdown", `{}`, emb, int64(700), "mk1", sigJSON(t, "mh"), "vs1"},
	})
	_, err := store.db.Exec(`
		INSERT INTO managed_documents
		  (id, source, title, kind, mime_type, content_hash, source_signature,
		   collection, tags, state, freshness, created_at, updated_at)
		VALUES ('0123456789abcdef0123456789abcdef', ?, 'My Doc', 'text', 'text/markdown',
		        'mh', 'sig', 'notes', '["x","y"]', 'indexed', 'fresh', 1, 1)`, source)
	if err != nil {
		t.Fatalf("insert managed document: %v", err)
	}

	got, err := store.SourceProvenanceBatch(ctx, []string{source})
	if err != nil {
		t.Fatalf("SourceProvenanceBatch: %v", err)
	}
	p := got[source]
	if !p.Managed || p.Title != "My Doc" || p.Collection != "notes" ||
		len(p.Tags) != 2 || p.Freshness != DocumentFreshnessFresh {
		t.Fatalf("managed provenance wrong: %+v", p)
	}
	// Invariant: the map key is always the entry's own Source. Task 11 and
	// Task 13's attribution rest on the map key being the real source.
	for key, entry := range got {
		if key != entry.Source {
			t.Fatalf("map key %q does not match entry.Source %q", key, entry.Source)
		}
	}
}

func TestSourceProvenanceBatchUnregisteredManagedPrefix(t *testing.T) {
	// The managed: prefix without a registry row is NOT ownership
	// (rag/indexer.go:259, rag/retriever.go:666).
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	source := "managed:ffffffffffffffffffffffffffffffff.txt"
	storeChunksRaw(t, store, [][]any{
		{"uid1", "legacy", source, 1, 1, "", `{}`, emb, int64(900), "", sigJSON(t, "lh"), "vs1"},
	})
	got, err := store.SourceProvenanceBatch(ctx, []string{source})
	if err != nil {
		t.Fatalf("SourceProvenanceBatch: %v", err)
	}
	if got[source].Managed {
		t.Fatal("prefix alone must not mark a source managed")
	}
}

// TestSourceProvenanceBatchRegistryRowWithoutChunks pins the out[source]
// guard in attachManagedProvenance: a registry row without matching chunks
// yields nothing to render, never a phantom zero-value entry.
//
// This is not a corruption-only edge case — it is a normal, reachable state
// during every ingest. ingestLocked commits the managed_documents row with
// state='indexing' in its own statement (rag/managed.go:268-278) and only
// THEN calls indexDocumentLocked to write chunks (rag/managed.go:280), so a
// progressive render racing an in-flight ingest — or reading a document
// stuck in 'indexing' or 'failed' — will see exactly this: a committed
// registry row with zero chunks.
func TestSourceProvenanceBatchRegistryRowWithoutChunks(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	source := "managed:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.md"
	_, err := store.db.Exec(`
		INSERT INTO managed_documents
		  (id, source, title, kind, mime_type, content_hash, source_signature,
		   collection, tags, state, freshness, created_at, updated_at)
		VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', ?, 'Pending Doc', 'text', 'text/markdown',
		        'h', 'sig', '', '["x"]', 'indexing', 'unknown', 1, 1)`, source)
	if err != nil {
		t.Fatalf("insert managed document: %v", err)
	}

	got, err := store.SourceProvenanceBatch(ctx, []string{source})
	if err != nil {
		t.Fatalf("SourceProvenanceBatch: %v", err)
	}
	if _, ok := got[source]; ok {
		t.Fatalf("registry row without chunks must yield nothing to render, got %+v", got[source])
	}
	// Invariant (vacuous here since the map is empty, but checked
	// unconditionally so it holds on every path, not just the populated one
	// in TestSourceProvenanceBatchManaged): the map key must equal the
	// entry's own Source field.
	for key, entry := range got {
		if key != entry.Source {
			t.Fatalf("map key %q does not match entry.Source %q", key, entry.Source)
		}
	}
}

// TestSourceProvenanceBatchMalformedTagsDegrades pins the degrade-not-propagate
// behavior for a malformed tags column: by the time tags fail to decode,
// Managed/Title/Collection/Freshness have already scanned successfully, so
// the source genuinely IS managed and the batch must not fail for every
// OTHER source in it over one corrupt decorative field. The closer precedent
// is the retrieval-path validity predicate at rag/retriever.go:702, which
// treats malformed tags as "not valid" and moves on — not the authoritative
// document read at rag/managed.go:733-734, which propagates on a different
// (single-document read/write) path.
func TestSourceProvenanceBatchMalformedTagsDegrades(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}

	source := "managed:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.md"
	storeChunksRaw(t, store, [][]any{
		{"bid1", "# Doc", source, 1, 1, "markdown", `{}`, emb, int64(800), "bk1", sigJSON(t, "bh"), "vs1"},
	})
	_, err := store.db.Exec(`
		INSERT INTO managed_documents
		  (id, source, title, kind, mime_type, content_hash, source_signature,
		   collection, tags, state, freshness, created_at, updated_at)
		VALUES ('bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', ?, 'Corrupt Tags Doc', 'text', 'text/markdown',
		        'bh', 'sig', 'notes', 'not-json', 'indexed', 'fresh', 1, 1)`, source)
	if err != nil {
		t.Fatalf("insert managed document: %v", err)
	}

	got, err := store.SourceProvenanceBatch(ctx, []string{source})
	if err != nil {
		t.Fatalf("SourceProvenanceBatch must degrade, not propagate, on malformed tags: %v", err)
	}
	p := got[source]
	if !p.Managed || p.Title != "Corrupt Tags Doc" || p.Collection != "notes" || p.Freshness != DocumentFreshnessFresh {
		t.Fatalf("malformed tags must not blank the other managed fields: %+v", p)
	}
	if p.Tags == nil || len(p.Tags) != 0 {
		t.Fatalf("malformed tags must degrade to an empty (non-nil) slice, got %#v", p.Tags)
	}
}
