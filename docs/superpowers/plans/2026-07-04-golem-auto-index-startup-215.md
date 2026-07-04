# Golem auto-index on startup (#215) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On `golem` startup, auto-build the workspace RAG index if absent and incrementally refresh it if present, in a background goroutine, with a ready-gated `retrieve` tool and graceful degrade when the embedder is down.

**Architecture:** A `readyRetrieve` wrapper tool registers up front (warming -> ready | failed, mutex-guarded for #235 parallel dispatch). A background `indexWarmer` goroutine probes the embedder with one tiny embed, self-heals torn/mismatched stores at the golem-private auto path, reuses `executeIndex` into a buffer, closes the write store, and only then opens the immutable read-only retriever and swaps it in. `-no-auto-index`, `-no-rag`, `-rag-db`, and one-shot `-p` fall through to today's `enableRetrieve` path unchanged.

**Tech Stack:** Go stdlib + existing `rag`, `agent`, `provider` packages. No new deps.

**Spec:** `docs/superpowers/specs/2026-07-04-golem-auto-index-startup-215-design.md` (read it first).

**Toolchain (every task):** run all go commands as `env -u GOROOT go ...` (split-GOROOT workaround). Gates per task: `env -u GOROOT go test ./cmd/golem/ -race`, final task runs the full suite. gofmt only the files you touched.

**Commit style:** conventional commits, plain text, NO emojis.

---

## File structure

- Create: `cmd/golem/retrieve_ready.go` — `readyRetrieve` wrapper tool (one responsibility: late-binding tool state).
- Create: `cmd/golem/retrieve_ready_test.go`
- Create: `cmd/golem/autoindex.go` — `autoIndexMode` decision, `autoStartLine`, `prepareAutoIndexStore` self-heal, `autoWarmDeps`, `indexWarmer`, `startAutoIndex`, `runAutoIndex`, default dep factories (`newEmbedProbe`, `newAutoIndexerFactory`, `newOpenToolBinding`), `readyLine`, `firstLine`.
- Create: `cmd/golem/autoindex_test.go`
- Modify: `cmd/golem/main.go` — `-no-auto-index` flag, auto-mode branch around the `enableRetrieve` call site, warmer start after startup notices, `replSession.warm` wiring.
- Modify: `cmd/golem/repl.go` — `replSession.warm` field + one `/tools` line.
- Modify: `README.md`, `docs/GETTING_STARTED.md` — auto-index is now the default; `golem index` optional/manual.

---

### Task 1: `-no-auto-index` flag + `autoIndexMode` decision

**Files:**
- Create: `cmd/golem/autoindex.go`
- Create: `cmd/golem/autoindex_test.go`
- Modify: `cmd/golem/main.go` (flags struct + parseFlags)

- [ ] **Step 1: Write the failing test**

In `cmd/golem/autoindex_test.go`:

```go
package main

import (
	"errors"
	"testing"
)

func TestAutoIndexMode(t *testing.T) {
	tests := []struct {
		name        string
		f           flags
		autoErr     error
		embChainErr error
		want        bool
	}{
		{name: "default on", f: flags{}, want: true},
		{name: "no-rag off", f: flags{noRag: true}, want: false},
		{name: "explicit rag-db off", f: flags{ragDB: "/tmp/x.db"}, want: false},
		{name: "no-auto-index off", f: flags{noAutoIndex: true}, want: false},
		{name: "one-shot off", f: flags{promptSet: true}, want: false},
		{name: "auto path unresolvable off", f: flags{}, autoErr: errors.New("no data dir"), want: false},
		{name: "no embedding chain off", f: flags{}, embChainErr: errors.New("no embedding model configured"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := autoIndexMode(tt.f, tt.autoErr, tt.embChainErr); got != tt.want {
				t.Fatalf("autoIndexMode = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAutoStartLine(t *testing.T) {
	if got := autoStartLine(false); got != "retrieve: building workspace index in the background (first build)" {
		t.Fatalf("absent: %q", got)
	}
	if got := autoStartLine(true); got != "retrieve: refreshing workspace index in the background" {
		t.Fatalf("present: %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run 'TestAutoIndexMode|TestAutoStartLine' -v`
Expected: FAIL (undefined: autoIndexMode, autoStartLine, f.noAutoIndex).

- [ ] **Step 3: Implement**

Create `cmd/golem/autoindex.go`:

```go
package main

// autoIndexMode decides at startup whether background auto-indexing runs.
// Every false cause falls through to the existing enableRetrieve path, whose
// warnings/notices already cover the OFF cases (spec 5.2 stage 1).
func autoIndexMode(f flags, autoErr, embChainErr error) bool {
	if f.noRag || f.ragDB != "" || f.noAutoIndex || f.promptSet {
		return false
	}
	return autoErr == nil && embChainErr == nil
}

// autoStartLine is the synchronous startup notice for auto mode.
func autoStartLine(dbExists bool) string {
	if dbExists {
		return "retrieve: refreshing workspace index in the background"
	}
	return "retrieve: building workspace index in the background (first build)"
}
```

In `cmd/golem/main.go`, add to the `flags` struct after `noRag bool`:

```go
	noAutoIndex      bool
```

and in `parseFlags`, after the `-no-rag` line:

```go
	fs.BoolVar(&f.noAutoIndex, "no-auto-index", false, "disable automatic background index build/refresh on startup (an existing index is still used as-is)")
```

No validateFlags change: `-no-auto-index` composes harmlessly with every flag (it only narrows behavior).

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -run 'TestAutoIndexMode|TestAutoStartLine' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/autoindex.go cmd/golem/autoindex_test.go cmd/golem/main.go
git commit -m "feat(golem): add -no-auto-index flag and auto-index mode decision"
```

---

### Task 2: `readyRetrieve` wrapper tool

**Files:**
- Create: `cmd/golem/retrieve_ready.go`
- Create: `cmd/golem/retrieve_ready_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/golem/retrieve_ready_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// staticTool is a stub inner tool with a fixed result.
type staticTool struct{ result string }

func (s staticTool) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }
func (s staticTool) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }
func (s staticTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: s.result}, nil
}

