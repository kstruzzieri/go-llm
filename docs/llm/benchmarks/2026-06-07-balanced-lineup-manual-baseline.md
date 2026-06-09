<!--
Per spec docs/superpowers/specs/2026-05-25-harness-followups-design.md §5.2:
Do NOT paste raw prompts, transcripts, judge justifications, or error
messages into this file. Only sanitized aggregates + provenance. Raw
traces, labels, artifacts, and run logs are gitignored.

STATUS: first accepted run (manual-label path). All six gate criteria
met; conclusion reviewed and approved by the repo owner (keith) 2026-06-08.
Accepted as a plain-chat/manual baseline only — NOT validation of tool-use
or agent roles.
-->

# balanced-lineup-manual-baseline — 2026-06-07

> **Scope — read first.** This is the **first accepted *plain-chat / manual*
> baseline** for the Setup 1 "Balanced Daily Driver" lineup. It is **not** a
> tool-use or agent benchmark: the corpus is plain chat/code Q&A with **zero
> tool-call traces**, so it makes **no claim** about tool-calling or agentic
> behavior. Quality is human-judged on a **fully paired** 20-trace set; it
> validates the *lineup ranking on chat/code Q&A* and **does not, by itself,
> justify any `models.json` role change**. Stronger, decision-grade ranking
> (natural-vs-challenge partitions, real tool-use coverage, automated-judge
> reproducibility) is deferred to the enriched Round-2 run.

## Provenance
- **Harness commit (latency replay + reports)**: `3b37097` (branch
  `docs/56-first-accepted-manual-baseline`, off `develop@912c810`; `dirty: no`)
