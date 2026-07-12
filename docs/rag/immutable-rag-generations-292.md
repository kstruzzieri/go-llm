# Immutable RAG generations (#292)

Golem-owned workspace indexes use immutable generation directories and one
atomic active pointer. Existing version-1 `<workspace>.db` plus
`<workspace>.json` pairs remain readable as legacy generations, but startup
refresh never mutates them. Explicit `-rag-db` paths keep their existing
behavior and are never enrolled in this protocol.

## Chosen protocol

For the legacy base path `<workspace>.db`, generation state lives at:

```text
<workspace>.active.json                 atomic active pointer
<workspace>.generations/<generation>/index.db
<workspace>.generations/<generation>/index.json
<workspace>.lock                        workspace writer lease
```

A writer acquires the workspace lease, removes only `.staging-*` directories
left by earlier interrupted writers, and creates a new unique staging
directory. Finalized historical generations are retained because another
process may still be draining one. When the active vector space matches the successful
startup embedding probe, the writer copies the immutable active database into
the staging generation for incremental refresh. Otherwise it starts a fresh
database. The active files are never opened writable.

After indexing, the writer checkpoints and closes SQLite, then reopens the
staging database immutable and validates its database integrity, source and
chunk counts, completeness status, generation metadata, and exactly one known
vector space equal to the probe's actual provider/model identity. Only then
does it fsync and atomically rename one pointer file. Directory fsync makes the
rename durable. There is no claim that the database and metadata renames are a
single atomic operation: they become immutable staging artifacts first, and
the pointer rename is the sole publication event.

Crash recovery is deterministic:

- Before the generation-directory rename, restart serves the old pointer (or
  legacy pair) and the next lease holder removes the `.staging-*` directory.
- After the generation-directory rename but before pointer rename, restart
  serves the old pointer and leaves the finalized orphan untouched; it cannot
  be distinguished safely from a generation still served by another process.
- After pointer rename, restart validates and serves the new generation; an
  orphaned pointer temporary file is ignored and later removed under the lease.
- A missing, corrupt, mixed-space, count-mismatched, or generation-mismatched
  target is rejected without falling through to a different generation.

## Invariants

- **Active generation:** the active pointer names one complete immutable
  generation. Readers never infer the newest directory and never modify the
  active database or metadata.
- **Staging generation:** every build has a unique directory. It is inactive
  until full validation and pointer publication, and failures remove only that
  directory.
- **Lease:** one OS-backed nonblocking file lock covers build, validation,
  cleanup, and publication for a workspace. Contention performs no cleanup or
  writes and leaves the active reader installed.
- **Publication:** the fsynced pointer rename is the only visibility change.
  Every observable pointer is either the complete old JSON object or the
  complete new JSON object.
- **Vector space:** the successful probe's actual vector-space identity, not
  the configured first selector, chooses incremental reuse. Validation rejects
  missing, unknown, mixed, or different stored identities.
- **Reader drain:** each delegate owns its immutable SQLite store and optional
  behavioral-feedback database. Swap removes the old delegate from new calls,
  waits for already-admitted calls, then closes both resources exactly once.
- **Shutdown:** close prevents later installation, waits for active calls,
  releases resources, and makes late background completions retire their new
  delegate instead of resurrecting it.
- **Cleanup:** staging cleanup runs only while holding the workspace lease and
  is restricted to `.staging-*` directories and the pointer temp file. It never
  deletes finalized generations or touches explicit `-rag-db` data.

## Alternatives rejected

- Replacing `<workspace>.db` and `<workspace>.json` in place needs two renames
  and admits mismatched crash states.
- A multi-step publication journal can recover those states, but is larger and
  harder to audit than immutable directories plus one pointer.
- A process mutex cannot serialize independent Golem processes or recover when
  a writer exits.

The process-resident snapshot and retrieval scoring contracts from #291 remain
unchanged: each opened generation owns an independent lazy immutable snapshot.
