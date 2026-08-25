package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

// mutateFixture publishes a workspace generation (via existing helpers) and
// returns baseDB + workspaceID. vsid must match the embedder used by the test.
func mutateFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	baseDB := filepath.Join(dir, "ws.db")
	workspaceID := "workspace:mutseam"
	publishTestGenerationVS(t, baseDB, workspaceID, strings.Repeat("a", 32), "test/space")
	return baseDB, workspaceID
}

func TestMutateModeIngestsIntoStagingCopyAndPublishes(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	origin := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(origin, []byte("mutate seam ingest content"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	var got rag.Document
	built, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath:              baseDB,
		workspaceID:         workspaceID,
		requestedModel:      "test-model",
		actualVectorSpace:   "test/space",
		embedder:            autoIndexTestEmbedder("test/space", ""),
		refuseInvalidActive: true,
		out:                 &out,
		mutate: func(ctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			got, err = managed.IngestFile(ctx, origin, rag.DocumentOptions{})
			return err
		},
	})
	if err != nil {
		t.Fatalf("mutate build: %v\n%s", err, out.String())
	}
	if got.State != rag.DocumentStateIndexed {
		t.Fatalf("want indexed, got %+v", got)
	}
	// Pre-existing workspace sources survive the copy: source count grew by 1.
	if built.generation.metadata.SourceCount < 2 {
		t.Fatalf("staging copy lost workspace sources: %+v", built.generation.metadata)
	}
	if built.generation.metadata.Status != "complete" || built.generation.metadata.ErrorCount != 0 {
		t.Fatalf("managed generation must be complete/0 errors: %+v", built.generation.metadata)
	}
}

func TestMutateModePreservesPartialActiveHealth(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	active, err := resolveActiveGeneration(context.Background(), baseDB, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	active.metadata.Status = "partial"
	active.metadata.ErrorCount = 2
	if err := writeGenerationMetadata(context.Background(), active.metadataPath, active.metadata); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	built, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: nil, refuseInvalidActive: true, out: &out,
		mutate: func(context.Context, *rag.Indexer, *rag.SQLiteStore) error { return nil },
	})
	if err != nil {
		t.Fatalf("mutate partial generation: %v", err)
	}
	if built.sidecar.Status != "partial" || built.sidecar.ErrorCount != 2 ||
		built.generation.metadata.Status != "partial" || built.generation.metadata.ErrorCount != 2 {
		t.Fatalf("managed mutation hid inherited errors: sidecar=%+v metadata=%+v",
			built.sidecar, built.generation.metadata)
	}
	if line := autoGenerationLine(built.generation.metadata, built.stats); !strings.Contains(line, "partial") {
		t.Fatalf("retrieval warning was suppressed: %q", line)
	}
}

func TestMutateModeNilEmbedderSupportsDelete(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	// First: ingest via mutate mode so there is a managed doc to delete.
	origin := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(origin, []byte("delete me later"), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc rag.Document
	var out bytes.Buffer
	built, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: autoIndexTestEmbedder("test/space", ""), refuseInvalidActive: true, out: &out,
		mutate: func(ctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			doc, err = managed.IngestFile(ctx, origin, rag.DocumentOptions{})
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	publishBuilt(t, baseDB, workspaceID, built)

	// Now: delete with a NIL embedder (offline rm path).
	out.Reset()
	built, err = buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: nil, refuseInvalidActive: true, out: &out,
		mutate: func(ctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			if indexer != nil {
				t.Error("nil embedder must yield nil indexer")
			}
			managed, err := rag.NewManagedSources(nil, store)
			if err != nil {
				return err
			}
			return managed.DeleteDocument(ctx, doc.ID)
		},
	})
	if err != nil {
		t.Fatalf("nil-embedder delete build: %v\n%s", err, out.String())
	}
	if built.generation.metadata.SourceCount < 1 {
		t.Fatalf("workspace sources must survive delete: %+v", built.generation.metadata)
	}
}