func TestReadyRetrieve_SpecEffectMirrorRetrieve(t *testing.T) {
	r := newReadyRetrieve()
	if !reflect.DeepEqual(r.Spec(), agenttools.Retrieve{}.Spec()) {
		t.Fatal("Spec() must mirror agenttools.Retrieve so toolSchemaHash is stable across the swap")
	}
	// agent.Effect contains a slice field (Scope.Paths), so it is not
	// comparable with ==; DeepEqual is required.
	if !reflect.DeepEqual(r.Effect(), agenttools.Retrieve{}.Effect()) {
		t.Fatal("Effect() must mirror agenttools.Retrieve")
	}
}

func TestReadyRetrieve_WarmingThenReadyThenDelegates(t *testing.T) {
	r := newReadyRetrieve()
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("warming result must not be an error (a not-yet capability is a normal observation)")
	}
	if !strings.Contains(res.Content, "warming") {
		t.Fatalf("warming content = %q", res.Content)
	}
	if got := r.StateLine(); got != "retrieve: index warming in the background" {
		t.Fatalf("StateLine = %q", got)
	}

	r.SetReady(staticTool{result: "chunks"})
	res, err = r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "chunks" {
		t.Fatalf("ready must delegate, got %q", res.Content)
	}
	if got := r.StateLine(); got != "retrieve: index ready" {
		t.Fatalf("StateLine = %q", got)
	}
}

func TestReadyRetrieve_Failed(t *testing.T) {
	r := newReadyRetrieve()
	r.SetFailed("embedder unavailable: connection refused")
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("failed result must not be an error result")
	}
	if !strings.Contains(res.Content, "embedder unavailable") || !strings.Contains(res.Content, "file/search tools") {
		t.Fatalf("failed content = %q", res.Content)
	}
	if got := r.StateLine(); got != "retrieve: index unavailable (embedder unavailable: connection refused)" {
		t.Fatalf("StateLine = %q", got)
	}
}

// Invoke may run from parallel read-only dispatch goroutines (#235) while the
// warmer swaps the inner tool in; must be race-clean under -race.
func TestReadyRetrieve_ConcurrentInvokeAndSetReady(t *testing.T) {
	r := newReadyRetrieve()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
				_ = r.StateLine()
			}
		}()
	}
	r.SetReady(staticTool{result: "chunks"})
	wg.Wait()
	res, _ := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if res.Content != "chunks" {
		t.Fatalf("post-swap invoke = %q", res.Content)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestReadyRetrieve -race -v`
Expected: FAIL (undefined: newReadyRetrieve).

- [ ] **Step 3: Implement**

Create `cmd/golem/retrieve_ready.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// readyRetrieve is a late-binding retrieve tool. It registers at startup while
// the background warmer builds/refreshes the index, then the real retriever is
// swapped in. Tri-state: warming (inner nil, failed "") -> ready (inner set) |
// failed (reason set). Spec/Effect mirror agenttools.Retrieve verbatim so the
// model-facing schema and toolSchemaHash are stable across the swap. Invoke may
// be called from parallel read-only dispatch goroutines, so state is
// mutex-guarded.
type readyRetrieve struct {
	mu     sync.Mutex
	inner  agent.Tool
	failed string
}

func newReadyRetrieve() *readyRetrieve { return &readyRetrieve{} }

func (r *readyRetrieve) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }

func (r *readyRetrieve) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }

// Invoke delegates once ready. Before that it returns a NON-error result: an
// unavailable-yet capability is a normal observation, and an error result
// pushes some models to abandon the tool for the whole session.
func (r *readyRetrieve) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	r.mu.Lock()
	inner, failed := r.inner, r.failed
	r.mu.Unlock()
	switch {
	case inner != nil:
		return inner.Invoke(ctx, args)
	case failed != "":
		return agent.ToolResult{Content: "retrieve: index unavailable: " + failed + "; use the file/search tools instead."}, nil
	default:
		return agent.ToolResult{Content: "retrieve: the workspace index is still warming in the background; use the file/search tools for now and retry retrieve later in this session."}, nil
	}
}

// SetReady swaps in the real tool. First terminal transition wins: a SetReady
// after SetFailed (or vice versa) is ignored so racing outcomes cannot flap.
func (r *readyRetrieve) SetReady(t agent.Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inner == nil && r.failed == "" {
		r.inner = t
	}
}

// SetFailed records a terminal failure reason (shown in Invoke and /tools).
func (r *readyRetrieve) SetFailed(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inner == nil && r.failed == "" {
		r.failed = reason
	}
}

