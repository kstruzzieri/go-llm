package rag

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func validSummary() SourceSummary {
	return SourceSummary{
		Source:        "pkg/a.go",
		ContentHash:   "hash1",
		VectorSpaceID: "vs1",
		Abstract:      "Handles A.",
		Overview:      "Defines A; returns immediately.",
		SummaryModel:  "qwen3:8b",
		FormatVersion: SourceSummaryFormatVersion,
		SummarizedAt:  1700000000,
	}
}

func TestUpsertSourceSummaryValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(*SourceSummary)
		wantErr string
	}{
		{"blank source", func(s *SourceSummary) { s.Source = "" }, "source"},
		{"blank content hash", func(s *SourceSummary) { s.ContentHash = "" }, "content_hash"},
		{"blank vector space", func(s *SourceSummary) { s.VectorSpaceID = "" }, "vector_space_id"},
		{"blank abstract", func(s *SourceSummary) { s.Abstract = "" }, "abstract"},
		{"blank overview", func(s *SourceSummary) { s.Overview = "" }, "overview"},
		{"blank model", func(s *SourceSummary) { s.SummaryModel = "" }, "summary_model"},
		{"wrong format version", func(s *SourceSummary) { s.FormatVersion = SourceSummaryFormatVersion + 1 }, "format_version"},
		{"zero timestamp", func(s *SourceSummary) { s.SummarizedAt = 0 }, "summarized_at"},
		{"negative timestamp", func(s *SourceSummary) { s.SummarizedAt = -5 }, "summarized_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := validSummary()
			tt.mutate(&row)
			err := store.UpsertSourceSummary(ctx, row)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("UpsertSourceSummary = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestUpsertSourceSummaryRoundTripAndReplace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	row := validSummary()
	if err := store.UpsertSourceSummary(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := store.SourceSummaryBatch(ctx, []string{row.Source})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if got[row.Source] != row {
		t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", got[row.Source], row)
	}

	// Replace, not duplicate.
	row2 := row
	row2.Abstract = "Updated."
	row2.ContentHash = "hash2"
	if err := store.UpsertSourceSummary(ctx, row2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM source_summaries WHERE source = ?`, row.Source).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row after replace, got %d", count)
	}
	got, err = store.SourceSummaryBatch(ctx, []string{row.Source})
	if err != nil {
		t.Fatalf("batch read after replace: %v", err)
	}
	if got[row.Source] != row2 {
		t.Fatalf("replace did not stick: %+v", got[row.Source])
	}
}

func TestDeleteSourceSummary(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	if err := store.DeleteSourceSummary(ctx, ""); err == nil {
		t.Fatal("blank source must be rejected")
	}
	row := validSummary()
	if err := store.UpsertSourceSummary(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.DeleteSourceSummary(ctx, row.Source); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err := store.SourceSummaryBatch(ctx, []string{row.Source})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("row survived delete: %+v", got)
	}
	// Deleting an absent row is a no-op, not an error.
	if err := store.DeleteSourceSummary(ctx, row.Source); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestUpsertSourceSummaryNormalizesIdentityFields pins the PK-collapse
// behavior: Source is both the primary key and the join key against
// chunks.source, so a padded value must normalize to the same row as its
// trimmed counterpart rather than coexisting as a second, unreachable row.
func TestUpsertSourceSummaryNormalizesIdentityFields(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	row := validSummary()
	padded := row
	padded.Source = "  " + row.Source + "  "
	padded.ContentHash = " " + row.ContentHash + " "
	padded.VectorSpaceID = " " + row.VectorSpaceID + " "
	padded.SummaryModel = " " + row.SummaryModel + " "
	if err := store.UpsertSourceSummary(ctx, padded); err != nil {
		t.Fatalf("upsert padded: %v", err)
	}

	got, err := store.SourceSummaryBatch(ctx, []string{row.Source})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if got[row.Source] != row {
		t.Fatalf("canonical lookup mismatch:\n got  %+v\n want %+v", got[row.Source], row)
	}

	// A second upsert differing only in padding must replace, not duplicate.
	repadded := row
	repadded.Source = "\t" + row.Source + "\n"
	if err := store.UpsertSourceSummary(ctx, repadded); err != nil {
		t.Fatalf("upsert repadded: %v", err)
	}
	// Unfiltered: the whole point is that two *different* input strings must
	// land on one row, so counting WHERE source = <canonical> would only ever
	// prove "the canonical row exists," not "no padded duplicate exists."
	// The test creates exactly one logical source, so this is unambiguous.
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM source_summaries`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row for logically-same source, got %d", count)
	}

	// Delete must use the same canonical key form as write.
	if err := store.DeleteSourceSummary(ctx, "  "+row.Source+"  "); err != nil {
		t.Fatalf("delete padded: %v", err)
	}
	got, err = store.SourceSummaryBatch(ctx, []string{row.Source})
	if err != nil {
		t.Fatalf("batch read after delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("row survived padded delete: %+v", got)
	}
}

// TestSourceSummaryBatchImmutableDegradation exercises both branches of the
// SourceSummaryBatch missing-table gate (s.immutable && isMissingTableErr):
// a read-only v7 snapshot degrades to summary-missing, but the identical
// missing-table condition on a writable store is corruption and must
// propagate rather than degrade.
func TestSourceSummaryBatchImmutableDegradation(t *testing.T) {
	ctx := context.Background()

	t.Run("read-only snapshot degrades to empty result", func(t *testing.T) {
		path := t.TempDir() + "/v7.db"
		rw, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatalf("NewSQLiteStore: %v", err)
		}
		defer func() { _ = rw.Close() }()
		if _, err := rw.db.Exec(`DROP TABLE source_summaries`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		// Explicit checked close: the file must be flushed before reopening
		// read-only below. The deferred Close above then becomes a harmless
		// idempotent no-op on the success path, and the safety net on any
		// earlier t.Fatalf.
		if err := rw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}

		ro, err := OpenSQLiteStoreReadOnly(path)
		if err != nil {
			t.Fatalf("OpenSQLiteStoreReadOnly: %v", err)
		}
		defer func() { _ = ro.Close() }()

		got, err := ro.SourceSummaryBatch(ctx, []string{"pkg/a.go"})
		if err != nil {
			t.Fatalf("SourceSummaryBatch = %v, want nil error", err)
		}
		if len(got) != 0 {
			t.Fatalf("SourceSummaryBatch = %+v, want empty map", got)
		}
	})

	t.Run("writable store propagates missing table as corruption", func(t *testing.T) {
		store := newTestStore(t)
		if _, err := store.db.Exec(`DROP TABLE source_summaries`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		if _, err := store.SourceSummaryBatch(ctx, []string{"pkg/a.go"}); err == nil {
			t.Fatal("SourceSummaryBatch = nil error, want propagated corruption error")
		}
	})
}

func TestSummaryRowMalformed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SourceSummary)
		want   bool
	}{
		{"valid", func(s *SourceSummary) {}, false},
		// Whitespace-only pins two properties at once: the clause must exist
		// (delete it and this goes red) and must use TrimSpace, not == ""
		// (drop the TrimSpace and this goes red too). Exact "" only pins the
		// former, which is why "empty abstract" below is kept alongside it.
		{"whitespace-only abstract", func(s *SourceSummary) { s.Abstract = "   " }, true},
		{"empty abstract", func(s *SourceSummary) { s.Abstract = "" }, true},
		{"whitespace-only overview", func(s *SourceSummary) { s.Overview = "   " }, true},
		{"whitespace-only model", func(s *SourceSummary) { s.SummaryModel = "   " }, true},
		{"whitespace-only content hash", func(s *SourceSummary) { s.ContentHash = "   " }, true},
		{"whitespace-only vector space", func(s *SourceSummary) { s.VectorSpaceID = "   " }, true},
		{"zero timestamp", func(s *SourceSummary) { s.SummarizedAt = 0 }, true},
		// Above-current format: written by a newer build — cannot be interpreted.
		{"format above current", func(s *SourceSummary) { s.FormatVersion = SourceSummaryFormatVersion + 1 }, true},
		// Below-current format is stale, NOT malformed (spec section 5 matrix).
		{"format below current", func(s *SourceSummary) { s.FormatVersion = SourceSummaryFormatVersion - 1 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := validSummary()
			tt.mutate(&row)
			if got := summaryRowMalformed(row); got != tt.want {
				t.Fatalf("summaryRowMalformed = %v, want %v", got, tt.want)
			}
		})
	}
}

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
		// Blank vector space and unparseable signature (legacy row).
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
