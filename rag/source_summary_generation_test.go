package rag

import (
	"context"
	"errors"
	"testing"
)

func seedSummaryGenerationSource(t *testing.T, store *SQLiteStore, source, hash, vectorSpaceID, content string) {
	t.Helper()
	chunk := Chunk{
		ID: "chunk-" + hash, Content: content, Source: source,
		StartLine: 1, EndLine: 1, Language: "go",
	}
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(
		context.Background(), source, []Chunk{chunk}, [][]float64{{1, 0}},
		sigJSON(t, hash), vectorSpaceID,
	); err != nil {
		t.Fatalf("seed source %q: %v", source, err)
	}
}

func TestGenerateSourceSummariesPersistsExactProvenance(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const (
		source = "  pkg/a.go  "
		hash   = "  hash-a  "
		vsid   = "  provider/embedding  "
	)
	seedSummaryGenerationSource(t, store, source, hash, vsid, "package a\nfunc A() {}")

	calls := 0
	err := store.GenerateSourceSummaries(ctx, func(_ context.Context, in SourceSummaryInput) (GeneratedSourceSummary, error) {
		calls++
		if in.Source != source || len(in.Chunks) != 1 || in.Chunks[0].Content != "package a\nfunc A() {}" {
			t.Fatalf("generation input = %+v", in)
		}
		return GeneratedSourceSummary{
			Abstract: "Provides A.",
			Overview: "Defines A and its package behavior.",
			Model:    "provider/summarizer",
		}, nil
	})
	if err != nil {
		t.Fatalf("GenerateSourceSummaries: %v", err)
	}
	if calls != 1 {
		t.Fatalf("generator calls = %d, want 1", calls)
	}

	rows, err := store.SourceSummaryBatch(ctx, []string{source})
	if err != nil {
		t.Fatalf("SourceSummaryBatch: %v", err)
	}
	got, ok := rows[source]
	if !ok {
		t.Fatal("generated summary row is missing")
	}
	if got.Source != source || got.ContentHash != hash || got.VectorSpaceID != vsid {
		t.Fatalf("stored identity = %#v, want exact source=%q hash=%q vector=%q", got, source, hash, vsid)
	}
	if got.Abstract != "Provides A." || got.Overview != "Defines A and its package behavior." ||
		got.SummaryModel != "provider/summarizer" {
		t.Fatalf("stored summary = %+v", got)
	}
	if got.FormatVersion != SourceSummaryFormatVersion || got.SummarizedAt <= 0 {
		t.Fatalf("stored version/time = %d/%d", got.FormatVersion, got.SummarizedAt)
	}
}

func TestGenerateSourceSummariesProviderFailureWritesNothing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSummaryGenerationSource(t, store, "pkg/a.go", "hash-a", "provider/embedding", "package a")
	wantErr := errors.New("summarizer unavailable")

	err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
		return GeneratedSourceSummary{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("GenerateSourceSummaries error = %v, want %v", err, wantErr)
	}
	rows, err := store.SourceSummaryBatch(ctx, []string{"pkg/a.go"})
	if err != nil {
		t.Fatalf("SourceSummaryBatch: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("provider failure wrote summaries: %+v", rows)
	}
}

func TestGenerateSourceSummariesRejectsStaleSourceCAS(t *testing.T) {
	for _, tc := range []struct {
		name, nextHash, nextVectorSpace string
	}{
		{name: "content changed", nextHash: "hash-b", nextVectorSpace: "provider/embed-a"},
		{name: "vector space changed", nextHash: "hash-a", nextVectorSpace: "provider/embed-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			const source = "pkg/a.go"
			seedSummaryGenerationSource(t, store, source, "hash-a", "provider/embed-a", "package a")

			err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
				if err := store.DeleteBySource(ctx, source); err != nil {
					t.Fatalf("delete before concurrent reindex: %v", err)
				}
				seedSummaryGenerationSource(t, store, source, tc.nextHash, tc.nextVectorSpace, "package changed")
				return GeneratedSourceSummary{
					Abstract: "Stale A.",
					Overview: "Describes the superseded source.",
					Model:    "provider/summarizer",
				}, nil
			})
			if !errors.Is(err, ErrSourceSummaryStale) {
				t.Fatalf("GenerateSourceSummaries error = %v, want ErrSourceSummaryStale", err)
			}
			rows, readErr := store.SourceSummaryBatch(ctx, []string{source})
			if readErr != nil {
				t.Fatalf("SourceSummaryBatch: %v", readErr)
			}
			if len(rows) != 0 {
				t.Fatalf("stale generation was published: %+v", rows)
			}
		})
	}
}

func TestGenerateSourceSummariesPreservesNewerFormatWrittenDuringGeneration(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSummaryGenerationSource(t, store, "pkg/a.go", "hash-current", "provider/embed", "package a")

	err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
		insertRawSummaryVersion(t, store, SourceSummaryFormatVersion+1)
		return GeneratedSourceSummary{
			Abstract: "Older generator.",
			Overview: "Must not replace the newer row.",
			Model:    "provider/older",
		}, nil
	})
	if err == nil {
		t.Fatal("GenerateSourceSummaries unexpectedly overwrote a concurrently written newer format")
	}
	rows, readErr := store.SourceSummaryBatch(ctx, []string{"pkg/a.go"})
	if readErr != nil {
		t.Fatal(readErr)
	}
	got := rows["pkg/a.go"]
	if got.FormatVersion != SourceSummaryFormatVersion+1 || got.Abstract != "Stored." {
		t.Fatalf("newer summary was overwritten: %+v", got)
	}
}

