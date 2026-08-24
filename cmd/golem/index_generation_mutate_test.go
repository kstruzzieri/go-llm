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

func TestMutateModeDeleteToEmptyRefused(t *testing.T) {
	// Fresh index containing ONLY one managed doc; deleting it must refuse to
	// publish an empty generation.
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
	if err == nil {
		t.Fatal("deleting the last source must refuse to publish")
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
