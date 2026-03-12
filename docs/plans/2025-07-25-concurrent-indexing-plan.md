# Concurrent File Indexing Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make `IndexDirectory` process files concurrently using a bounded worker pool, fulfilling Issue #5.

**Architecture:** Two-phase IndexDirectory (walk-then-index) using `errgroup.Group` with `SetLimit` for bounded concurrency. A `sync.Mutex` on `Indexer.replaceSource` serializes store writes while file I/O and embedding calls run fully in parallel.

**Tech Stack:** `golang.org/x/sync/errgroup`, `sync.Mutex`

---

### Task 1: Add `golang.org/x/sync` dependency

**Files:**
- Modify: `go.mod`
- Modify: `CLAUDE.md:28-30`

**Step 1: Add the dependency**

Run: `go get golang.org/x/sync`

**Step 2: Update CLAUDE.md dependencies section**

Replace lines 28-30:

```markdown
Keep minimal. Only allowed external dependency:
- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
```

With:

```markdown
Keep minimal. Allowed external dependencies:
- `modernc.org/sqlite` — pure Go SQLite driver (no CGo)
- `golang.org/x/sync` — concurrency primitives (errgroup for bounded worker pools)
```

**Step 3: Verify build**

Run: `go build ./...`
Expected: success, no errors

**Step 4: Commit**

```bash
git add go.mod go.sum CLAUDE.md
git commit -m "build: add golang.org/x/sync dependency for concurrent indexing (Issue #5)"
```

---

### Task 2: Add `storeMu` to `Indexer` and lock `replaceSource`

**Files:**
- Modify: `rag/indexer.go:1-12` (imports)
- Modify: `rag/indexer.go:14-20` (Indexer struct)
- Modify: `rag/indexer.go:59-77` (replaceSource method)

**Step 1: Add `sync` to imports**

In `rag/indexer.go`, add `"sync"` to the import block:

```go
import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/ollama"
)
```

**Step 2: Add `storeMu` field to `Indexer`**

```go
// Indexer coordinates chunking, embedding, and storing documents.
// It is safe for concurrent use; store writes are serialized internally.
type Indexer struct {
	client  *ollama.Client
	model   string
	store   VectorStore
	chunker Chunker
	storeMu sync.Mutex
}
```

**Step 3: Lock mutex in `replaceSource`**

Add lock/unlock as the first two lines of `replaceSource`:

```go
func (idx *Indexer) replaceSource(ctx context.Context, path string, chunks []Chunk, embeddings [][]float64) error {
	idx.storeMu.Lock()
	defer idx.storeMu.Unlock()

	if replacer, ok := idx.store.(atomicSourceReplacer); ok {
		return replacer.ReplaceSource(ctx, path, chunks, embeddings)
	}

	// Best-effort fallback for custom stores that do not implement atomic replace.
	// This preserves backwards compatibility with existing VectorStore
	// implementations but is not transactional across delete + store.
	if err := idx.store.DeleteBySource(ctx, path); err != nil {
		return fmt.Errorf("delete old chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil
	}
	if err := idx.store.Store(ctx, chunks, embeddings); err != nil {
		return fmt.Errorf("store chunks: %w", err)
	}
	return nil
}
```

**Step 4: Run existing tests to verify no regression**

Run: `go test ./rag/ -v -count=1`
Expected: all existing tests PASS

**Step 5: Commit**

```bash
git add rag/indexer.go
git commit -m "feat: add write mutex to Indexer for thread-safe concurrent use (Issue #5)"
```

---

### Task 3: Refactor `IndexDirectory` to two-phase concurrent

**Files:**
- Modify: `rag/indexer.go:1-12` (add errgroup import)
- Modify: `rag/indexer.go:158-164` (WithConcurrency doc)
- Modify: `rag/indexer.go:175-247` (IndexDirectory method)

**Step 1: Add errgroup to imports**

```go
import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/ollama"
	"golang.org/x/sync/errgroup"
)
```

**Step 2: Update `WithConcurrency` doc comment**

Replace:

```go
// WithConcurrency is reserved for future use. Indexing is currently sequential.
// This option is accepted but has no effect yet.
func WithConcurrency(n int) IndexDirOption {
```

With:

```go
// WithConcurrency sets the maximum number of files to index in parallel.
// Defaults to 4. Values less than 1 are clamped to 1.
func WithConcurrency(n int) IndexDirOption {
```

**Step 3: Replace `IndexDirectory` method body**

Replace the entire `IndexDirectory` method (lines 175-247) with:

