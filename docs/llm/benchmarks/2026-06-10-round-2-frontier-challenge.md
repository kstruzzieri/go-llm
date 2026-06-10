<!--
Per spec docs/superpowers/specs/2026-05-25-harness-followups-design.md section 5.2:
Do NOT paste raw prompts, transcripts, judge justifications, or error
messages into this file. Only sanitized aggregates + provenance. Raw traces,
labels, artifacts, worksheets, and run logs are gitignored.

STATUS: inconclusive Round-2 frontier-vs-local diagnostic. Not an accepted run
and not promoted to docs/llm/harness-results.md because the challenge partition
failed the de-saturation validity gate.
-->

# round-2-frontier-challenge - 2026-06-10

> **Verdict - read first.** This run is **inconclusive** for the
> frontier-vs-local question. GLM-5.1 reached 1.00 on the challenge partition,
> so the corpus failed the de-saturation gate and does not characterize GLM's
> ceiling. GLM's measured edge over the strongest local models is a one-trace
> difference, at the corpus resolution limit. The next useful step is a harder
> Round-3 challenge corpus, not latency or judge-calibration work on this
> saturated corpus.

## Provenance

- **Harness/docs commit during labeling**: `f1be673` on `develop`; workspace
  was dirty during the local labeling/doc pass, so this run is not eligible for
  accepted-run promotion.
- **Machine**: MacBook Pro M3 Max, 128 GB unified memory.
- **Trace set**: Round-2 natural + challenge evidence set.
  - Natural partition: **24** trace IDs, **119** scored artifacts.
  - Challenge partition: **18** trace IDs, **90** scored artifacts.
  - `tool-canary-01` is a judge-validation fixture and excluded from
    model-evidence math.
  - `gemma4:31b` x `conversation-fa-m01` timed out during capture, so the
    scored artifact count is **209**, not 210.
- **Corpus manifest hash**:
  `sha256:44204a6758dceb2ae3599f7698648c8fed2ac15a892c3fd5446d6c02a8abc200`
- **Models under test**:
  `ollama/gemma4:31b`, `ollama/qwen3-coder-next:latest`,
  `ollama/qwen3.6:35b-a3b`, `ollama/qwen3:8b`,
  `openai-compat/glm-5.1`.
- **Scorer**: `manual` via blind worksheet.
  - Artifact manifest hash:
    `sha256:1ecb611cc37631544ca8f99351ea7cf4035c368884df362c4175ffd4bfd6245f`
  - Label manifest hash:
    `sha256:faa0f99dd1aa3638ce3ce4bde4606814652eb44b5c8c7e6eaaa0b5e4b0b0dd96`
  - Valid labeled artifacts: **209**.
  - Stale / missing labels: **0 stale**, **0 unscored/skipped**.
  - Label score distribution: **173 x 1.0 / 28 x 0.5 / 8 x 0.0**.
  - Labeler: `manual`.
- **Latency**: not measured in this pass. Frozen artifacts carry no timing.

Exact commands used for the checked aggregates:

```bash
llm-bench -blind-ingest \
  -worksheet docs/llm/calibration/worksheet.round2.txt \
  -artifacts docs/llm/calibration/artifacts.round2.jsonl \
  -labels-out docs/llm/calibration/labels.round2.jsonl \
  -labeler manual

llm-bench -manual-report \
  -labels docs/llm/calibration/labels.round2.jsonl \
  -artifacts docs/llm/calibration/artifacts.round2.jsonl \
  -corpus-manifest docs/llm/traces/round2-challenge/corpus-manifest.jsonl \
  -corpus-partitions natural -corpus-only-evidence \
  -report docs/llm/calibration/q-natural.md

llm-bench -manual-report \
  -labels docs/llm/calibration/labels.round2.jsonl \
  -artifacts docs/llm/calibration/artifacts.round2.jsonl \
  -corpus-manifest docs/llm/traces/round2-challenge/corpus-manifest.jsonl \
  -corpus-partitions challenge -corpus-only-evidence \
  -report docs/llm/calibration/q-challenge.md

llm-bench -paired-report \
  -labels docs/llm/calibration/labels.round2.jsonl \
  -artifacts docs/llm/calibration/artifacts.round2.jsonl \
  -corpus-manifest docs/llm/traces/round2-challenge/corpus-manifest.jsonl \
  -corpus-partitions challenge -corpus-only-evidence \
  -baseline ollama/qwen3-coder-next:latest \
  -report docs/llm/calibration/paired-coder.md
```

## Validity Gates

| Gate | Verdict | Evidence |
| --- | --- | --- |
| No-trivia upper bound | PASS | Every challenge trace has at least one candidate scored 1.0. |
| De-saturation | FAIL | `openai-compat/glm-5.1` challenge mean is **1.00**. |
| Floor separation | PASS | `qwen3:8b` is lowest at **0.72**; gap to GLM is **0.28**, above the 0.15 bar. |

