package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
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

// autoIndexTestEmbedder emits fixed vectors under vsid, failing any batch whose
// input contains failSubstr ("" never fails). The startup probe input contains
// no source code, so content-targeted failures leave the probe passing.
func autoIndexTestEmbedder(vsid, failSubstr string) rag.Embedder {
	return rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		if failSubstr != "" {
			for _, in := range inputs {
				if strings.Contains(in, failSubstr) {
					return rag.EmbedResult{}, errTestEmbedCmd
				}
			}
		}
		vecs := make([][]float64, len(inputs))
		for i := range vecs {
			vecs[i] = []float64{1, 0, 0}
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: vsid}, nil
	})
}

// realReadOnlyOpen is an openRetriever seam that performs a genuine immutable
// read-only open + Stats on dbPath. With immutable=1 the open may SUCCEED
// against a stale pre-checkpoint snapshot even while a writer is live, so the
// ordering invariant is pinned by the source counts the callers assert on
// (a stale snapshot reads 0 sources), not by this open failing.
func realReadOnlyOpen(dbPath string) func(context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error) {
	return func(ctx context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error) {
		store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
		if err != nil {
			return nil, nil, "", vsDecision{}, rag.StoreStats{}, err
		}
		defer func() { _ = store.Close() }()
		stats, err := store.Stats(ctx)
		if err != nil {
			return nil, nil, "", vsDecision{}, rag.StoreStats{}, err
		}
		return &agenttools.Retrieve{}, nil, "", vsDecision{}, stats, nil
	}
}

// retrieveStateOf reads the wrapper state for assertions.
func retrieveStateOf(r *readyRetrieve) retrieveReadyState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// newAutoIndexTestJob wires a runAutoIndex job over root/dbPath with the given
// embedder, a real read-only openRetriever seam, and notice capture.
func newAutoIndexTestJob(root, dbPath string, emb rag.Embedder, notices *[]string) autoIndexJob {
	return autoIndexJob{
		root:        root,
		dbPath:      dbPath,
		sidecarPath: sidecarPath(dbPath),
		workspaceID: "workspace:k",
		embedder:    emb,
		embChain:    []string{"ollama/nomic"},
		ready:       newReadyRetrieve(warmingRetrieveMessage),
		notice:      func(s string) { *notices = append(*notices, s) },

		openRetriever: realReadOnlyOpen(dbPath),
	}
}

func TestRunAutoIndex_FirstRunBuildsAndMarksReady(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	var notices []string
	job := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices)
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveReady {
		t.Fatalf("state = %d, want ready; notices = %v", got, notices)
	}
	want := "retrieve: auto-index ready, 1 sources, ollama/nomic, updated "
	if len(notices) != 1 || !strings.HasPrefix(notices[0], want) {
		t.Fatalf("notices = %v, want one line with prefix %q", notices, want)
	}
	sc, err := readSidecar(sidecarPath(dbPath))
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	if verr := validateSidecar(sc, "workspace:k"); verr != nil {
		t.Fatalf("sidecar invalid: %v", verr)
	}
	assertIndexDBModes(t, dbPath)
}

func TestRunAutoIndex_PartialRunMarksReadyWithWarning(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "ok.go", "package a\n\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "bad.go", "package a\n\nfunc B() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	var notices []string
	job := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", "func B"), &notices)
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveReady {
		t.Fatalf("partial usable run must end ready, state = %d; notices = %v", got, notices)
	}
	want := "warning: retrieve auto-index partial, 1 sources, 1 errors; retrieval enabled"
	if len(notices) != 1 || notices[0] != want {
		t.Fatalf("notices = %v, want exactly [%q]", notices, want)
	}
}

func TestRunAutoIndex_EmbedderDownMarksFailedWithoutTouchingStore(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{}, errTestEmbedCmd
	})
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveFailed {
		t.Fatalf("state = %d, want failed", got)
	}
	if len(notices) != 1 ||
		!strings.HasPrefix(notices[0], "warning: retrieve auto-index failed: ") ||
		!strings.HasSuffix(notices[0], "; using file/search tools") {
		t.Fatalf("notices = %v, want one failed line", notices)
	}
	if fileExists(dbPath) {
		t.Fatal("dead embedder must not create the index DB")
	}
}

func TestRunAutoIndex_InvalidSidecarRebuildsAndEndsReady(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	seedAutoIndexStore(t, dbPath, "ollama/nomic", "workspace:k")
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}

	var notices []string
	job := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices)
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveReady {
		t.Fatalf("self-heal run must end ready, state = %d; notices = %v", got, notices)
	}
	if len(notices) != 2 ||
		!strings.HasPrefix(notices[0], "warning: retrieve auto-index rebuilding private store: ") ||
		!strings.HasPrefix(notices[1], "retrieve: auto-index ready, ") {
		t.Fatalf("notices = %v, want rebuild warning then ready line", notices)
	}
}

