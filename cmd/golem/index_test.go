package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

var errTestEmbedCmd = errors.New("cmd test embed failure")

func realisticTestVector() []float64 {
	vector := make([]float64, 768)
	vector[0] = 1
	return vector
}

// buildTestIndexer wires a fake embedder (fixed vsid) to a fresh on-disk store.
func buildTestIndexer(t *testing.T, dbPath, vsid string, opts ...rag.IndexerOption) (*rag.SQLiteStore, *rag.Indexer) {
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
			vecs[i] = realisticTestVector()
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: vsid}, nil
	})
	idx, err := rag.NewIndexerWithEmbedder(emb, store, append(opts, rag.WithEmbeddingModel(vsid))...)
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

func rewriteEmbeddingsAsLegacyJSON(t *testing.T, dbPath string) {
	t.Helper()
	store, err := rag.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(realisticTestVector())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE chunks SET embedding = ?`, string(encoded)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	removeSQLiteSidecars(t, dbPath)
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
			vecs[i] = realisticTestVector()
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

func indexPolicyCandidate() string { return "ghp_" + strings.Repeat("aB3d", 6) }

func TestExecuteIndex_PolicyOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		mixed, redact, unsafe   bool
		wantSources, wantErrors int
		wantStatus              string
	}{
		{name: "sole skip"},
		{name: "mixed skip", mixed: true, wantSources: 1, wantErrors: 1, wantStatus: "partial"},
		{name: "sole redact", redact: true, wantSources: 1, wantStatus: "complete"},
		{name: "mixed redact", mixed: true, redact: true, wantSources: 2, wantStatus: "complete"},
		{name: "unsafe cleanup", mixed: true, unsafe: true, wantSources: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, dbPath := t.TempDir(), filepath.Join(t.TempDir(), "k.db")
			var opts []rag.IndexerOption
			if tc.redact {
				opts = append(opts, rag.WithSensitiveRedaction(rag.SensitiveGitHubToken))
			}
			store, idx := buildTestIndexer(t, dbPath, "ollama/nomic", opts...)
			t.Cleanup(func() { _ = store.Close() })
			path := filepath.Join(root, "sensitive.md")
			writeWorkspaceFile(t, root, "sensitive.md", "old source")
			if err := idx.IndexFile(context.Background(), path); err != nil {
				t.Fatal("seed failed")
			}
			if tc.unsafe {
				if _, err := store.DB().Exec(`CREATE TRIGGER deny_clear BEFORE DELETE ON chunks BEGIN SELECT RAISE(FAIL, 'clear denied'); END`); err != nil {
					t.Fatal(err)
				}
			}
			writeWorkspaceFile(t, root, "sensitive.md", "credential: "+indexPolicyCandidate()+"\n")
			if tc.mixed {
				writeWorkspaceFile(t, root, "safe.md", "safe source")
			}
			var out bytes.Buffer
			res := executeIndex(context.Background(), indexJob{indexer: idx, store: store, root: root, dbPath: dbPath,
				sidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k", requestedModel: "ollama/nomic", out: &out})
			if strings.Contains(out.String(), indexPolicyCandidate()) {
				t.Fatal("diagnostic exposed candidate")
			}
			if (res.exitErr == nil) != tc.redact {
				t.Error("exit status does not reflect policy outcome")
			}
			if !tc.redact {
				var policy *rag.IndexPolicyError
				if !errors.As(res.exitErr, &policy) || policy.Outcome.Unsafe != tc.unsafe {
					t.Error("typed policy cause missing")
				}
			}
			stats, err := store.Stats(context.Background())
			if err != nil || stats.TotalSources != tc.wantSources {
				t.Errorf("sources = %d, want %d", stats.TotalSources, tc.wantSources)
			}
			sc, err := readSidecar(sidecarPath(dbPath))
			if tc.wantStatus == "" {
				if !os.IsNotExist(err) {
					t.Error("unusable generation wrote a sidecar")
				}
			} else if err != nil || sc.Status != tc.wantStatus || sc.ErrorCount != tc.wantErrors {
				t.Errorf("sidecar status/count = %q/%d, want %q/%d", sc.Status, sc.ErrorCount, tc.wantStatus, tc.wantErrors)
			}
			if !strings.Contains(out.String(), "index policy:") {
				t.Error("policy counts missing")
			}
		})
	}
}

func TestExecuteIndex_OrdinaryErrorSensitiveFilename(t *testing.T) {
	root, dbPath := t.TempDir(), filepath.Join(t.TempDir(), "k.db")
	store, _ := buildTestIndexer(t, dbPath, "ollama/nomic")
	t.Cleanup(func() { _ = store.Close() })
	name := indexPolicyCandidate() + ".md"
	writeWorkspaceFile(t, root, name, "ordinary content")
	writeWorkspaceFile(t, root, "safe.md", "safe source")
	cause := errors.New("provider failure for " + filepath.Join(root, name))
	emb := rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		if strings.Contains(inputs[0], "ordinary") {
			return rag.EmbedResult{}, cause
		}
		return autoIndexTestEmbedder("ollama/nomic", "").Embed(ctx, model, inputs)
	})
	idx, err := rag.NewIndexerWithEmbedder(emb, store)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k", out: &out})
	if strings.Contains(out.String(), indexPolicyCandidate()) {
		t.Error("ordinary failure diagnostic exposed filename candidate")
	}
	if !errors.Is(res.exitErr, cause) {
		t.Error("ordinary internal cause lost")
	}
}

type indexOutputHook struct {
	bytes.Buffer
	hook func(string)
}

func (w *indexOutputHook) Write(p []byte) (int, error) {
	if w.hook != nil {
		w.hook(string(p))
	}
	return w.Buffer.Write(p)
}

func TestRunIndex_PolicyPublication(t *testing.T) {
	for _, mode := range []string{"partial", "empty", "publication"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			configPath, _ := subcommandHarness(t, "local")
			root := t.TempDir()
			root, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, root, "secret.md", "previous content")
			if mode != "empty" {
				writeWorkspaceFile(t, root, "safe.md", "safe content")
			}
			args := []string{"-config", configPath, "-root", root}
			var out, errOut bytes.Buffer
			if err := runIndex(context.Background(), args, &out, &errOut); err != nil {
				t.Fatal("seed manual index failed")
			}
			dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, root)
			if err != nil {
				t.Fatal(err)
			}
			before, err := readActivePointer(context.Background(), dbPath)
			if err != nil {
				t.Fatal(err)
			}
			writeWorkspaceFile(t, root, "secret.md", "credential: "+indexPolicyCandidate())
			var output indexOutputHook
			if mode == "publication" {
				output.hook = func(text string) {
					if strings.HasPrefix(text, "indexed ") {
						if err := os.Mkdir(activePointerPath(dbPath)+".tmp", 0o700); err != nil {
							t.Error("install publication fault failed")
						}
					}
				}
			}
			errOut.Reset()
			err = runIndex(context.Background(), args, &output, &errOut)
			if !errors.Is(err, errIndexFailed) {
				t.Error("manual policy result lost rendered nonzero sentinel")
			}
			var policy *rag.IndexPolicyError
			if !errors.As(err, &policy) {
				t.Error("manual policy result lost typed cause")
			}
			if strings.Contains(output.String()+errOut.String(), indexPolicyCandidate()) {
				t.Fatal("manual diagnostic exposed candidate")
			}
			pointer, readErr := readActivePointer(context.Background(), dbPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch mode {
			case "empty":
				if !pointer.Retired {
					t.Error("sole skip retained active pointer")
				}
			case "publication":
				if pointer != before {
					t.Error("failed publication changed durable pointer")
				}
				if !strings.Contains(output.String()+errOut.String(), "retirement could not be persisted") {
					t.Error("manual durability failure not surfaced")
				}
				var pathErr *os.PathError
				if !errors.As(err, &pathErr) {
					t.Error("joined retirement I/O cause lost")
				}
			case "partial":
				gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
				if err != nil || gen.id == before.Generation || gen.metadata.Status != "partial" || gen.metadata.ErrorCount != 1 || gen.metadata.SourceCount != 1 {
					t.Error("manual safe partial generation was not published with counts")
				}
			}
		})
	}
}

func TestExecuteIndex_RedactionStateSurvivesEarlyReturns(t *testing.T) {
	for _, mode := range []string{"unchanged", "stats", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, dbPath := t.TempDir(), filepath.Join(t.TempDir(), "k.db")
			store, idx := buildTestIndexer(t, dbPath, "ollama/nomic", rag.WithSensitiveRedaction(rag.SensitiveGitHubToken))
			t.Cleanup(func() { _ = store.Close() })
			writeWorkspaceFile(t, root, "secret.md", "credential: "+indexPolicyCandidate())
			if err := idx.IndexFile(context.Background(), filepath.Join(root, "secret.md")); err != nil {
				t.Fatal("seed redaction failed")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out indexOutputHook
			out.hook = func(text string) {
				if !strings.HasPrefix(text, "index policy:") {
					return
				}
				if mode == "stats" {
					_ = store.Close()
				}
				if mode == "cancel" {
					cancel()
				}
			}
			res := executeIndex(ctx, indexJob{indexer: idx, store: store, root: root, dbPath: dbPath,
				sidecarPath: sidecarPath(dbPath), workspaceID: "workspace:k", requestedModel: "ollama/nomic", out: &out})
			if !res.policyAffected || res.policyUnsafe {
				t.Error("nil-error redaction lost policy state before early return")
			}
			if (res.exitErr == nil) != (mode == "unchanged") {
				t.Error("redaction exit state incorrect")
			}
			if mode == "cancel" && !errors.Is(res.exitErr, context.Canceled) {
				t.Error("cancellation cause lost")
			}
			if !strings.Contains(out.String(), "1 redacted (1 indexed files)") {
				t.Error("unchanged sanitized file not counted as indexed/redacted")
			}
			if strings.Contains(out.String(), indexPolicyCandidate()) {
				t.Error("redaction diagnostic exposed candidate")
			}
		})
	}
}