- **Frozen artifacts produced at**: `8b97696` (develop; judge transports +
  manual scorer, PR #135) via `-calibrate-capture`, 2026-05-30/31
- **Machine**: MacBook Pro M3 Max, 128 GB unified memory
- **Trace set**: `first-accepted-run` (paired subset), count: **20**,
  trace-set manifest hash: `sha256:64dd2a9fe214c0144c8445915ed18a534f86e080c8a6f559654c6b5e87ea6037`
  - Retained trace files:
    `conversation-fa-a05`, `conversation-fa-c02`, `conversation-fa-c04`,
    `conversation-fa-c05`, `conversation-fa-c07`, `conversation-fa-c08`,
    `conversation-fa-c09`, `conversation-fa-f02`, `conversation-fa-f03`,
    `conversation-fa-f04`, `conversation-fa-f06`, `conversation-fa-g01`,
    `conversation-fa-g03`, `conversation-fa-g04`, `conversation-fa-g05`,
    `conversation-fa-l02`, `conversation-fa-l03`, `conversation-fa-l05`,
    `conversation-fa-m02`, `conversation-fa-m04`.
- **Models under test (generative lineup)**: `ollama/qwen3-coder-next:latest,
  ollama/gemma4:31b, ollama/qwen3.6:35b-a3b, ollama/qwen3:8b` (all Q4_K_M).
  The embedding model (`qwen3-embedding:8b`) is not answer-quality-judged here.
- **Scorer**: `manual` (human labels)
  - Label manifest hash: `sha256:b030bcd06efb84a3aafd8f0c772e0b5f90a75f232ea448fc1203f18992b63d92`
  - Artifact manifest hash (80 scored cells): `sha256:a984e52e5feba1cc6006b5b13a32e613a73137549e81fa12d92c1e1c23e62723`
  - Valid labeled artifacts: **80** (≥50 required → SUFFICIENT)
  - Paired label coverage: **20 / 20** retained traces complete (≥20 → SUFFICIENT)
  - Stale / missing labels: **0 stale**; 4 traces excluded pre-scoring (see Exclusions)
  - Labeler / reviewer: **keith / keith** (29 of 80 labels AI-pre-labeled as
    review assistance; every label human-reviewed and owned by keith — the 6
    non-1.0 calls and the `fa-g03` borderline were adjudicated individually)
- **Latency**: separate fresh timed replay on `3b37097` over the same 20-trace
  set (`-scorer exact-match`, `-timeout 10m`, thinking on); frozen artifacts
  carry no timing.
- **Exact commands**:
  ```
  # Quality (manual-label path, over frozen artifacts):
  llm-bench -manual-report \
    -labels  docs/llm/calibration/labels.accepted.jsonl \
    -artifacts docs/llm/calibration/artifacts.jsonl \
    -report <scratch>.md

  # Paired statistics (win/loss/tie + bootstrap delta CIs + resolution):
  llm-bench -paired-report \
    -labels  docs/llm/calibration/labels.accepted.jsonl \
    -artifacts docs/llm/calibration/artifacts.jsonl \
    -baseline gemma4:31b -report <scratch>.md

  # Latency (separate fresh replay over the same 20-trace set):
  # `-traces` is one filepath.Glob pattern, so copy the retained files into a
  # scratch directory before replay. Do not use `conversation-*.json`; it also
  # matches the four excluded traces listed below.
  RUN_TRACES="$(mktemp -d "${TMPDIR:-/tmp}/llm-bench-retained.XXXXXX")"
  for id in \
    conversation-fa-a05 conversation-fa-c02 conversation-fa-c04 \
    conversation-fa-c05 conversation-fa-c07 conversation-fa-c08 \
    conversation-fa-c09 conversation-fa-f02 conversation-fa-f03 \
    conversation-fa-f04 conversation-fa-f06 conversation-fa-g01 \
    conversation-fa-g03 conversation-fa-g04 conversation-fa-g05 \
    conversation-fa-l02 conversation-fa-l03 conversation-fa-l05 \
    conversation-fa-m02 conversation-fa-m04
  do
    cp "docs/llm/traces/first-accepted-run/${id}.json" "$RUN_TRACES/"
  done

  llm-bench -traces "$RUN_TRACES/*.json" \
    -models 'ollama/qwen3-coder-next:latest,ollama/gemma4:31b,ollama/qwen3.6:35b-a3b,ollama/qwen3:8b' \
    -scorer exact-match -timeout 10m -report <scratch>.md
  ```
  (Traces, labels, artifacts, and run logs are gitignored; only sanitized
  aggregates appear here.)

## Calibration / Labeling (manual path)
- Quality is human-judged (`manual` scorer), so there is **no LLM judge to
  calibrate**. The automated-judge path (frontier judge via `claude-cli`)
  reached 51/60 = 85% once but is **single-draw, stability not pinned** →
  classed diagnostic-only by the gate; it is Round-2 work and is **not** used here.
- **80 valid labeled artifacts**, **20 / 20** retained traces fully paired
  across the 4-model lineup, **0 stale** (all label `artifact_hash` values
  match current frozen artifacts), labeler/reviewer = keith/keith.
- Label score distribution (paired-20 set): **64 × 1.0 / 15 × 0.5 / 1 × 0.0**.

## Results

Quality n = 20 per model (human labels over frozen artifacts), **fully paired**
(every model labeled on every retained trace), so per-model means are directly
comparable. Latency/tokens are from the separate fresh replay (latency n shown
separately where a replay failed). ToolArgsValid is **vacuous** (no tool traces)
and is not evidence of tool-calling — see the Tool-call subset section.

| Model | AnswerQuality (mean / p25 / p50 / p75 / p90) | ToolArgsValid | LatencyMs (p50 / p90, successful-only) | TotalTokens | quality n / latency n |
| --- | --- | --- | --- | --- | --- |
| gemma4:31b | 1.00 / 1.00 / 1.00 / 1.00 / 1.00 | 1.00 vacuous (computed=19) | 127,048 / 297,281 | 21,043 | 20 / 19 |
| qwen3-coder-next:latest | 0.90 / 1.00 / 1.00 / 1.00 / 1.00 | 1.00 vacuous (computed=20) | **17,250 / 69,636** | 13,333 | 20 / 20 |
| qwen3.6:35b-a3b | 0.90 / 1.00 / 1.00 / 1.00 / 1.00 | 1.00 vacuous (computed=20) | 78,902 / 125,542 | 58,376 | 20 / 20 |
| qwen3:8b | 0.78 / 0.50 / 1.00 / 1.00 / 1.00 | 1.00 vacuous (computed=19) | 52,148 / 113,729 | 35,614 | 20 / 19 |

> The latency-pass `AnswerQuality` (exact-match scorer) is structurally 0.00 for
> free-form answers and is **not** used; quality comes only from the manual
> scorer above. Latency p50/p90 are **successful-only** → p90 is optimistic for
> the two models with a replay failure (see Exclusions).

Token breakdown (gen vs prompt-eval; Ollama folds reasoning into gen, so
attribute latency to output volume, not isolated thinking):

| Model | gen tokens (sum / mean) | prompt-eval (sum / mean) | latency n |
| --- | --- | --- | --- |
| gemma4:31b | 18,932 / 996 | 2,111 / 111 | 19 |
| qwen3-coder-next:latest | 11,299 / 564 | 2,034 / 101 | 20 |
| qwen3.6:35b-a3b | 56,229 / 2,811 | 2,147 / 107 | 20 |
| qwen3:8b | 33,604 / 1,768 | 2,010 / 105 | 19 |

Latency is driven by output token volume **and** per-token speed: dense
`gemma4:31b` is **slowest per token** (996 gen tokens but 127 s p50), while MoE
`qwen3.6:35b-a3b` (a3b ≈ 3 B active) is faster per token despite emitting **≈3×
gemma's gen tokens** (56 k vs 19 k); `coder` wins on both axes (fewest tokens +
fast per token).