func TestRunAutoIndex_WriterClosesBeforeRetrieverAndPrunesDeleted(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	writeWorkspaceFile(t, root, "b.go", "package a\n\nfunc B() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	// First run: realReadOnlyOpen in the seam opens with immutable=1, so it
	// sees only the checkpointed main DB file. The ready-notice source count
	// is the ordering detector: an un-closed writer leaves the rows in the
	// WAL and the immutable snapshot reads 0 sources, not 2.
	var notices1 []string
	job1 := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices1)
	runAutoIndex(context.Background(), job1)
	if got := retrieveStateOf(job1.ready); got != retrieveReady {
		t.Fatalf("first run state = %d, want ready; notices = %v", got, notices1)
	}
	if len(notices1) == 0 || !strings.Contains(notices1[len(notices1)-1], "2 sources") {
		t.Fatalf("first-run ready notice must count both sources (writer closed before open): %v", notices1)
	}

	// Refresh after a deletion: the pruned source must be gone.
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	var notices2 []string
	job2 := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices2)
	runAutoIndex(context.Background(), job2)
	if got := retrieveStateOf(job2.ready); got != retrieveReady {
		t.Fatalf("refresh state = %d, want ready; notices = %v", got, notices2)
	}

	store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open after refresh: %v", err)
	}
	defer func() { _ = store.Close() }()
	sources, err := store.ListSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sawA bool
	for _, s := range sources {
		if strings.Contains(s, "b.go") {
			t.Fatalf("deleted source still indexed: %q", s)
		}
		if strings.Contains(s, "a.go") {
			sawA = true
		}
	}
	if !sawA {
		t.Fatalf("surviving source missing after prune: %v", sources)
	}
}

func TestRunAutoIndex_ContextCanceledStaysSilent(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	// The probe (first call) succeeds; the first indexing embed cancels the
	// run, simulating shutdown mid-index.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	emb := rag.EmbedderFunc(func(c context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		if calls.Add(1) == 1 {
			return rag.EmbedResult{Embeddings: [][]float64{{1, 0, 0}}, Model: "nomic", Provider: "ollama", VectorSpaceID: "ollama/nomic"}, nil
		}
		cancel()
		return rag.EmbedResult{}, context.Canceled
	})

	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	runAutoIndex(ctx, job)

	if got := retrieveStateOf(job.ready); got != retrieveWarming {
		t.Fatalf("cancelled run must stay warming, state = %d", got)
	}
	if len(notices) != 0 {
		t.Fatalf("cancelled run must be silent, notices = %v", notices)
	}
}

func TestClassifyAutoIndex_SidecarWithoutDBIsFull(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "k.db")
	// Torn artifact removal / hand-deleted .db: only the sidecar remains.
	if err := os.WriteFile(sidecarPath(dbPath), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := classifyAutoIndex(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if !got.full {
		t.Fatal("sidecar without a database must select full rebuild (removes the stale marker)")
	}
	if !strings.Contains(got.reason, "sidecar exists without a database") {
		t.Errorf("reason = %q", got.reason)
	}
}

// A refresh that fails BEFORE writing a new marker, while a valid complete
// sidecar from the previous run survives, must serve the previous index but
// say so — not report a plain ready line. The deterministic pre-marker
// failure: the sidecar directory is unwritable, so executeIndex's final
// writeSidecar fails after an otherwise clean incremental run.
func TestRunAutoIndex_FailedRefreshWarnsServingPreviousIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permissions not enforced this way on windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	scDir := t.TempDir()
	scPath := filepath.Join(scDir, "k.json")

	// Seed a valid complete index whose sidecar lives in its own directory.
	store, idx := buildTestIndexer(t, dbPath, "ollama/nomic")
	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer: idx, store: store, root: root, dbPath: dbPath,
		sidecarPath: scPath, workspaceID: "workspace:k",
		requestedModel: "ollama/nomic", out: &out,
	})
	_ = store.Close()
	if res.exitErr != nil {
		t.Fatalf("seed index failed: %v\n%s", res.exitErr, out.String())
	}
	removeSQLiteSidecars(t, dbPath)

	// Make the sidecar directory unwritable so the refresh cannot write a new
	// marker; verify the OS actually enforces it (some filesystems do not).
	if err := os.Chmod(scDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(scDir, 0o700) })
	if probe := os.WriteFile(filepath.Join(scDir, "probe"), nil, 0o600); probe == nil {
		t.Skip("filesystem does not enforce directory write permissions")
	}

	var notices []string
	job := autoIndexJob{
		root:          root,
		dbPath:        dbPath,
		sidecarPath:   scPath,
		workspaceID:   "workspace:k",
		embedder:      autoIndexTestEmbedder("ollama/nomic", ""),
		embChain:      []string{"ollama/nomic"},
		ready:         newReadyRetrieve(warmingRetrieveMessage),
		notice:        func(s string) { notices = append(notices, s) },
		openRetriever: realReadOnlyOpen(dbPath),
	}
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveReady {
		t.Fatalf("state = %d, want ready over the previous index; notices = %v", got, notices)
	}
	var sawStaleWarn bool
	for _, n := range notices {
		if strings.Contains(n, "warning: retrieve auto-index refresh failed:") &&
			strings.Contains(n, "serving previous index") {
			sawStaleWarn = true
		}
	}
	if !sawStaleWarn {
		t.Fatalf("missing serving-previous-index warning; notices = %v", notices)
	}
	last := notices[len(notices)-1]
	if !strings.Contains(last, "retrieve: auto-index ready,") {
		t.Fatalf("final notice should still be the ready line, got %q; all = %v", last, notices)
	}
}

func TestRunAutoIndex_DeletedLastSourceFailsInsteadOfServingStaleMarker(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")

	var notices1 []string
	job1 := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices1)
	runAutoIndex(context.Background(), job1)
	if got := retrieveStateOf(job1.ready); got != retrieveReady {
		t.Fatalf("seed state = %d, want ready; notices = %v", got, notices1)
	}

	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	var notices2 []string
	job2 := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices2)
	runAutoIndex(context.Background(), job2)

	if got := retrieveStateOf(job2.ready); got != retrieveFailed {
		t.Fatalf("empty refresh must fail, state = %d; notices = %v", got, notices2)
	}
	if len(notices2) != 1 ||
		!strings.Contains(notices2[0], "corpus not usable (sources=0") ||
		!strings.Contains(notices2[0], "using file/search tools") {
		t.Fatalf("notices = %v, want unusable-corpus failure", notices2)
	}
}
