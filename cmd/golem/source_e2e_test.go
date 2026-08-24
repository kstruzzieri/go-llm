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

// TestSourceE2ERmRemovesChunks: raw SQL proof that delete removed the chunks
// and the workspace survives.
func TestSourceE2ERmRemovesChunks(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	keep := filepath.Join(root, "keep.md")
	gone := filepath.Join(root, "gone.md")
	for _, p := range []string{keep, gone} {
		if err := os.WriteFile(p, []byte("content "+p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = addTestDoc(t, deps, root, keep)
	id := addTestDoc(t, deps, root, gone)
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv}); err != nil {
		t.Fatalf("rm: %v\n%s%s", err, out.String(), errOut.String())
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
	defer func() { _ = store.Close() }()
	var orphanChunks int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM chunks WHERE json_extract(metadata, '$.managed_document_id') = ?`, id,
	).Scan(&orphanChunks); err != nil {
		t.Fatal(err)
	}
	if orphanChunks != 0 {
		t.Fatalf("deleted document left %d chunks", orphanChunks)
	}
	var total int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total < 1 {
		t.Fatal("surviving document lost its chunks")
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

// TestSourceE2ERmLastSourceRefused: acceptance edge from the spec (D11).
func TestSourceE2ERmLastSourceRefused(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	only := filepath.Join(root, "only.md")
	if err := os.WriteFile(only, []byte("solitary"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addTestDoc(t, deps, root, only)
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	pointerBefore, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err = runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv})
	if !errors.Is(err, errSourceFailed) {
		t.Fatalf("want refusal, got %v", err)
	}
	if !strings.Contains(errOut.String(), "keep at least one source") {
		t.Fatalf("refusal is not actionable: %q", errOut.String())
	}
	pointerAfter, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("last-source refusal changed the active pointer")
	}
	gen, resolveErr := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if resolveErr != nil {
		t.Fatalf("active generation must survive refusal: %v", resolveErr)
	}
	if gen.metadata.SourceCount != 1 {
		t.Fatalf("refused rm still changed the generation: %+v", gen.metadata)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	managed, err := rag.NewManagedSources(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 1 || documents[0].ID != id || documents[0].ChunkCount < 1 {
		t.Fatalf("last-source refusal lost the surviving document/chunks: %#v", documents)
	}
}
