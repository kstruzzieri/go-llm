# Recommended Lineup + Migration Plan

## Decision

Adopt **Setup 1 — Balanced Daily Driver** as the production `go-llm`
lineup. Run **Setup 2 (GLM-5.1 in LM Studio)** as a parallel off-Ollama
experiment to gather evidence for whether second-backend work is
justified.

## Target lineup

| Role | Model | Source | Status |
|---|---|---|---|
| `coding` | `qwen3-coder-next:latest` | Ollama | **keep** |
| `agent` | `gemma4:31b` | Ollama | **add** |
| `general` | `gemma4:31b` | Ollama | **retarget** (was `qwen3.5:27b`) |
| `analysis` | `gemma4:31b` | Ollama | **retarget** (was `qwen3.5:27b`) |
| `fast` | `qwen3.6:35b-a3b` | Ollama | **add** (replaces `qwen3.5:35b-a3b`) |
| `lightweight` | `qwen3:8b` | Ollama | **keep** |
| `embedding` | `qwen3-embedding:8b` | Ollama | **keep** |

## Migration steps

### Phase 1 — Pull new models (10 min)

```bash
ollama pull gemma4:31b          # ~20GB dense
ollama pull gemma4:26b          # ~18GB MoE (optional, latency alternative)
ollama pull qwen3.6:35b-a3b     # ~28GB MoE
```

### Phase 2 — Validate (1–2 days of normal use)

Run with both new and old models pulled. `models.json` updated to point
to new models, but old models still available. Monitor:

- `gemma4:31b` on agent workloads (Quantum Trader MCP loops, Firn code
  actions)
- `qwen3.6:35b-a3b` on general chat / analysis
- Existing `qwen3-coder-next` workflows for regression (they should be
  unchanged)

### Phase 3 — Retire legacy (5 min)

Once Phase 2 passes:

```bash
ollama rm qwen3.5:27b
ollama rm qwen3.5:35b-a3b
```

### Phase 4 — Parallel GLM-5.1 experiment

Independent of the main migration:

1. Install LM Studio (or `llama.cpp` server).
2. Pull `glm-5.1-UD-Q2_K_XL` (via Unsloth).
3. Run the benchmark harness (see [benchmark-plan.md](benchmark-plan.md))
   comparing Qwen3-Coder-Next and GLM-5.1 on real captured traces.
4. **Decision gate**: if GLM-5.1 shows ≥5% quality improvement on real
   workloads (not synthetic benchmarks), invest in the second-backend
   abstraction (Setup 2).

Expected outcome: in most cases the quality gain will not justify the
integration cost. The experiment's real value is *evidence* that
defending Setup 1 against speculative criticism ("but GLM-5.1 is better
on SWE-bench!") is correct for this workload.

## Unvalidated claims to verify on deploy

The `context_window` values in `models.json` (256000 for `gemma4:31b` /
`qwen3.6:35b-a3b`, 262144 for `qwen3-coder-next:latest`) are the
published model maxima and have **not** been validated at build time —
the library does not boot Ollama in unit tests. First use on a given
machine, the `fingerprint/` package will observe the real context
window Ollama reports and cache it. If a published figure is wrong,
prompt-overflow bugs will surface at runtime, not at install.

Mitigation: after pulling, run `ollama show <model>` once and compare
the `num_ctx` figure with `models.json`. If they disagree, update
`models.json` rather than trusting the source.

The catalog keeps both `qwen3-coder-next:latest` and `qwen3-coder-next:80b`
variants with identical specs. `latest` tracks Ollama's floating tag and
is what `models.json` references; `80b` is retained so a consumer that
pins explicitly still gets curated scoring data. `TestQwen3CoderNextVariantsInSync`
enforces they stay identical.

## Files touched in this migration

- `models.json` — role assignments
- `provider/catalog.json` — add `qwen3.6`, `qwen3-coder-next`, `gemma4`
  families
- `docs/GETTING_STARTED.md` — update "Pull Required Models" section
  *(follow-up PR, not blocking)*
- `docs/llm/` — this directory

No code changes required to `config/`, `provider/`, `ollama/`, `rag/`,
`completion/`, `analysis/`, `mcp/`, `conversation/`, `feedback/`,
`fingerprint/`, or `prefetch/` for Phase 1–3. The `provider.Router`
signature is stable; new models get picked up automatically via
`config.Load` and the catalog.

## Router weight profiles

`provider/router_score.go` ships default profiles for `fim`, `chat`,
`embedding`, `reasoning`, `code-review`, `agent`, and `tool-use`
(`agent` and `tool-use` are aliased to the same values). The agent
profile prioritizes Speed and Feedback more heavily than chat, on the
reasoning that tool-calling loops make many small calls and tool-call
accuracy is directly observable as success/failure. If real traces
suggest these weights are wrong, retune them — they are opinions, not
benchmarked values, and the benchmark harness (`cmd/llm-bench`) is the
right tool to validate them.

Consumers can override any profile at construction via
`provider.WithWeightOverrides(map[string]*WeightProfile{...})`.