// StateLine renders the current state for /tools and notices.
func (r *readyRetrieve) StateLine() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.inner != nil:
		return "retrieve: index ready"
	case r.failed != "":
		return "retrieve: index unavailable (" + r.failed + ")"
	default:
		return "retrieve: index warming in the background"
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestReadyRetrieve -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/retrieve_ready.go cmd/golem/retrieve_ready_test.go
git commit -m "feat(golem): ready-gated retrieve wrapper tool"
```

---

### Task 3: `prepareAutoIndexStore` self-heal

**Files:**
- Modify: `cmd/golem/autoindex.go`
- Modify: `cmd/golem/autoindex_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/golem/autoindex_test.go` (test helpers `buildTestIndexer`, `writeWorkspaceFile`, `executeIndex`, `indexJob`, `writeSidecar`, `indexSidecar`, `sidecarPath` already exist in the package):

```go
// buildValidAutoIndex builds a real usable index + sidecar at dbPath for
// workspace root, using the fake embedder with vector space vsid.
func buildValidAutoIndex(t *testing.T, root, dbPath, workspaceID, vsid string) {
	t.Helper()
	store, idx := buildTestIndexer(t, dbPath, vsid)
	var out bytes.Buffer
	res := executeIndex(context.Background(), indexJob{
		indexer:        idx,
		store:          store,
		root:           root,
		dbPath:         dbPath,
		sidecarPath:    sidecarPath(dbPath),
		workspaceID:    workspaceID,
		requestedModel: vsid,
		out:            &out,
	})
	_ = store.Close()
	if res.exitErr != nil {
		t.Fatalf("seed index failed: %v\n%s", res.exitErr, out.String())
	}
}

func TestPrepareAutoIndexStore_AbsentCreatesFresh(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	store, healReason, err := prepareAutoIndexStore(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if healReason != "" {
		t.Fatalf("healReason = %q, want empty on fresh create", healReason)
	}
}

func TestPrepareAutoIndexStore_ValidExistingOpensIncremental(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	buildValidAutoIndex(t, root, dbPath, "workspace:k", "ollama/nomic")

	store, healReason, err := prepareAutoIndexStore(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if healReason != "" {
		t.Fatalf("healReason = %q, want empty for a valid existing index", healReason)
	}
	// The existing corpus must survive (incremental open, not a rebuild).
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalSources == 0 {
		t.Fatal("valid existing index was wiped; expected incremental open")
	}
}

func TestPrepareAutoIndexStore_TornStateSelfHeals(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	buildValidAutoIndex(t, root, dbPath, "workspace:k", "ollama/nomic")
	// Tear it: DB present, sidecar gone (interrupted background build shape).
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}

	store, healReason, err := prepareAutoIndexStore(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/nomic"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if healReason == "" {
		t.Fatal("torn state must report a heal reason")
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalSources != 0 {
		t.Fatal("torn store must be rebuilt from scratch (artifacts removed)")
	}
}

func TestPrepareAutoIndexStore_VectorSpaceMismatchSelfHeals(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	buildValidAutoIndex(t, root, dbPath, "workspace:k", "ollama/old-model")

	// Current chain expects a different vector space => full self-heal rebuild.
	store, healReason, err := prepareAutoIndexStore(context.Background(), dbPath, sidecarPath(dbPath), "workspace:k", []string{"ollama/new-model"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if healReason == "" {
		t.Fatal("vector-space mismatch must report a heal reason")
	}
	stats, err := store.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalSources != 0 {
		t.Fatal("mismatched store must be rebuilt from scratch")
	}
}
```

Add the imports the new tests need at the top of `autoindex_test.go` (merge with the existing import block):

```go
import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestPrepareAutoIndexStore -v`
Expected: FAIL (undefined: prepareAutoIndexStore).

- [ ] **Step 3: Implement**

Append to `cmd/golem/autoindex.go`:

```go
// prepareAutoIndexStore opens the write store for a background auto-index run,
// self-healing states the manual `golem index` command refuses without -full:
// a preflight failure (torn/missing sidecar, foreign workspace id,
// vector-space mismatch, probe failure) removes the artifacts and starts
// fresh, returning the heal reason for the notice. Self-heal is safe HERE and
// only here because dbPath is golem's private per-workspace auto path
// (validated outside the workspace) — never a user-supplied -rag-db.
func prepareAutoIndexStore(ctx context.Context, dbPath, sidecar, workspaceID string, expected []string) (*rag.SQLiteStore, string, error) {
	if !fileExists(dbPath) {
		st, err := prepareIndexStore(ctx, dbPath, sidecar, workspaceID, expected, false)
		return st, "", err
	}
	if perr := preflightExistingIndex(ctx, dbPath, sidecar, workspaceID, expected); perr != nil {
		st, err := prepareIndexStore(ctx, dbPath, sidecar, workspaceID, expected, true)
		if err != nil {
			return nil, "", err
		}
		return st, perr.Error(), nil
	}
	st, err := prepareIndexStore(ctx, dbPath, sidecar, workspaceID, expected, false)
	return st, "", err
}
```

Add to the imports of `autoindex.go`:

```go
import (
	"context"

	"github.com/kstruzzieri/go-llm/rag"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestPrepareAutoIndexStore -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/autoindex.go cmd/golem/autoindex_test.go
git commit -m "feat(golem): self-healing store preparation for auto-index"
```

---

### Task 4: `indexWarmer` background pipeline

**Files:**
- Modify: `cmd/golem/autoindex.go`
- Modify: `cmd/golem/autoindex_test.go`

- [ ] **Step 1: Write the failing tests**

Append to `cmd/golem/autoindex_test.go`:

```go
// noticeRecorder collects async notify lines safely.
type noticeRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (n *noticeRecorder) notify(line string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lines = append(n.lines, line)
}

func (n *noticeRecorder) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.lines...)
}

func (n *noticeRecorder) joined() string { return strings.Join(n.all(), "\n") }

// countingEmbedder wraps the fixed-vsid fake embedder and counts embedded inputs.
type countingEmbedder struct {
	mu     sync.Mutex
	inputs int
	vsid   string
	fail   map[string]bool // input substring -> fail
}

func (c *countingEmbedder) Embed(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
	c.mu.Lock()
	c.inputs += len(inputs)
	c.mu.Unlock()
	for _, in := range inputs {
		for sub := range c.fail {
			if strings.Contains(in, sub) {
				return rag.EmbedResult{}, errTestEmbedCmd
			}
		}
	}
	vecs := make([][]float64, len(inputs))
	for i := range vecs {
		vecs[i] = []float64{1, 0, 0}
	}
	return rag.EmbedResult{Embeddings: vecs, Model: "nomic", Provider: "ollama", VectorSpaceID: c.vsid}, nil
}

func (c *countingEmbedder) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inputs
}

// testWarmDeps wires real store/index/executeIndex machinery with a fake
// embedder and a stub openTool that does a real immutable read-only open (so
// the open-after-write sequencing is actually exercised).
func testWarmDeps(t *testing.T, root, dbPath string, emb *countingEmbedder, rec *noticeRecorder) autoWarmDeps {
	t.Helper()
	return autoWarmDeps{
		root:        root,
		dbPath:      dbPath,
		sidecarPath: sidecarPath(dbPath),
		workspaceID: "workspace:k",
		expected:    []string{emb.vsid},
		embChain:    []string{emb.vsid},
		probeEmbed: func(ctx context.Context) error {
			_, err := emb.Embed(ctx, emb.vsid, []string{"probe"})
			return err
		},
		newIndexer: func(store *rag.SQLiteStore) (*rag.Indexer, error) {
			return rag.NewIndexerWithEmbedder(emb, store, rag.WithEmbeddingModel(emb.vsid))
		},
		openTool: func(ctx context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error) {
			store, err := rag.OpenSQLiteStoreReadOnly(dbPath)
			if err != nil {
				return nil, nil, "", vsDecision{}, rag.StoreStats{}, err
			}
			stats, err := store.Stats(ctx)
			if err != nil {
				_ = store.Close()
				return nil, nil, "", vsDecision{}, rag.StoreStats{}, err
			}
			return staticTool{result: "chunks"}, nil, "", vsDecision{register: true}, stats, nil
		},
		notify: rec.notify,
	}
}

func waitWarm(t *testing.T, w *indexWarmer) {
	t.Helper()
	select {
	case <-w.done:
	case <-time.After(30 * time.Second):
		t.Fatal("warmer did not finish")
	}
}

func TestRunAutoIndex_BuildsWhenAbsentAndSwapsReady(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n\nfunc A() {}\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	emb := &countingEmbedder{vsid: "ollama/nomic"}
	rec := &noticeRecorder{}

	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb, rec), wrapper)
	waitWarm(t, w)
	w.Close()

	if got := wrapper.StateLine(); got != "retrieve: index ready" {
		t.Fatalf("state = %q; notices:\n%s", got, rec.joined())
	}
	res, err := wrapper.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil || res.Content != "chunks" {
		t.Fatalf("invoke after ready = %q, %v", res.Content, err)
	}
	if !strings.Contains(rec.joined(), "retrieve: index ready") {
		t.Fatalf("missing ready notice:\n%s", rec.joined())
	}
	if _, err := readSidecar(sidecarPath(dbPath)); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	assertIndexDBModes(t, dbPath)
}

