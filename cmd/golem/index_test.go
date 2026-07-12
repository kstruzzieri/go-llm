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

var errTestEmbedCmd = errors.New("cmd test embed failure")

// buildTestIndexer wires a fake embedder (fixed vsid) to a fresh on-disk store.
func buildTestIndexer(t *testing.T, dbPath, vsid string) (*rag.SQLiteStore, *rag.Indexer) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	emb := rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: vsid}, nil
	})
	idx, err := rag.NewIndexerWithEmbedder(emb, store, rag.WithEmbeddingModel(vsid))
	if err != nil {
		t.Fatal(err)
	}
	return store, idx
}

func writeWorkspaceFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func removeSQLiteSidecars(t *testing.T, dbPath string) {
	t.Helper()
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func assertNoSQLiteSidecars(t *testing.T, dbPath string) {
	t.Helper()
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist, stat err=%v", p, err)
		}
	}
}

func assertIndexDBModes(t *testing.T, dbPath string) {
	t.Helper()
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("db mode = %v, want 0600", info.Mode().Perm())
	}
	for _, p := range []string{dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", p, info.Mode().Perm())
		}
	}
}

func TestExecuteIndex_HappyPath(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "doc.md", "# Doc\n\nbody\n")

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	store, idx := buildTestIndexer(t, dbPath, "ollama/nomic")
	defer func() { _ = store.Close() }()

	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer:        idx,
		store:          store,
		root:           root,
		dbPath:         dbPath,
		sidecarPath:    sidecarPath(dbPath),
		workspaceID:    "workspace:k",
		requestedModel: "ollama/nomic",
		out:            &out,
	})
	if res.exitErr != nil {
		t.Fatalf("happy path exitErr = %v", res.exitErr)
	}
	sc, err := readSidecar(sidecarPath(dbPath))
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	if sc.Status != "complete" || sc.ErrorCount != 0 {
		t.Errorf("sidecar status/errorCount = %q/%d, want complete/0", sc.Status, sc.ErrorCount)
	}
	if sc.VectorSpaceID != "ollama/nomic" {
		t.Errorf("sidecar vsid = %q, want ollama/nomic (from probe)", sc.VectorSpaceID)
	}
	if sc.WorkspaceID != "workspace:k" {
		t.Errorf("sidecar workspaceID = %q", sc.WorkspaceID)
	}
	assertIndexDBModes(t, dbPath)
	if !strings.Contains(out.String(), "sources") {
		t.Errorf("summary missing source count: %q", out.String())
	}
}

func TestExecuteIndex_PartialExitsNonZeroButWritesSidecar(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "ok.go", "package a\n\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "bad.go", "package a\n\nfunc B() {}\n")

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	emb := rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		for _, in := range inputs {
			if strings.Contains(in, "func B") {
				return rag.EmbedResult{}, errTestEmbedCmd
			}
		}
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: "ollama/nomic"}, nil
	})
	idx, err := rag.NewIndexerWithEmbedder(emb, store, rag.WithEmbeddingModel("ollama/nomic"))
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k",
		requestedModel: "ollama/nomic", out: &out,
	})
	if res.exitErr == nil {
		t.Fatal("partial run must return a non-nil exitErr")
	}
	sc, err := readSidecar(sidecarPath(dbPath))
	if err != nil {
		t.Fatalf("partial run must still write a sidecar: %v", err)
	}
	if sc.Status != "partial" || sc.ErrorCount != 1 {
		t.Errorf("sidecar = %q/%d, want partial/1", sc.Status, sc.ErrorCount)
	}
}

func TestExecuteIndex_EmptyCorpusNoSidecar(t *testing.T) {
	root := t.TempDir() // no indexable files
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	store, idx := buildTestIndexer(t, dbPath, "ollama/nomic")
	defer func() { _ = store.Close() }()

	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k",
		requestedModel: "ollama/nomic", out: &out,
	})
	if res.exitErr == nil {
		t.Fatal("empty corpus should be a non-zero outcome")
	}
	if _, err := os.Stat(sidecarPath(dbPath)); !os.IsNotExist(err) {
		t.Errorf("empty corpus must not write a sidecar (stat err=%v)", err)
	}
}

func TestRun_DispatchUnknownCommand(t *testing.T) {
	// A non-flag, non-"index" positional arg => unknown command error.
	err := run([]string{"frobnicate"}, os.Stdin, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("want unknown-command error, got %v", err)
	}
}
