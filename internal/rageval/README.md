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
  -out internal/rageval/testdata/baseline.json \
  -no-latency
```

Or from the package directory:

```sh
go generate ./internal/rageval
```

The committed baseline is generated with `-no-latency` so routine regeneration
is stable across machines. Omit `-no-latency` only for ad hoc local timing
experiments; do not commit those volatile latency samples.

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
`WarmRuns` repeats. Both report P50 and P95 in milliseconds when latency
measurement is enabled. The committed baseline disables latency measurement,
so these fields are stable zeros. **Treat sub-millisecond P95 numbers on this
corpus as noise**, not signal.

## When to regenerate

Regenerate the baseline after **any** change to:

- `rag.Retriever`, `rag.SQLiteStore.Search`, or `rag.SQLiteStore.SearchMulti`
- Any scorer under `rag/scorer_*.go` (semantic, keyword, structural, temporal)
- `rag.Retriever.BuildContext` formatting (affects `context_tokens` estimate)
- `rag.QueryContext` shape or temporal-scorer defaults
- The fixtures themselves (chunks, queries, embeddings, categories)

`TestBaselineReproducible` asserts the committed deterministic metrics match
the code. `TestBaselineReportShape` asserts the committed file is well-formed,
uses the exact ratified threshold values, and still satisfies those threshold
floors. Don't skip the regeneration step after intentional metric changes.

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
  `Report`, has the exact ratified threshold values, has both modes present,
  and satisfies the threshold floors.
- `TestBaselineReproducible` — re-runs `Run` and asserts all deterministic
  summary fields match the committed `baseline.json` within 1e-9 tolerance.
  Catches code-vs-baseline drift (someone changed a scorer without
  regenerating) AND baseline-vs-code drift (someone edited the JSON by
  hand). Latency fields are deliberately excluded — they vary run-to-run.
  On a drift failure, run `go generate ./internal/rageval` if the change
  was intentional.

## Outline retrieval experiment (#246)

Run the production-scale comparison with:

```sh
rtk go run ./cmd/rag-eval \
  -experiment outline \
  -dimensions 768 \
  -candidate-m 50 \
  -warm-runs 5 \
  -out /private/tmp/go-llm-246-outline-report.json
```

`-warm-runs` is the measured sample count in outline mode; it retains its
baseline warm-run meaning when `-experiment baseline` (the default) is used.
The outline report compares:

- `full_corpus_search_multi`: mutable `SearchMulti` over all corpus chunks.
- `resident_exact`: the #291 resident snapshot, exact scoring over all chunks
  with only the final results hydrated.
- `bounded_semantic_keyword_union`: semantic top-M plus non-zero FTS5 keyword
  top-M, stable-identity deduplication, then final scoring.
- `outline_then_content`: deterministic metadata-only top-M selection, final
  scoring, then content hydration.
- `hierarchical`: resident-exact retrieval of M hydrated candidates followed
  by the existing hierarchical post-retrieval policy.

Quality fields are recall, reciprocal rank (MRR), expected-source coverage,
citation/source accuracy, and final formatted-context tokens at K=5 and K=10.
Cost fields are P50/P95 latency, bytes and allocations, and three distinct work
counters: `candidates_inspected` is the unique candidate records consulted
before final hydration (lean rows in the snapshot/planning adapters, full rows
in the full-corpus mode); `ranked_candidates` is the final scoring or selection
set; `hydrated_content_chunks` is every full-content row loaded by the adapter.
`deterministic_ordering` checks repeated result-ID order. `planning_tokens` is
`null` because all selectors are deterministic and model-free.

This is a generated 1,401-chunk, 138-source corpus persisted to a temporary
SQLite file. Its embeddings and queries are deterministic, so it isolates
retrieval behavior without representing live-repository relevance, production
concurrency, or model answer quality. See the measured
[issue #246 report](../../docs/rag/outline-retrieval-eval-246.md).
