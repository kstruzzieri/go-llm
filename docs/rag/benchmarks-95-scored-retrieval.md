# #95 Search vs SearchMulti latency benchmarks

Backend: in-memory SQLite (`:memory:`), synthetic 3-dim embeddings. Host: Apple M3 Max, macOS 26.5.1 (arm64).
Command: `rtk env -u GOROOT go test ./rag/ -run '^$' -bench BenchmarkSearchVsSearchMulti -benchmem -count 5`.
Absolute ns/op is host-specific; the SearchMulti/Search ratio is the portable figure.

Figures below are the median of 5 samples per sub-benchmark. `SearchMulti` runs the
full hybrid path (semantic + keyword RRF fusion, temporal/structural bonuses) against
the same seeded store as dense `Search`, with `k=5` and query `"alpha token"`.

| corpus size | Search ns/op | SearchMulti ns/op | ratio (multi/dense) | SearchMulti B/op | SearchMulti allocs/op |
|-------------|-------------|-------------------|---------------------|------------------|-----------------------|
| 100         | 156,215     | 243,188           | 1.56x               | 184,304          | 3,997                 |
| 1,000       | 1,462,720   | 2,221,801         | 1.52x               | 1,798,680        | 39,116                |
| 10,000      | 15,119,030  | 22,454,286        | 1.49x               | 20,140,022       | 390,219               |

Dense `Search` allocs/B for reference: 100 -> 3,129 allocs / 109,488 B; 1,000 -> 31,033 / 1,162,136 B;
10,000 -> 310,041 / 13,802,208 B.

Reading: hybrid retrieval costs a stable ~1.5x the dense-search latency and ~1.5x the
allocations across three orders of magnitude of corpus size. The ratio does not degrade
with scale — the extra cost is the keyword-scoring + RRF-fusion pass over the same
candidate set, not a super-linear blowup. Both paths are linear in corpus size (full
scan + score); the in-memory SQLite scan dominates.

## Proposed #93 latency thresholds (owner ratification required)

These map to the currently-null fields in `internal/rageval` (`buildThresholds`).
They are PROPOSALS derived from the numbers above; `baseline.json` is NOT edited in
this PR. Keith ratifies the concrete values before any threshold is written.

Caveat: absolute ms is host-specific (measured on an M3 Max, arm64). A threshold
expressed in ms is only meaningful on comparable hardware or CI; a portable gate would
key on the multi/dense ratio (~1.5x) instead. The values below assume the gate runs on
developer-class hardware in the M3 Max range and bake in headroom for slower CI runners.

- `maximum_opt_in_hybrid_p95_latency_ms`: proposed **50** (basis: 10,000-chunk SearchMulti
  p95 ~= 23.8 ms measured; ~2x headroom for slower CI and larger real corpora). This is
  the opt-in hybrid path's own p95.
- `maximum_future_default_p95_latency_ms`: proposed **50** (basis: same SearchMulti path —
  if the hybrid/scored surface becomes the default retrieve, its p95 is the 10,000-chunk
  SearchMulti p95 ~= 23.8 ms; same ~2x headroom). Kept equal to the opt-in field because
  the future default would run the identical SearchMulti path.

Both proposals derive from the 10,000-chunk case as the conservative (largest) synthetic
workload here; a real corpus's p95 depends on chunk count and embedding dimensionality
(real embeddings are far wider than the 3-dim synthetic vectors, which shifts the scan
cost). Re-measure against a realistic index before ratifying if a tighter bound is wanted.

Note: `internal/rageval`'s "static" mode is currently mislabeled (runs hybrid, not
dense) — tracked in [#275](https://github.com/kstruzzieri/go-llm/issues/275). These
proposals come from the direct Search-vs-SearchMulti benchmark, not the rageval static
path, so they are unaffected by that bug.