func TestGenerateSourceSummaryRejectsChunksFromDifferentSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const source = "pkg/a.go"
	seedSummaryGenerationSource(t, store, source, "hash-a", "provider/embed", "package a")
	provenance, err := store.SourceProvenanceBatch(ctx, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	captured := provenance[source]

	if err := store.DeleteBySource(ctx, source); err != nil {
		t.Fatal(err)
	}
	seedSummaryGenerationSource(t, store, source, "hash-b", "provider/embed", "package b")
	calls := 0
	err = store.generateSourceSummary(ctx, captured, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
		calls++
		return GeneratedSourceSummary{
			Abstract: "Wrong snapshot.",
			Overview: "Must never be labeled with hash-a.",
			Model:    "provider/summary",
		}, nil
	})
	if !errors.Is(err, ErrSourceSummaryStale) {
		t.Fatalf("generateSourceSummary error = %v, want ErrSourceSummaryStale", err)
	}
	if calls != 0 {
		t.Fatalf("generator calls = %d, want none for mismatched chunks/provenance", calls)
	}
}

func TestGenerateSourceSummariesSkipsUnknownAndMixedProvenance(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	emb := []byte{0, 0, 0, 0}
	storeChunksRaw(t, store, [][]any{
		{"unknown", "unknown", "unknown.go", 1, 1, "go", `{}`, emb, int64(1), "", "", ""},
		{"mixed-a", "a", "mixed.go", 1, 1, "go", `{}`, emb, int64(1), "", sigJSON(t, "a"), "provider/embed"},
		{"mixed-b", "b", "mixed.go", 2, 2, "go", `{}`, emb, int64(1), "", sigJSON(t, "b"), "provider/embed"},
	})
	calls := 0
	err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
		calls++
		return GeneratedSourceSummary{}, nil
	})
	if err != nil {
		t.Fatalf("GenerateSourceSummaries: %v", err)
	}
	if calls != 0 {
		t.Fatalf("generator calls = %d, want none for unknown/mixed provenance", calls)
	}
}

func TestGenerateSourceSummariesFreshnessDecisions(t *testing.T) {
	tests := []struct {
		name        string
		seedSummary func(*testing.T, *SQLiteStore)
		wantCall    bool
		wantVersion int
	}{
		{name: "missing", wantCall: true, wantVersion: SourceSummaryFormatVersion},
		{
			name: "fresh",
			seedSummary: func(t *testing.T, store *SQLiteStore) {
				row := validSummary()
				row.ContentHash, row.VectorSpaceID = "hash-current", "provider/embed"
				if err := store.UpsertSourceSummary(context.Background(), row); err != nil {
					t.Fatal(err)
				}
			},
			wantVersion: SourceSummaryFormatVersion,
		},
		{
			name: "stale content",
			seedSummary: func(t *testing.T, store *SQLiteStore) {
				row := validSummary()
				row.ContentHash, row.VectorSpaceID = "hash-old", "provider/embed"
				if err := store.UpsertSourceSummary(context.Background(), row); err != nil {
					t.Fatal(err)
				}
			},
			wantCall: true, wantVersion: SourceSummaryFormatVersion,
		},
		{
			name: "stale vector space",
			seedSummary: func(t *testing.T, store *SQLiteStore) {
				row := validSummary()
				row.ContentHash, row.VectorSpaceID = "hash-current", "provider/embed-old"
				if err := store.UpsertSourceSummary(context.Background(), row); err != nil {
					t.Fatal(err)
				}
			},
			wantCall: true, wantVersion: SourceSummaryFormatVersion,
		},
		{
			name: "below current format regenerates",
			seedSummary: func(t *testing.T, store *SQLiteStore) {
				insertRawSummaryVersion(t, store, SourceSummaryFormatVersion-1)
			},
			wantCall: true, wantVersion: SourceSummaryFormatVersion,
		},
		{
			name: "above current format is unreadable and preserved",
			seedSummary: func(t *testing.T, store *SQLiteStore) {
				insertRawSummaryVersion(t, store, SourceSummaryFormatVersion+1)
			},
			wantVersion: SourceSummaryFormatVersion + 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			seedSummaryGenerationSource(t, store, "pkg/a.go", "hash-current", "provider/embed", "package a")
			if tt.seedSummary != nil {
				tt.seedSummary(t, store)
			}

			calls := 0
			err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
				calls++
				return GeneratedSourceSummary{
					Abstract: "Generated.",
					Overview: "Generated overview.",
					Model:    "provider/summarizer",
				}, nil
			})
			if err != nil {
				t.Fatalf("GenerateSourceSummaries: %v", err)
			}
			wantCalls := 0
			if tt.wantCall {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("generator calls = %d, want %d", calls, wantCalls)
			}
			rows, err := store.SourceSummaryBatch(ctx, []string{"pkg/a.go"})
			if err != nil {
				t.Fatal(err)
			}
			got := rows["pkg/a.go"]
			if got.FormatVersion != tt.wantVersion {
				t.Fatalf("format version = %d, want %d", got.FormatVersion, tt.wantVersion)
			}
			if tt.wantCall && got.Abstract != "Generated." {
				t.Fatalf("stale row was not regenerated: %+v", got)
			}
			if !tt.wantCall && got.Abstract == "Generated." {
				t.Fatalf("row was unexpectedly regenerated: %+v", got)
			}
		})
	}
}

func insertRawSummaryVersion(t *testing.T, store *SQLiteStore, version int) {
	t.Helper()
	if _, err := store.db.Exec(`
		INSERT INTO source_summaries
		  (source, content_hash, vector_space_id, abstract, overview, summary_model, format_version, summarized_at)
		VALUES ('pkg/a.go', 'hash-current', 'provider/embed', 'Stored.', 'Stored overview.', 'provider/model', ?, 1700000000)`,
		version); err != nil {
		t.Fatalf("insert raw summary version %d: %v", version, err)
	}
}
