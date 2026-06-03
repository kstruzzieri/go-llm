# go-llm: Local Model Strategy

This directory documents the analysis and decisions behind the reference
model lineup shipped with `go-llm` and its consumers (Firn IDE, Flux ML,
Quantum Trader) on Apple Silicon workstations.

**Target hardware:** MacBook Pro M3 Max, 128GB unified memory (~97GB usable
after OS overhead).

## Documents

| File | Purpose |
|---|---|
| [analysis.md](analysis.md) | 2026 open-weight landscape, benchmark summary, sources |
| [setups.md](setups.md) | Three candidate setup combinations with tradeoffs |
| [recommendation.md](recommendation.md) | Current reference lineup + how to customize |
| [benchmark-plan.md](benchmark-plan.md) | Harness design for A/B'ing models on real traces |

## Bring-your-own model

`go-llm` does **not** hard-code a model roster. `models.json` is the only
source of truth for which models exist, what role each plays, and which
provider serves them. You can substitute any model the provider can load:
edit `models.json`, restart, and the router picks it up. See
[recommendation.md#customizing-the-lineup](recommendation.md#customizing-the-lineup)
for the full story.

On first use, the `fingerprint/` package probes the model via the
provider's `/api/show` (or equivalent) endpoint, captures capabilities
(chat / embedding / tool-call) plus latency and throughput, and persists
the profile to SQLite. No code changes are required to add a model — only
config.

The specific model IDs in this documentation describe the reference
lineup checked into `models.json`. Treat them as "what ships by default",
not "what `go-llm` requires."

## TL;DR — Reference lineup (April 2026)

- **`qwen3-coder-next`** — primary code generation (80B / 3.9B active MoE)
- **`qwen3-embedding:8b`** — embeddings (#1 MTEB multilingual)
- **`gemma4:31b`** (dense) — agent / judge / reasoning (86.4% τ2-bench,
  80.0% LiveCodeBench, native function calling)
- **`qwen3.6:35b-a3b`** — fast MoE (73.4% SWE-bench, 3B active)
- **`qwen3:8b`** — lightweight / FIM

The fleet co-resides in ~77GB with comfortable headroom. The architecture
stays on Ollama (as of 0.19, Ollama is backed by MLX on Apple Silicon, so
the historical MLX-vs-Ollama speed gap is closed). A second backend
(llama.cpp, LM Studio, mlx-lm) is deferred until the benchmark harness
shows a measurable quality delta on real workloads — see
[setups.md](setups.md) for Setup 2/3 (GLM-5.1, MiniMax M2.7) as
alternative paths.

## Manual smoke test

To smoke-test `cmd/llm-bench` without waiting for real captured traces,
use the checked-in synthetic trace:

```bash
go run ./cmd/llm-bench \
  -traces 'cmd/llm-bench/testdata/smoke/*.json' \
  -models 'ollama/gemma4:31b,ollama/qwen3.6:35b-a3b' \
  -scorer exact-match \
  -report /tmp/llm-bench-smoke.md
```

Substitute whichever models are pulled on your machine. Success criteria:
the command exits `0`, writes `/tmp/llm-bench-smoke.md`, and the report
shows `Errors = 0` with `Mean Quality = 1.00` for each model on the
`smoke-minimal-001` trace. This only verifies harness plumbing and basic
model reachability, not real MCP-agent quality.

To score free-form answers with the local judge model instead of the
substring baseline, switch the scorer and keep the judge model separate
from the candidate under test:

```bash
go run ./cmd/llm-bench \
  -traces 'cmd/llm-bench/testdata/smoke/*.json' \
  -models 'ollama/qwen3.6:35b-a3b' \
  -scorer llm-judge \
  -judge-model gemma4:31b \
  -judge-timeout 5m \
  -report /tmp/llm-bench-judge-smoke.md
```

`llm-judge` records an error instead of scoring a trace when the candidate
model and judge model are the same, which avoids self-preference in
calibration runs. Judge latency is excluded from the model latency metric
in the report and shown separately as scorer latency. Use
`-judge-ollama-url` when the judge should run on a different Ollama
instance than the candidate models.

`llm-judge` can also use a frontier judge without replaying candidate models
through a non-Ollama backend:

```bash
go run ./cmd/llm-bench \
  -traces 'cmd/llm-bench/testdata/smoke/*.json' \
  -models 'ollama/qwen3.6:35b-a3b' \
  -scorer llm-judge \
  -judge-transport openai-compat \
  -judge-base-url https://api.openai.com \
  -judge-model '<model-listed-by-/v1/models>' \
  -report /tmp/llm-bench-frontier-judge.md
```

For `openai-compat`, set the Bearer token with
`LLM_BENCH_JUDGE_API_KEY`; `-judge-api-key` exists for local experiments but
can leak through shell history or process listings. Report provenance records
the judge provider as `openai-compat:<endpoint-id>` so two endpoints with the
same model id do not share cached digest-less verdicts.

For local subscription-backed diagnostics, `-judge-transport claude-cli`
adapts `claude -p` headless mode:

```bash
go run ./cmd/llm-bench \
  -traces 'cmd/llm-bench/testdata/smoke/*.json' \
  -models 'ollama/qwen3.6:35b-a3b' \
  -scorer llm-judge \
  -judge-transport claude-cli \
  -judge-model opus \
  -report /tmp/llm-bench-claude-cli-judge.md
```

Treat `claude-cli` provenance as "Claude Code CLI judge", not a raw model API:
the CLI does not expose a temperature pin, and its headless runtime is still an
agent surface. Use `-judge-stability-runs 3` before citing a borderline pass.

## Local trace capture

Captured traces are written under `docs/llm/traces/`, which is gitignored
because traces can contain local paths, prompts, and tool results. Export
from a SQLite database that contains the `conversation/` tables with:

```bash
go run ./cmd/llm-bench \
  -capture \
  -capture-db /path/to/go-llm.sqlite \
  -capture-out docs/llm/traces \
  -capture-source firn-ide
```

If older conversations do not include a persisted system message, provide
the exact prompt used by that app:

```bash
go run ./cmd/llm-bench \
  -capture \
  -capture-db /path/to/go-llm.sqlite \
  -capture-system 'You are the local coding assistant...' \
  -capture-limit 50
```

The capture pass exports one JSON trace per valid conversation, moves the
captured final assistant answer into `golden.final_answer_substring`,
redacts absolute local paths, obvious secret values, bearer tokens,
authorization headers, URL credentials, private key blocks, and email
addresses, and skips conversations that cannot form a replayable trace.
The first capture PR uses `conversation/` as the source of truth;
feedback-linked sampling comes later because the current `feedback/`
schema does not store a conversation id.

Tool-call names and tool-result turns are preserved, and `llm-bench`
can replay multi-turn conversations with frozen tool results when the
trace includes the needed tool schemas. Replay matches candidate tool
calls against the scripted assistant turn in lock-step (same count,
same names, same order); mismatches surface as `errToolCallMismatch`,
divergence into plain text bypasses the scripted tool route and is
recorded in `Score.Notes`. Tool-argument schema validation against
`trace.Tools` is computed by the scorer when schemas are present.
Capture can populate `trace.Tools` from a live MCP server with
`-mcp-stdio-command` or `-mcp-url`; without either flag, capture writes
empty tools and replay marks `ToolArgsValid` as not-computed for actual
tool calls.

## Date stamp

Research was conducted April 16, 2026 and refreshed against the live
`models.json` as of that date. Benchmark numbers and release dates
reflect that snapshot.
