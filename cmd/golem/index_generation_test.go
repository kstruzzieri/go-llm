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

func TestResolveActiveGeneration_ValidatesPublished(t *testing.T) {
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

func TestResolveActiveGeneration_NoFallbackOnCorruptPtr(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	seedIndex(t, baseDB, "workspace:k", "ollama/nomic")
	if err := os.WriteFile(activePointerPath(baseDB), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k"); err == nil {
		t.Fatal("corrupt active pointer must fail closed instead of serving legacy files")
	}
}

func TestResolveActiveGeneration_RejectsBadMetaAndDB(t *testing.T) {
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
				if err := store.Store(context.Background(), []rag.Chunk{{ID: "mixed", Content: "x", Source: "mixed.go", StartLine: 1, EndLine: 1, Language: "go"}}, [][]float64{realisticTestVector()}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.DB().Exec(`UPDATE chunks SET vector_space_id = 'ollama/nomic' WHERE id = 'mixed'`); err != nil {
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

func TestCleanupStaleGenerations_KeepsActiveAndLegacy(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	activeID := strings.Repeat("a", 32)
	active := publishTestGeneration(t, baseDB, "workspace:k", activeID)
	superseded := seedPublishedGeneration(t, baseDB, strings.Repeat("c", 32), "workspace:k", "ollama/nomic")
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

	// Without finalized GC (active resolution failed for an unknown reason)
	// every finalized generation survives.
	if _, err := cleanupStaleGenerations(context.Background(), baseDB, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(superseded.dbPath); err != nil {
		t.Fatalf("superseded generation must survive without finalized GC: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(stale.dbPath)); !os.IsNotExist(err) {
		t.Fatalf("stale staging generation still exists: %v", err)
	}
	if _, err := os.Stat(activePointerPath(baseDB) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("orphan pointer temp still exists: %v", err)
	}

	warn, err := cleanupStaleGenerations(context.Background(), baseDB, activeID, true)
	if err != nil {
		t.Fatal(err)
	}
	if warn != "" {
		t.Fatalf("unexpected GC warning: %q", warn)
	}
	for _, path := range []string{active.dbPath, baseDB, sidecarPath(baseDB)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved path %q: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Dir(superseded.dbPath)); !os.IsNotExist(err) {
		t.Fatalf("superseded generation still exists after finalized GC: %v", err)
	}
}

func TestCleanupStaleGenerations_ReportsRemovalFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("removal-failure injection via directory permissions requires non-root")
	}
	baseDB := filepath.Join(t.TempDir(), "k.db")
	activeID := strings.Repeat("a", 32)
	publishTestGeneration(t, baseDB, "workspace:k", activeID)
	supersededID := strings.Repeat("c", 32)
	superseded := seedPublishedGeneration(t, baseDB, supersededID, "workspace:k", "ollama/nomic")
	dir := filepath.Dir(superseded.dbPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})

	warn, err := cleanupStaleGenerations(context.Background(), baseDB, activeID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn, supersededID) || !strings.Contains(warn, "retry") {
		t.Fatalf("warn = %q, want failed removal of %q with retry note", warn, supersededID)
	}
	if _, err := os.Stat(superseded.dbPath); err != nil {
		t.Fatalf("superseded generation must survive a failed removal: %v", err)
	}
}

func TestShouldRemoveUnpublished(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	genID := strings.Repeat("a", 32)
	if shouldRemoveUnpublished(context.Background(), baseDB, genID) {
		t.Fatal("missing pointer must keep the unpublished generation")
	}
	publishTestGeneration(t, baseDB, "workspace:k", genID)
	if shouldRemoveUnpublished(context.Background(), baseDB, genID) {
		t.Fatal("pointer naming the generation must keep it")
	}
	if !shouldRemoveUnpublished(context.Background(), baseDB, strings.Repeat("b", 32)) {
		t.Fatal("pointer naming another generation must allow removal")
	}
	if err := os.WriteFile(activePointerPath(baseDB), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if shouldRemoveUnpublished(context.Background(), baseDB, strings.Repeat("b", 32)) {
		t.Fatal("unreadable pointer must keep the unpublished generation")
	}
}

func TestResolveActiveGeneration_RejectsLegacyLiveWAL(t *testing.T) {
	baseDB := filepath.Join(t.TempDir(), "k.db")
	seedIndex(t, baseDB, "workspace:k", "ollama/nomic")
	if err := os.WriteFile(baseDB+"-wal", []byte("frame"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveActiveGeneration(context.Background(), baseDB, "workspace:k")
	if err == nil || !strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("error = %v, want live-WAL rejection", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("live-WAL rejection must not classify as no-index")
	}
}

func TestPublishActiveGeneration_InterruptRecoversSame(t *testing.T) {
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
			if _, err := cleanupStaleGenerations(context.Background(), baseDB, oldID, true); err != nil {
				t.Fatal(err)
			}
			if !tc.wantFinal {
				if _, err := os.Stat(filepath.Dir(staging.dbPath)); !os.IsNotExist(err) {
					t.Fatalf("stale staging was not cleaned: %v", err)
				}
			} else if _, err := os.Stat(filepath.Dir(final.dbPath)); !os.IsNotExist(err) {
				t.Fatalf("finalized orphan was not collected by the next lease holder: %v", err)
			}
		})
	}
}