### Paired statistics (baseline = gemma4:31b)

Pairwise win/loss/tie over the 20 paired traces:

| A vs B | A wins | A losses | ties |
| --- | --- | --- | --- |
| gemma4:31b vs qwen3-coder-next:latest | 4 | 0 | 16 |
| gemma4:31b vs qwen3.6:35b-a3b | 4 | 0 | 16 |
| gemma4:31b vs qwen3:8b | 8 | 0 | 12 |
| qwen3-coder-next:latest vs qwen3.6:35b-a3b | 3 | 3 | 14 |
| qwen3-coder-next:latest vs qwen3:8b | 5 | 1 | 14 |
| qwen3.6:35b-a3b vs qwen3:8b | 7 | 2 | 11 |

Mean per-trace quality delta vs baseline, with deterministic bootstrap 95% CI
(seed=1, n=10000):

| Model | mean Δ vs gemma4:31b | 95% CI | wins / losses / ties |
| --- | --- | --- | --- |
| qwen3-coder-next:latest | −0.10 | [−0.20, −0.03] | 0 / 4 / 16 |
| qwen3.6:35b-a3b | −0.10 | [−0.20, −0.03] | 0 / 4 / 16 |
| qwen3:8b | −0.23 | [−0.35, −0.10] | 0 / 8 / 12 |

**Resolution diagnostic**: over n=20 paired traces, one full label flip (0↔1)
moves a mean by 0.05; one rubric step (0.5) moves it by 0.03. Do not over-read
a one-label gap. → `coder` vs `qwen3.6` (3/3/14, both 0.90) are **statistically
indistinguishable**; `gemma`'s +0.10 over each (CI excludes 0) is resolvable
**on the answered subset only** — see the survivorship caveat below.

## Tool-call subset
- Expected-tool-call traces: **0** (plain-chat corpus; `trace.Tools` empty).
- Overall `ToolArgsValid` `computed=true` rows (Results table) pass by **vacuous
  truth** — no tool calls were expected or made — so they are **not** evidence of
  tool-calling correctness. The harness's expected-tool-call subset reports
  `insufficient: 0 expected-tool-call pairs (need ≥10)` for every model.
- This run therefore makes **no tool-use claim**; gate criterion #3 is satisfied
  by *not* claiming tool-use (rather than by passing a tool-use bar).

## Errors and exclusions
- **Excluded traces (reported separately for auditability):**
  - `fa-a04`, `fa-m01`, `fa-m03` — **missing-artifact**: each lacks **only the
    `gemma4:31b` artifact** (gemma did not complete these during the original
    capture/replay). Dropped so pairing stays honest, not silently averaged.
  - `fa-n02` — **label-incomplete**: artifacts present for all 4 models but not
    fully labeled; excluded rather than partially counted.
  - The 4 excluded traces leave **20** fully paired, fully labeled retained
    traces (the scored set). `fa-n02` remains available to extend the set later.
