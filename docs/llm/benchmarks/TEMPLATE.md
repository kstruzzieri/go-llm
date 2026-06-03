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
- **Scorer**: `llm-judge`
  - Judge provider: `<ollama | openai-compat:<endpoint-id> | claude-cli>`
  - Judge model: `<name>`
  - Judge model digest (when available via /api/show): `<digest>`
  - Judge cache hit rate: X%
  - Judge cache key version: V
- **Exact command**:
  ```
  llm-bench -traces '<glob>' -models '...' -scorer llm-judge -judge-model ... \
    -judge-cache <path> -report <path>
  ```

## Calibration (embedded — not linked to a gitignored file)
- Judge model: `<name>`
- Labels manifest hash: `sha256:...`
- Valid labeled artifacts: X (≥50 required → SUFFICIENT)
- Agreement: X / Y (Z%) — overall ≥85%, borderline/fail ≥80% when present,
  known subtle-bug fixtures pass → **PASS**
- Stability runs (M=3, diagnostic): max spread observed: 0.NN

## Results
| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolSequenceMatch | ToolArgsValid (computed=N) | LatencyMs (p50 / p90) | TotalTokens | n |
| --- | --- | --- | --- | --- | --- | --- |

## Errors and exclusions
- N traces failed (categorized by sanitized reason; raw messages live in the gitignored run log)
- N traces excluded from `ToolArgsValid` (computed=false)

## Conclusion
- **Verdict**: accept / propose change to <X> / inconclusive
- **Quality delta vs prior accepted run**: ±N on AnswerQuality.p50
- **Latency delta vs prior accepted run**: ±N ms on LatencyMs.p90
- **Justification**: 1–3 sentences (no PII, no raw trace content)

## Caveats
- Flags or trace-set anomalies that limit generalization