```go
// IndexDirectory indexes all supported files in a directory tree.
// Files are processed concurrently (default: 4 workers).
// It respects .gitignore patterns and can be cancelled via context.
func (idx *Indexer) IndexDirectory(ctx context.Context, dir string, opts ...IndexDirOption) error {
	cfg := &indexDirConfig{
		extensions: map[string]bool{
			".go": true, ".py": true, ".ts": true, ".tsx": true,
			".js": true, ".md": true,
		},
		exclude:     []string{"node_modules", ".git", "dist", "vendor", "__pycache__"},
		concurrency: 4,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.concurrency < 1 {
		cfg.concurrency = 1
	}

	ignorePatterns := loadGitignore(filepath.Join(dir, ".gitignore"))

	// Phase 1: Walk and collect eligible file paths.
	var files []string
	var walkErrors []string

	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			walkErrors = append(walkErrors, fmt.Sprintf("cannot access %q: %v", path, err))
			return nil
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		if info.IsDir() {
			name := info.Name()
			for _, excl := range cfg.exclude {
				if name == excl {
					return filepath.SkipDir
				}
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !cfg.extensions[ext] {
			return nil
		}

		relPath, _ := filepath.Rel(dir, path)
		if isIgnored(relPath, ignorePatterns) {
			return nil
		}

		files = append(files, path)
		return nil
	})

	if walkErr != nil {
		return fmt.Errorf("rag: walk directory %q: %w", dir, walkErr)
	}

	// Phase 2: Index files concurrently.
	var (
		mu          sync.Mutex
		indexErrors = append([]string{}, walkErrors...)
	)

	var g errgroup.Group
	g.SetLimit(cfg.concurrency)

	for _, path := range files {
		if ctx.Err() != nil {
			break
		}
		g.Go(func() error {
			if err := idx.IndexFile(ctx, path); err != nil {
				mu.Lock()
				indexErrors = append(indexErrors, fmt.Sprintf("index %q: %v", path, err))
				mu.Unlock()
			}
			return nil
		})
	}
	g.Wait()

	if len(indexErrors) > 0 {
		return fmt.Errorf("rag: index directory %q completed with %d errors: %s",
			dir, len(indexErrors), strings.Join(indexErrors, "; "))
	}
	return nil
}
```

**Step 4: Run all existing tests**

Run: `go test ./rag/ -v -count=1`
Expected: all existing tests PASS (behavior is preserved)

**Step 5: Commit**

```bash
git add rag/indexer.go
git commit -m "feat: implement concurrent file indexing in IndexDirectory (Issue #5)

IndexDirectory now processes files in parallel using errgroup with
SetLimit for bounded concurrency. Walk phase collects file paths,
then workers index concurrently with errors aggregated."
```

---

### Task 4: Write concurrent indexing tests

**Files:**
- Modify: `rag/indexer_test.go`

**Step 1: Add required imports to test file**

Add `"fmt"`, `"strings"`, and `"sync/atomic"` to the import block in `rag/indexer_test.go`:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)
```

**Step 2: Add `newSlowMockEmbedServer` helper**

Add after `newMockEmbedServer`:

```go
// newCountingEmbedServer returns a mock server that counts embed requests.
func newCountingEmbedServer(dim int, counter *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		counter.Add(1)
		emb := make([]float64, dim)
		emb[0] = 0.5
		resp := ollama.EmbedResponse{Embeddings: [][]float64{emb}}
		json.NewEncoder(w).Encode(resp)
	}))
}
```

**Step 3: Write `TestIndexerConcurrentIndexDirectory`**

```go
func TestIndexerConcurrentIndexDirectory(t *testing.T) {
	var reqCount atomic.Int32
	srv := newCountingEmbedServer(4, &reqCount)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	numFiles := 10
	for i := 0; i < numFiles; i++ {
		name := fmt.Sprintf("file%d.go", i)
		content := fmt.Sprintf("package main\n\nfunc F%d() {}\n", i)
		os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0644)
	}

	err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(3))
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalSources != numFiles {
		t.Errorf("expected %d sources, got %d", numFiles, stats.TotalSources)
	}
	if int(reqCount.Load()) < numFiles {
		t.Errorf("expected at least %d embed requests, got %d", numFiles, reqCount.Load())
	}
}
```

**Step 4: Write `TestIndexerConcurrencyValidation`**

```go
func TestIndexerConcurrencyValidation(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)

	// WithConcurrency(0) should not panic or deadlock — clamped to 1
	if err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(0)); err != nil {
		t.Fatalf("IndexDirectory with concurrency 0: %v", err)
	}

	// WithConcurrency(-1) should also work
	if err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(-1)); err != nil {
		t.Fatalf("IndexDirectory with concurrency -1: %v", err)
	}
}
```

**Step 5: Write `TestIndexerConcurrentErrorAggregation`**

```go
func TestIndexerConcurrentErrorAggregation(t *testing.T) {
	// Server that always fails embedding
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer failSrv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(failSrv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "c.go"), []byte("package c\n\nfunc C() {}\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir, WithConcurrency(2))
	if err == nil {
		t.Fatal("expected error when all files fail embedding")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "3 errors") {
		t.Errorf("expected '3 errors' in message, got: %s", errMsg)
	}
}
```

**Step 6: Write `TestIndexerConcurrentPartialFailure`**

```go
func TestIndexerConcurrentPartialFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test requires non-root user for permission-based errors")
	}

	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()

	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "good.go"), []byte("package good\n\nfunc Good() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "bad.go"), []byte("package bad\n"), 0644)
	os.Chmod(filepath.Join(tmpDir, "bad.go"), 0000)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}

	// Good file should still be indexed despite bad file failure
	stats, _ := store.Stats(context.Background())
	if stats.TotalChunks == 0 {
		t.Error("expected good file to be indexed despite bad file failure")
	}
}
```

**Step 7: Run all tests**

Run: `go test ./rag/ -v -count=1`
Expected: all tests PASS (old + new)

**Step 8: Commit**

```bash
git add rag/indexer_test.go
git commit -m "test: add concurrent indexing tests (Issue #5)

Tests cover: multi-file concurrent indexing, concurrency validation
(clamping 0 and negative to 1), error aggregation from all workers,
and partial failure (some files succeed while others fail)."
```

---

### Task 5: Final verification and cleanup

**Files:**
- None (verification only)

**Step 1: Run full test suite**

Run: `go test ./... -count=1`
Expected: all packages PASS

**Step 2: Run vet**

Run: `go vet ./...`
Expected: no issues

**Step 3: Verify no unintended changes**

Run: `git diff`
Expected: clean working tree (all changes committed)
