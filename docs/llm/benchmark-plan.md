# Benchmark Harness Plan

## Goal

Replace speculative leaderboard comparisons with **evidence from real
workloads**. Given a set of captured MCP tool-use traces, compare
candidate models (e.g. Qwen3-Coder-Next vs Gemma 4 31B vs GLM-5.1) on
metrics that actually correlate with user value.

## Why not just use SWE-bench / τ2-bench?

Public benchmarks answer "which model is best on this benchmark." That's
not the question. The question is **"which model is best on
Keith's workloads"** — MCP tool calls for code review, ML metrics
analysis, trading signal generation, and RAG-backed Q&A.

A 3-point SWE-bench advantage means nothing if the model fumbles the
MCP protocol handshake or picks the wrong tool 10% of the time.

## Architecture

```
cmd/llm-bench/
├── main.go           # CLI entry point
├── capture.go        # Conversation-store trace export + redaction
├── trace.go          # Trace loading + validation
├── runner.go         # Per-model replay loop
├── scorer.go         # Pluggable scoring strategies
└── report.go         # Output formatting

docs/llm/
└── traces/           # Captured traces (gitignored; sensitive)
    ├── mcp-review-*.json
    ├── mcp-rag-*.json
    └── ...
```

## Trace format

Each trace is a replayable conversation:

```jsonc
{
  "id": "mcp-review-2026-04-10-a3f",
  "source": "firn-ide",
  "captured_at": "2026-04-10T14:23:11Z",
  "system": "You are a code review assistant...",
  "tools": [ /* MCP tool schemas */ ],
  "turns": [
    {
      "role": "user",
      "content": "Review the diff in provider/router.go"
    },
    {
      "role": "assistant",
      "content": "...",
      "tool_calls": [
        {"name": "read_file", "arguments": {"path": "provider/router.go"}}
      ]
    },
    // tool response, more turns, etc.
  ],
  "golden": {
    "tool_calls": ["read_file", "git_diff"],
    "final_answer_criteria": "Identifies the circular fallback bug"
  }
}
```

The `golden` field is the rubric: what tools *should* have been called,
and what the final answer *should* contain. Traces can be seeded from
the existing `feedback/` and `conversation/` SQLite stores.

## Metrics

### Per-trace

1. **Tool call sequence match** — Jaccard set overlap between actual and
   golden tool-call names, normalized to [0,1]. Order-agnostic in Phase 1
   (`toolSequenceScore` in `scorer.go`); ordered Levenshtein comparison is
   a Phase 2 follow-up once the tool loop records real per-turn calls.
2. **Tool argument validity** — JSON schema validation against the
   declared tool schemas. Binary pass/fail per call, averaged.
3. **Final answer quality** — *User-selected scoring strategy*:
   - `llm-judge` — a separate local Ollama judge model scores the
     response against `final_answer_criteria` on a 0–1 rubric.
   - `exact-match` — substring match (cheap, brittle).
   - `manual` — dump to CSV for human rating.
4. **Latency** — end-to-end wall time + per-token throughput.
5. **Token cost** — input + output tokens (for context-window
   pressure analysis).

### Aggregate (per model across all traces)

- Mean + p50 / p95 of each metric
- Win rate: % of traces where this model's quality score exceeds the
  baseline by >5%
- Cost of failure: when this model failed a trace, how did it fail?
  (wrong tool / malformed args / bad answer / timeout)

## Scoring strategy is a pluggable interface

```go
type Scorer interface {
    Score(ctx context.Context, trace Trace, actual Result) (Score, error)
}

type Score struct {
    ToolSequenceMatch     float64 // [0,1]
    ToolArgsValid         float64 // [0,1] — JSON Schema validation against trace.Tools
    ToolArgsValidComputed bool    // false = ToolArgsValid is a placeholder (n/a in reports)
    AnswerQuality         float64 // [0,1]
    LatencyMs             int64
    ScorerLatencyMs       int64
    TotalTokens           int
    Notes                 string
}
```

