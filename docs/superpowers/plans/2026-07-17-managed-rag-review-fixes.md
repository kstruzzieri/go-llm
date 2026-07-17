# Managed RAG Review Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close every confirmed PR #312 review finding while preserving unscoped legacy RAG behavior.

**Architecture:** Correct compatibility and lifecycle ownership at their shared write seams, make managed reindex validate the corpus excluding the replaced source, add additive Retriever scope methods, and recompute returned managed-file freshness without writes. Managed input and list work are bounded with fixed pre-merge constants and a cursor carried by the existing list filter.

**Tech Stack:** Go standard library, `modernc.org/sqlite`, existing RAG and MCP packages, Go tests.

## Global Constraints

- Work only in `/private/tmp/go-llm-track-b-80` on `codex/80-managed-rag-sources`.
- Every production behavior change must have a test that was observed failing first.
- Existing Indexer and MCP tool behavior must remain byte-compatible when managed-only options are absent.
- Use no new dependency, registry abstraction, background watcher, lease, or search tool.
- Enforce 16 MiB documents, 4 KiB metadata fields, 64 tags of 256 bytes, and 100 list results.
- Keep multi-source whole-corpus migration fail-closed and tracked by #313.

---

### Task 1: Restore compatibility and honest lifecycle state

**Files:**
- Modify: `mcp/tools_rag.go`
- Modify: `mcp/tools_rag_test.go`
- Modify: `rag/indexer.go`
- Modify: `rag/managed.go`
- Modify: `rag/managed_test.go`

**Interfaces:**
- Produces: `(*Indexer).rejectManagedDocumentSource(context.Context, string) error`.
- Preserves: legacy `rag_delete` text and arbitrary unregistered `managed:` source strings.

- [ ] **Step 1: Write failing compatibility and concurrency tests**

Change the delete assertion to:

```go
func TestRAGDeleteToolPreservesLegacySuccessText(t *testing.T) {
	// Use the existing connected test server setup.
	// Call rag_delete with source test.go.
	if text := extractText(result); text != "deleted source test.go" {
		t.Fatalf("result = %q, want legacy success text", text)
	}
}
```

Add an Indexer test that permits `managed:notes.md`, plus a two-service test whose
blocking embedder leaves a visible row in `indexing` while the second service
lists it:

```go
documents, err := second.ListDocuments(ctx, DocumentFilter{})
if err != nil {
	t.Fatal(err)
}
if len(documents) != 1 || documents[0].State != DocumentStateIndexing {
	t.Fatalf("documents = %#v, want active indexing row", documents)
}
```

- [ ] **Step 2: Verify the tests fail for the reviewed regressions**

Run:

```bash
rtk go test ./rag ./mcp -run 'RAGDeleteToolPreserves|ManagedPrefix|ActiveIndexing'
```

Expected: delete returns JSON, unregistered `managed:` indexing is rejected, and the second service persists `failed`.

- [ ] **Step 3: Implement the minimum shared fixes**

Restore:

```go
return toolResult("deleted source " + args.Source), nil
```

Delete the `DocumentStateIndexing` mutation from `ListDocuments`. Replace the
prefix-only helper with this ownership check and call it from both replacement
chokepoints before any chunk/embed work:

```go
func (idx *Indexer) rejectManagedDocumentSource(ctx context.Context, source string) error {
	if !strings.HasPrefix(source, managedSourcePrefix) {
		return nil
	}
	store, ok := idx.store.(*SQLiteStore)
	if !ok {
		return nil
	}
	managed, err := store.hasManagedDocumentSource(ctx, source)
	if err != nil {
		return err
	}
	if managed {
		return fmt.Errorf("rag: source %q belongs to a managed document; use ManagedSources", source)
	}
	return nil
}
```

- [ ] **Step 4: Verify focused packages are green**

```bash
rtk go test ./rag ./mcp -run 'RAGDeleteToolPreserves|ManagedPrefix|ActiveIndexing|IndexDirectoryPrune'
```

Expected: all selected tests pass.

- [ ] **Step 5: Commit Task 1**

