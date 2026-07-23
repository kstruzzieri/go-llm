# Harness Results — Latest Accepted

> Index of accepted benchmark runs. Every entry links to a dated, immutable
> artifact under `docs/llm/benchmarks/`. Once an accepted run exists, the top
> entry is what `recommendation.md` currently cites.

## Acceptance gate

A benchmark run is "accepted" iff **all** of:

1. **Commit is clean** (`dirty: no` in provenance).
2. **Quality scorer provenance is complete**, using exactly one of:
   - **Automated judge path:** Calibration PASS — exact categorical agreement
     ≥85% on ≥50 valid labeled artifacts, borderline/fail agreement ≥80%
     when present, no known subtle-bug fixture judged as `1.0`, the old
     tolerance diagnostic reported for context, and Judge cache hit-rate
     reported (any value; reproducibility signal). Judge stability is reported
     and passed: either the judge transport is temperature-pinned/deterministic,
     or a documented multi-vote/stability policy passes. Single-draw unpinned
     judge runs are diagnostic only.
   - **Manual-label path:** human labels are fully paired across the retained
     candidate lineup, with at least 50 valid labeled artifacts and at least
     20 fully paired retained traces. Label/artifact manifest hashes are
     recorded, stale or missing labels are excluded and reported, and the
     labeler/reviewer is named in provenance.
3. **Tool-call evidence is non-vacuous when tool-use is claimed.**
   `ToolArgsValid` coverage is reported both overall and on the expected
   tool-call subset. For accepted tool-use benchmarks, `ToolArgsValid` is `computed=true`
   for ≥80% of expected-tool-call (model, trace) pairs. Rows
   where no tool calls were expected or made may still report `computed=true`,
   but they do not count as evidence of tool-calling correctness.
4. **Failure-aware latency is reported** — timeout/error rate per model is
   shown, and the latency table states whether timeouts are capped-in or
   successful-only.
5. **Trace set manifest hash is recorded.**
6. **Conclusion has been written and reviewed by the repo owner.**

`harness-results.md` only indexes runs meeting all six. Failed/inconclusive
runs stay as artifacts in `benchmarks/` but aren't promoted.

## Accepted runs (newest first)

### [2026-06-07 — balanced-lineup-manual-baseline](benchmarks/2026-06-07-balanced-lineup-manual-baseline.md)

- **Scope**: first accepted **plain-chat / manual baseline** for the Setup 1
  lineup. Validates the *lineup ranking on chat/code Q&A* only — **not** a
  tool-use or agent benchmark (zero tool-call traces), and **not** grounds for a
  `models.json` role change. Decision-grade tool/agent ranking is Round-2 work.
- **Method**: `manual` scorer (human labels, keith/keith), **80 valid labels**,
  **20 / 20 fully paired** traces, 0 stale; latency from a separate fresh replay
  on `3b37097` (`dirty: no`); trace-set manifest
  `sha256:64dd2a9f…ea6037`.
- **Quality (paired, n=20)**: gemma4:31b 1.00 · qwen3-coder-next 0.90 ·
  qwen3.6:35b-a3b 0.90 · qwen3:8b 0.78. coder≈qwen3.6 indistinguishable (3/3/14);
  gemma's +0.10 is CI-resolvable but **survivorship-biased** (top observed on the
  subset it completed; ≈0.87 completion-aware).
- **Latency (p50, successful-only)**: coder **17 s** · qwen3:8b 52 s ·
  qwen3.6 79 s · gemma **127 s** (slowest; 1 replay timeout).
- **Verdict**: accept as plain-chat/manual baseline; **keep the current lineup**.
  Tactical read (not a role change): prefer `qwen3-coder-next` for coding/dev-chat
  (near-top quality, best latency).
