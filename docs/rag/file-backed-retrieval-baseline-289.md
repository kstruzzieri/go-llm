# File-backed retrieval baseline (#289)

This report establishes a deterministic, production-representative baseline for
the current SQLite/JSON retrieval implementation. It measures; it does not
optimize retrieval or ratify release thresholds.

## Method

- Code under measurement: `e6d74acf6c0a8ab150fda572cf3fd2d60860e00a`
  (`origin/develop` after #290), plus this PR's nil-by-default stage
  instrumentation. Instrumentation is inert in the uninstrumented paths.
- Host: Apple M3 Max, Darwin 25.5.0, arm64, 16 logical benchmark workers.
- Runtime: Go 1.26.1, `modernc.org/sqlite`, immutable read-only SQLite opens.
- Corpus: 1,401 chunks across 138 sources, matching the observed Golem corpus
  size in #289. Chunk IDs, source paths, content, metadata, stable keys, and
  vectors are generated deterministically.
- Vectors: xorshift64-derived `float64` values in `[-1,1)`, persisted through
  the production JSON encoding. Cases cover 768 and 4,096 dimensions.
- Query: deterministic seed `0`; hybrid query text is
  `find alpha retrieval token`, `k=5`, with current-file context and a
  deterministic non-zero behavioral weighter so every implemented signal runs
  on the store-level hybrid rows. The retriever rows call the production
  `Retriever.Retrieve`, whose signature carries no editor context, so the
  structural scorer is inert there by API design (it costs well under 0.1 ms
  either way). Every chunk's content deliberately contains the query tokens,
  so FTS matches the full corpus: keyword timing below is the conservative
  worst-case fan-out, not typical selectivity, and this corpus must not be
  reused for retrieval-quality benchmarks.
- Sampling: one query per benchmark sample (`-benchtime=1x`, `-count=5`, all
  five samples within one process); tables report the median. One untimed
  warmup query precedes every warm and stage-attribution sample; warmup stage
  timings are discarded.
- Cold means a new immutable SQLite connection plus its first query. The OS
  filesystem cache is not flushed, so this is connection-cold, not powered-off
  storage cold. `open_read_only` isolates connection open from corpus loading.
- Database size is the checkpointed main SQLite file. Generated databases live
  under the benchmark's temporary directory and are not checked in.

The opt-in gate keeps ordinary `go test ./...` lightweight:

```sh
GO_LLM_RAG_FILE_BENCH=1 \
  go test ./rag/ -run '^$' -bench '^BenchmarkFileBackedRAG$' \
  -benchmem -benchtime=1x -count=5
```

Limit a diagnostic run to one dimension with
`GO_LLM_RAG_FILE_BENCH_DIMS=4096`. Normal tests validate the 4,096-component
generator's determinism without creating the corpus.

## Store and retriever results

Median of five samples:

| dimensions | path | time/op | bytes/op | allocs/op | DB bytes |
|---:|---|---:|---:|---:|---:|
| 768 | open read-only | 0.187 ms | 5,096 | 61 | 23,306,240 |
| 768 | dense cold open + query | 196.158 ms | 92,521,472 | 76,207 | 23,306,240 |
| 768 | dense warm query | 196.154 ms | 92,516,408 | 76,146 | 23,306,240 |
| 768 | hybrid all-signals cold open + query | 195.372 ms | 93,543,056 | 87,525 | 23,306,240 |
| 768 | hybrid all-signals warm query | 194.765 ms | 93,537,928 | 87,463 | 23,306,240 |
| 768 | retriever hybrid warm | 196.844 ms | 93,539,912 | 87,506 | 23,306,240 |
| 768 | context construction, five finalists | 0.012 ms | 6,576 | 51 | 23,306,240 |
| 4,096 | open read-only | 0.191 ms | 5,528 | 61 | 115,122,176 |
| 4,096 | dense cold open + query | 976.507 ms | 526,676,064 | 83,214 | 115,122,176 |
| 4,096 | dense warm query | 980.353 ms | 526,671,112 | 83,154 | 115,122,176 |
| 4,096 | hybrid all-signals cold open + query | 956.860 ms | 527,696,624 | 94,531 | 115,122,176 |
| 4,096 | hybrid all-signals warm query | 974.812 ms | 527,691,224 | 94,469 | 115,122,176 |
| 4,096 | retriever hybrid warm | 978.579 ms | 527,693,000 | 94,510 | 115,122,176 |
| 4,096 | context construction, five finalists | 0.014 ms | 6,576 | 51 | 115,122,176 |

Cold/warm differences are smaller than sample variability because open itself
is below 1 ms and every query reloads and decodes the full corpus. Dense and
hybrid are the same full-scan-dominated workload at 4,096 dimensions. In this
collection hybrid ran slightly faster than dense (all five hybrid cold samples
sit below all five dense cold samples, roughly 2%); a plausible mechanism is
that dense constructs a content-carrying result for every row inside the scan
loop while hybrid hydrates and scores in separate passes. The gap is within
run-to-run drift between collections and does not change where the time goes,
so it carries no weight in the recommendation below.

Raw `time/op` samples in milliseconds, in collection order:

| dimensions | path | five samples (ms) |
|---:|---|---|
| 768 | open | 0.554, 0.187, 0.148, 0.194, 0.130 |
| 768 | dense cold | 196.174, 196.158, 192.004, 190.697, 211.130 |
| 768 | dense warm | 198.759, 204.869, 196.154, 190.732, 189.410 |
| 768 | hybrid cold | 194.094, 196.011, 193.206, 195.372, 196.560 |
| 768 | hybrid warm | 199.448, 193.163, 194.521, 195.030, 194.765 |
| 768 | retriever hybrid warm | 196.844, 196.101, 196.412, 198.570, 208.683 |
| 4,096 | open | 0.257, 0.213, 0.191, 0.158, 0.151 |
| 4,096 | dense cold | 976.507, 999.288, 975.443, 1,009.129, 974.669 |
| 4,096 | dense warm | 980.353, 972.122, 987.542, 982.464, 972.441 |
| 4,096 | hybrid cold | 956.860, 964.619, 954.807, 958.750, 956.417 |
| 4,096 | hybrid warm | 1,015.721, 986.662, 956.812, 974.812, 952.096 |
| 4,096 | retriever hybrid warm | 979.949, 965.180, 978.579, 980.317, 969.041 |

## Stage attribution

`SQLiteStore.recordStage` is an unexported, same-package benchmark hook. It is
nil in production. Disabled retrieval performs only coarse nil checks: no clock
reads, callbacks, maps, or allocations. The hook is unsynchronized and must be
invoked from a single goroutine; parallelizing retrieval stages in #291 must
revisit it. Stage-attribution sub-benchmarks are separate from the
uninstrumented warm latency results above.

Median stage time from five instrumented hybrid queries:

| stage | 768 dimensions | 4,096 dimensions |
|---|---:|---:|
| corpus SQL scan + vector JSON decode + candidate hydration | 186.773 ms | 929.758 ms |
| vector dimension validation/map | 0.029 ms | 0.035 ms |
| semantic scoring | 1.160 ms | 6.356 ms |
| keyword/FTS scoring | 0.192 ms | 0.205 ms |
| temporal scoring | 5.041 ms | 19.386 ms |
| structural scoring | 0.075 ms | 0.073 ms |
| behavioral scoring, including its rank fold | 0.401 ms | 0.373 ms |
| fusion + ranking + top-k | 0.661 ms | 0.661 ms |
| retriever vector-space probe/validation | 3.890 ms | 18.619 ms |
| deterministic query embedding adapter | <0.001 ms | <0.001 ms |
| context construction from five results | 0.012 ms | 0.014 ms |

The behavioral stage covers scoring plus computing its RRF rank list; the
fusion stage covers the semantic and keyword rank lists, RRF folding, bonus
weighting, sorting, and the top-k slice. The 4,096-dimensional
load/decode/hydration stage is about 95% of warm hybrid time. Semantic scoring
is about 0.7%. The current SQL query selects full chunk
content and metadata for every candidate, so the implementation has no distinct
"finalist hydration" phase: all 1,401 candidates are hydrated before scoring,
then the top five are sliced. Dense search similarly combines scan, decode,
candidate hydration, and semantic scoring in one row loop; its attribution
reports that combined stage rather than pretending they are separable.

## Proposed budgets (not binding)

These are review proposals for comparable M3 Max-class hardware, derived from
the five 4,096-dimensional samples. They are not added to `internal/rageval`,
CI, or any owner-ratified threshold file.

- Warm dense and all-signals hybrid store query: **≤ 1.30 s/op**.
- Connection-cold open plus first dense/hybrid query: **≤ 1.35 s/op**.
- Retriever hybrid warm, including vector-space validation but excluding a
  live embedding service: **≤ 1.35 s/op**.
- Retrieval allocation: **≤ 600 MiB/op and ≤ 110,000 allocs/op**.
- Read-only open: **≤ 1 ms/op**; context construction: **≤ 0.10 ms/op**.

The latency proposals leave roughly 28–38% over the largest observed sample in
this collection; the allocation proposals leave roughly 19% over the ~503 MiB
hybrid median.
They should be ratified only after deciding which host class and workload CI
will enforce.

## Recommendation for #291

Start #291 with a resident decoded snapshot focused on removing repeated
full-corpus SQL reads and JSON decoding. The evidence does not support ANN,
ranking changes, or scorer optimization as the first move: at 4,096 dimensions,
load/decode/all-candidate hydration dominates, while cosine scoring and fusion
are single-digit milliseconds. Preserve the current exact ordering and compare
top IDs against the existing deterministic quality fixtures before pursuing
later generation swapping (#292) or packed persistence (#293).