**Why pluggable:** the `AnswerQuality` dimension has no one-size-fits-all
answer. Some tasks (code review) need LLM-as-judge. Some (RAG retrieval)
need exact-match on citations. Some (trading signal analysis) need
manual review because the "right answer" is domain-specific.

## Implementation phases

### Phase 1 — Skeleton + synthetic traces (this PR)

- `cmd/llm-bench/main.go` with CLI args, trace loader, runner loop
- Synthetic traces in `testdata/` so the harness is testable
- `Scorer` interface with an `exact-match` implementation as a baseline

### Phase 2 — Real trace capture (follow-up PR)

- Extend `feedback/` or `conversation/` store to export replayable traces
- Redaction pass for sensitive data (paths, tokens, customer info)
- Initial capture target: 20–30 traces spanning all three consumer apps

First implementation slice:

- `go run ./cmd/llm-bench -capture -capture-db <sqlite-db>` exports
  persisted `conversation/` rows into one JSON trace per conversation
  using a read-only SQLite connection and no store migrations.
- Output defaults to `docs/llm/traces/`, which is gitignored and should
  remain local unless explicitly reviewed and exported.
- The captured final assistant answer is stored as
  `golden.final_answer_substring`; prompt turns stop before that answer
  so simple one-user-turn traces can replay against the current harness.
- Capture redacts absolute local paths, obvious secret assignments,
  bearer tokens, authorization headers, URL credentials, private key
  blocks, and email addresses before files are written.
- Conversations without a system prompt, user turn, final assistant
  answer, or parseable tool-call JSON are skipped with a warning.
- Tool-call names and tool-result turns are captured, and `llm-bench`
  can replay multi-turn/tool-use traces by injecting frozen tool results
  after matching candidate tool calls. Matching is strict and lock-step:
  the candidate must emit the same number of tool calls in the same
  order, with the same names, as the scripted assistant turn. Mismatches
  surface as `errToolCallMismatch`; a candidate that emits tool calls
  with no scripted assistant turn surfaces as `errMissingScriptedAssistant`;
  a candidate that bypasses the scripted tool route by replying in plain
  text has the skip recorded in `Score.Notes` (the historical
  "refuse rather than mislead" intent, now in observability form).
  Tool schemas are sourced from the live MCP server at capture time
  via `-mcp-stdio-command` or `-mcp-url`. The chosen transport is
  invoked once per capture run; `trace.Tools` is populated with the
  normalized minimal `{name, description, inputSchema}` projection of
  the resulting `tools/list` snapshot. A conversation whose
  scripted assistant turn references a tool not in the snapshot is
  skipped with a `capture skipped <id>: tool 'X' not in MCP snapshot`
  warning so replay never falsely validates against a stale schema.
  Replay then evaluates `ToolArgsValid` by JSON Schema validation
  against the captured `trace.Tools` (see `validateToolArguments` for
  semantics). When no transport is configured, capture writes
  `trace.Tools = []` and replay reports `ToolArgsValid` as
  not-computed.
- Capture-derivable stratification: `llm-bench -capture -capture-sample
  <spec>` partitions the enriched trace pool by any cross-product of
  `token-length`, `turn-count`, `has-tool-calls`,
  `has-final-answer-criteria`, `source`, and `recency`, then samples
  uniformly within each cell. The seed (`-capture-sample-seed`) makes
  the selection reproducible across machines; a `_sample-manifest.json`
  in the output directory records the seed, parsed spec, cell counts,
  and the IDs of every trace written.
- The sampling pipeline runs `list → convert → enrich → sample → write`,
  so conversations skipped during enrichment (e.g. undeclared tool
  references) are not eligible for sampling and never appear in the
  manifest.
- Feedback-driven sampling is still deferred: today
  `feedback_retrievals` and `feedback_signals` do not carry a
  conversation id. Once retrieval/session provenance lands, a future
  capture flag can join sampling against that source.

### Phase 3 — LLM-as-judge scorer