func TestRunAutoIndex_IncrementalOnlyReembedsChanges(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.md", "alpha body\n")
	writeWorkspaceFile(t, root, "b.md", "beta body\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	emb := &countingEmbedder{vsid: "ollama/nomic"}
	rec := &noticeRecorder{}

	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb, rec), wrapper)
	waitWarm(t, w)
	w.Close()
	if wrapper.StateLine() != "retrieve: index ready" {
		t.Fatalf("first warm: %q; notices:\n%s", wrapper.StateLine(), rec.joined())
	}
	firstCount := emb.count()
	if firstCount == 0 {
		t.Fatal("first warm embedded nothing")
	}

	// Unchanged repo: refresh must embed nothing beyond the probe.
	emb2 := &countingEmbedder{vsid: "ollama/nomic"}
	rec2 := &noticeRecorder{}
	wrapper2 := newReadyRetrieve()
	w2 := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb2, rec2), wrapper2)
	waitWarm(t, w2)
	w2.Close()
	if wrapper2.StateLine() != "retrieve: index ready" {
		t.Fatalf("second warm: %q; notices:\n%s", wrapper2.StateLine(), rec2.joined())
	}
	if got := emb2.count(); got != 1 { // exactly the probe input
		t.Fatalf("unchanged repo re-embedded: %d inputs (want 1, the probe)", got)
	}

	// One changed file: only its chunks re-embed (probe + b.md's chunk).
	writeWorkspaceFile(t, root, "b.md", "beta body CHANGED\n")
	emb3 := &countingEmbedder{vsid: "ollama/nomic"}
	rec3 := &noticeRecorder{}
	wrapper3 := newReadyRetrieve()
	w3 := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb3, rec3), wrapper3)
	waitWarm(t, w3)
	w3.Close()
	if wrapper3.StateLine() != "retrieve: index ready" {
		t.Fatalf("third warm: %q; notices:\n%s", wrapper3.StateLine(), rec3.joined())
	}
	if got := emb3.count(); got >= firstCount {
		t.Fatalf("changed-one-file re-embedded %d inputs, want fewer than first build's %d", got, firstCount)
	}
}

