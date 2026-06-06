<!--
Per spec docs/superpowers/specs/2026-05-25-harness-followups-design.md §5.2:
Do NOT paste raw prompts, transcripts, judge justifications, or error
messages into this file. The harness report renderer sanitizes its own
output; manual fills must obey the same rules. Sensitive content
sources: per-trace Notes, per-trace Errors, local paths, judge debug
output.
-->

# <slug> — YYYY-MM-DD

## Provenance
- **Commit**: `<sha>` (dirty: no)
- **go-llm version**: <module version or commit>
- **Machine**: <hardware profile, e.g. M3 Max 128GB>
- **Trace set**: `<label>`, count: N, manifest hash: `sha256:...`
- **Models under test**: `<comma list, provider/name>`
- **Scorer**: `<llm-judge | manual>`
  - If `llm-judge`:
    - Judge provider: `<ollama | openai-compat:<endpoint-id> | claude-cli>`
    - Judge model: `<name>`
    - Judge model digest (when available via /api/show): `<digest>`
    - Judge cache hit rate: X%
    - Judge cache key version: V
  - If `manual`:
    - Label manifest hash: `sha256:...`
    - Artifact manifest hash: `sha256:...`
    - Valid labeled artifacts: X (≥50 required → SUFFICIENT)
    - Paired label coverage: X/Y retained traces complete (≥20 complete
      traces required → SUFFICIENT)
    - Stale/missing labels: N stale, N missing
    - Labeler / reviewer: `<name> / <name>`
- **Exact command** (match the declared scorer path):
  ```
  # llm-judge path:
  llm-bench -traces '<glob>' -models '...' -scorer llm-judge -judge-model ... \
    -judge-transport <ollama|openai-compat|claude-cli> -judge-cache <path> -report <path>

  # manual-label path, quality from human labels over frozen artifacts:
  llm-bench -manual-report -labels <labels.jsonl> -artifacts <artifacts.jsonl> -report <path>

  # manual-label path, separate replay metrics over the same trace set:
  llm-bench -traces '<glob>' -models '...' -scorer exact-match -report <path>
  ```

## Calibration / Labeling (embedded — not linked to a gitignored file)
- Judge model: `<name>`
- Labels manifest hash: `sha256:...`
- Valid labeled artifacts: X (≥50 required → SUFFICIENT)
- Agreement: X / Y (Z%) — overall ≥85%, borderline/fail ≥80% when present,
  known subtle-bug fixtures pass → **PASS**
- Stability runs (M=3, diagnostic): max spread observed: 0.NN
- Manual-label path: paired labels complete for X/Y retained traces; stale and
  missing labels excluded and reported; ≥50 valid labeled artifacts and ≥20
  complete retained traces.

## Results
| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | LatencyMs (p50 / p90) | TotalTokens | n |
| --- | --- | --- | --- | --- | --- | --- |

Partitioned results:
- Natural set: reported separately.
- Challenge/discrimination set: reported separately.
- Do not average natural and challenge results into one headline.

Paired statistics:
- Per-trace win/loss/tie table included or linked.
- Confidence / resolution diagnostic: `<bootstrap/permutation CI, or one-label-flip sensitivity>`.

Tool-call subset:
- Expected-tool-call traces: N
- `ToolArgsValid` coverage on expected-tool-call pairs: X/Y
- Overall `ToolArgsValid` computed coverage: X/Y

## Errors and exclusions
- N traces failed (categorized by sanitized reason; raw messages live in the gitignored run log)
- N traces excluded from `ToolArgsValid` (computed=false)
- Timeout/error rate by model: `<table or bullets>`
- Latency policy: `<capped-in timeouts | successful-only; if successful-only, state that p90 is optimistic>`

## Conclusion
- **Verdict**: accept / propose change to <X> / inconclusive
- **Quality delta vs prior accepted run**: ±N on AnswerQuality.p50
- **Latency delta vs prior accepted run**: ±N ms on LatencyMs.p90
- **Justification**: 1–3 sentences (no PII, no raw trace content)

## Caveats
- Flags or trace-set anomalies that limit generalization
