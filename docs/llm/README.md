# go-llm: Local Model Strategy (April 2026)

This directory documents the analysis and decisions behind the model lineup
powering `go-llm` and its consumers (Firn IDE, Flux ML, Quantum Trader) on
Apple Silicon workstations.

**Target hardware:** MacBook Pro M3 Max, 128GB unified memory (~97GB usable
after OS overhead).

## Documents

| File | Purpose |
|---|---|
| [analysis.md](analysis.md) | 2026 open-weight landscape, benchmark summary, sources |
| [setups.md](setups.md) | Three candidate setup combinations with tradeoffs |
| [recommendation.md](recommendation.md) | Chosen lineup + migration steps |
| [benchmark-plan.md](benchmark-plan.md) | Harness design for A/B'ing models on real traces |

## TL;DR

- **Keep** `qwen3-coder-next` (primary code generation) and
  `qwen3-embedding:8b` (embeddings).
- **Add** `gemma4:31b` (dense) — agent / tool-use / reasoning specialist.
  Fills the tool-calling gap; 86.4% τ2-bench, 80.0% LiveCodeBench.
- **Add** `qwen3.6:35b-a3b` — MoE upgrade over `qwen3.5:35b-a3b` (73.4%
  SWE-bench Verified with 3B active).
- **Retire** `qwen3.5:27b` and `qwen3.5:35b-a3b` after validation.
- **Stay on Ollama** — as of 0.19, it is backed by MLX on Apple Silicon,
  so the historical MLX-vs-Ollama speed gap is closed.
- **Defer** a llama.cpp / LM Studio backend abstraction until the
  benchmark harness shows Gemma 4 or a frontier non-Ollama model
  (GLM-5.1, MiniMax M2.7) produces a measurable quality delta on real
  workloads.

## Date stamp

All research was conducted April 16, 2026. Benchmark numbers and release
dates reflect that snapshot.
