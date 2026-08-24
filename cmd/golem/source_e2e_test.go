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

// TestSourceE2EAddRetrievableNextSession: ingest, then simulate a NEW session
// by resolving the active pointer fresh and retrieving through the standard
// rag retriever.
func TestSourceE2EAddRetrievableNextSession(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "handbook.md")
	if err := os.WriteFile(doc, []byte("the golem handbook says: feed it clay"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""), &out, &errOut, deps); err != nil {
		t.Fatalf("add: %v\n%s%s", err, out.String(), errOut.String())
	}

	// "Next session": fresh pointer resolve + fresh read-only open.
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	retriever, err := rag.NewRetrieverWithEmbedder(deps.embedder, store, rag.WithRetrieverModel("test-model"))
	if err != nil {
		t.Fatal(err)
	}
	results, err := retriever.Retrieve(context.Background(), "feed it clay", 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	found := false
	for _, r := range results {
		if strings.Contains(r.Chunk.Content, "feed it clay") {
			found = true
		}
	}
	if !found {
		t.Fatalf("ingested content not retrievable next session: %+v", results)
	}
}

// TestSourceE2ERmRemovesChunks: deleting the sole managed source retires the
// active generation, list reports empty, and a subsequent add starts fresh.
func TestSourceE2ERmRemovesChunks(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	gone := filepath.Join(root, "gone.md")
	if err := os.WriteFile(gone, []byte("content "+gone), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addTestDoc(t, deps, root, gone)
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	// A retired pointer remains authoritative even when a pre-generation
	// legacy pair is still on disk.
	seedIndex(t, dbPath, workspaceID, "test/space")
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv}); err != nil {
		t.Fatalf("rm: %v\n%s%s", err, out.String(), errOut.String())
	}
	if _, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID); !errors.Is(err, errNoActiveGeneration) {
		t.Fatalf("resolve after deleting sole source = %v, want no active generation", err)
	}
	out.Reset()
	errOut.Reset()
	if err := runSourceWith(context.Background(), []string{"list", "-root", root, "-json"}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv}); err != nil {
		t.Fatalf("list after rm: %v\n%s%s", err, out.String(), errOut.String())
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("list after deleting sole source = %q, want []", out.String())
	}

	replacement := filepath.Join(root, "replacement.md")
	if err := os.WriteFile(replacement, []byte("replacement content"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = addTestDoc(t, deps, root, replacement)
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatalf("resolve after replacement add: %v", err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	var deletedChunks int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM chunks WHERE json_extract(metadata, '$.managed_document_id') = ?`, id,
	).Scan(&deletedChunks); err != nil {
		t.Fatal(err)
	}
	if deletedChunks != 0 {
		t.Fatalf("replacement generation resurrected %d deleted chunks", deletedChunks)
	}
}

func TestSourceE2ERmPreservesZeroChunkManagedSources(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	first := filepath.Join(root, "first.md")
	zero := filepath.Join(root, "zero.md")
	if err := os.WriteFile(first, []byte("first content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zero, []byte("content that becomes empty"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstID := addTestDoc(t, deps, root, first)
	zeroID := addTestDoc(t, deps, root, zero)
	if err := os.WriteFile(zero, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"reindex", "-root", root, zeroID}, strings.NewReader(""), &out, &errOut, deps); err != nil {
		t.Fatalf("reindex empty file: %v\n%s%s", err, out.String(), errOut.String())
	}

	out.Reset()
	errOut.Reset()
	err := runSourceWith(context.Background(), []string{"rm", "-root", root, firstID}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv})
	if !errors.Is(err, errSourceFailed) || !strings.Contains(errOut.String(), "zero-chunk managed sources remain") {
		t.Fatalf("rm chunk-bearing source error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}

	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := rag.NewManagedSources(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	closeErr := store.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	chunks := map[string]int{}
	for _, doc := range docs {
		chunks[doc.ID] = doc.ChunkCount
	}
	if len(docs) != 2 || chunks[firstID] < 1 || chunks[zeroID] != 0 {
		t.Fatalf("documents after refused rm = %#v", docs)
	}

	for _, id := range []string{zeroID, firstID} {
		out.Reset()
		errOut.Reset()
		if err := runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv}); err != nil {
			t.Fatalf("rm %s: %v\n%s%s", id, err, out.String(), errOut.String())
		}
	}
	if _, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID); !errors.Is(err, errNoActiveGeneration) {
		t.Fatalf("resolve after ordered removals = %v, want no active generation", err)
	}
}

// TestSourceE2ECancelMidIngestLeavesActiveIntact: cancellation during embed
// discards staging; pointer and active generation unchanged.
func TestSourceE2ECancelMidIngestLeavesActiveIntact(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	first := filepath.Join(root, "first.md")
	if err := os.WriteFile(first, []byte("seed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDeps := sourceDeps{getenv: getenv, embedder: autoIndexTestEmbedder("test/space", ""), embChain: []string{"test-model"}}
	_ = addTestDoc(t, seedDeps, root, first)

	_, dbPath, _, err := sourceWorkspace(root, getenv)
	if err != nil {
		t.Fatal(err)
	}
	pointerBefore, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelling := rag.EmbedderFunc(func(ectx context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		for _, in := range inputs {
			if strings.Contains(in, "victim content") {
				cancel()
				<-ectx.Done()
				return rag.EmbedResult{}, ectx.Err()
			}
		}
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = realisticTestVector()
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: "test/space"}, nil
	})
	victim := filepath.Join(root, "victim.md")
	if err := os.WriteFile(victim, []byte("victim content"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err = runSourceWith(ctx, []string{"add", "-root", root, victim}, strings.NewReader(""), &out, &errOut,
		sourceDeps{getenv: getenv, embedder: cancelling, embChain: []string{"test-model"}})
	if err == nil {
		t.Fatal("cancelled add must fail")
	}
	pointerAfter, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("cancelled add changed the active pointer")
	}
	entries, err := os.ReadDir(generationsPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".staging-") {
			t.Fatalf("cancelled add leaked staging dir %s", e.Name())
		}
	}
}

// TestSourceE2EVectorSpaceMismatchRefused: a different embedding route must
// hard-error with -full guidance, never publish, never lose sources.
func TestSourceE2EVectorSpaceMismatchRefused(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	first := filepath.Join(root, "first.md")
	if err := os.WriteFile(first, []byte("seed content"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedDeps := sourceDeps{getenv: getenv, embedder: autoIndexTestEmbedder("space/alpha", ""), embChain: []string{"test-model"}}
	_ = addTestDoc(t, seedDeps, root, first)

	other := filepath.Join(root, "other.md")
	if err := os.WriteFile(other, []byte("other content"), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatchDeps := sourceDeps{getenv: getenv, embedder: autoIndexTestEmbedder("space/beta", ""), embChain: []string{"test-model"}}
	var out, errOut bytes.Buffer
	err := runSourceWith(context.Background(), []string{"add", "-root", root, other}, strings.NewReader(""), &out, &errOut, mismatchDeps)
	if !errors.Is(err, errSourceFailed) {
		t.Fatalf("want rendered failure, got %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "golem index -full") {
		t.Fatalf("want -full guidance, got %q %q", out.String(), errOut.String())
	}
	// Active generation still serves the seed content.
	_, dbPath, workspaceID, err := sourceWorkspace(root, getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if gen.metadata.SourceCount != 1 || gen.metadata.VectorSpaceID != "space/alpha" {
		t.Fatalf("mismatch attempt disturbed the active generation: %+v", gen.metadata)
	}
}
