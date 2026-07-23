<!--
Round-1 human baseline — NOT an accepted run. Pipeline evidence + rough signal,
not a decision-grade ranking. Per spec §5.2: aggregates only — no raw prompts,
transcripts, judge justifications, or error messages.
-->

# round-1-human-baseline — 2026-06-06

> **This is not an accepted run.** Even on the gate's manual-label path it falls
> short: the human labels are **not fully paired** across the lineup (only 5/24
> traces have all 4 models labeled), and the corpus is **plain-chat with no
> tool-use evidence**. It is a human-judged baseline — evidence the
> capture→label→replay pipeline works, plus a *rough* quality/latency signal —
> not a decision-grade model ranking. Do not promote to `harness-results.md` or
> cite in `recommendation.md`.

## Provenance
- **Harness commit**: `8b97696` (develop; judge transports + manual scorer, PR #135)
- **Machine**: MacBook Pro M3 Max, 128 GB unified memory
- **Trace set**: `first-accepted-run`, count: 24, manifest hash: `sha256:a94b74bd7cd631ba2290ba9e8a0d4238878bd9dceac873424ef8579438319d81`
- **Models**: `ollama/qwen3-coder-next:latest, ollama/gemma4:31b, ollama/qwen3.6:35b-a3b, ollama/qwen3:8b` (all Q4_K_M)
- **Quality scorer**: `manual` (human labels); 60 labels, 15 per model
- **Latency**: separate fresh timed replay (`-scorer exact-match`, `-timeout 10m`, thinking on); frozen artifacts carry no timing
- **Latency command**:
  ```
  llm-bench -traces 'docs/llm/traces/first-accepted-run/conversation-*.json' \
    -models 'ollama/qwen3-coder-next:latest,ollama/gemma4:31b,ollama/qwen3.6:35b-a3b,ollama/qwen3:8b' \
    -scorer exact-match -timeout 10m -report <path>
  ```

## Calibration
N/A — quality is human-judged (`manual` scorer), so there is no LLM judge to
calibrate. The automated-judge calibration (frontier judge via `claude-cli`)
passed the gate once but is **not citable** (single-draw nondeterminism); it is
Round-2 work and is not used here.

## Results
Quality n = 15 per model; latency n = 24 per model (23 where a replay was
excluded). **These are different trace subsets** — see paired-labeling caveat.

| Model | AnswerQuality (human, mean) | LatencyMs p50 / p90 (successful-only) | tokens (sum) | quality n | latency n |
| --- | --- | --- | --- | --- | --- |
| ollama/qwen3-coder-next:latest | 0.93 | 21,668 / 94,372 | 25,841 | 15 | 24 |
| ollama/gemma4:31b | 1.00 | 123,436 / 275,212 | 30,391 | 15 | 23 |
| ollama/qwen3.6:35b-a3b | 0.93 | 75,565 / 180,653 | 78,385 | 15 | 24 |
| ollama/qwen3:8b | 0.73 | 54,576 / 145,142 | 59,193 | 15 | 23 |

Tool metrics omitted: plain-chat corpus (zero tool-call traces), so they are
trivially satisfied and are **not** evidence of tool-calling correctness.

## Errors and exclusions
- **Error/timeout rate: 2 / 96 replays (2.1%)** — `conversation-fa-m01` (timeout, exceeded the 10 m cap) and `conversation-fa-c07` (other). Raw messages live in the gitignored run log.
- **Latency p50/p90 are successful-only** → p90 is *optimistic*: the timed-out worst case is excluded, not capped-in. A run with more/harder traces should report timeout rate alongside latency and either cap-in timeouts or label "successful-only" explicitly.

## Findings (signal strength varies)
- **Strong: `qwen3-coder-next`** — near-top human quality at the **best p50 latency** (21.7 s), despite being the largest model. Worth taking seriously for coding/dev-chat.
- **Meaningful: `qwen3:8b`** — on this chat/code corpus it is **worse *and* slower** than `coder-next`, so keep it to lightweight/low-stakes use unless a separate FIM/classification benchmark proves a niche.
- **Weak: the quality ordering among `gemma4:31b`, `qwen3-coder-next`, `qwen3.6:35b-a3b`.** With 15 labels/model on a saturated, largely-unpaired set, `1.00` vs `0.93` is one or two borderline labels. **Do not read Gemma as "best quality"** — it is "top observed score, but indistinguishable under this corpus."
- **Latency driver**: reported token volume / output verbosity appears to drive latency (the harness records total tokens = prompt-eval + gen-eval, **not** isolated thinking tokens). `coder-next` emitted the fewest tokens and was fastest; `qwen3.6:35b-a3b` the most.

## Conclusion
- **Verdict**: not decision-grade. Round-1 human baseline; pipeline validated; one strong model signal (`coder-next`).
- **Lineup**: keep the current lineup. Tactical reads only (not a `recommendation.md` change): prefer `qwen3-coder-next` for coding/dev-chat when memory allows; keep `gemma4:31b` for judge/agent/general until a hard tool-use corpus justifies otherwise; re-test `qwen3.6:35b-a3b` and `qwen3:8b` with thinking off / tighter generation before assigning them "fast" roles.

## Caveats
- **Corpus saturation/skew**: labels are 49×1.0 / 10×0.5 / 1×0.0 — limited discrimination (ceiling effect). The quality numbers sit near the corpus's resolution limit.
- **Labels are largely unpaired**: only 5/24 traces have all 4 models labeled (17/24 have 2). Per-model means therefore reflect different trace-difficulty mixes and can be biased — Round 2 must label paired outputs (all candidates on every retained trace).
- **Two-pass methodology**: quality is on the exact frozen outputs the human judged; latency is a fresh replay (new outputs). Acceptable (latency ≈ model+prompt property) but not the identical generations.
- **Thinking-on, chat/analysis traces**: high latency variance (p90 ≫ p50); says nothing about inline-completion (FIM) latency, which is a separate measurement.
- **Model config**: 4/5 models run with a raw `{{ .Prompt }}` Ollama template (confirmed functional for chat) and `presence_penalty 1.5` baked on two — flagged for awareness, not a defect in this run.
