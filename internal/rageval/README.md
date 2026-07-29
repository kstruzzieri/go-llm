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
go run ./cmd/rag-eval \
  -experiment outline \
  -dimensions 768 \
  -candidate-m 50 \
  -samples 5 \
  -out /private/tmp/go-llm-246-outline-report.json
```

`-samples` sets the outline measured-sample count (default 5); `-warm-runs` sets
the baseline warm-run count (default 3). For back-compatibility `-warm-runs` is
still accepted as a deprecated alias for `-samples` in outline mode when
`-samples` is not given. Every experiment requires an explicit `-out` path, so
no invocation can overwrite the committed baseline.
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
`source_path_precision`, and final formatted-context tokens at K=5 and K=10.
`source_path_precision` is only the fraction of returned chunks whose source
path is one of the expected support paths. It is a retrieval proxy; no
generated citation or answer quality is measured.

Cost fields are P50/P95 latency, bytes and allocations, and four distinct work
counters: `candidates_inspected` is the unique candidate records consulted
before final hydration (lean rows in the snapshot/planning adapters, full rows
in the full-corpus mode); `ranked_candidates` is the upstream final scoring
set; `hydrated_content_chunks` is every full-content row loaded by the adapter;
and `post_retrieval_candidates_inspected` counts a separate downstream
selection stage. Hierarchical retrieval ranks all 1,401 chunks upstream, then
post-inspects and hydrates 50. Modes without a post-retrieval stage report zero.
`deterministic_ordering` checks repeated result-ID order. `planning_tokens` is
`null` because all selectors are deterministic and model-free.

This is a generated 1,401-chunk, 138-source corpus persisted to a temporary
SQLite file. Its embeddings and queries are deterministic, so it isolates
retrieval behavior without representing live-repository relevance, production
concurrency, or model answer quality. Allowed outline token sets are precomputed
once before measurements; their build cost and retained memory, like
long-lived persistent-memory pressure, are excluded. The raw JSON contains
measurements only and no recommendation or conclusion. See the measured
[issue #246 report](../../docs/rag/outline-retrieval-eval-246.md).

## Progressive rendering experiment (#331)

Run with:

```sh
go run ./cmd/rag-eval \
  -experiment progressive \
  -dimensions 768 \
  -out internal/rageval/testdata/progressive-baseline.json
```

This experiment measures flat `Retriever.BuildContext` against
`Retriever.RenderProgressive` at equal budget over the same outline fixture
corpus used by `#246`. Retrieval selection runs once per query so both arms
render the identical result slice (`candidate_sets_equal` records this rather
than asserting it). Fresh summaries are seeded on the even-indexed half of the
corpus sources before rendering; the other half exercises the deterministic
metadata-overview fallback, so `summary.total_metadata_fallback` should always
be nonzero on a passing regeneration.

On this fixture `summary.mean_token_reduction` is negative (~-0.116):
`total_omitted_sources` is 0 across the corpus (the 512-token budget never
binds), and 16 of 20 queries select all top-10 chunks from a single source, so
progressive rendering only adds its orientation block and v2 headers on top of
content the flat renderer already emits in full — pure overhead in this
regime. That is a mechanical property of this fixture's selection, not a
quality verdict on progressive rendering: the depth-for-coverage trade the
renderer exists for needs either a `MaxTokens` budget below flat's natural
~445-token size, or a selection spanning more distinct sources, to actually
engage. Relatedly, the one query with `sources_at_l1 > 0` is a reachability
indicator, not a measurement: L1 can only appear on the 4 multi-source
(`distributed_support`) queries, and the fixture is intentionally thin there;
broadening it is out of scope for this baseline.

`contextPrecision` is deliberately not claimed for this experiment: the frozen
`BuildContext` exposes no emitted-result trace to compute it against (spec
3.8), so only token/byte reduction and progressive-arm trace counters
(`sources_at_l0`, `sources_at_l1`, `sources_with_evidence`, `omitted_sources`)
are reported.

Like the outline baseline, every experiment requires an explicit `-out` path,
so no invocation can overwrite the committed baseline by accident.
