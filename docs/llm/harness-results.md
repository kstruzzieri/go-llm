# Harness Results — Latest Accepted

> Index of accepted benchmark runs. Every entry links to a dated, immutable
> artifact under `docs/llm/benchmarks/`. Once an accepted run exists, the top
> entry is what `recommendation.md` currently cites.

## Acceptance gate

A benchmark run is "accepted" iff **all** of:

1. **Commit is clean** (`dirty: no` in provenance).
2. **Calibration PASS** — exact categorical agreement ≥85% on ≥50 valid
   labeled artifacts, borderline/fail agreement ≥80% when present, no known
   subtle-bug fixture judged as `1.0`, and the old tolerance diagnostic
   reported for context.
3. **Judge cache hit-rate is reported** (any value; reproducibility signal).
4. **`ToolArgsValid` is `computed=true` for ≥80% of (model, trace) pairs.**
5. **Trace set manifest hash is recorded.**
6. **Conclusion has been written and reviewed by the repo owner.**

`harness-results.md` only indexes runs meeting all six. Failed/inconclusive
runs stay as artifacts in `benchmarks/` but aren't promoted.

## Accepted runs (newest first)

No accepted runs yet. The first accepted-run PR replaces this paragraph with
the newest run entry.