Because de-saturation failed, the challenge partition is too easy for the top
frontier candidate and cannot support a robust frontier-vs-local conclusion.

## Results

Quality is the human label (`expected_answer_quality`) over frozen artifacts.
Natural and challenge partitions are reported separately and are not averaged.

### Natural Partition

| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | n |
| --- | --- | --- |
| `ollama/gemma4:31b` | 0.96 / 1.00 / 1.00 / 1.00 / 1.00 | 23 |
| `ollama/qwen3-coder-next:latest` | 0.88 / 0.50 / 1.00 / 1.00 / 1.00 | 24 |
| `ollama/qwen3.6:35b-a3b` | 0.90 / 1.00 / 1.00 / 1.00 / 1.00 | 24 |
| `ollama/qwen3:8b` | 0.77 / 0.50 / 1.00 / 1.00 / 1.00 | 24 |
| `openai-compat/glm-5.1` | 0.94 / 1.00 / 1.00 / 1.00 / 1.00 | 24 |

The natural partition preserves the prior local-run shape: qwen8 remains the
floor, qwen3.6/coder remain near 0.90, and gemma remains near the top. This
supports serving-parity for the OpenAI-compatible GLM path, but it is not a
frontier decision by itself.

### Challenge Partition

| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | n |
| --- | --- | --- |
| `openai-compat/glm-5.1` | **1.00 / 1.00 / 1.00 / 1.00 / 1.00** | 18 |
| `ollama/gemma4:31b` | 0.97 / 1.00 / 1.00 / 1.00 / 1.00 | 18 |
| `ollama/qwen3-coder-next:latest` | 0.94 / 1.00 / 1.00 / 1.00 / 1.00 | 18 |
| `ollama/qwen3.6:35b-a3b` | 0.89 / 1.00 / 1.00 / 1.00 / 1.00 | 18 |
| `ollama/qwen3:8b` | 0.72 / 0.50 / 1.00 / 1.00 / 1.00 | 18 |

Paired challenge statistics with `qwen3-coder-next` as baseline:

| Model | mean delta vs coder | 95% CI | wins | losses | ties |
| --- | --- | --- | --- | --- | --- |
| `openai-compat/glm-5.1` | +0.06 | [+0.00, +0.17] | 1 | 0 | 17 |
| `ollama/gemma4:31b` | +0.03 | [+0.00, +0.08] | 1 | 0 | 17 |
| `ollama/qwen3.6:35b-a3b` | -0.06 | [-0.28, +0.11] | 1 | 2 | 15 |
| `ollama/qwen3:8b` | -0.22 | [-0.42, -0.06] | 0 | 5 | 13 |

Resolution diagnostic: over 18 paired challenge traces, one full label flip
(0 <-> 1) moves a model mean by **0.06**; one rubric step (0.5) moves it by
**0.03**. GLM's measured +0.06 edge over coder is exactly one full-label unit
on this corpus.

## Signal Diagnosis

- **12 / 18 challenge traces are saturated**: every candidate scored 1.0.
  They validate solvability but provide no ranking signal.
- **Only 6 / 18 challenge traces discriminate at all**, and most of that
  discrimination separates `qwen3:8b` from the field rather than separating
  the frontier from the strongest local models.
- **GLM's frontier edge is single-trace-fragile**:
  `r2c-subtle-correctness-01` is the only GLM-vs-coder and GLM-vs-gemma
  separating trace. GLM ties coder and gemma on the other 17 challenge traces.
- **GLM's true ceiling is uncharacterized** because the top candidate is fully
  saturated at 1.00.

## R7 / R8 Deferral

- **R7 local-judge calibration** is deferred. It gates a future Round-3 judge
  path, but this Round-2 corpus already failed de-saturation under manual
  labels. Calibrating a judge against a saturated corpus would not change the
  quality verdict.
- **R8 latency pass** is deferred. Latency cannot rescue a quality comparison
  that is below or at the +0.05 bar and one-label-fragile. A latency pass is
  useful only after a harder corpus produces a robust quality delta.

## Conclusion

- **Headline verdict**: **inconclusive - corpus needs harder challenge traces**.
- **No second-backend investment is justified by this run**. GLM-5.1 did not
  clear a robust >=0.05 quality-improvement bar on measurable evidence; its
  observed edge is a one-label effect on an over-easy challenge set.
- **Next step**: loop back to corpus construction before Round 3. Replace or
  enrich the saturated challenge traces with cases that discriminate among
  GLM, gemma, coder, and qwen3.6, while retaining solvability from committed
  rubrics and blind labeling.
