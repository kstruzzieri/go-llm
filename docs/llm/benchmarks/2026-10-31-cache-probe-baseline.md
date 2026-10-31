# cache-probe-baseline — 2026-10-31

Baseline for the prompt-caching work (P0 of the Golem prompt-caching and cost
plan): how much of each request a local llama.cpp server serves from its
prompt cache today, before any change to fence keys, compaction or session
affinity.

## Provenance
- **Commit**: `287620fd` (dirty: no)
- **Machine**: Apple M3 Max, 128 GB
- **Server**: llama-swap in front of llama-server (llama.cpp build `b11146-7fe450e19`), local, OpenAI-compatible
- **Model under test**: `openai-compat/qwen3-coder-next:latest`
- **Cloud targets**: not run in P0 (local only by decision); cloud baselines arrive with the P1 measurement
- **Exact commands**:
  ```
  llm-bench -cache-probe -cache-probe-scenario uncompacted \
    -candidate-base-url http://127.0.0.1:8090 -models openai-compat/qwen3-coder-next:latest \
    -cache-probe-out probe-local-uncompacted.md
  llm-bench -cache-probe -cache-probe-scenario pressure \
    -candidate-base-url http://127.0.0.1:8090 -models openai-compat/qwen3-coder-next:latest \
    -cache-probe-out probe-local-pressure.md
  ```

## What the probe does

A scripted Golem session through the real agent loop: turn 1 reads eight
~6 KB fixture files (one `read_file` per step) and answers; turns 2-4 are
one-step follow-ups that carry the prior user and assistant turns as history.
Each request goes to the server for its usage counters; the replies are
scripted, so every run sends the same conversation.

- **Cached** is the server's `prompt_tokens_details.cached_tokens`: the ground truth.
- **Prefix ratio (proxy)** is the common byte prefix of consecutive requests'
  message lists (provider message JSON, not the outbound body) divided by the
  previous request's length. It explains *why* the counter moves; it is not
  itself a cache measurement.
- `uncompacted` uses a 131072-token input ceiling, so nothing is compacted;
  `pressure` uses Golem's default 8192, so compaction runs from step 6.

## Results

### uncompacted

| Turn | Step | Prompt tokens | Cached | Prefix ratio (proxy) |
|---:|---:|---:|---:|---:|
| 1 | 1 | 523 | 0 | 0.000 |
| 1 | 2 | 2500 | 523 | 0.999 |
| 1 | 3 | 4540 | 519 | 0.204 |
| 1 | 4 | 6576 | 0 | 0.112 |
| 1 | 5 | 8624 | 0 | 0.077 |
| 1 | 6 | 10676 | 0 | 0.059 |
| 1 | 7 | 12709 | 0 | 0.047 |
| 1 | 8 | 14780 | 0 | 0.040 |
| 1 | 9 | 16811 | 0 | 0.034 |
| 2 | 1 | 551 | 498 | 0.027 |
| 3 | 1 | 579 | 551 | 0.999 |
| 4 | 1 | 607 | 579 | 0.999 |

### pressure

| Turn | Step | Prompt tokens | Cached | Prefix ratio (proxy) |
|---:|---:|---:|---:|---:|
| 1 | 1 | 523 | 498 | 0.000 |
| 1 | 2 | 2500 | 523 | 0.999 |
| 1 | 3 | 4540 | 519 | 0.204 |
| 1 | 4 | 6570 | 0 | 0.112 |
| 1 | 5 | 8624 | 0 | 0.077 |
| 1 | 6 | 8691 | 0 | 0.053 |
| 1 | 7 | 8700 | 0 | 0.053 |
| 1 | 8 | 8693 | 0 | 0.053 |
| 1 | 9 | 8710 | 0 | 0.053 |
| 2 | 1 | 551 | 498 | 0.052 |
| 3 | 1 | 579 | 551 | 0.999 |
| 4 | 1 | 607 | 579 | 0.999 |

## Reading

Inside one goal, the cache stops working after the second tool result. In the
uncompacted run the server serves 523 and 519 tokens from cache at steps 2-3,
then **0 from step 4 through step 9**: about 1,000 of the turn's 77,739 prompt
tokens (1.3%) were cached. The proxy shows why. Every request re-frames every
earlier tool result with a fresh fence key, so consecutive requests share only
the bytes before the first tool result (~520 tokens), and that shared share
falls from 20% to 3% as results accumulate. The counter then drops to zero
rather than staying near 520; the likely cause (not verified here) is
llama-server's slot-similarity check declining to reuse a slot whose cached
prompt matches only a small fraction of the new one.

Under pressure the prompt plateaus near 8.7k tokens once compaction starts, and
the cache stays at zero: compaction changes the history every step on top of
the key churn.

Follow-up turns already cache well (498, 551 and 579 of 551, 579 and 607
tokens). Golem replays only user and assistant text across turns, so no fence
keys appear there. The cost is inside a goal, which is what the next phase
(per-result fence keys) targets; the expected result there is a proxy ratio at
or above 0.95 for steps 3-9 and a cached count that tracks the prompt.
