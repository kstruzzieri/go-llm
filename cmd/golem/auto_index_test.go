package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestProbeAutoIndexEmbedder_CallsWithOneInputAndModel(t *testing.T) {
	var gotModel string
	var gotInputs []string
	var hadDeadline bool
	emb := rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		gotModel = model
		gotInputs = inputs
		_, hadDeadline = ctx.Deadline()
		return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
	})

	if err := probeAutoIndexEmbedder(context.Background(), emb, "ollama/nomic"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotModel != "ollama/nomic" {
		t.Errorf("model = %q, want ollama/nomic", gotModel)
	}
	if len(gotInputs) != 1 || gotInputs[0] != "golem startup index probe" {
		t.Fatalf(`inputs = %v, want exactly ["golem startup index probe"]`, gotInputs)
	}
	if !hadDeadline {
		t.Error("probe context must carry a deadline")
	}
}

func TestProbeAutoIndexEmbedder_EmbedErrorIsOrdinaryError(t *testing.T) {
	emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{}, errTestEmbedCmd
	})
	err := probeAutoIndexEmbedder(context.Background(), emb, "m")
	if err == nil {
		t.Fatal("embed failure must fail the probe")
	}
	if !strings.Contains(err.Error(), errTestEmbedCmd.Error()) {
		t.Errorf("error should carry the cause: %v", err)
	}
}

func TestProbeAutoIndexEmbedder_WrongVectorCount(t *testing.T) {
	for name, vecs := range map[string][][]float64{
		"zero":      {},
		"two":       {{1}, {2}},
		"one-empty": {{}},
	} {
		t.Run(name, func(t *testing.T) {
			emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
				return rag.EmbedResult{Embeddings: vecs}, nil
			})
			if err := probeAutoIndexEmbedder(context.Background(), emb, "m"); err == nil {
				t.Fatal("probe must require exactly one vector")
			}
		})
	}
}

// seedAutoIndexStore builds and indexes a small workspace at dbPath with the
// given vsid, writing the sidecar via executeIndex, then closes the store and
// strips WAL/SHM so read-only classification can open it.
func seedAutoIndexStore(t *testing.T, dbPath, vsid, workspaceID string) {
	t.Helper()
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	store, idx := buildTestIndexer(t, dbPath, vsid)
	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: sidecarPath(dbPath), workspaceID: workspaceID,
		requestedModel: vsid, out: &out,
	})
	_ = store.Close()
	if res.exitErr != nil {
		t.Fatalf("seed index failed: %v\n%s", res.exitErr, out.String())
	}
	removeSQLiteSidecars(t, dbPath)
}

func TestClassifyAutoIndex_AbsentDBIsIncremental(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if got.full {
		t.Fatalf("absent DB must be incremental, got full (reason %q)", got.reason)
	}
}

func TestClassifyAutoIndex_MissingSidecarIsFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/nomic", "workspace:k")
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if !got.full {
		t.Fatal("existing DB without sidecar must select full rebuild")
	}
	if got.reason == "" {
		t.Error("full rebuild must carry a reason for the notice")
	}
}

func TestClassifyAutoIndex_CorruptSidecarIsFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/nomic", "workspace:k")
	if err := os.WriteFile(sidecarPath(dbPath), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if !got.full || got.reason == "" {
		t.Fatalf("corrupt sidecar must select full rebuild with reason, got %+v", got)
	}
}

func TestClassifyAutoIndex_WrongWorkspaceSidecarIsFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/nomic", "workspace:other")

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if !got.full || got.reason == "" {
		t.Fatalf("wrong-workspace sidecar must select full rebuild with reason, got %+v", got)
	}
}

func TestClassifyAutoIndex_VectorSpaceMismatchIsFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/OLD", "workspace:k")

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/NEW"})
	if !got.full {
		t.Fatal("vector-space mismatch must select full rebuild")
	}
	if !strings.Contains(got.reason, "ollama/OLD") {
		t.Errorf("reason should name the stored vector space: %q", got.reason)
	}
}

func TestClassifyAutoIndex_CompatibleStoreIsIncremental(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/nomic", "workspace:k")

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if got.full {
		t.Fatalf("compatible store must be incremental, got full (reason %q)", got.reason)
	}
	// Read-only classification must not have created WAL/SHM.
	assertNoSQLiteSidecars(t, dbPath)
}