- Local Ollama-backed `llm-judge` scorer for `AnswerQuality`
- Dedicated `-judge-model` selection. When omitted, `cmd/llm-bench`
  reads the reference `models.json` `judge` role when available, then
  falls back to `gemma4:31b`.
- Optional `-judge-ollama-url` and `-judge-timeout` keep judge placement
  and timeout budget separate from candidate replay.
- Anti-self-preference guard: a candidate model is skipped rather than
  judged by itself
- Judge latency is excluded from `Score.LatencyMs`; that metric remains
  replay-only. Scorer latency is reported separately.
- Judge prompts compact trace turns before scoring so stored raw payloads
  and oversized tool results do not silently dominate the context window.
- Follow-up: persistent cache keyed by `(judge_model, trace_id,
  transcript_hash)` so re-running over the same corpus is cheap

### Calibration

The judge scorer is calibrated against human labels via a two-phase
workflow documented in [CALIBRATION.md](CALIBRATION.md). Headline numbers:

- Primary agreement criterion: exact categorical match, `judge == expected`,
  where both values are one of `{0.0, 0.5, 1.0}`.
- Diagnostic only: the report also prints the retired tolerance agreement
  count, `|judge - expected| ≤ 0.25`.
- Pass threshold: ≥85% exact agreement on ≥50 matched non-stale labels,
  borderline/fail agreement ≥80% when that subset is present, and no known
  subtle-bug fixture judged as `1.0`.
- Below 50 matched non-stale labels: verdict is `INSUFFICIENT_LABELS`
  (never PASS).
- Stale labels (label `artifact_hash` ≠ frozen `artifacts.jsonl` hash) are
  listed in the report and excluded from agreement.
- Stability runs (`-judge-stability-runs M`) are diagnostic only and do
  not gate the verdict.

Active labeling (`-calibrate-suggest`) is a planned follow-up.

### Phase 4 — Live comparison runs

- **Currently-configured model vs candidate** on each role's captured
  traces. The harness takes `-models <provider>/<name>,...`; today only
  `ollama` and `openai-compat` candidate transports are wired; broader
  provider routing remains follow-up work alongside the multi-provider
  client factory.

OpenAI-compatible candidate replay uses the `openai-compat/<model>`
selector plus an endpoint:

```bash
go run ./cmd/llm-bench \
  -traces 'docs/llm/traces/*.json' \
  -models 'openai-compat/Qwen/Qwen3-Coder' \
  -candidate-base-url 'http://localhost:8080' \
  -scorer exact-match \
  -report ./bench-report.md
```

Candidate answers are scored on final content only: inline `<think>...</think>`
reasoning emitted by a server (for example llama.cpp) is stripped before
scoring, so results reflect the answer rather than serving-stack reasoning
formatting — matching the Ollama path, where the server separates reasoning
into a field the harness drops. A residual `<think` marker surviving the strip
is recorded in the result notes so a reviewer can discount that answer.

- Initial target comparisons against the reference lineup in `models.json`:
  - `coding` — Qwen3-Coder-Next vs GLM-5.1 on code-gen traces (Setup 2
    experiment; see [setups.md](setups.md))
  - `agent` — Gemma 4 31B vs MiniMax M2.7 on MCP tool-use traces
    (Setup 3 experiment)
- Report in `docs/llm/benchmarks/YYYY-MM-DD-<name>.md`

### Round-2 challenge protocol

The Round-2 corpus adds a challenge partition alongside the natural partition.
The two partitions are always reported separately — never averaged — so that
saturation on natural traces does not mask model differences on harder inputs.

**Rubric-first commit barrier.** Trace files and their `golden` rubrics are
committed to the repository before any candidate replay is run. Committing
rubrics after seeing outputs is disqualifying; the barrier is enforced by
convention.

**Solvable-from-context gate.** Every challenge trace must be answerable
from the information present in the trace context alone. A trace that
requires knowledge the model cannot reach is excluded before labeling.

**Numeric validity gates (checked after labeling, before citing the run):**

