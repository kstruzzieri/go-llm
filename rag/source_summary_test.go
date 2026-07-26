package rag

import (
	"context"
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

func TestSummaryLifecycle(t *testing.T) {
	ctx := context.Background()
	emb4 := []float64{1, 0, 0, 0}

	seed := func(t *testing.T) *SQLiteStore {
		store := newTestStore(t)
		chunk := Chunk{ID: "lc1", Content: "v1", Source: "pkg/lc.go", StartLine: 1, EndLine: 1}
		if err := store.Store(ctx, []Chunk{chunk}, [][]float64{emb4}); err != nil {
			t.Fatalf("seed chunks: %v", err)
		}
		row := validSummary()
		row.Source = "pkg/lc.go"
		if err := store.UpsertSourceSummary(ctx, row); err != nil {
			t.Fatalf("seed summary: %v", err)
		}
		return store
	}

	summaryExists := func(t *testing.T, store *SQLiteStore) bool {
		got, err := store.SourceSummaryBatch(ctx, []string{"pkg/lc.go"})
		if err != nil {
			t.Fatalf("summary batch: %v", err)
		}
		_, ok := got["pkg/lc.go"]
		return ok
	}

	t.Run("reindex with content retains", func(t *testing.T) {
		store := seed(t)
		chunk := Chunk{ID: "lc2", Content: "v2", Source: "pkg/lc.go", StartLine: 1, EndLine: 1}
		if err := store.ReplaceSource(ctx, "pkg/lc.go", []Chunk{chunk}, [][]float64{emb4}); err != nil {
			t.Fatalf("replace: %v", err)
		}
		if !summaryExists(t, store) {
			t.Fatal("summary must survive a content reindex (it becomes derived-stale)")
		}
	})

	t.Run("replace to zero chunks deletes", func(t *testing.T) {
		store := seed(t)
		if err := store.ReplaceSource(ctx, "pkg/lc.go", nil, nil); err != nil {
			t.Fatalf("empty replace: %v", err)
		}
		if summaryExists(t, store) {
			t.Fatal("summary must be deleted when the source empties")
		}
	})

	t.Run("DeleteBySource deletes", func(t *testing.T) {
		store := seed(t)
		if err := store.DeleteBySource(ctx, "pkg/lc.go"); err != nil {
			t.Fatalf("DeleteBySource: %v", err)
		}
		if summaryExists(t, store) {
			t.Fatal("summary must be deleted with its source")
		}
	})
}

// TestSummaryLifecycleTrimsPaddedChunkSource pins DEV-2 on the deletion
// paths: UpsertSourceSummary trims Source before storing, but chunks.source
// is never normalized (validateStoreInputs does not inspect it), so a
// padded chunk source and its trimmed summary row are different strings.
// DeleteBySource and replaceSourceTx must trim before matching against
// source_summaries or the row is orphaned forever (SourceSummaryBatch
// deliberately does not trim its own inputs, so nothing else reclaims it).
func TestSummaryLifecycleTrimsPaddedChunkSource(t *testing.T) {
	ctx := context.Background()
	emb4 := []float64{1, 0, 0, 0}
	const padded = "  pkg/pad.go  "
	const trimmed = "pkg/pad.go"

	seed := func(t *testing.T) *SQLiteStore {
		store := newTestStore(t)
		chunk := Chunk{ID: "pad1", Content: "v1", Source: padded, StartLine: 1, EndLine: 1}
		if err := store.Store(ctx, []Chunk{chunk}, [][]float64{emb4}); err != nil {
			t.Fatalf("seed chunks: %v", err)
		}
		row := validSummary()
		row.Source = trimmed
		if err := store.UpsertSourceSummary(ctx, row); err != nil {
			t.Fatalf("seed summary: %v", err)
		}
		return store
	}

	summaryExists := func(t *testing.T, store *SQLiteStore) bool {
		got, err := store.SourceSummaryBatch(ctx, []string{trimmed})
		if err != nil {
			t.Fatalf("summary batch: %v", err)
		}
		_, ok := got[trimmed]
		return ok
	}

	t.Run("DeleteBySource reclaims the trimmed row", func(t *testing.T) {
		store := seed(t)
		if err := store.DeleteBySource(ctx, padded); err != nil {
			t.Fatalf("DeleteBySource: %v", err)
		}
		if summaryExists(t, store) {
			t.Fatal("padded source must still reclaim the trimmed summary row")
		}
	})

	t.Run("replaceSourceTx empty replace reclaims the trimmed row", func(t *testing.T) {
		store := seed(t)
		if err := store.ReplaceSource(ctx, padded, nil, nil); err != nil {
			t.Fatalf("empty replace: %v", err)
		}
		if summaryExists(t, store) {
			t.Fatal("padded source must still reclaim the trimmed summary row")
		}
	})
}