- **Quality-scoring errors**: 0 (80 / 80 scored, 0 stale).
- **Latency replay failures (fresh pass, distinct from the quality set):**
  | Model | failures / total | kind |
  | --- | --- | --- |
  | gemma4:31b | 1 / 20 | `fa-g04` timeout (exceeded 10 m cap) |
  | qwen3:8b | 1 / 20 | `fa-c07` other error |
  | qwen3-coder-next:latest | 0 / 20 | — |
  | qwen3.6:35b-a3b | 0 / 20 | — |
  These are **fresh-replay** failures: both `fa-g04`/gemma and `fa-c07`/qwen3:8b
  have frozen artifacts and human labels in the quality set (g04/gemma = 1.0,
  c07/qwen3:8b = 0.5), so they count toward quality (n=20) but not latency
  (n=19). Latency p50/p90 are **successful-only** → p90 is *optimistic* for
  gemma and qwen3:8b (the failed worst case is excluded, not capped-in).

## Conclusion
- **Verdict**: **accept as the first plain-chat/manual baseline; keep the
  current lineup.** This is *not* a decision-grade, tool-aware ranking and does
  **not** by itself justify any `models.json` role change.
- **Quality delta vs prior accepted run**: n/a — this is the first accepted run
  (replaces "No accepted runs yet").
- **Latency delta vs prior accepted run**: n/a — first accepted run.
- **Lineup read (tactical, not a `recommendation.md` role change):**
  - `gemma4:31b` posts the top observed quality (1.00) and its +0.10 edge is
    CI-resolvable — **but on the answered subset only**. It is also the model
    that failed to complete 3 traces at capture (`fa-a04/m01/m03`) **and** timed
    out on a 4th (`fa-g04`) in the fresh replay, and is **by far the slowest**
    (127 s p50 / 297 s p90, vs 17 s p50 for coder). Its perfect score is
    **survivorship-biased**; it is "top observed where it answered," not
    "best model." **Completion-aware sensitivity**: scoring the 3 capture
    non-completions as 0.0 drops gemma to ≈ **20/23 = 0.87** over the
    retained-plus-missing scoreable set — below coder/qwen3.6's 0.90. Keep it for
    judge/agent/general until the enriched run says more.
  - `qwen3-coder-next` and `qwen3.6:35b-a3b` are **tied and statistically
    indistinguishable** on quality (0.90; 3/3/14), but `coder` is **decisively
    faster** (17 s vs 79 s p50, fewest output tokens, 0 failures) — so prefer
    `coder` for coding/dev-chat when memory allows; it is the run's standout for
    practical use.
  - `qwen3:8b` is the clear laggard (0.78; **net-loses every pairwise model
    comparison** — it wins a few individual traces but loses each head-to-head on
    balance), consistent with a lightweight/low-stakes role only.
- **Justification**: on a fully paired, manually-labeled 20-trace chat/code
  corpus, the lineup ranking is gemma ≥ {coder ≈ qwen3.6} > qwen3:8b, with the
  top three within ~0.10 (near the corpus resolution limit). No regression; no
  basis to change roles. The corpus is saturated and tool-free, so this is a
  baseline, not a verdict on tool-use or agentic capability.

## Caveats
- **Corpus saturation / ceiling effect**: labels are 64×1.0 / 15×0.5 / 1×0.0;
  the top three sit near the corpus's resolution limit (one flip = 0.05).
- **Survivorship in gemma's score** (above): gemma's 1.00 excludes 3 traces it
  did not complete; a completion-aware view (those 3 as 0.0) puts it at
  ≈ 20/23 = 0.87, below coder/qwen3.6's 0.90.
- **Two-pass methodology**: quality is on the exact frozen artifacts the human
  judged; latency is a fresh replay (new generations). Acceptable (latency ≈
  model+prompt property) but not the identical generations.
- **Thinking-on, chat/analysis traces**: high latency variance (p90 ≫ p50);
  says nothing about inline-completion (FIM) latency, a separate measurement.
- **Quantization**: all four candidates run Q4_K_M (4-bit); an fp16/Q8 probe is
  a separate question.
- **AI-assisted labeling**: 29 of 80 labels were AI-pre-labeled as review
  assistance and then human-reviewed/owned by keith; the 6 sub-1.0 calls and
  the `fa-g03` borderline were individually adjudicated.