func TestRunAutoIndex_EmbedderDownSkipsWithoutArtifacts(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	rec := &noticeRecorder{}
	emb := &countingEmbedder{vsid: "ollama/nomic"}

	deps := testWarmDeps(t, root, dbPath, emb, rec)
	deps.probeEmbed = func(context.Context) error { return errTestEmbedCmd }

	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), deps, wrapper)
	waitWarm(t, w)
	w.Close()

	if !strings.Contains(wrapper.StateLine(), "unavailable") {
		t.Fatalf("state = %q", wrapper.StateLine())
	}
	if !strings.Contains(rec.joined(), "warning: auto-index skipped: embedder unavailable") {
		t.Fatalf("missing warning:\n%s", rec.joined())
	}
	// A dead embedder must not leave an empty DB behind (probe precedes store prep).
	if fileExists(dbPath) {
		t.Fatal("db created despite dead embedder")
	}
}

func TestRunAutoIndex_SelfHealNoticeOnTornState(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "a.go", "package a\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	buildValidAutoIndex(t, root, dbPath, "workspace:k", "ollama/nomic")
	if err := os.Remove(sidecarPath(dbPath)); err != nil {
		t.Fatal(err)
	}

	emb := &countingEmbedder{vsid: "ollama/nomic"}
	rec := &noticeRecorder{}
	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb, rec), wrapper)
	waitWarm(t, w)
	w.Close()

	if wrapper.StateLine() != "retrieve: index ready" {
		t.Fatalf("state = %q; notices:\n%s", wrapper.StateLine(), rec.joined())
	}
	if !strings.Contains(rec.joined(), "retrieve: rebuilding workspace index:") {
		t.Fatalf("missing rebuild notice:\n%s", rec.joined())
	}
}

func TestRunAutoIndex_PartialServesWithWarning(t *testing.T) {
	root := t.TempDir()
	writeWorkspaceFile(t, root, "ok.md", "fine body\n")
	writeWorkspaceFile(t, root, "bad.md", "POISON body\n")
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	emb := &countingEmbedder{vsid: "ollama/nomic", fail: map[string]bool{"POISON": true}}
	rec := &noticeRecorder{}

	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), testWarmDeps(t, root, dbPath, emb, rec), wrapper)
	waitWarm(t, w)
	w.Close()

	if wrapper.StateLine() != "retrieve: index ready" {
		t.Fatalf("partial store must still serve; state = %q; notices:\n%s", wrapper.StateLine(), rec.joined())
	}
	if !strings.Contains(rec.joined(), "partial") {
		t.Fatalf("missing partial warning:\n%s", rec.joined())
	}
}

func TestIndexWarmer_CloseCancelsPromptly(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	rec := &noticeRecorder{}
	emb := &countingEmbedder{vsid: "ollama/nomic"}

	deps := testWarmDeps(t, root, dbPath, emb, rec)
	started := make(chan struct{})
	deps.probeEmbed = func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // simulate a hung embedder; Close must unblock us
		return ctx.Err()
	}

	wrapper := newReadyRetrieve()
	w := startAutoIndex(context.Background(), deps, wrapper)
	<-started
	doneCh := make(chan struct{})
	go func() { w.Close(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not cancel the in-flight warm")
	}
	// Canceled shutdown stays silent: no failure warning notices.
	if strings.Contains(rec.joined(), "warning:") {
		t.Fatalf("canceled warm must be silent, got:\n%s", rec.joined())
	}
}
```

Extend the import block of `autoindex_test.go` with (merge, keep gofmt grouping):

```go
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run 'TestRunAutoIndex|TestIndexWarmer' -v`
Expected: FAIL (undefined: autoWarmDeps, startAutoIndex, indexWarmer).

- [ ] **Step 3: Implement**

Append to `cmd/golem/autoindex.go`:

```go
// autoWarmDeps carries the injectable dependencies for one background
// auto-index run, so tests drive real store/executeIndex machinery with a
// fake embedder and a stub open.
type autoWarmDeps struct {
	root        string
	dbPath      string
	sidecarPath string
	workspaceID string
	expected    []string // provider-qualified vector-space chain for the preflight gate
	embChain    []string // configured embedding selector chain; embChain[0] is the indexer model
	probeEmbed  func(ctx context.Context) error
	newIndexer  func(store *rag.SQLiteStore) (*rag.Indexer, error)
	openTool    openToolFunc
	notify      func(line string)
}

// openToolFunc opens the finished index read-only and returns the retrieve
// tool (buildGatedRetriever's signature, bound to the auto DB path).
type openToolFunc func(ctx context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error)

// indexWarmer owns the background auto-index goroutine's lifecycle.
type indexWarmer struct {
	wrapper *readyRetrieve
	cancel  context.CancelFunc
	done    chan struct{}

	mu       sync.Mutex
	feedback *behavioralWeighterHandle
}

