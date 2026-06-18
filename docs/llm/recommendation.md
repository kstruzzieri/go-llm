# Current Lineup + Customization

> **Update (2026-06): llama.cpp is now the primary local backend.** The
> "all-Ollama, second-backend-deferred" stance below was the April 2026
> decision and is kept as a historical record. The current direction runs the
> lineup through `llama-server` (OpenAI-compatible API) via the `openai-compat`
> provider format, with Ollama as a supported alternative. The reference
> *models* are unchanged; only the serving backend changed. See
> [Local model backends](../../README.md#local-model-backends) for how to
> configure providers.

## Reference lineup (shipped in `models.json`)

Quality-ranked on chat/code Q&A by the accepted **plain-chat/manual
baseline** [2026-06-07](benchmarks/2026-06-07-balanced-lineup-manual-baseline.md)
(see [harness-results.md](harness-results.md) for the accepted-run index).
That run validates the *lineup ranking on plain chat/code Q&A* only —
**tool-use and agent roles are not yet harness-validated**, and it does
not by itself justify a role change. Originally adopted from the (now
historical) [April 2026 analysis](analysis.md). This is **Setup 1 —
Balanced Daily Driver** from [setups.md](setups.md): drop-in upgrades,
all-Ollama, full co-residency, zero architectural change to `go-llm`.

| Role | Model | Source |
|---|---|---|
| `coding` | `qwen3-coder-next:latest` | Ollama |
| `agent` | `gemma4:31b` | Ollama |
| `general` | `gemma4:31b` | Ollama |
| `analysis` | `gemma4:31b` | Ollama |
| `fast` | `qwen3.6:35b-a3b` | Ollama |
| `lightweight` | `qwen3:8b` | Ollama |
| `embedding` | `qwen3-embedding:8b` | Ollama |

These specific IDs are **the reference lineup**, not a contract. `go-llm`
has no hard-coded model list: the router, registry, and every consumer
read from `models.json`.

The latest frontier probe,
[2026-06-10 Round-2 frontier challenge](benchmarks/2026-06-10-round-2-frontier-challenge.md),
was **inconclusive**: GLM-5.1 saturated the challenge partition at 1.00, so the
corpus failed the de-saturation validity gate and did not justify a
second-backend investment. The current lineup remains unchanged pending a
harder Round-3 corpus.

### How to verify

To rerun the accepted workflow against the same local evidence, use the
exact `llm-bench` commands in the artifact linked from
[harness-results.md](harness-results.md) (manual-label quality + paired
statistics + a separate latency replay over the listed retained trace
IDs). The accepted traces, labels, and artifacts are gitignored local
files; for a new run, capture your own corpus via
`llm-bench -capture -mcp-stdio-command "..."`, then label and run the
manual-report path. The accepted run is a plain-chat baseline — a
tool-use or agent ranking requires a tool-bearing corpus. The Round-2
frontier challenge was useful as a diagnostic but was not accepted because
its challenge partition was too easy for the frontier candidate.

## Customizing the lineup

### The short story

Edit `models.json`. Restart. Done.

`go-llm` loads models at boot from a single config file and profiles each
one on first use. There is no roster file to edit in Go, no build-time
list to regenerate, and no capability list to keep in sync by hand —
capabilities are discovered at runtime via the provider's model-info
endpoint and cached in the fingerprint store (SQLite).

### What a model entry looks like

```json
{
  "models": {
    "coding": {
      "name": "llama-4-scout:q5_k_m",
      "provider": "ollama",
      "type": "dense",
      "context_window": 131072,
      "fallbacks": ["fast", "lightweight"]
    }
  }
}
```

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Whatever the provider uses to load the model |
| `provider` | no | Defaults to `"ollama"`; any key from the `providers` map |
| `type` | yes | `"dense"`, `"moe"`, or `"embedding"` (validates fallback compatibility) |
| `context_window` | no | Observed/overridden at runtime by fingerprint if wrong |
| `dimensions` | no | For embedding models |
| `fallbacks` | no | Role names to degrade to when the primary is unavailable |
| `description`, `parameters` | no | Human-readable only |

### Adding a new provider

`providers` is config-driven. `api_format` selects the backend client and
accepts exactly two values: `openai-compat` (llama.cpp / vLLM / LM Studio /
any OpenAI `/v1` server) and `ollama` (the default when omitted). `base_url`
is the **server root — without `/v1`** for `openai-compat`; go-llm appends the
per-endpoint paths internally.

```json
{
  "providers": {
    "llamacpp":  { "base_url": "http://127.0.0.1:8090", "timeout": "5m", "api_format": "openai-compat" },
    "lm-studio": { "base_url": "http://localhost:1234",  "timeout": "5m", "api_format": "openai-compat" },
    "ollama":    { "base_url": "http://localhost:11434", "timeout": "5m" }
  }
}
```

Then any model's `provider` field references the key. The `openai-compat`
client (`provider/openaicompat`) is implemented and is how llama.cpp and the
Setup 2 / Setup 3 paths from [setups.md](setups.md) run today; set the
provider's `api_key` field only if the server requires a Bearer token.

### Capability detection

The first time a model is used, `fingerprint/` queries the provider for
model metadata and records:

- **Kind** — completion, embedding, tool-call, thinking-mode support
- **Latency** — first-token and inter-token latency
- **Throughput** — tokens per second under load
- **Resources** — peak memory, cold-start time

The catalog (`provider/catalog.json`) matches model **families** (e.g.
`qwen3`, `gemma4`, `codellama`, `deepseek`) by name prefix and supplies
FIM-policy and thinking-mode defaults. If a model's family isn't in the
catalog, the system falls back to runtime-detected capabilities and a
neutral default policy — it still works; it just won't get family-specific
tuning until you add a catalog entry.

### Router weight profiles

`provider/router_score.go` ships default profiles for `fim`, `chat`,
`embedding`, `reasoning`, `code-review`, `agent`, and `tool-use`
(`agent` and `tool-use` are aliased to the same values). The profiles
weigh generic traits — speed, quality, feedback — not specific models,
so they apply unchanged to any lineup. If your own models have unusual
speed/quality tradeoffs (e.g., a reasoner that's slow but very high
quality), retune by constructing the router with
`provider.WithWeightOverrides(map[string]*WeightProfile{...})`.

### OpenAI-compat aliases

If you expose the optional `compat/` package's OpenAI-compatible HTTP
façade, `compat.RecommendedAliases()` provides a conservative preset for
tools that hardcode OpenAI model names (`gpt-4`, `gpt-4o`,
`text-embedding-3-large`, etc.). The server default is an empty alias
map; install aliases explicitly with `compat.WithAliases()`.

For a custom lineup, start from the recommended preset and override the
entries that should point somewhere else:

```go
aliases := compat.RecommendedAliases()
aliases["gpt-4"] = "ollama/llama-4-scout:q5_k_m"
aliases["text-embedding-3-large"] = "ollama/nomic-embed-code"

srv := compat.New(router, registry, providers,
    compat.WithAliases(aliases),
)
```

**These aliases reference specific go-llm model IDs and will move as the
reference lineup evolves.** Treat the alias map as a view over
`models.json`, not as a second source of truth.

## Unvalidated claims to verify on deploy

The `context_window` values in `models.json` (256000 for `gemma4:31b` /
`qwen3.6:35b-a3b`, 262144 for `qwen3-coder-next:latest`) are the
published model maxima and have **not** been validated at build time —
the library does not boot Ollama in unit tests. First use on a given
machine, the `fingerprint/` package will observe the real context
window the provider reports and cache it. If a published figure is wrong,
prompt-overflow bugs will surface at runtime, not at install.

Mitigation: after pulling, run `ollama show <model>` (or the equivalent
for your provider) once and compare the `num_ctx` figure with
`models.json`. If they disagree, update `models.json` rather than
trusting the source.

The catalog keeps both `qwen3-coder-next:latest` and `qwen3-coder-next:80b`
variants with identical specs. `latest` tracks Ollama's floating tag and
is what `models.json` references; `80b` is retained so a consumer that
pins explicitly still gets curated scoring data. `TestQwen3CoderNextVariantsInSync`
enforces they stay identical.

## Alternate paths — GLM-5.1 and MiniMax experiments

Setup 2 (GLM-5.1 in LM Studio) and Setup 3 (MiniMax M2.7) from
[setups.md](setups.md) remain candidate upgrades. They're deferred,
not rejected. The Round-2 GLM-5.1 probe was inconclusive because the
challenge corpus saturated at the top model; the next GLM/MiniMax attempt
needs a harder corpus before latency or provider-integration work can change
the decision. The benchmark harness
([benchmark-plan.md](benchmark-plan.md)) is the decision gate: if a
frontier non-Ollama model demonstrates, on an accepted harness run
(acceptance criteria in
[harness-results.md](harness-results.md#acceptance-gate)), a median
`AnswerQuality` improvement of **≥0.05** with no latency regression beyond
**1.5× p90**, that's the evidence to invest in the second-backend
abstraction. Until that evidence exists, Setup 1 wins on integration cost.

## Files consulted when changing the lineup

- `models.json` — role assignments (the only required edit for most
  changes)
- `provider/catalog.json` — family metadata (add a family entry if your
  model belongs to a new family not already listed)
- `docs/GETTING_STARTED.md` — "Pull Required Models" section, if you
  ship a consumer-facing install guide
- `docs/llm/` — this directory, if the change is significant enough to
  warrant narrative updates

No code changes are required in `config/`, `provider/`, `ollama/`,
`rag/`, `completion/`, `analysis/`, `mcp/`, `conversation/`,
`feedback/`, `fingerprint/`, `prefetch/`, or `compat/` to add or swap
models. The `provider.Router` signature is stable; new models get picked
up automatically via `config.Load` and the catalog.
