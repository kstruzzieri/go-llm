# File-backed retrieval baseline (#289)

This report establishes a deterministic, production-representative baseline for
the current SQLite/JSON retrieval implementation. It measures; it does not
optimize retrieval or ratify release thresholds.

## Method

- Commit: `e6d74acf6c0a8ab150fda572cf3fd2d60860e00a` (`origin/develop` after #290).
- Host: Apple M3 Max, Darwin 25.5.0, arm64, 16 logical benchmark workers.
- Runtime: Go 1.26.1, `modernc.org/sqlite`, immutable read-only SQLite opens.
- Corpus: 1,401 chunks across 138 sources, matching the observed Golem corpus
  size in #289. Chunk IDs, source paths, content, metadata, stable keys, and
  vectors are generated deterministically.
- Vectors: xorshift64-derived `float64` values in `[-1,1)`, persisted through
  the production JSON encoding. Cases cover 768 and 4,096 dimensions.
- Query: deterministic seed `0`; hybrid query text is
  `find alpha retrieval token`, `k=5`, with current-file context and a
  deterministic non-zero behavioral weighter so every implemented signal runs.
- Sampling: one query per process benchmark sample (`-benchtime=1x`), five
  samples (`-count=5`); tables report the median. One untimed warmup query
  precedes every warm store/retriever sample.
- Cold means a new immutable SQLite connection plus its first query. The OS
  filesystem cache is not flushed, so this is connection-cold, not powered-off
  storage cold. `open_read_only` isolates connection open from corpus loading.
- Database size is the checkpointed main SQLite file. Generated databases live
  under the benchmark's temporary directory and are not checked in.

The opt-in gate keeps ordinary `go test ./...` lightweight:

```sh
rtk env -u GOROOT GO_LLM_RAG_FILE_BENCH=1 \
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
| 768 | open read-only | 0.231 ms | 4,872 | 60 | 23,306,240 |
| 768 | dense cold open + query | 203.340 ms | 92,521,872 | 76,209 | 23,306,240 |
| 768 | dense warm query | 205.812 ms | 92,516,600 | 76,147 | 23,306,240 |
| 768 | hybrid all-signals cold open + query | 207.233 ms | 93,543,392 | 87,526 | 23,306,240 |
| 768 | hybrid all-signals warm query | 208.234 ms | 93,538,224 | 87,466 | 23,306,240 |
| 768 | retriever hybrid warm | 214.006 ms | 93,540,208 | 87,509 | 23,306,240 |
| 768 | context construction, five finalists | 0.014 ms | 6,576 | 51 | 23,306,240 |
| 4,096 | open read-only | 0.259 ms | 5,048 | 60 | 115,122,176 |
| 4,096 | dense cold open + query | 1,055.741 ms | 526,676,496 | 83,216 | 115,122,176 |
| 4,096 | dense warm query | 1,053.116 ms | 526,671,256 | 83,156 | 115,122,176 |
| 4,096 | hybrid all-signals cold open + query | 1,011.108 ms | 527,696,752 | 94,529 | 115,122,176 |
| 4,096 | hybrid all-signals warm query | 1,012.162 ms | 527,691,304 | 94,470 | 115,122,176 |
| 4,096 | retriever hybrid warm | 1,033.269 ms | 527,693,160 | 94,512 | 115,122,176 |
| 4,096 | context construction, five finalists | 0.018 ms | 6,576 | 51 | 115,122,176 |

Cold/warm differences are smaller than sample variability because open itself
is below 0.5 ms and every query reloads and decodes the full corpus. Dense and
hybrid results are therefore effectively the same full-scan workload at 4,096
dimensions; their small ordering reversals are noise, not evidence that hybrid
is faster.

Raw `time/op` samples in milliseconds, in collection order:

| dimensions | path | five samples (ms) |
|---:|---|---|
| 768 | open | 0.400, 0.228, 0.231, 0.237, 0.179 |
| 768 | dense cold | 202.951, 203.340, 202.245, 207.325, 205.136 |
| 768 | dense warm | 205.812, 201.970, 210.739, 201.878, 210.625 |
| 768 | hybrid cold | 207.186, 208.430, 207.969, 205.439, 207.233 |
| 768 | hybrid warm | 211.635, 208.234, 210.037, 206.612, 202.740 |
| 768 | retriever hybrid warm | 214.006, 211.664, 218.280, 214.682, 212.287 |
| 4,096 | open | 0.463, 0.306, 0.188, 0.259, 0.247 |
| 4,096 | dense cold | 1,036.758, 1,228.347, 1,050.644, 1,055.741, 1,057.212 |
| 4,096 | dense warm | 1,053.116, 1,046.070, 1,043.534, 1,053.659, 1,068.287 |
| 4,096 | hybrid cold | 1,011.108, 1,016.541, 1,039.050, 1,003.639, 1,001.327 |
| 4,096 | hybrid warm | 1,024.174, 1,143.733, 1,012.060, 1,007.228, 1,012.162 |
| 4,096 | retriever hybrid warm | 1,035.211, 1,041.402, 1,033.269, 1,026.362, 1,005.380 |

## Stage attribution

`SQLiteStore.recordStage` is an unexported, same-package benchmark hook. It is
nil in production. Disabled retrieval performs only coarse nil checks: no clock
reads, callbacks, maps, or allocations. Stage-attribution sub-benchmarks are
separate from the uninstrumented warm latency results above.

Median stage time from five instrumented hybrid queries:

| stage | 768 dimensions | 4,096 dimensions |
|---|---:|---:|
| corpus SQL scan + vector JSON decode + candidate hydration | 203.107 ms | 985.329 ms |
| vector dimension validation/map | 0.043 ms | 0.037 ms |
| semantic scoring | 1.305 ms | 6.664 ms |
| keyword/FTS scoring | 0.268 ms | 0.290 ms |
| temporal scoring | 6.265 ms | 21.363 ms |
| structural scoring | 0.076 ms | 0.078 ms |
| behavioral scoring | 0.085 ms | 0.084 ms |
| fusion + ranking + top-k | 1.063 ms | 1.013 ms |
| retriever vector-space probe/validation | 5.123 ms | 20.562 ms |
| deterministic query embedding adapter | <0.001 ms | <0.001 ms |
| context construction from five results | 0.014 ms | 0.018 ms |

The 4,096-dimensional load/decode/hydration stage is about 97% of warm hybrid
time. Semantic scoring is about 0.7%. The current SQL query selects full chunk
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

The latency proposals leave roughly 15–30% over the largest observed sample;
the allocation proposals leave roughly 19% over the ~503 MiB hybrid median.
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