// startAutoIndex launches the background warm and returns its lifecycle
// handle. wrapper transitions exactly once to ready or failed (unless the
// run is canceled by Close, which leaves it warming and silent).
func startAutoIndex(ctx context.Context, deps autoWarmDeps, wrapper *readyRetrieve) *indexWarmer {
	wctx, cancel := context.WithCancel(ctx)
	w := &indexWarmer{wrapper: wrapper, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		runAutoIndex(wctx, deps, w)
	}()
	return w
}

// Close cancels an in-flight warm, waits for the goroutine, and closes the
// feedback handle if the run opened one.
func (w *indexWarmer) Close() {
	w.cancel()
	<-w.done
	w.mu.Lock()
	f := w.feedback
	w.feedback = nil
	w.mu.Unlock()
	if f != nil && f.db != nil {
		_ = f.db.Close()
	}
}

func (w *indexWarmer) setFeedback(f *behavioralWeighterHandle) {
	w.mu.Lock()
	w.feedback = f
	w.mu.Unlock()
}

// runAutoIndex is the background pipeline: probe embedder -> prepare store
// (self-heal) -> executeIndex into a buffer -> close write store -> open
// read-only -> swap into the wrapper. Cancellation exits silently at every
// stage; every other failure lands in exactly one SetFailed + one warning
// notice.
func runAutoIndex(ctx context.Context, deps autoWarmDeps, w *indexWarmer) {
	if err := deps.probeEmbed(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.wrapper.SetFailed("embedder unavailable: " + err.Error())
		deps.notify("warning: auto-index skipped: embedder unavailable: " + err.Error())
		return
	}

	store, healReason, err := prepareAutoIndexStore(ctx, deps.dbPath, deps.sidecarPath, deps.workspaceID, deps.expected)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.wrapper.SetFailed(err.Error())
		deps.notify("warning: auto-index failed: " + err.Error())
		return
	}
	if healReason != "" {
		deps.notify("retrieve: rebuilding workspace index: " + healReason)
	}

	indexer, err := deps.newIndexer(store)
	if err != nil {
		_ = store.Close()
		w.wrapper.SetFailed(err.Error())
		deps.notify("warning: auto-index failed: " + err.Error())
		return
	}

	var buf bytes.Buffer
	res := executeIndex(ctx, indexJob{
		indexer:        indexer,
		store:          store,
		root:           deps.root,
		dbPath:         deps.dbPath,
		sidecarPath:    deps.sidecarPath,
		workspaceID:    deps.workspaceID,
		requestedModel: deps.embChain[0],
		out:            &buf,
	})
	// The immutable read-only open below MUST be sequenced after the writer
	// is closed; close unconditionally before any open.
	_ = store.Close()
	if ctx.Err() != nil {
		return // shutdown: stay silent; a torn store self-heals next startup
	}

	// Outcome classification: executeIndex writes the sidecar only behind the
	// usable-store gate, so a valid sidecar now means "serve" (possibly
	// partial); anything else is a failed build.
	sc, scErr := readSidecar(deps.sidecarPath)
	if scErr != nil || validateSidecar(sc, deps.workspaceID) != nil {
		reason := firstLine(buf.String())
		w.wrapper.SetFailed(reason)
		deps.notify("warning: auto-index failed: " + reason)
		return
	}

	tool, feedback, feedbackWarn, dec, stats, err := deps.openTool(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.wrapper.SetFailed(err.Error())
		deps.notify("warning: auto-index open failed: " + err.Error())
		return
	}
	if tool == nil {
		// Freshly built store failing the gate should be impossible (we just
		// built it against the current chain); surface it rather than hide it.
		msg := autoMismatchWarning(dec, deps.expected)
		w.wrapper.SetFailed(msg)
		deps.notify("warning: " + msg)
		return
	}
	w.setFeedback(feedback)
	if feedbackWarn != "" {
		deps.notify("warning: " + feedbackWarn)
	}
	w.wrapper.SetReady(tool)
	deps.notify(readyLine(sc, stats))
	_ = res // partial runs (res.exitErr set, sidecar valid) are reflected by readyLine via sc.Status
}

// readyLine renders the async completion notice.
func readyLine(sc indexSidecar, stats rag.StoreStats) string {
	if sc.Status == "partial" {
		return fmt.Sprintf("retrieve: index ready (partial, %d errors from last run; rerun \"golem index\"), %d sources", sc.ErrorCount, stats.TotalSources)
	}
	return fmt.Sprintf("retrieve: index ready, %d sources, %s", stats.TotalSources, sc.VectorSpaceID)
}

