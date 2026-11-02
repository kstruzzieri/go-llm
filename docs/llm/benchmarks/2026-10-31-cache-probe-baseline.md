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

## P2: cache-stable compaction (2026-11-01)

P2 changes compaction so it stops moving the cached prefix. Inside a goal, a
compacting step now drops to 75% of the state budget instead of "just enough",
and what it dropped stays out of every later request in that goal. Across
turns, Golem's history compression counts only the text it replays and evicts
to half its trigger.

### Provenance
- **Commit**: `f27b63a9` (dirty: no)
- **Machine, server, model**: as above (llama-swap in front of llama-server,
  `openai-compat/qwen3-coder-next:latest`, local)
- **Cloud targets**: not run
- **Comparison**: the P1 head (`00912c90`, per-result fence keys, before P2),
  same commands, same server
- **Exact commands**: the two commands above, run with a binary built at
  `f27b63a9`. The probe now assembles with `golem.NewContextManager(false)`,
  which sets the 0.75 low-water mark Golem uses.

### pressure

| Turn | Step | P1 prompt | P1 cached | P2 prompt | P2 cached | P2 prefix ratio (proxy) |
|---:|---:|---:|---:|---:|---:|---:|
| 1 | 1 | 523 | 519 | 523 | 519 | 0.000 |
| 1 | 2 | 2504 | 523 | 2500 | 523 | 0.999 |
| 1 | 3 | 4550 | 2504 | 4546 | 2500 | 1.000 |
| 1 | 4 | 6596 | 4550 | 6590 | 4546 | 1.000 |
| 1 | 5 | 8636 | 6596 | 8640 | 6590 | 1.000 |
| 1 | 6 | 8699 | 0 | 6659 | 0 | 0.053 |
| 1 | 7 | 8696 | 0 | 8702 | 6659 | 1.000 |
| 1 | 8 | 8699 | 0 | 6655 | 0 | 0.053 |
| 1 | 9 | 8702 | 0 | 8696 | 6655 | 1.000 |
| 2 | 1 | 551 | 498 | 551 | 498 | 0.052 |
| 3 | 1 | 579 | 551 | 579 | 551 | 0.999 |
| 4 | 1 | 607 | 579 | 607 | 579 | 0.999 |

### uncompacted (P2, unchanged from P1)

| Turn | Step | Prompt tokens | Cached | Prefix ratio (proxy) |
|---:|---:|---:|---:|---:|
| 1 | 1 | 523 | 498 | 0.000 |
| 1 | 2 | 2498 | 523 | 0.999 |
| 1 | 3 | 4542 | 2498 | 1.000 |
| 1 | 4 | 6584 | 4542 | 1.000 |
| 1 | 5 | 8626 | 6584 | 1.000 |
| 1 | 6 | 10668 | 8626 | 1.000 |
| 1 | 7 | 12715 | 10668 | 1.000 |
| 1 | 8 | 14756 | 12715 | 1.000 |
| 1 | 9 | 16799 | 14756 | 1.000 |
| 2 | 1 | 551 | 519 | 0.027 |
| 3 | 1 | 579 | 551 | 0.999 |
| 4 | 1 | 607 | 579 | 0.999 |

### Reading

Under pressure, turn 1 now serves **27,992 of 53,511 prompt tokens (52.3%)**
from cache, against **14,692 of 57,605 (25.5%)** at the P1 head. Before P2,
every step from 6 on re-fitted the history just under the ceiling, so the first
surviving tool result moved and the cache served nothing. Now a compacting step
(6 and 8) still misses once, and the step after it (7 and 9) serves the whole
previous request from cache. The prompt is also smaller (53.5k against 57.6k
tokens), because a compacting step keeps less history.

Compaction still runs every other step in this scenario. That is a property of
the fixture, not of the mark: each tool result is about 2,000 tokens, a quarter
of the 8,192-token budget, so dropping to 75% leaves room for exactly one more
result. With a larger ceiling relative to each result, the cut point holds for
more steps. The 0.75 mark is the spec's chosen value and was not tuned here.

The uncompacted scenario is unchanged from P1 (each step serves the previous
request from cache), as expected: nothing in it is compacted.

### Unit-level measurements (from the P2 tests)

- **In-Run cut point** (`TestRunCompactionCutPointStable`, 13 requests): the
  first retained tool result changed in 3 requests at the 0.75 mark, against 7
  with the mark off.
- **Mixed assembly alternative churn** (`TestMixedAlternativeChurnCharacterized`,
  Deviation 3): an earlier alternative's rendered bytes changed in 3 of 7
  requests. Mixed assembly (structured retrieval anchors) still upgrades and
  downgrades alternatives as free budget changes. P2 makes its evictions sticky
  but does not fix this. The live probe never enters mixed assembly.
- **Cross-turn summary rewrites** (`TestCompressionDoesNotRewriteSummaryEveryTurn`,
  60 tool-heavy turns at an 8,192 ceiling): the summarizer ran 56 times before
  P2 and 3 times after.
