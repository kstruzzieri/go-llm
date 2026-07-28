package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestGenerateSourceSummariesProviderFailureDoesNotBlockOtherSources(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSummaryGenerationSource(t, store, "pkg/a.go", "hash-a", "provider/embedding", "package a")
	seedSummaryGenerationSource(t, store, "pkg/b.go", "hash-b", "provider/embedding", "package b")
	wantErr := errors.New("summarizer unavailable")

	var calls []string
	err := store.GenerateSourceSummaries(ctx, func(_ context.Context, in SourceSummaryInput) (GeneratedSourceSummary, error) {
		calls = append(calls, in.Source)
		if in.Source == "pkg/a.go" {
			return GeneratedSourceSummary{}, wantErr
		}
		return GeneratedSourceSummary{
			Abstract: "Provides B.",
			Overview: "Defines B.",
			Model:    "provider/summarizer",
		}, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("GenerateSourceSummaries error = %v, want %v", err, wantErr)
	}
	if len(calls) != 2 || calls[0] != "pkg/a.go" || calls[1] != "pkg/b.go" {
		t.Fatalf("generator calls = %v, want both sources in order", calls)
	}
	rows, err := store.SourceSummaryBatch(ctx, []string{"pkg/a.go", "pkg/b.go"})
	if err != nil {
		t.Fatalf("SourceSummaryBatch: %v", err)
	}
	if _, ok := rows["pkg/a.go"]; ok {
		t.Fatalf("provider failure wrote a summary for pkg/a.go: %+v", rows)
	}
	if got, ok := rows["pkg/b.go"]; !ok || got.Abstract != "Provides B." {
		t.Fatalf("healthy source summary = %+v, present=%v", got, ok)
	}
}

// Callers surface this error through a first-line-only warning, so the tally
// has to lead: errors.Join renders one error per line, and without a count a
// single failure and every source failing print identically.
func TestGenerateSourceSummariesErrorLeadsWithFailureCount(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"pkg/a.go", "pkg/b.go", "pkg/c.go"} {
		seedSummaryGenerationSource(t, store, name, "hash-"+name, "provider/embedding", "package x")
	}
	wantErr := errors.New("summarizer unavailable")

	err := store.GenerateSourceSummaries(ctx, func(_ context.Context, in SourceSummaryInput) (GeneratedSourceSummary, error) {
		if in.Source == "pkg/c.go" {
			return GeneratedSourceSummary{
				Abstract: "Provides C.", Overview: "Defines C.", Model: "provider/summarizer",
			}, nil
		}
		return GeneratedSourceSummary{}, wantErr
	})
	if err == nil {
		t.Fatal("GenerateSourceSummaries: want error")
	}
	first, _, _ := strings.Cut(err.Error(), "\n")
	if !strings.Contains(first, "2 of 3 sources failed") {
		t.Fatalf("first line = %q, want a leading \"2 of 3 sources failed\" tally", first)
	}
	// The tally must not cost errors.Is traversal into the joined causes.
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want errors.Is(err, wantErr)", err)
	}
}

// GenerateSourceSummaries passes EVERY source in the index to these batch
// readers, unlike the retrieval path that introduced them (a handful of
// results). SQLite's SQLITE_MAX_VARIABLE_NUMBER is 32766 and exceeding it fails
// the whole statement with "too many SQL variables" rather than degrading.
func TestSourceBatchReadsExceedSQLiteVariableLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const seeded = "pkg/real.go"
	seedSummaryGenerationSource(t, store, seeded, "hash-real", "provider/embedding", "package real")

	sources := []string{seeded}
	for i := 0; i < 40000; i++ {
		sources = append(sources, fmt.Sprintf("pkg/absent-%d.go", i))
	}

	prov, err := store.SourceProvenanceBatch(ctx, sources)
	if err != nil {
		t.Fatalf("SourceProvenanceBatch(%d sources): %v", len(sources), err)
	}
	if got, ok := prov[seeded]; !ok || got.ContentHash != "hash-real" {
		t.Fatalf("batching lost the real row: %+v present=%v", got, ok)
	}
	if len(prov) != 1 {
		t.Fatalf("provenance rows = %d, want only the seeded source", len(prov))
	}

	row := validSummary()
	row.Source, row.ContentHash, row.VectorSpaceID = seeded, "hash-real", "provider/embedding"
	if err := store.UpsertSourceSummary(ctx, row); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.SourceSummaryBatch(ctx, sources)
	if err != nil {
		t.Fatalf("SourceSummaryBatch(%d sources): %v", len(sources), err)
	}
	if len(summaries) != 1 || summaries[seeded].ContentHash != "hash-real" {
		t.Fatalf("summary rows = %+v, want only the seeded source", summaries)
	}
}

func TestGenerateSourceSummariesContextCancellationStopsRemainingSources(t *testing.T) {
	store := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	seedSummaryGenerationSource(t, store, "pkg/a.go", "hash-a", "provider/embedding", "package a")
	seedSummaryGenerationSource(t, store, "pkg/b.go", "hash-b", "provider/embedding", "package b")

	calls := 0
	err := store.GenerateSourceSummaries(ctx, func(context.Context, SourceSummaryInput) (GeneratedSourceSummary, error) {
		calls++
		cancel()
		return GeneratedSourceSummary{}, context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GenerateSourceSummaries error = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("generator calls = %d, want cancellation to stop before the second source", calls)
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