```bash
rtk git add mcp/tools_rag.go mcp/tools_rag_test.go rag/indexer.go rag/managed.go rag/managed_test.go
rtk git commit -m "fix(rag): restore legacy source compatibility"
```

### Task 2: Make managed reads safe and bounded

**Files:**
- Create: `rag/managed_file_unix.go`
- Create: `rag/managed_file_other.go`
- Modify: `rag/managed.go`
- Modify: `rag/managed_test.go`
- Modify: `rag/managed_unix_test.go`
- Modify: `mcp/tools_rag_managed.go`
- Modify: `mcp/tools_rag_managed_test.go`

**Interfaces:**
- Produces: exported managed limit constants, `DocumentFilter.AfterID`, and `DocumentFilter.Limit`.
- Produces: `openManagedFile(string) (*os.File, error)` on every platform.

- [ ] **Step 1: Write failing limit, paging, and file tests**

Cover content at `MaxManagedDocumentBytes+1`, 65 unique tags, a 257-byte tag,
a 4097-byte metadata field, `Limit=2` with `AfterID`, `Limit=101`, a regular
file read, and the existing FIFO rejection. The page assertions are:

```go
first, err := managed.ListDocuments(ctx, DocumentFilter{Limit: 2})
if err != nil || len(first) != 2 {
	t.Fatalf("first page = %#v, err = %v", first, err)
}
second, err := managed.ListDocuments(ctx, DocumentFilter{AfterID: first[1].ID, Limit: 2})
if err != nil || len(second) != 1 || second[0].ID <= first[1].ID {
	t.Fatalf("second page = %#v, err = %v", second, err)
}
```

Pin MCP schema `maxLength`, `maxItems`, `after_id`, `limit`, and `maximum: 100`.

- [ ] **Step 2: Verify the new tests fail**

```bash
rtk go test ./rag ./mcp -run 'Managed.*(Limit|Page|Oversize|Regular|FIFO)|ManagedRAGToolSchemas'
```

Expected: missing constants/filter fields and unbounded inputs cause compilation or assertion failures.

- [ ] **Step 3: Implement single-descriptor bounded reads**

Use these platform helpers:

```go
//go:build unix

package rag

import (
	"os"
	"syscall"
)

func openManagedFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
```

```go
//go:build !unix

package rag

import "os"

func openManagedFile(path string) (*os.File, error) { return os.Open(path) }
```

Read through the validated descriptor:

```go
func readManagedRegularFile(path string) ([]byte, error) {
	f, err := openManagedFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxManagedDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManagedDocumentBytes {
		return nil, fmt.Errorf("document exceeds %d-byte limit", MaxManagedDocumentBytes)
	}
	return data, nil
}
```

- [ ] **Step 4: Implement validation and bounded listing**

Define:

```go
const (
	MaxManagedDocumentBytes = 16 << 20
	MaxManagedMetadataBytes = 4 << 10
	MaxManagedTags          = 64
	MaxManagedTagBytes      = 256
	MaxManagedListLimit     = 100
)
```

Validate normalized options in `ManagedSources` before registry writes. Extend
`DocumentFilter` with `AfterID string` and `Limit int`; default a non-positive
limit to 100 and reject values over 100. Load lightweight rows in ID-ordered
batches no larger than 100, advance an internal scan cursor, reconcile and
apply dynamic state/freshness filters, and stop after the requested matches.

- [ ] **Step 5: Expose the same MCP bounds**

Add `after_id` and `limit` to `rag_list_documents`, pass them into
`DocumentFilter`, and apply the constants as JSON Schema `maxLength`,
`maxItems`, and `maximum` values. Keep the result as the existing JSON array.

- [ ] **Step 6: Verify Task 2**

```bash
rtk go test ./rag ./mcp -run 'Managed.*(Limit|Page|Oversize|Regular|FIFO)|ManagedRAGToolSchemas'
rtk go test -race ./rag ./mcp -run 'Managed'
```

Expected: focused and race tests pass.

- [ ] **Step 7: Commit Task 2**

