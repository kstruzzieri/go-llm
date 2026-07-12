package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func seedPublishedGeneration(t *testing.T, baseDB, generation, workspaceID, vsid string) indexGeneration {
	t.Helper()
	gen := generationPaths(baseDB, generation)
	if err := os.MkdirAll(filepath.Dir(gen.dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, gen.dbPath, workspaceID, vsid)
	legacy, err := readSidecar(gen.metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := store.Stats(context.Background())
	closeErr := store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	gen.metadata = generationMetadata{
		SchemaVersion:           generationSchemaVersion,
		Generation:              generation,
		WorkspaceID:             workspaceID,
		RequestedEmbeddingModel: legacy.RequestedEmbeddingModel,
		VectorSpaceID:           vsid,
		IndexedAt:               legacy.IndexedAt,
		Status:                  legacy.Status,
		ErrorCount:              legacy.ErrorCount,
		SourceCount:             stats.TotalSources,
		ChunkCount:              stats.TotalChunks,
	}
	if err := writeGenerationMetadata(context.Background(), gen.metadataPath, gen.metadata); err != nil {
		t.Fatal(err)
	}
	return gen
}

func publishTestGeneration(t *testing.T, baseDB, workspaceID, generation string) indexGeneration {
	t.Helper()
	gen := seedPublishedGeneration(t, baseDB, generation, workspaceID, "ollama/nomic")
	if err := publishActiveGeneration(context.Background(), baseDB, activeGenerationPointer{
		SchemaVersion: activePointerSchemaVersion,
		WorkspaceID:   workspaceID,
		Generation:    generation,
	}, nil); err != nil {
		t.Fatal(err)
	}
	return gen
}

func TestResolveActiveGeneration_ValidatesPublishedGeneration(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	want := publishTestGeneration(t, baseDB, "workspace:k", strings.Repeat("a", 32))

	got, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k")
	if err != nil {
		t.Fatalf("resolveActiveGeneration: %v", err)
	}
	if got.id != want.id || got.dbPath != want.dbPath || got.legacy {
		t.Fatalf("generation = %+v, want %+v", got, want)
	}
}

func TestResolveActiveGeneration_DoesNotFallbackWhenPointerIsCorrupt(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	seedIndex(t, baseDB, "workspace:k", "ollama/nomic")
	if err := os.WriteFile(activePointerPath(baseDB), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k"); err == nil {
		t.Fatal("corrupt active pointer must fail closed instead of serving legacy files")
	}
}

func TestResolveActiveGeneration_RejectsInvalidMetadataAndDatabase(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, indexGeneration)
		want   string
	}{
		{
			name: "generation mismatch",
			mutate: func(t *testing.T, gen indexGeneration) {
				gen.metadata.Generation = strings.Repeat("b", 32)
				if err := writeGenerationMetadata(context.Background(), gen.metadataPath, gen.metadata); err != nil {
					t.Fatal(err)
				}
			},
			want: "generation",
		},
		{
			name: "source count mismatch",
			mutate: func(t *testing.T, gen indexGeneration) {
				gen.metadata.SourceCount++
				if err := writeGenerationMetadata(context.Background(), gen.metadataPath, gen.metadata); err != nil {
					t.Fatal(err)
				}
			},
			want: "source count",
		},
		{
			name: "missing vector space",
			mutate: func(t *testing.T, gen indexGeneration) {
				store, err := rag.NewSQLiteStore(gen.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE chunks SET vector_space_id = ''`); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			},
			want: "vector space",
		},
		{
			name: "mixed vector spaces",
			mutate: func(t *testing.T, gen indexGeneration) {
				store, err := rag.NewSQLiteStore(gen.dbPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE chunks SET vector_space_id = 'other/model' WHERE rowid = (SELECT MIN(rowid) FROM chunks)`); err != nil {
					t.Fatal(err)
				}
				vectorJSON := "[1" + strings.Repeat(",0", 767) + "]"
				if _, err := store.DB().Exec(`
					INSERT INTO chunks (id, content, source, start_line, end_line, language, metadata, embedding, indexed_at, stable_key, source_content_hash, vector_space_id)
					VALUES ('mixed', 'x', 'mixed.go', 1, 1, 'go', '{}', ?, 1, '', '', 'ollama/nomic')`, vectorJSON); err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				gen.metadata.SourceCount++
				gen.metadata.ChunkCount++
				if err := writeGenerationMetadata(context.Background(), gen.metadataPath, gen.metadata); err != nil {
					t.Fatal(err)
				}
			},
			want: "vector space",
		},
		{
			name: "corrupt database",
			mutate: func(t *testing.T, gen indexGeneration) {
				if err := os.WriteFile(gen.dbPath, []byte("not sqlite"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "database",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			baseDB := filepath.Join(t.TempDir(), "k.db")
			gen := publishTestGeneration(t, baseDB, "workspace:k", strings.Repeat("a", 32))
			tc.mutate(t, gen)
			_, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCleanupStaleGenerations_PreservesActiveAndLegacy(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	activeID := strings.Repeat("a", 32)
	active := publishTestGeneration(t, baseDB, "workspace:k", activeID)
	stale, err := createStagingGeneration(context.Background(), baseDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale.dbPath, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, baseDB, "workspace:k", "ollama/nomic")
	if err := os.WriteFile(activePointerPath(baseDB)+".tmp", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cleanupStaleGenerations(context.Background(), baseDB); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active.dbPath, baseDB, sidecarPath(baseDB)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path %q: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(stale.dbPath)); !os.IsNotExist(err) {
		t.Fatalf("stale generation still exists: %v", err)
	}
	if _, err := os.Stat(activePointerPath(baseDB) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("orphan pointer temp still exists: %v", err)
	}
}

func TestPublishActiveGeneration_InterruptionHasDeterministicRecovery(t *testing.T) {
	stop := errors.New("stop")
	tests := []struct {
		boundary publicationBoundary
		wantNew  bool
	}{
		{publicationBeforePointerTemp, false},
		{publicationAfterPointerTempSync, false},
		{publicationAfterPointerRename, true},
		{publicationAfterPointerDirSync, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.boundary), func(t *testing.T) {
			baseDB := filepath.Join(t.TempDir(), "k.db")
			oldID, newID := strings.Repeat("a", 32), strings.Repeat("b", 32)
			publishTestGeneration(t, baseDB, "workspace:k", oldID)
			seedPublishedGeneration(t, baseDB, newID, "workspace:k", "ollama/nomic")
			err := publishActiveGeneration(context.Background(), baseDB, activeGenerationPointer{
				SchemaVersion: activePointerSchemaVersion,
				WorkspaceID:   "workspace:k",
				Generation:    newID,
			}, func(got publicationBoundary) error {
				if got == tc.boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatalf("publish error = %v, want injected stop", err)
			}
			active, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k")
			if err != nil {
				t.Fatal(err)
			}
			want := oldID
			if tc.wantNew {
				want = newID
			}
			if active.id != want {
				t.Fatalf("active generation = %q, want %q", active.id, want)
			}
		})
	}
}

func TestFinalizeStagingGeneration_InterruptionRecovery(t *testing.T) {
	stop := errors.New("stop")
	tests := []struct {
		boundary  finalizationBoundary
		wantFinal bool
	}{
		{finalizationBeforeRename, false},
		{finalizationAfterRename, true},
		{finalizationAfterDirSync, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.boundary), func(t *testing.T) {
			baseDB := filepath.Join(t.TempDir(), "k.db")
			oldID := strings.Repeat("a", 32)
			publishTestGeneration(t, baseDB, "workspace:k", oldID)
			staging, err := createStagingGeneration(context.Background(), baseDB)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(staging.dbPath, []byte("staging"), 0o600); err != nil {
				t.Fatal(err)
			}

			_, err = finalizeStagingGeneration(context.Background(), baseDB, staging, func(got finalizationBoundary) error {
				if got == tc.boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatalf("finalize error = %v, want injected stop", err)
			}
			active, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k")
			if err != nil || active.id != oldID {
				t.Fatalf("active generation = %+v, %v; want old %q", active, err, oldID)
			}
			final := generationPaths(baseDB, staging.id)
			_, finalErr := os.Stat(filepath.Dir(final.dbPath))
			if tc.wantFinal && finalErr != nil {
				t.Fatalf("finalized orphan missing: %v", finalErr)
			}
			if !tc.wantFinal {
				if err := cleanupStaleGenerations(context.Background(), baseDB); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Dir(staging.dbPath)); !os.IsNotExist(err) {
					t.Fatalf("stale staging was not cleaned: %v", err)
				}
			}
		})
	}
}
