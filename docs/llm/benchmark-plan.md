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
   - `llm-judge` — Claude API scores response against
     `final_answer_criteria` on a 0–1 rubric.
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
    ToolSequenceMatch   float64 // [0,1]
    ToolArgsValid       float64 // [0,1]
    AnswerQuality       float64 // [0,1]
    LatencyMs           int64
    TotalTokens         int
    Notes               string
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
  Tool schemas are still not recoverable from `conversation/` today;
  schema sourcing remains follow-up capture work.
- Feedback-driven sampling is deferred until feedback rows can be tied
  to conversations; today `feedback_retrievals` and `feedback_signals`
  do not carry a conversation id.

### Phase 3 — LLM-as-judge scorer (follow-up PR)

- Optional integration with Claude API for `AnswerQuality`
- Careful prompt engineering + cache the judgments

### Phase 4 — Live comparison runs

- **Currently-configured model vs candidate** on each role's captured
  traces. The harness takes `-models <provider>/<name>,...`; today only
  `ollama` is wired (`parseModelTarget` rejects other providers), so the
  "any model reachable through an existing `provider.Provider`" target
  remains follow-up work alongside the multi-provider client factory.
- Initial target comparisons against the reference lineup in `models.json`:
  - `coding` — Qwen3-Coder-Next vs GLM-5.1 on code-gen traces (Setup 2
    experiment; see [setups.md](setups.md))
  - `agent` — Gemma 4 31B vs MiniMax M2.7 on MCP tool-use traces
    (Setup 3 experiment)
- Report in `docs/llm/benchmarks/YYYY-MM-DD-<name>.md`

## User decision required

The `AnswerQuality` scorer is the load-bearing piece. Three choices with
distinct tradeoffs:

| Strategy | Cost per trace | Quality of judgment | Reproducibility |
|---|---|---|---|
| `exact-match` | ~0 | Low (brittle) | High |
| `llm-judge` (Claude) | $0.01–0.05 | High | Moderate (varies with Claude updates) |
| `manual` | ~5 min of human time | Highest | Low (rater variance) |

**TODO for Keith**: decide which Scorer ships in Phase 1. The skeleton
assumes `exact-match` because it has no external dependencies, but for
the actual comparison runs (Phase 4) we likely want `llm-judge` as the
primary with `manual` spot-checks.

This is a real decision point, not busywork — the scorer choice
determines whether the harness is trustworthy enough to override the
public benchmark narrative. A wrong choice here makes the whole exercise
advisory rather than authoritative.