```bash
rtk git add rag/managed.go rag/managed_file_unix.go rag/managed_file_other.go rag/managed_test.go rag/managed_unix_test.go mcp/tools_rag_managed.go mcp/tools_rag_managed_test.go
rtk git commit -m "fix(rag): bound managed source work"
```

### Task 3: Permit safe managed vector-space migration

**Files:**
- Modify: `rag/managed.go`
- Modify: `rag/sqlite_store.go`
- Modify: `rag/managed_test.go`
- Modify: `rag/embedding_storage_test.go`
- Modify: `mcp/tools_rag_managed_test.go`

**Interfaces:**
- Extends private `replaceSourceOptions` with `replaceVectorSpace bool`.
- Leaves every exported low-level replacement method unchanged.

- [ ] **Step 1: Write failing migration tests**

Change the sole-document drift test to require stable-ID success with a new
vector ID and dimension. Add another source with the old vector and require
rollback:

```go
migrated, err := managed.ReindexDocument(ctx, document.ID)
if err != nil {
	t.Fatal(err)
}
if migrated.ID != document.ID || migrated.VectorSpaceID != "test/new" {
	t.Fatalf("migrated = %#v", migrated)
}
```

The incompatible-corpus test must still satisfy `errors.Is(err,
ErrVectorSpaceDrift)` and prove both old chunk batches remain unchanged.

- [ ] **Step 2: Verify the migration tests fail for the retained-ID guard**

```bash
rtk go test ./rag ./mcp -run 'ManagedSources.*VectorSpace|ManagedRAG.*Reindex.*Model'
```

Expected: sole-source migration returns `ErrVectorSpaceDrift`.

- [ ] **Step 3: Implement the private full-replacement option**

In managed commit, remove the immutable retained-ID check, retain the old ID
only for zero-chunk preparations, keep `validateCorpusVectorSpaceTx`, and set:

```go
opts.replaceVectorSpace = true
opts.checkExistingVectorSpace = false
```

In `replaceSourceTx`, keep the old validation order for every normal call. For
`replaceVectorSpace`, delete the old source first and then validate the incoming
dimension against the remaining transaction-visible corpus before inserting.
Rollback restores the source on any validation or insert failure.

- [ ] **Step 4: Verify managed migration and low-level invariants**

```bash
rtk go test ./rag ./mcp -run 'VectorSpace|Dimension|ManagedRAG.*Reindex'
```

Expected: managed sole/last-source migration passes; normal low-level dimension drift and incompatible remaining corpus still fail without mutation.

- [ ] **Step 5: Commit Task 3**

```bash
rtk git add rag/managed.go rag/sqlite_store.go rag/managed_test.go rag/embedding_storage_test.go mcp/tools_rag_managed_test.go
rtk git commit -m "fix(rag): allow safe managed vector migration"
```

### Task 4: Add opt-in scoped retrieval and truthful result freshness

**Files:**
- Modify: `rag/retriever.go`
- Modify: `rag/retriever_test.go`
- Modify: `rag/retriever_multi_test.go`
- Modify: `rag/managed.go`
- Modify: `rag/managed_test.go`
- Modify: `mcp/tools_rag.go`
- Modify: `mcp/tools_rag_test.go`
- Modify: `mcp/tools_rag_managed_test.go`

**Interfaces:**
- Produces: `RetrievalScope{Collection string; Tags []string}`.
- Produces: `(*Retriever).RetrieveScoped` and `(*Retriever).RetrieveScoredScoped`.

- [ ] **Step 1: Write failing scope and freshness tests**

Create managed documents in different collections/tags, make an out-of-scope
chunk rank first globally, and assert `top_k` is applied after filtering. Modify
a managed file after ingest and call retrieval without listing:

```go
results, err := retriever.Retrieve(ctx, "query", 5)
if err != nil {
	t.Fatal(err)
}
if got := results[0].Chunk.Metadata["managed_freshness"]; got != "stale" {
	t.Fatalf("freshness = %q, want stale", got)
}
```

Add MCP schema and handler tests for collection and all-tags scope while keeping
the existing default JSON compatibility test unchanged.

- [ ] **Step 2: Verify scope APIs are missing and freshness remains stale-inaccurate**

