package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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
		return rag.EmbedResult{Embeddings: [][]float64{realisticTestVector()}, VectorSpaceID: "ollama/nomic"}, nil
	})

	actual, err := probeAutoIndexEmbedder(context.Background(), emb, "ollama/nomic")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if actual != "ollama/nomic" {
		t.Fatalf("actual vector space = %q", actual)
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
	_, err := probeAutoIndexEmbedder(context.Background(), emb, "m")
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
			if _, err := probeAutoIndexEmbedder(context.Background(), emb, "m"); err == nil {
				t.Fatal("probe must require exactly one vector")
			}
		})
	}
}

func TestProbeAutoIndexEmbedder_RejectsMissingVectorSpaceIdentity(t *testing.T) {
	emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{Embeddings: [][]float64{realisticTestVector()}}, nil
	})
	if _, err := probeAutoIndexEmbedder(context.Background(), emb, "m"); err == nil || !strings.Contains(err.Error(), "vector-space identity") {
		t.Fatalf("error = %v, want missing vector-space identity", err)
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
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if res.exitErr != nil {
		t.Fatalf("seed index failed: %v\n%s", res.exitErr, out.String())
	}
	removeSQLiteSidecars(t, dbPath)
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
			vecs[i] = realisticTestVector()
		}
		return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: vsid}, nil
	})
}

// realReadOnlyOpen is an openRetriever seam that performs a genuine immutable
// read-only open + Stats on dbPath. With immutable=1 the open may SUCCEED
// against a stale pre-checkpoint snapshot even while a writer is live, so the
// ordering invariant is pinned by the source counts the callers assert on
// (a stale snapshot reads 0 sources), not by this open failing.
func realReadOnlyOpen(_ string) func(context.Context, string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
	return func(ctx context.Context, dbPath string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
		store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
		if err != nil {
			return nil, "", vsDecision{}, rag.StoreStats{}, err
		}
		stats, err := store.Stats(ctx)
		if err != nil {
			return nil, "", vsDecision{}, rag.StoreStats{}, errors.Join(err, store.Close())
		}
		return newOwnedRetrievalReader(&agenttools.Retrieve{}, store, nil), "", vsDecision{}, stats, nil
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
		workspaceID: "workspace:k",
		embedder:    emb,
		embChain:    []string{"ollama/nomic"},
		ready:       newReadyRetrieve(warmingRetrieveMessage),
		notice:      func(s string) { *notices = append(*notices, s) },

		openRetriever: realReadOnlyOpen(dbPath),
	}
}

func installActiveTestReader(t *testing.T, ready *readyRetrieve, dbPath string) indexGeneration {
	t.Helper()
	gen, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	reader, _, _, _, err := realReadOnlyOpen(dbPath)(context.Background(), gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.install(reader, "active") {
		t.Fatal("install active reader")
	}
	return gen
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
	gen, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatalf("active generation not published: %v", err)
	}
	if gen.metadata.VectorSpaceID != "ollama/nomic" || gen.metadata.SourceCount != 1 {
		t.Fatalf("generation metadata = %+v", gen.metadata)
	}
	assertIndexDBModes(t, gen.dbPath)
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
	if len(notices) != 1 || !strings.HasPrefix(notices[0], "retrieve: auto-index ready, ") {
		t.Fatalf("notices = %v, want ready line", notices)
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

	active, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	store, err := rag.OpenSQLiteStoreReadOnly(active.dbPath)
	if err != nil {
		t.Fatalf("open after refresh: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
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
			return rag.EmbedResult{Embeddings: [][]float64{realisticTestVector()}, Model: "nomic", Provider: "ollama", VectorSpaceID: "ollama/nomic"}, nil
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

func TestRunAutoIndex_FailedRefreshPreservesActiveGenerationByteForByte(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	var seedNotices []string
	seed := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &seedNotices)
	runAutoIndex(context.Background(), seed)
	active := installActiveTestReader(t, seed.ready, dbPath)
	wantDB, err := os.ReadFile(active.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wantMetadata, err := os.ReadFile(active.metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPointer, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	failing := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{}, errTestEmbedCmd
	})
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, failing, &notices)
	installActiveTestReader(t, job.ready, dbPath)
	runAutoIndex(context.Background(), job)

	if got := retrieveStateOf(job.ready); got != retrieveReady {
		t.Fatalf("state = %d, want active reader preserved; notices = %v", got, notices)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "serving active generation") {
		t.Fatalf("notices = %v, want one active-generation warning", notices)
	}
	for path, want := range map[string][]byte{active.dbPath: wantDB, active.metadataPath: wantMetadata, activePointerPath(dbPath): wantPointer} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("active artifact %q changed after failed refresh", path)
		}
	}
}

