# RAG Evaluation Baseline

This internal package supports issue `#93`: agentic RAG Phase 0 evaluation
fixtures and baseline metrics.

It is intentionally under `internal/` so the harness does not decide the public
package boundary before `#94`.

## Fixtures

`testdata/fixtures.json` contains:

- 12 synthetic code chunks
- 20 golden queries
- 5 required categories with 4 queries each:
  - `single_hop`
  - `cross_file_trace`
  - `compare`
  - `architecture`
  - `code_review`

The fixture uses deterministic synthetic embeddings and does not require
Ollama or any live model. `Fixture.Validate` rejects duplicate chunk IDs,
duplicate query IDs, duplicate query text, missing/empty fields, embedding
dimension mismatches, and queries that reference unknown chunks.

## Baseline

Regenerate the committed report with:

```sh
go run ./cmd/rag-eval \
  -fixtures internal/rageval/testdata/fixtures.json \
  -out internal/rageval/testdata/baseline.json
```

Or from the package directory:

```sh
go generate ./internal/rageval
```

The report compares:

- `static`: `rag.Retriever.Retrieve` (dense cosine only)
- `hybrid_search_multi`: `rag.SQLiteStore.SearchMulti` (dense + keyword +
  structural + temporal scorers)

## How to read the baseline

The report has three top-level sections.

### `corpus`

Static metadata about what the run measured. `vector_space_id` isolates the
fixture from any local corpus a developer may have indexed. `notes` carries
human-readable caveats that travel with the data — always read these.

### `thresholds`

**Regression floors, not aspirations.** Each non-null value is set at "best
known minus a small buffer" so a real regression trips CI without flapping on
natural metric noise. Posture rules:

1. Floors detect drops. They do not chase improvements. Tightening after
   sustained improvement is a `#95`-and-later activity.
2. Where N is too small for a metric to be stable, the threshold is `null`
   with a documented reason in `buildThresholds` (see `runner.go`).
3. `status: "thresholds_ratified"` means an owner has approved the current
   values. `status: "pending_owner_values_before_95"` means values are
   placeholders.

Latency thresholds are intentionally null on this baseline. The 12-chunk
in-memory SQLite corpus with N=20 cold samples produces sub-millisecond P95
numbers that bear no relation to production workload. Setting a number here
would either flap on noise or be ignored when `#95` raises it against a
realistic corpus — either way it trains the team to distrust threshold
breaches.

### `modes`

Per-mode summaries (`static`, `hybrid_search_multi`) plus per-query detail.

Per-query metrics are computed at K=5 and K=10. **`recall@K`'s denominator
is `len(expected_ids)`, not K** — so a "recall@5" against a retriever that
returned only 3 results is computed over those 3. `mrr` (reciprocal rank)
uses 1-indexed positions and returns 0 if no expected ID appears in the
limited result set.

`context_precision_proxy` is `len(expected hits in results) / len(results)`
— a rough proxy for "fraction of context that's relevant." `context_tokens`
uses the same 4-chars-per-token heuristic that `rag.Retriever.BuildContext`
uses, applied to the same formatted output.

Cold latency is one measurement per query. Warm latency aggregates
`WarmRuns` repeats. Both report P50 and P95 in milliseconds. **Treat
sub-millisecond P95 numbers on this corpus as noise**, not signal.

## When to regenerate

Regenerate the baseline after **any** change to:

- `rag.Retriever`, `rag.SQLiteStore.Search`, or `rag.SQLiteStore.SearchMulti`
- Any scorer under `rag/scorer_*.go` (semantic, keyword, structural, temporal)
- `rag.Retriever.BuildContext` formatting (affects `context_tokens` estimate)
- `rag.QueryContext` shape or temporal-scorer defaults
- The fixtures themselves (chunks, queries, embeddings, categories)

`TestBaselineReportShape` asserts the committed file is well-formed. If you
change retriever behavior without regenerating, that test still passes but
the committed numbers no longer reflect what the code produces. Don't skip
the regeneration step.

## Tests

```sh
go test ./internal/rageval ./cmd/rag-eval
```

Notable invariants:

- `TestFixtureValidate` — open-set category check: every query category
  must be in the allowed set, and each allowed category must have ≥ 4
  queries.
- `TestFixtureValidateRejectsDuplicateQueryText` — fixture embedder relies
  on exact-text lookup, so query-text uniqueness is a hard requirement.
- `TestFixtureEmbedderRejectsUnknownQuery` — locks the error shape produced
  when the fixture's exact-match contract is violated.
- `TestRunFixtureWithoutLiveModel` — end-to-end smoke against both modes,
  no Ollama required.
- `TestRunModeHonorsZeroWarmRuns` — confirms `WarmRuns: 0` means zero warm
  runs (no implicit default).
- `TestBaselineReportShape` — committed baseline parses cleanly into
  `Report`, has the ratified threshold posture, has both modes present.
- `TestBaselineReproducible` — re-runs `Run` and asserts all deterministic
  summary fields match the committed `baseline.json` within 1e-9 tolerance.
  Catches code-vs-baseline drift (someone changed a scorer without
  regenerating) AND baseline-vs-code drift (someone edited the JSON by
  hand). Latency fields are deliberately excluded — they vary run-to-run.
  On a drift failure, run `go generate ./internal/rageval` if the change
  was intentional.