func TestMutateModeErrorDiscardsStagingAndLeavesActive(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	pointerBefore, err := os.ReadFile(activePointerPath(baseDB))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sentinel := errors.New("mutation exploded")
	_, err = buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: autoIndexTestEmbedder("test/space", ""), refuseInvalidActive: true, out: &out,
		mutate: func(context.Context, *rag.Indexer, *rag.SQLiteStore) error { return sentinel },
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want mutation error surfaced, got %v", err)
	}
	entries, err := os.ReadDir(generationsPath(baseDB))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Fatalf("staging directory leaked: %s", e.Name())
		}
	}
	pointerAfter, err := os.ReadFile(activePointerPath(baseDB))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("active pointer changed on failed mutation")
	}
}

func TestMutateModeRefusesVectorSpaceMismatch(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	var out bytes.Buffer
	_, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "other/space",
		embedder: autoIndexTestEmbedder("other/space", ""), refuseInvalidActive: true, out: &out,
		mutate: func(context.Context, *rag.Indexer, *rag.SQLiteStore) error {
			t.Error("mutate must not run on vector-space mismatch")
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "golem index -full") {
		t.Fatalf("want -full guidance error, got %v", err)
	}
}

func TestWorkspaceRebuildRefusesToDropManagedDocuments(t *testing.T) {
	tests := []struct {
		name              string
		full              bool
		actualVectorSpace string
		removeMetadata    bool
	}{
		{name: "full rebuild", full: true, actualVectorSpace: "test/space"},
		{name: "vector-space change", actualVectorSpace: "other/space"},
		{name: "full rebuild with missing metadata", full: true, actualVectorSpace: "test/space", removeMetadata: true},
		{name: "automatic rebuild with missing metadata", actualVectorSpace: "test/space", removeMetadata: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDB, workspaceID, built := managedRebuildFixture(t)
			if tt.removeMetadata {
				if err := os.Remove(built.generation.metadataPath); err != nil {
					t.Fatal(err)
				}
			}

			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "workspace.go"), []byte("package workspace\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			_, err := buildIndexGeneration(context.Background(), generationBuildOptions{
				root: root, dbPath: baseDB, workspaceID: workspaceID,
				requestedModel: "test-model", actualVectorSpace: tt.actualVectorSpace,
				embedder: autoIndexTestEmbedder(tt.actualVectorSpace, ""),
				full:     tt.full, out: &out,
			})
			if err == nil || !strings.Contains(err.Error(), "managed sources") {
				t.Fatalf("rebuild must refuse to drop managed sources, got %v", err)
			}
		})
	}
}

