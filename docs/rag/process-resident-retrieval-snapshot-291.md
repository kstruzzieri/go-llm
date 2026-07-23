# Process-resident retrieval snapshot (#291)

This report compares the immutable SQLite retrieval snapshot from #291 with the
file-backed contract established by #289. It preserves the benchmark corpus,
persisted JSON schema, ranking weights, query, `k=5`, sampling method, and host
class documented in `file-backed-retrieval-baseline-289.md`.

## Implementation

`OpenSQLiteStoreReadOnly` now initializes a snapshot lazily on the first probe
or search. Exactly one load runs at a time; concurrent callers share that
attempt. The load is decoupled from the initiating request: it runs on its own
goroutine with cancellation stripped (`context.WithoutCancel`), so a canceled
or deadline-expired initiator gets its own context error back while the shared
load still completes and warms the process for every other caller. A completed
snapshot is published atomically. A failed attempt publishes nothing, returns
the same failure to its waiters, and permits a later call to retry.

The snapshot is scoped to the lifetime of one immutable `SQLiteStore` and holds:

- lean ranking chunks: ID, source, line range, language, and stable key;
- `indexed_at` values and the generation-stable vector-space probe result;
- one rowid-ordered, contiguous, normalized `float64` vector array.

The representative 1,401 × 4,096 vector payload is exactly 45,907,968 bytes
(43.78 MiB), plus lean chunk strings/slices. Full content and metadata are not
retained. Selected rowids are hydrated from SQLite in batches of at most 500,
so the common top-five query decodes only five metadata payloads. The SQLite
file remains 115,122,176 bytes and its persisted embedding JSON is unchanged.

Dense and hybrid selection use a bounded worst-first heap. The comparison key
is score descending, then original row order ascending, which gives stable,
deterministic ties. Hybrid continues to compute the same semantic, keyword,
temporal, structural, and optional behavioral signals, the same tied ranks,
RRF contributions, and bonus weights; only fused finalists are materialized as
`ScoredResult`. `Score` remains semantic cosine similarity and `Distance` is
always `1-Score`.

Writable stores retain the prior SQL/JSON search, probe, indexing, export, and
migration paths. Snapshot behavior is enabled only by
`OpenSQLiteStoreReadOnly`.

## Method

- Host: Apple M3 Max, Darwin 25.5.0, arm64, 16 logical benchmark workers.
- Runtime: Go 1.26.1 and `modernc.org/sqlite`.
- Corpus: 1,401 deterministic chunks across 138 sources, 4,096 dimensions.
- Sampling: one operation per sample, five samples in one process; medians
  below. Each warm path performs one untimed warmup.
- Command:

```sh
GO_LLM_RAG_FILE_BENCH=1 \
GO_LLM_RAG_FILE_BENCH_DIMS=4096 \
go test ./rag/ -run '^$' -bench '^BenchmarkFileBackedRAG$' \
  -benchmem -benchtime=1x -count=5
```

## Before and after

| path | #289 median | #291 median | #289 bytes/op | #291 bytes/op | #291 allocs/op |
|---|---:|---:|---:|---:|---:|
| open read-only | 0.191 ms | 0.267 ms | 5,528 | 5,784 | 63 |
| dense cold open + query | 976.507 ms | 1,021.116 ms | 526,676,064 | 570,894,568 | 63,571 |
| dense warm | 980.353 ms | 6.604 ms | 526,671,112 | 41,816 | 223 |
| hybrid cold open + query | 956.860 ms | 1,032.126 ms | 527,696,624 | 571,214,840 | 63,641 |
| hybrid all-signals warm | 974.812 ms | 7.640 ms | 527,691,224 | 362,408 | 294 |
| retriever hybrid warm | 978.579 ms | 7.552 ms | 527,693,000 | 363,120 | 300 |
| context construction | 0.014 ms | 0.014 ms | 6,576 | 6,576 | 51 |

Warm dense is about 148× faster and allocates about 12,595× fewer bytes. Warm
hybrid and retriever are about 128× and 130× faster and allocate about 1,456×
and 1,453× fewer bytes. Connection-cold latency is intentionally similar: it
performs the one-time persisted JSON load and constructs the resident payload.
Cold total allocation rises by about 43 MiB because the normalized resident
vectors survive the transient JSON decode; that cost is paid once per immutable
reader rather than once per query.

Raw `time/op` samples in milliseconds:

| path | five samples |
|---|---|
| open | 0.374, 0.267, 0.203, 0.290, 0.202 |
| dense cold | 1,016.251, 1,031.367, 1,022.813, 1,021.116, 1,012.834 |
| dense warm | 6.837, 6.604, 6.491, 6.851, 6.483 |
| hybrid cold | 1,018.347, 1,021.978, 1,032.126, 1,059.416, 1,074.927 |
| hybrid warm | 7.775, 7.640, 7.506, 7.663, 7.577 |
| retriever warm | 7.584, 7.430, 7.552, 7.590, 7.444 |
| context construction | 0.019, 0.019, 0.012, 0.013, 0.014 |

Median warm hybrid stage attribution is approximately 6.47 ms semantic
scoring, 0.42 ms fused ranking/top-K, 0.41 ms behavioral scoring, 0.17 ms
keyword scoring, 0.08 ms structural scoring, and 0.09 ms finalist hydration.
Vector-space validation and resident temporal scoring are below 0.01 ms. The
former ~950 ms corpus-load stage is absent after warmup.

## Verification and budgets

Focused differential tests compare writable and immutable hybrid results for
IDs, hydrated chunks/metadata, order, rank score, semantic score/distance, and
every signal including behavioral weighting. Additional tests cover one-time
loading, cached probe copies, deterministic ties/top-K equivalence, mixed
dimensions, decode failure/retry, initiator cancellation during a shared load
(other callers still succeed), cancellation during scoring, and concurrent
searches. The race-enabled RAG suite passes.

Every proposed #289 budget is met:

- warm dense/hybrid and retriever are below 7.8 ms in every sample,
  versus the 1.30/1.35 s proposals;
- cold dense/hybrid remain below 1.08 s, versus 1.35 s;
- cold allocation is about 544.8 MiB and 63,641 allocations at worst by median,
  below 600 MiB and 110,000 allocations;
- read-only open is 0.267 ms and context construction is 0.014 ms, below
  1 ms and 0.10 ms.

## Compatibility and follow-on readiness

The main compatibility risk is floating-point roundoff from pre-normalizing
vectors once rather than recomputing both magnitudes for every query. The
differential fixtures hold semantic and fused values within `1e-12`, preserve
exact result IDs/order, and explicitly pin equal-score row-order ties. No public
interface or persisted value changes.

#292 can build generation publication/swapping around this store-scoped
snapshot: opening a new immutable generation naturally creates an independent
snapshot, while an existing reader remains stable. #293 is not required for
warm-query performance. It remains useful if connection-cold startup and the
roughly 545 MiB one-time JSON decode allocation need improvement; a packed
persisted representation could reduce that load without changing the resident
retrieval seam.