// firstLine trims s to its first non-empty line for compact failure notices.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "index build failed"
	}
	return s
}
```

Extend `autoindex.go` imports to:

```go
import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -run 'TestRunAutoIndex|TestIndexWarmer|TestPrepareAutoIndexStore|TestReadyRetrieve' -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/autoindex.go cmd/golem/autoindex_test.go
git commit -m "feat(golem): background auto-index warmer pipeline"
```

---

### Task 5: Default dep factories (probe, indexer, open binding)

**Files:**
- Modify: `cmd/golem/autoindex.go`
- Modify: `cmd/golem/autoindex_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/golem/autoindex_test.go`:

```go
func TestNewEmbedProbe_TimesOutAndPropagatesErrors(t *testing.T) {
	// The probe embeds exactly one input through the supplied embedder and
	// bounds it with autoIndexProbeTimeout.
	calls := 0
	probe := newEmbedProbeWithEmbedder(rag.EmbedderFunc(func(ctx context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		calls++
		if len(inputs) != 1 {
			t.Fatalf("probe embedded %d inputs, want 1", len(inputs))
		}
		if model != "emb-model" {
			t.Fatalf("probe model = %q", model)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("probe context must carry a deadline")
		}
		return rag.EmbedResult{Embeddings: [][]float64{{1}}}, nil
	}), "emb-model")
	if err := probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d", calls)
	}

	failing := newEmbedProbeWithEmbedder(rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{}, errTestEmbedCmd
	}), "emb-model")
	if err := failing(context.Background()); !errors.Is(err, errTestEmbedCmd) {
		t.Fatalf("err = %v, want errTestEmbedCmd", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestNewEmbedProbe -v`
Expected: FAIL (undefined: newEmbedProbeWithEmbedder).

- [ ] **Step 3: Implement**

Append to `cmd/golem/autoindex.go`:

```go
// autoIndexProbeTimeout bounds the single-input embedder probe. Generous
// because a cold local model may need to load into memory first.
const autoIndexProbeTimeout = 30 * time.Second

// newEmbedProbeWithEmbedder builds the probe from any embedder (test seam).
func newEmbedProbeWithEmbedder(emb rag.Embedder, model string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		pctx, cancel := context.WithTimeout(ctx, autoIndexProbeTimeout)
		defer cancel()
		_, err := emb.Embed(pctx, model, []string{"golem auto-index warmup probe"})
		return err
	}
}

// newEmbedProbe builds the production probe on the same chain-embedder path
// the indexer uses: routing + model availability are exercised end to end
// (the "Health() or equivalent" probe from the issue, provider-agnostic).
func newEmbedProbe(router *provider.Router, embChain []string) func(ctx context.Context) error {
	emb := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
		return router.Route(rc, rr)
	}, embChain)
	return newEmbedProbeWithEmbedder(emb, embChain[0])
}

// newAutoIndexerFactory builds per-run indexers on the chain embedder.
func newAutoIndexerFactory(router *provider.Router, embChain []string) func(store *rag.SQLiteStore) (*rag.Indexer, error) {
	return func(store *rag.SQLiteStore) (*rag.Indexer, error) {
		emb := newChainEmbedder(func(rc context.Context, rr provider.RoutingRequest) (embedExecutor, error) {
			return router.Route(rc, rr)
		}, embChain)
		return rag.NewIndexerWithEmbedder(emb, store, rag.WithEmbeddingModel(embChain[0]))
	}
}