func TestWorkspaceRebuildFailsClosedWhenManagedStateCannotBeInspected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string, generationBuildResult)
	}{
		{
			name: "malformed pointer",
			mutate: func(t *testing.T, baseDB string, _ generationBuildResult) {
				if err := os.WriteFile(activePointerPath(baseDB), []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "dangling pointer symlink",
			mutate: func(t *testing.T, baseDB string, _ generationBuildResult) {
				if err := os.Remove(activePointerPath(baseDB)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing-active.json", activePointerPath(baseDB)); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		{
			name: "unreadable database",
			mutate: func(t *testing.T, _ string, built generationBuildResult) {
				if err := os.WriteFile(built.generation.dbPath, []byte("not sqlite"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unstatable database",
			mutate: func(t *testing.T, _ string, built generationBuildResult) {
				if err := os.Remove(built.generation.dbPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(built.generation.dbPath), built.generation.dbPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
		{
			name: "dangling database symlink",
			mutate: func(t *testing.T, _ string, built generationBuildResult) {
				if err := os.Remove(built.generation.dbPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("missing.db", built.generation.dbPath); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseDB, workspaceID, built := managedRebuildFixture(t)
			tt.mutate(t, baseDB, built)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "workspace.go"), []byte("package workspace\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			_, err := buildIndexGeneration(context.Background(), generationBuildOptions{
				root: root, dbPath: baseDB, workspaceID: workspaceID,
				requestedModel: "test-model", actualVectorSpace: "test/space",
				embedder: autoIndexTestEmbedder("test/space", ""), full: true, out: &out,
			})
			if err == nil || !strings.Contains(err.Error(), "cannot verify damaged index") {
				t.Fatalf("rebuild inspection error = %v", err)
			}
		})
	}
}

func managedRebuildFixture(t *testing.T) (string, string, generationBuildResult) {
	t.Helper()
	baseDB, workspaceID := mutateFixture(t)
	var out bytes.Buffer
	built, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: autoIndexTestEmbedder("test/space", ""), out: &out,
		mutate: func(ctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			_, err = managed.IngestText(ctx, "retained.txt", "irreplaceable stdin content", rag.DocumentOptions{})
			return err
		},
	})
	if err != nil {
		t.Fatalf("seed managed source: %v", err)
	}
	publishBuilt(t, baseDB, workspaceID, built)
	return baseDB, workspaceID, built
}

func TestWorkspaceRebuildAllowsGenerationWithoutManagedTable(t *testing.T) {
	baseDB, workspaceID := mutateFixture(t)
	active, err := resolveActiveGeneration(context.Background(), baseDB, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.NewSQLiteStore(active.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`DROP TABLE managed_documents`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "workspace.go"), []byte("package workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err = buildIndexGeneration(context.Background(), generationBuildOptions{
		root: root, dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: autoIndexTestEmbedder("test/space", ""), full: true, out: &out,
	})
	if err != nil {
		t.Fatalf("pre-managed generation should remain rebuildable: %v", err)
	}
}

func TestMutateModeDeleteToEmptyReturnsRetirementSignal(t *testing.T) {
	dir := t.TempDir()
	baseDB := filepath.Join(dir, "ws.db")
	workspaceID := "workspace:mutempty"
	origin := filepath.Join(dir, "only.txt")
	if err := os.WriteFile(origin, []byte("the only source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc rag.Document
	var out bytes.Buffer
	built, err := buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: autoIndexTestEmbedder("test/space", ""), refuseInvalidActive: true, out: &out,
		mutate: func(ctx context.Context, indexer *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(indexer, store)
			if err != nil {
				return err
			}
			doc, err = managed.IngestFile(ctx, origin, rag.DocumentOptions{})
			return err
		},
	})
	if err != nil {
		t.Fatalf("no-index add build: %v\n%s", err, out.String())
	}
	publishBuilt(t, baseDB, workspaceID, built)

	out.Reset()
	_, err = buildIndexGeneration(context.Background(), generationBuildOptions{
		dbPath: baseDB, workspaceID: workspaceID,
		requestedModel: "test-model", actualVectorSpace: "test/space",
		embedder: nil, refuseInvalidActive: true, out: &out,
		mutate: func(ctx context.Context, _ *rag.Indexer, store *rag.SQLiteStore) error {
			managed, err := rag.NewManagedSources(nil, store)
			if err != nil {
				return err
			}
			return managed.DeleteDocument(ctx, doc.ID)
		},
	})
	if !errors.Is(err, errEmptyStagingGeneration) {
		t.Fatalf("delete last source build error = %v, want retirement signal", err)
	}
	active, err := resolveActiveGeneration(context.Background(), baseDB, workspaceID)
	if err != nil || active.id != built.generation.id {
		t.Fatalf("active generation changed after unpublishable staging: %+v, %v", active, err)
	}
	entries, err := os.ReadDir(generationsPath(baseDB))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") {
			t.Fatalf("empty mutation leaked staging directory %q", entry.Name())
		}
	}
}

// publishBuilt publishes a built generation's pointer (test helper).
func publishBuilt(t *testing.T, baseDB, workspaceID string, built generationBuildResult) {
	t.Helper()
	pointer := activeGenerationPointer{SchemaVersion: activePointerSchemaVersion, WorkspaceID: workspaceID, Generation: built.generation.id}
	if err := publishActiveGeneration(context.Background(), baseDB, pointer, nil); err != nil {
		t.Fatal(err)
	}
}

func publishTestGenerationVS(t *testing.T, baseDB, workspaceID, generation, vsid string) indexGeneration {
	t.Helper()
	gen := seedPublishedGeneration(t, baseDB, generation, workspaceID, vsid)
	if err := publishActiveGeneration(context.Background(), baseDB, activeGenerationPointer{
		SchemaVersion: activePointerSchemaVersion,
		WorkspaceID:   workspaceID,
		Generation:    generation,
	}, nil); err != nil {
		t.Fatal(err)
	}
	return gen
}
