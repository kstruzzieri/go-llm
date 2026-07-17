# Managed RAG Review Fixes Design

## Goal

Close the remaining correctness, compatibility, scoped-retrieval, freshness,
and resource-bound findings on PR #312 without changing any legacy RAG behavior
when the new managed options are absent.

## Chosen approach

Fix each issue at its existing shared seam. Do not add another registry,
retriever implementation, MCP search tool, ownership lease, or background
watcher.

### Compatibility and lifecycle

- Restore the legacy `rag_delete` success text exactly.
- Treat `managed:` as owned only when the SQLite registry contains that exact
  source. Unregistered prefixed sources remain valid low-level Indexer inputs.
- Stop converting visible `indexing` rows to `failed` during listing. Separate
  `ManagedSources` instances and processes have separate gates, so absence of a
  local operation is not proof that an indexing row is orphaned. Reindex and
  delete remain the explicit recovery operations.

### Safe vector-space reindex

`ReindexDocument` remains a single-document operation. It may replace an old
vector space or embedding dimension only when the same SQLite transaction
proves that every other chunk is absent or already compatible with the incoming
vector space and dimension. Validation happens against the corpus excluding the
source; the old source is deleted before the dimension check, and rollback
preserves it on failure.

This preserves the document ID and permits safe sole-source or last-source
migration. A multi-source old corpus remains fail-closed until the other sources
are rebuilt; generic bulk corpus migration remains issue #313 rather than being
smuggled into this PR.

### Scoped retrieval and freshness

Add an additive `RetrievalScope` plus `RetrieveScoped` and
`RetrieveScoredScoped`. Non-empty collection/tags scope retrieves the ranked
SQLite candidates, filters by the existing managed metadata with exact
collection and all-tags semantics, then applies `top_k`. Empty scope routes
through the unchanged methods.

`rag_search` gains optional `collection` and `tags` fields and calls the scoped
methods only when either is present. Existing request and response behavior is
unchanged by default.

Before returning final results, retrieval recomputes freshness only for returned
indexed managed-file chunks from their retained origin and content hash. It
updates a cloned result metadata map without writing the registry. Listing
remains the durable reconciliation path; search remains read-only.

### File safety and resource bounds

Managed file reads use one descriptor: open nonblocking on Unix, validate that
descriptor as regular, then read through a limit. This removes the pathname
stat/open race and prevents FIFO blocking.

The managed API enforces these pre-merge limits:

- 16 MiB document content;
- 4 KiB for each name/title/MIME type/collection field;
- 64 normalized tags, each at most 256 bytes;
- 100 documents per list page.

`DocumentFilter` gains `AfterID` and `Limit`; non-positive limits default to 100
and larger limits are rejected. Listing reads lightweight registry rows in
bounded batches, reconciles only candidates needed for the page, and preserves
ID ordering. MCP exposes the same cursor and limit without changing the result
array shape.

## Rejected approaches

- Calling `ListDocuments` before every MCP search would turn reads into global
  filesystem scans and database writes.
- Putting scope into `QueryContext` would mix filtering policy with ranking
  context and still leave dense/custom-store behavior ambiguous.
- A new filtered-store interface or generation-swapped bulk rebuild is more
  machinery than #80 needs; add pushdown or bulk migration only when measured
  workloads require it.
- Age-based orphan detection is incorrect for slow embeddings. Durable automatic
  orphan recovery would require an owner lease and heartbeat.

## Verification

Every behavior change gets a failing regression first: legacy output, exact
registry ownership, two-instance active indexing, vector-space/dimension
migration and incompatible-corpus rollback, collection/tag scope, direct
retrieval freshness, FIFO handling, size/tag validation, and list paging. Final
gates are focused tests, race tests, repository-wide tests, vet, diff hygiene,
independent review, and GitHub CI after push.