```bash
rtk go test ./rag ./mcp -run 'Scoped|FreshnessWithoutList|RAGSearch.*Scope|DefaultResponseRemains'
```

Expected: scope methods/fields are absent or tests return global results and `fresh` metadata.

- [ ] **Step 3: Refactor retrieval into raw private paths**

Move current bodies into `retrieve(ctx, query, k)` and
`retrieveScored(ctx, query, k, qCtx)`. Public unscoped methods call the raw path,
refresh only final managed-file results, and return the same shapes and ordering.

- [ ] **Step 4: Implement scope filtering after ranking and before top-k**

```go
type RetrievalScope struct {
	Collection string
	Tags       []string
}

func (r *Retriever) RetrieveScoped(ctx context.Context, query string, k int, scope RetrievalScope) ([]SearchResult, error) {
	if scope.empty() {
		return r.Retrieve(ctx, query, k)
	}
	if _, ok := r.store.(*SQLiteStore); !ok {
		return nil, fmt.Errorf("rag: scoped retrieval requires SQLiteStore")
	}
	results, err := r.retrieve(ctx, query, 0)
	if err != nil {
		return nil, err
	}
	results = filterSearchResults(results, scope)
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	refreshManagedSearchResults(results)
	return results, nil
}
```

Implement the scored counterpart identically. Scope requires exact
`managed_collection`, all normalized `managed_tags`, and excludes chunks without
valid managed metadata.

- [ ] **Step 5: Recompute returned file freshness without registry writes**

For final indexed managed-file results, clone `Chunk.Metadata`, read the retained
origin through `readManagedRegularFile`, compare `managed_content_hash`, and set
only the cloned `managed_freshness` to `fresh` or `stale`. Do not acquire the
managed gate or persist status from retrieval.

- [ ] **Step 6: Wire optional MCP scope**

Add optional `collection` and `tags` to `rag_search`. Use scoped methods only
when either is non-empty; retain existing unscoped, contextual, and
`explain_scores` response paths.

- [ ] **Step 7: Verify Task 4**

```bash
rtk go test ./rag ./mcp -run 'Scoped|FreshnessWithoutList|RAGSearch.*Scope|DefaultResponseRemains'
rtk go test -race ./rag ./mcp -run 'Retrieve|RAGSearch|Managed'
```

Expected: scope/freshness tests pass and default compatibility tests remain green.

- [ ] **Step 8: Commit Task 4**

```bash
rtk git add rag/retriever.go rag/retriever_test.go rag/retriever_multi_test.go rag/managed.go rag/managed_test.go mcp/tools_rag.go mcp/tools_rag_test.go mcp/tools_rag_managed_test.go
rtk git commit -m "feat(rag): scope managed retrieval"
```

### Task 5: Review, verify, and publish

**Files:**
- Modify only files named by Tasks 1-4 plus documentation required to describe the final public API.

**Interfaces:**
- Consumes every preceding task.
- Produces a reviewed, verified update to PR #312.

- [ ] **Step 1: Format and run focused tests**

```bash
rtk gofmt -w rag/managed.go rag/managed_file_unix.go rag/managed_file_other.go rag/indexer.go rag/sqlite_store.go rag/retriever.go rag/*_test.go mcp/tools_rag.go mcp/tools_rag_managed.go mcp/tools_rag_test.go mcp/tools_rag_managed_test.go
rtk go test ./rag ./mcp
```

- [ ] **Step 2: Run race, repository, vet, and diff gates**

```bash
rtk go test -race ./rag ./mcp
rtk go test ./...
rtk go vet ./...
rtk git diff --check origin/develop...HEAD
```

- [ ] **Step 3: Request independent standards, specification, and security review**

Dispatch independent read-only reviewers against `origin/develop...HEAD`. Fix
every Critical/High finding and every Medium compatibility/data-integrity
finding with a new failing regression test, then repeat affected gates.

- [ ] **Step 4: Commit any review fixes and push**

```bash
rtk git status --short
rtk git push origin codex/80-managed-rag-sources
rtk gh pr checks 312
```

Expected: the branch is clean, the push succeeds, and GitHub CI is queued or green.