// newOpenToolBinding binds buildGatedRetriever to the auto DB path so main.go
// does not need the rag types in its signature.
func newOpenToolBinding(cfg *config.Config, router *provider.Router, dbPath string, expected []string, feedbackDB string) openToolFunc {
	return func(ctx context.Context) (agent.Tool, *behavioralWeighterHandle, string, vsDecision, rag.StoreStats, error) {
		return buildGatedRetriever(ctx, cfg, router, dbPath, expected, feedbackDB)
	}
}
```

Extend `autoindex.go` imports with:

```go
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestNewEmbedProbe -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/autoindex.go cmd/golem/autoindex_test.go
git commit -m "feat(golem): production dep factories for the auto-index warmer"
```

---

### Task 6: main.go + repl.go wiring

**Files:**
- Modify: `cmd/golem/main.go`
- Modify: `cmd/golem/repl.go`
- Modify: `cmd/golem/main_test.go` (only if a startupNotices-shape test exists to extend; otherwise the new test below)
- Modify: `cmd/golem/autoindex_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/golem/autoindex_test.go`:

```go
func TestToolsSlashCommandShowsWarmState(t *testing.T) {
	wrapper := newReadyRetrieve()
	sess := &replSession{tools: []agent.Tool{wrapper}, warm: wrapper}
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/tools")
	if !strings.Contains(out.String(), "retrieve: index warming in the background") {
		t.Fatalf("/tools output missing warm state:\n%s", out.String())
	}
	wrapper.SetFailed("embedder unavailable: down")
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/tools")
	if !strings.Contains(out.String(), "retrieve: index unavailable (embedder unavailable: down)") {
		t.Fatalf("/tools output missing failed state:\n%s", out.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `env -u GOROOT go test ./cmd/golem/ -run TestToolsSlashCommandShowsWarmState -v`
Expected: FAIL (unknown field warm in replSession).

- [ ] **Step 3: Implement the wiring**

In `cmd/golem/repl.go`:

1. Add to `replSession` after `retrieveOmitted bool`:

```go
	warm *readyRetrieve // non-nil in auto-index mode; /tools shows its state
```

2. In `dispatchSlash`, `/tools` case, after the tool-listing loop and BEFORE the `retrieveOmitted` note:

```go
		if sess.warm != nil {
			_, _ = fmt.Fprintln(out, sess.warm.StateLine())
		}
```

In `cmd/golem/main.go`, replace the retrieve block (currently the code from `rr := enableRetrieve(...)` through `retrieveOmitted := retrieve == nil`, keeping the `autoDBPath`/`feedbackDB` computation above it) with:

```go
	embChain, embChainErr := embeddingChain(bundle.Config)
	autoOn := autoIndexMode(f, autoErr, embChainErr)

	var (
		retrieve     agent.Tool
		retrieveLine string
		warmWrapper  *readyRetrieve
		rrSuppress   bool
	)
	if autoOn {
		// Auto mode: do NOT open the store now, even when present — the
		// immutable read-only open is sequenced after the background write
		// (warm once at startup, then static for the session).
		warmWrapper = newReadyRetrieve()
		retrieve = warmWrapper
		retrieveLine = autoStartLine(fileExists(autoDBPath))
		rrSuppress = true
	} else {
		rr := enableRetrieve(ctx, bundle.Config, bundle.Router, retrieveOpts{
			noRag:           f.noRag,
			ragDB:           f.ragDB,
			autoDBPath:      autoDBPath,
			autoSidecarPath: autoSidecar,
			workspaceID:     autoWorkspaceID,
			feedbackDB:      feedbackDB,
		})
		if rr.feedback != nil && rr.feedback.db != nil {
			defer func() { _ = rr.feedback.db.Close() }()
		}
		retrieve = rr.tool
		warns = append(warns, rr.warns...)
		retrieveLine = rr.line
		rrSuppress = rr.suppressNotice
	}
	tools, err := buildTools(root, retrieve)
	if err != nil {
		return err
	}
	retrieveOmitted := retrieve == nil
```

Update the `startupNotices` call's `retrieveRequested` field to use `rrSuppress`:

```go
		retrieveRequested:  f.ragDB != "" || f.noRag || rrSuppress,
```

Immediately AFTER the `startupNotices` print loop (so the warmer's async lines never interleave with the synchronous startup block), add:

```go
	if warmWrapper != nil {
		warmer := startAutoIndex(ctx, autoWarmDeps{
			root:        root,
			dbPath:      autoDBPath,
			sidecarPath: autoSidecar,
			workspaceID: autoWorkspaceID,
			expected:    expectedVectorSpaces(bundle.Config),
			embChain:    embChain,
			probeEmbed:  newEmbedProbe(bundle.Router, embChain),
			newIndexer:  newAutoIndexerFactory(bundle.Router, embChain),
			openTool:    newOpenToolBinding(bundle.Config, bundle.Router, autoDBPath, expectedVectorSpaces(bundle.Config), feedbackDB),
			notify: func(line string) {
				_, _ = fmt.Fprintln(stderr, line)
			},
		}, warmWrapper)
		defer warmer.Close()
	}
```

(Deferred AFTER `bundle`'s deferred Close, so it runs BEFORE it: the goroutine still holds the router while draining.)

Add to the `replSession` literal:

```go
		warm:            warmWrapper,
```

- [ ] **Step 4: Run the package tests**

Run: `env -u GOROOT go test ./cmd/golem/ -race`
Expected: PASS (all existing main/oneshot/preflight tests still green — the non-auto paths are byte-for-byte today's flow; existing tests exercising `run()` use flags that turn auto mode off, or hit workspaces where the warmer no-ops; if any existing `run()`-level test now starts a warmer against a live config, prefer adding `-no-auto-index` to that test's args and note it in the commit message).

- [ ] **Step 5: Run vet**

Run: `env -u GOROOT go vet ./cmd/golem/`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/golem/main.go cmd/golem/repl.go cmd/golem/autoindex_test.go
git commit -m "feat(golem): wire background auto-index into startup and /tools"
```

---

### Task 7: Docs + full gates

**Files:**
- Modify: `README.md` (Terminal Quick Start section)
- Modify: `docs/GETTING_STARTED.md` (wherever `golem index` is presented as a required pre-step)

- [ ] **Step 1: Update README**

In the Terminal Quick Start section, replace the two-step indexing block:

```markdown
Build a workspace RAG index, then start Golem with automatic retrieval enabled:

​```bash
golem index -root /path/to/project
golem -root /path/to/project
​```
```

with:

```markdown
Golem builds and refreshes a workspace RAG index automatically in the
background on startup (the `retrieve` tool reports "warming" until the index
is ready). Manual control remains available:

​```bash
golem index -root /path/to/project   # explicit (re)build
golem -no-auto-index                 # disable the automatic startup build/refresh
golem -no-rag                        # disable retrieval entirely
​```
```

- [ ] **Step 2: Update docs/GETTING_STARTED.md**

Find the `golem index` / `-rag-db` / `-no-rag` mentions (`grep -n "golem index\|no-rag\|rag-db" docs/GETTING_STARTED.md`) and adjust the narrative the same way: auto-index is the default; `golem index` is the manual path; document `-no-auto-index`. Keep the document's existing voice and formatting.

- [ ] **Step 3: Full gates**

Run each; all must be clean:

```bash
env -u GOROOT go test ./... -race
env -u GOROOT go vet ./...
env -u GOROOT go vet -tags integration ./cmd/golem/
env -u GOROOT gofmt -l cmd/golem/ README.md 2>/dev/null; env -u GOROOT gofmt -l cmd/golem/
```

Expected: tests PASS, vet clean, gofmt lists nothing.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/GETTING_STARTED.md
git commit -m "docs: auto-index on startup is the default; golem index is manual control"
```

---

## Final verification (after all tasks)

- `env -u GOROOT go test ./... -race` — full suite.
- `env -u GOROOT go vet ./...` and `env -u GOROOT go vet -tags integration ./cmd/golem/`.
- `env -u GOROOT gofmt -l` on every file the branch touched.
- Cross-task review: spec 5.4 behavior matrix row-by-row against the code.
- Push with `git push --no-verify` (docker pre-push hook cannot run from a linked worktree; run the native gate first).
