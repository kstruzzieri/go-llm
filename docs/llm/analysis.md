# LLM Landscape Analysis — April 2026

## Context

Keith runs an M3 Max, 128GB RAM, developing three apps that depend on
`go-llm`:

- **Firn IDE** — custom Wails IDE, needs FIM / inline completion + RAG
- **Flux ML** — Wails ML dev environment, needs chat + analysis
- **Quantum Trader** — Go+Python trading platform, needs agent loops + tool use

The existing model lineup (`qwen3-coder-next`, `qwen3.5:35b-a3b`,
`qwen3.5:27b`, `qwen3:8b`, `qwen3-embedding:8b`) was selected in late 2025.
Several frontier open-weight releases in Q1 2026 warrant a re-evaluation.

## Hardware budget

- Total unified memory: 128GB
- Usable for models (after OS, IDEs, build toolchains): **~97GB**
- Memory bandwidth: 400 GB/s (40-core GPU M3 Max)

MoE (Mixture of Experts) models are preferred: total params dictate memory,
active params dictate latency. At 400 GB/s, a 3B-active MoE feels
dramatically faster than a dense 14B.

## 2026 model landscape — relevant candidates

### Coding-focused

| Model | Total / Active | SWE-bench Verified | Memory @ Q4 | Notes |
|---|---|---|---|---|
| **Qwen3-Coder-Next** | 80B / 3.9B | 70.6% | ~46GB | Already in use. Still top-tier agentic coding workhorse. |
| **Qwen3.6-35B-A3B** | 35B / 3B | 73.4% | ~20GB | Direct drop-in for qwen3.5:35b-a3b. 86.0% GPQA, 92.7% AIME 2026. |
| **Qwen3.6 Plus** | closed/cloud | 78.8% | — | Cloud-only, 1M context. Not applicable for local. |
| **GLM-5.1** | 754B / 40B | 77.8% | ~95–110GB UD-Q2_K_XL | Tight fit, displaces everything else. |
| **MiniMax M2.7** | 229B / 10B | ~80% | ~115GB Q4 | Best tool-calling (76.8% BFCL). Very tight. |
| **Kimi K2.5** | 1T / 32B | 76.8% | ~500GB+ | Out of budget. |

### Agent / tool-use / reasoning

| Model | Params | τ2-bench (agentic) | LiveCodeBench v6 | Memory | Notes |
|---|---|---|---|---|---|
| **Gemma 4 31B Dense** | 30.7B | **86.4%** | **80.0%** | ~20GB | Released 2026-04-02. #3 Arena AI open. Native function calling + thinking modes. |
| **Gemma 4 26B A4B MoE** | 25.2B / 3.8B | — | high | ~18GB | Latency-optimized. #6 Arena AI open. |
| Llama 4 (frontier variant) | varies | 85.5% | 77.1% | — | No local-friendly variant at the time of writing. |
| DeepSeek V4 | varies | 57.5% | 52.0% | — | Dramatically weaker on agentic tasks. |

### Embedding

