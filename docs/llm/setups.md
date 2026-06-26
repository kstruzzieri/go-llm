# Three Candidate Setups

Each option is evaluated on co-residency (models that can stay warm
simultaneously), `go-llm` integration cost, and quality ceiling.

> **`models.json` is authoritative.** The model IDs below describe three
> candidate configurations. Setup 1 is what's shipped by default. To
> substitute your own models for any role (or add a new role), edit
> `models.json` — no Go code changes are required. See
> [recommendation.md#customizing-the-lineup](recommendation.md#customizing-the-lineup).

---

## Setup 1 — Balanced Daily Driver (current default)

**Philosophy:** All-Ollama, full co-residency, zero architectural change
to `go-llm`. This is what `models.json` ships with.

| Role | Model | Size on disk | Notes |
|---|---|---|---|
| `coding` | `qwen3-coder-next:latest` | ~46GB | 70.6% SWE-bench |
| `agent` | `gemma4:31b` (dense) | ~20GB | 86.4% τ2-bench, 80.0% LiveCodeBench, native function calling |
| `general` / `analysis` | `gemma4:31b` (thinking mode on) | shared | Gemma 4 doubles as reasoner |
| `fast` | `qwen3.6:35b-a3b` | ~28GB (Q6) | Superseded the legacy qwen3.5:35b-a3b |
| `lightweight` | `qwen3.5:9b-mtp` | ~6GB | MTP FIM completion |
| `embedding` | `qwen3-embedding:8b` | ~5GB | |

**Resident memory:** ~77GB with the optional `qwen3.6:35b-a3b`, or ~57GB
without. Comfortable headroom.

**Pros:**
- No backend-abstraction work in `go-llm`
- Every gap filled: coding, agent/tool-use, reasoning, fast chat, FIM, embeddings
- Qwen3-Coder-Next stays as the specialist code generator

**Cons:**
- No GLM-5.1 / MiniMax ceiling — we cap at ~77% SWE-bench equivalent
- Gemma 4 was ~2 weeks old at initial adoption; stability has held in
  daily use but long-term trajectory still depends on upstream maintenance

---

## Setup 2 — SWE-Bench Maximalist

**Philosophy:** Accept model-swapping for best-in-class agentic coding
ceiling.

| Role | Model | Runtime | Mem |
|---|---|---|---|
| `heavy-code` | **GLM-5.1** | LM Studio (MLX) or llama.cpp | ~95–110GB |
| `coding` (fallback) | `qwen3-coder-next` | Ollama | ~46GB |
| `agent` | `gemma4:31b` | Ollama | ~20GB |
| `general` | `qwen3.6:35b-a3b` | Ollama | ~28GB |
| `embedding` | Nomic-Embed-Code or Qwen3-Embedding-8B | Ollama | ~5GB |

**Pros:**
- 77.8% SWE-bench ceiling (close to Claude Opus 4.6's 80.8%)
- Nomic-Embed-Code is state-of-the-art for code retrieval

**Cons:**
- Requires a second inference backend in `go-llm` (~500–1000 LoC + tests)
- Cold-load of GLM-5.1 is ~60s; router must encode this in warmth score
- GLM-5.1 displaces everything while loaded — swap-in/swap-out UX

**Backend abstraction shape:**

```
provider/
├── ollama.go         (existing)
├── openai_compat.go  (NEW — LM Studio, llama.cpp server, vLLM)
└── mlx_lm.go         (NEW — optional native mlx-lm integration)
```

All routed through the existing `provider.Provider` interface.

---

## Setup 3 — Agentic Tool-Use Specialist

**Philosophy:** Optimize for MCP server + multi-step tool-calling
workflows.

| Role | Model | Runtime | Mem |
|---|---|---|---|
| `agent` | **MiniMax M2.7** | LM Studio (MLX) Q4 | ~115GB |
| `coding` (swap) | `qwen3-coder-next` | Ollama | ~46GB |
| `general` | `gemma4:31b` | Ollama | ~20GB |
| `fast` | `qwen3.6:35b-a3b` | Ollama | ~28GB |
| `embedding` | `qwen3-embedding:8b` | Ollama | ~5GB |

**Pros:**
- 76.8% BFCL (tool calling) — best in class among open-weight models
- Strong on multi-step planning and orchestration
- Good for MCP-heavy workloads (Quantum Trader live trading loops)

**Cons:**
- Same backend-abstraction cost as Setup 2
- M2.7 at Q4 dominates memory; displaces the Ollama fleet when loaded
- **BFCL vs τ2-bench** — BFCL is synthetic; τ2-bench (where Gemma 4 31B
  leads) is realistic retail-task agents. For Keith's MCP workload, the
  Gemma 4 advantage may matter more. This needs empirical validation —
  see [benchmark-plan.md](benchmark-plan.md).

---

## Decision matrix

| Criterion | Setup 1 | Setup 2 | Setup 3 |
|---|---|---|---|
| Quality ceiling | Good | Best | Best-for-tools |
| Co-residency | Full | Partial | Partial |
| `go-llm` changes | Config only | Second backend | Second backend |
| Time to deploy | ~30 min | ~1 week | ~1 week |
| Incremental cost if wrong | Low | High | High |

**Current default: Setup 1** (see `models.json`). Setups 2 and 3 remain
candidate upgrades; the benchmark harness
([benchmark-plan.md](benchmark-plan.md)) is the decision gate for when
second-backend work is justified. See
[recommendation.md](recommendation.md) for how to evolve the lineup
without touching Go code.