1. **No-trivia upper bound.** Every challenge trace must have at least one
   candidate labeled `1.0`. A trace where every candidate scores below `1.0`
   is not a valid difficulty signal — it may be unsolvable or badly specified.
2. **De-saturation.** The maximum per-model challenge-partition mean must be
   strictly below `1.00`. A fully-saturated challenge partition provides no
   differentiation and should be enriched before the run is cited.
3. **Floor separation.** The weakest model's challenge mean must be strictly
   the lowest across all candidates and must be at least `0.15` below the
   top model's challenge mean. A compressed floor indicates insufficient
   partition difficulty.

**Blind labeling.** Labels for Round-2 artifacts are produced via
the blind workflow: render a worksheet (model identity hidden), score each
block against the committed rubric, then ingest. See [CALIBRATION.md](CALIBRATION.md)
for the `-blind-render` / `-blind-ingest` command sequence and the
`-labels-out` guard.

**Separate reporting.** Natural and challenge partitions are always reported
with separate `-corpus-partitions` invocations. The corpus manifest
(`-corpus-manifest`) and `-corpus-only-evidence` flags scope each run to
its partition and exclude the tool canary from model-evidence math.

### Round-3 discriminating challenge protocol

Round 3 keeps the Round-2 rubric-first and blind-labeling controls, but the
purpose changes: the corpus is designed to resolve the top cluster, not just
separate the floor. The fresh challenge stratum is 24 Go-first
correctness-depth traces across five families:

- `type-semantics`
- `concurrency-lifetime`
- `stdlib-contract`
- `contract-edge`
- `algorithmic`

Each fresh trace is answerable from the prompt and has a committed
three-tier rubric before capture. The rubric names the exact `1.0` boundary,
bounded `0.5` partial-credit patterns, objective `0.0` defects, and concrete
restraint/provenance hard-fail cases. A structured oracle-screen note records
that the trace is non-trivial and solvable from context; it is not a candidate
output and must not use a scored panel model as the oracle.

The Round-3 manifest also references two re-anchor strata:

- `source=round2-challenge`: selected Round-2 valid-discriminator regression
  anchors. These stabilize difficulty tracking and are not the primary claim.
- `source=first-accepted-run`: 10 deterministic natural re-anchor traces used
  to check same-epoch lineup drift.

Round 3 adds a discrimination report before any accepted-run conclusion is
cited. The report classifies each labeled trace into one of six states:
`valid-discriminator`, `saturated`, `unsolved`, `floor-only`, `no-signal`, or
`unpaired/missing`. Only the derived `valid-discriminator` manifest emitted by
that report may define the primary top-resolution subset. If the fresh
Round-3 stratum yields fewer than 10 valid discriminators, the result is
under-resolved for a frontier-vs-local claim even if ordinary quality reports
can still be read descriptively.

Accepted Round-3 reporting must keep these views separate:

- R3-fresh full stratum (`-corpus-sources round3-challenge`) for the
  accepted-run quality view.
- R3-fresh valid-discriminator subset from the derived manifest for the
  primary top-resolution paired deltas.
- R2-anchor regression view (`-corpus-sources round2-challenge`).
- Natural re-anchor view (`-corpus-sources first-accepted-run`).

## User decision required

The `AnswerQuality` scorer is the load-bearing piece. Three choices with
distinct tradeoffs:

| Strategy | Cost per trace | Quality of judgment | Reproducibility |
|---|---|---|---|
| `exact-match` | ~0 | Low (brittle) | High |
| `llm-judge` (local Ollama) | Local inference time | High | Moderate (varies with judge model updates) |
| `manual` | ~5 min of human time | Highest | Low (rater variance) |

The skeleton shipped with `exact-match` because it has no external
dependencies. For actual comparison runs (Phase 4), use `llm-judge` as
the primary scorer with `manual` spot-checks.

This is a real decision point, not busywork — the scorer choice
determines whether the harness is trustworthy enough to override the
public benchmark narrative. A wrong choice here makes the whole exercise
advisory rather than authoritative.