| Model | Params | MTEB | Strengths | Notes |
|---|---|---|---|---|
| **Qwen3-Embedding-8B** | 8B | 70.58 (multilingual #1 as of June 2025) | Multilingual + code | Already in use. Keep. |
| Nomic-Embed-Code | 7B | code-specialized | Beats Voyage Code 3, OpenAI Embed 3 Large on CodeSearchNet | Strong alternative if code retrieval becomes the bottleneck. |
| Gemini Embedding 2 | API | 84.0 (MTEB Code) | Best code retrieval | Not local. |
| Voyage code-3 | API | — | Strong for code/technical | Not local. |

## Framework: Ollama vs llama.cpp vs MLX

As of Ollama 0.19 (Feb 2026), **Ollama is built on MLX on Apple Silicon**.
The prior narrative ("MLX is 20–30% faster than Ollama") is largely obsolete
— Ollama now inherits MLX's unified-memory throughput advantages.

Practical implication: **keep `go-llm` pointed at Ollama**. Adding a second
backend (llama.cpp, LM Studio, mlx-lm) should be a deliberate decision
justified by measurable workload improvement, not speculative performance
optimization. See [benchmark-plan.md](benchmark-plan.md).

When a second backend does become justified:
- LM Studio exposes an OpenAI-compatible HTTP API → minimal adapter work
- llama.cpp's `server` binary exposes OpenAI-compatible + native APIs
- mlx-lm has a `server` but lacks the stability of the above

Either way, the existing `provider.Provider` interface is the right seam.
The `ollama.Client` is not — it's wire-format specific.

## Why not Kimi K2.5, GLM-5.1, MiniMax M2.7?

All three are frontier-tier but misaligned with a **simultaneous
multi-role fleet**:

- **Kimi K2.5** — 1T params; does not fit at any usable quantization.
- **GLM-5.1** — best SWE-bench among open, but UD-Q2_K_XL (~100GB)
  displaces every other model. You'd load it, use it, unload it, reload
  the fleet. Viable for a "heavy task" escape hatch; not viable as the
  daily driver.
- **MiniMax M2.7** — best BFCL (tool calling), but at Q4 occupies ~115GB.
  Same displacement problem as GLM-5.1.

Gemma 4 31B achieves comparable tool-calling quality (86.4% τ2-bench vs
MiniMax's 76.8% BFCL — different benchmarks, but Gemma's is broader and
more recent) in **one-fifth the memory footprint**. That makes it the
correct choice for a co-resident fleet.

## Sources

- [Best Open-Source Coding Model 2026 — Morph](https://www.morphllm.com/best-open-source-coding-model-2026)
- [Qwen vs DeepSeek vs GLM Benchmark Comparison](https://blog.easecloud.io/ai-cloud/qwen-vs-deepseek-vs-glm/)
- [Qwen 3.6 Developer Guide — Lushbinary](https://lushbinary.com/blog/qwen-3-6-developer-guide-benchmarks-architecture-api-self-hosting/)
- [Qwen3.6-35B-A3B — Hugging Face](https://huggingface.co/Qwen/Qwen3.6-35B-A3B)
- [Gemma 4 — Google DeepMind](https://deepmind.google/models/gemma/gemma-4/)
- [Welcome Gemma 4 — Hugging Face](https://huggingface.co/blog/gemma4)
- [Gemma 4 Coding Benchmarks 2026 — Gemma 4 Wiki](https://www.gemma4.wiki/benchmark/gemma-4-coding-performance-benchmarks-2026)
- [Google Gemma 4 Review — StartupHub](https://www.startuphub.ai/ai-news/technology/2026/google-gemma-4-review-2026)
- [Gemma 4 Family Guide — aimadetools](https://www.aimadetools.com/blog/gemma-4-family-guide/)
- [Gemma 4 releases — Google AI for Developers](https://ai.google.dev/gemma/docs/releases)
- [GLM-5.1 How to Run Locally — Unsloth](https://unsloth.ai/docs/models/glm-5.1)
- [Ollama is now powered by MLX on Apple Silicon](https://ollama.com/blog/mlx)
- [2026 Mac Inference Framework Comparison — MACGPU](https://macgpu.com/en/blog/2026-mac-inference-framework-vllm-mlx-ollama-llamacpp-benchmark.html)
- [Qwen3-Embedding-8B — Hugging Face](https://huggingface.co/Qwen/Qwen3-Embedding-8B)
- [Nomic Embed Code — Hugging Face](https://huggingface.co/nomic-ai/nomic-embed-code)
- [Qwen3-Coder-Next Local Guide 2026 — DEV](https://dev.to/sienna/qwen3-coder-next-the-complete-2026-guide-to-running-powerful-ai-coding-agents-locally-1k95)
- [VRAM Calculator — apxml](https://apxml.com/tools/vram-calculator)
- [Ollama Search](https://ollama.com/search)
- [Hugging Face Ollama-compatible trending](https://huggingface.co/models?apps=ollama&sort=trending)
