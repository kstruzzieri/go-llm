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

func TestUpsertSourceSummaryPreservesExactIdentity(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	plain := validSummary()
	padded := plain
	padded.Source = "  " + plain.Source + "  "
	padded.ContentHash = " " + plain.ContentHash + " "
	padded.VectorSpaceID = " " + plain.VectorSpaceID + " "
	padded.SummaryModel = " " + plain.SummaryModel + " "
	padded.Abstract = "Padded source."
	if err := store.UpsertSourceSummary(ctx, plain); err != nil {
		t.Fatalf("upsert plain: %v", err)
	}
	if err := store.UpsertSourceSummary(ctx, padded); err != nil {
		t.Fatalf("upsert padded: %v", err)
	}

	got, err := store.SourceSummaryBatch(ctx, []string{plain.Source, padded.Source})
	if err != nil {
		t.Fatalf("batch read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("distinct source keys collapsed: got %+v", got)
	}
	if got[plain.Source] != plain {
		t.Fatalf("plain row mismatch:\n got  %+v\n want %+v", got[plain.Source], plain)
	}
	wantPadded := padded
	wantPadded.SummaryModel = plain.SummaryModel
	if got[padded.Source] != wantPadded {
		t.Fatalf("padded row mismatch:\n got  %+v\n want %+v", got[padded.Source], wantPadded)
	}
}

func TestDeleteSourceSummaryUsesExactSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	plain := validSummary()
	padded := plain
	padded.Source = "  " + plain.Source + "  "
	padded.Abstract = "Padded source."
	for _, row := range []SourceSummary{plain, padded} {
		if err := store.UpsertSourceSummary(ctx, row); err != nil {
			t.Fatalf("upsert %q: %v", row.Source, err)
		}
	}

	if err := store.DeleteSourceSummary(ctx, padded.Source); err != nil {
		t.Fatalf("delete padded: %v", err)
	}
	got, err := store.SourceSummaryBatch(ctx, []string{plain.Source, padded.Source})
	if err != nil {
		t.Fatalf("batch read after delete: %v", err)
	}
	if _, ok := got[padded.Source]; ok {
		t.Fatalf("padded row survived exact delete: %+v", got)
	}
	if got[plain.Source] != plain {
		t.Fatalf("deleting %q changed distinct row %q: %+v", padded.Source, plain.Source, got)
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

func TestSummaryLifecycleUsesExactPaddedSource(t *testing.T) {
	ctx := context.Background()
	emb4 := []float64{1, 0, 0, 0}
	const padded = "  pkg/pad.go  "
	const plain = "pkg/pad.go"

	seed := func(t *testing.T) *SQLiteStore {
		store := newTestStore(t)
		chunk := Chunk{ID: "pad1", Content: "v1", Source: padded, StartLine: 1, EndLine: 1}
		if err := store.Store(ctx, []Chunk{chunk}, [][]float64{emb4}); err != nil {
			t.Fatalf("seed chunks: %v", err)
		}
		row := validSummary()
		row.Source = padded
		row.Abstract = "Padded source."
		if err := store.UpsertSourceSummary(ctx, row); err != nil {
			t.Fatalf("seed padded summary: %v", err)
		}
		row.Source = plain
		row.Abstract = "Plain source."
		if err := store.UpsertSourceSummary(ctx, row); err != nil {
			t.Fatalf("seed plain summary: %v", err)
		}
		return store
	}

	summaryExists := func(t *testing.T, store *SQLiteStore, source string) bool {
		got, err := store.SourceSummaryBatch(ctx, []string{source})
		if err != nil {
			t.Fatalf("summary batch: %v", err)
		}
		_, ok := got[source]
		return ok
	}

	t.Run("DeleteBySource deletes only the exact row", func(t *testing.T) {
		store := seed(t)
		if err := store.DeleteBySource(ctx, padded); err != nil {
			t.Fatalf("DeleteBySource: %v", err)
		}
		if summaryExists(t, store, padded) {
			t.Fatal("padded source kept its summary row")
		}
		if !summaryExists(t, store, plain) {
			t.Fatal("deleting padded source removed the distinct plain summary")
		}
	})

	t.Run("replaceSourceTx empty replace deletes only the exact row", func(t *testing.T) {
		store := seed(t)
		if err := store.ReplaceSource(ctx, padded, nil, nil); err != nil {
			t.Fatalf("empty replace: %v", err)
		}
		if summaryExists(t, store, padded) {
			t.Fatal("padded source kept its summary row")
		}
		if !summaryExists(t, store, plain) {
			t.Fatal("empty-replacing padded source removed the distinct plain summary")
		}
	})
}