func TestRunAutoIndex_RetrieverOpenFailureDoesNotPublish(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	var seedNotices []string
	seed := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &seedNotices)
	runAutoIndex(context.Background(), seed)
	wantPointer, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}

	ready := newReadyRetrieve(warmingRetrieveMessage)
	ready.install(newRetrievalReader(&countingTool{content: "old"}, nil), "old")
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &notices)
	job.ready = ready
	job.openRetriever = func(context.Context, string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
		return nil, "", vsDecision{}, rag.StoreStats{}, errors.New("reader open failed")
	}
	runAutoIndex(context.Background(), job)

	gotPointer, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotPointer, wantPointer) {
		t.Fatal("retriever open failure changed the active generation pointer")
	}
	result, err := ready.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil || result.Content != "old" {
		t.Fatalf("active reader after failed replacement = %+v, %v", result, err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "serving active generation") {
		t.Fatalf("notices = %v", notices)
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
	installActiveTestReader(t, job2.ready, dbPath)
	runAutoIndex(context.Background(), job2)

	if got := retrieveStateOf(job2.ready); got != retrieveReady {
		t.Fatalf("empty refresh must preserve active reader, state = %d; notices = %v", got, notices2)
	}
	if len(notices2) != 1 ||
		!strings.Contains(notices2[0], "corpus not usable (sources=0") ||
		!strings.Contains(notices2[0], "serving active generation") {
		t.Fatalf("notices = %v, want unusable-corpus failure", notices2)
	}
}

func TestRunAutoIndex_ServesActiveWhileBlockedThenPublishesAndSwaps(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	var seedNotices []string
	seed := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &seedNotices)
	runAutoIndex(context.Background(), seed)
	old, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}

	ready := newReadyRetrieve(warmingRetrieveMessage)
	ready.install(newRetrievalReader(&countingTool{content: "old"}, nil), "old")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	emb := rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		once.Do(func() { close(entered); <-release })
		vectors := make([][]float64, len(inputs))
		for i := range vectors {
			vectors[i] = realisticTestVector()
		}
		return rag.EmbedResult{Embeddings: vectors, Provider: "ollama", Model: "nomic", VectorSpaceID: "ollama/nomic"}, nil
	})
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	job.ready = ready
	job.openRetriever = func(ctx context.Context, path string) (*retrievalReader, string, vsDecision, rag.StoreStats, error) {
		store, err := rag.OpenSQLiteStoreReadOnly(path)
		if err != nil {
			return nil, "", vsDecision{}, rag.StoreStats{}, err
		}
		stats, err := store.Stats(ctx)
		if err != nil {
			return nil, "", vsDecision{}, rag.StoreStats{}, errors.Join(err, store.Close())
		}
		return newOwnedRetrievalReader(&countingTool{content: "new"}, store, nil), "", vsDecision{}, stats, nil
	}
	done := make(chan struct{})
	go func() { runAutoIndex(context.Background(), job); close(done) }()
	<-entered
	result, err := ready.Invoke(context.Background(), json.RawMessage(`{"query":"during"}`))
	if err != nil || result.Content != "old" {
		t.Fatalf("retrieval during refresh = %+v, %v", result, err)
	}
	close(release)
	<-done
	result, err = ready.Invoke(context.Background(), json.RawMessage(`{"query":"after"}`))
	if err != nil || result.Content != "new" {
		t.Fatalf("retrieval after swap = %+v, %v", result, err)
	}
	current, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	if current.id == old.id {
		t.Fatal("successful refresh did not publish a new generation")
	}
}

func TestRunAutoIndex_LeaseContentionPreservesActiveAndLiveStaging(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	var seedNotices []string
	seed := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &seedNotices)
	runAutoIndex(context.Background(), seed)
	active, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	pointerBefore, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	staging, err := createStagingGeneration(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireIndexWriterLease(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Error(err)
		}
	})

	ready := newReadyRetrieve(warmingRetrieveMessage)
	ready.install(newRetrievalReader(&countingTool{content: "active"}, nil), "active")
	var embedCalls atomic.Int32
	emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		embedCalls.Add(1)
		return rag.EmbedResult{}, errors.New("must not run")
	})
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	job.ready = ready
	runAutoIndex(context.Background(), job)
	if embedCalls.Load() != 0 {
		t.Fatal("contending writer reached embedding probe")
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "writer lease already held") {
		t.Fatalf("notices = %v", notices)
	}
	if _, err := os.Stat(filepath.Dir(staging.dbPath)); err != nil {
		t.Fatalf("contending writer removed live staging: %v", err)
	}
	pointerAfter, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("lease contention changed active pointer")
	}
	current, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil || current.id != active.id {
		t.Fatalf("active after contention = %+v, %v", current, err)
	}
}

func TestRunAutoIndex_CancellationPreservesActiveGenerationByteForByte(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	var seedNotices []string
	seed := newAutoIndexTestJob(root, dbPath, autoIndexTestEmbedder("ollama/nomic", ""), &seedNotices)
	runAutoIndex(context.Background(), seed)
	active, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	wantDB, err := os.ReadFile(active.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	wantMetadata, err := os.ReadFile(active.metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPointer, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() { println(\"changed\") }\n")

	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	emb := rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		if calls.Add(1) > 1 {
			cancel()
			return rag.EmbedResult{}, context.Canceled
		}
		return rag.EmbedResult{Embeddings: [][]float64{realisticTestVector()}, Provider: "ollama", Model: "nomic", VectorSpaceID: "ollama/nomic"}, nil
	})
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	installActiveTestReader(t, job.ready, dbPath)
	runAutoIndex(ctx, job)
	if len(notices) != 0 {
		t.Fatalf("cancellation notices = %v", notices)
	}
	if !job.ready.hasReader() {
		t.Fatal("cancellation removed active reader")
	}
	for path, want := range map[string][]byte{active.dbPath: wantDB, active.metadataPath: wantMetadata, activePointerPath(dbPath): wantPointer} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("active artifact %q changed after cancellation", path)
		}
	}
}

func TestRunAutoIndex_PinsGenerationToSuccessfulProbeVectorSpace(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dbPath := filepath.Join(t.TempDir(), "indexes", "k.db")
	emb := autoIndexTestEmbedder("actual/fallback", "")
	var notices []string
	job := newAutoIndexTestJob(root, dbPath, emb, &notices)
	job.embChain = []string{"configured/primary", "actual/fallback"}
	runAutoIndex(context.Background(), job)
	active, err := resolveActiveGeneration(context.Background(), dbPath, "workspace:k")
	if err != nil {
		t.Fatal(err)
	}
	if active.metadata.VectorSpaceID != "actual/fallback" {
		t.Fatalf("published vector space = %q, want actual/fallback", active.metadata.VectorSpaceID)
	}
}
