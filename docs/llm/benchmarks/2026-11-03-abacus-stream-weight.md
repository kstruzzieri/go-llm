# abacus-stream-weight — 2026-11-03

How Abacus RouteLLM weights cached tokens in a streamed `prompt_tokens`, and
whether the exact input count can be recovered from it. Since #753, go-llm
reported a streamed Abacus request that reused cached input as input `0`
(unknown), and Golem left it out of the dollar total.

**Result: for four models the streamed count inverts exactly.** Abacus streams
`prompt_tokens = uncached + floor(cached * w)`, where `w` is the served
model's `cached_input_token_rate / input_token_rate` from Abacus's own
`/v1/models`. Given `w`, `uncached = prompt_tokens - floor(cached * w)` and the
input is `uncached + cached`. All 84 measured frames with cached tokens on
those four models, and 12 more from growing conversations, recovered the exact
count. Models that bill cache writes (the three Claude models probed,
`gpt-6-luna`, `gpt-6-astra` and `gpt-5.6-luna`) cannot be inverted this way, even on a cold
request, so go-llm reports a stream from any model outside the table as
unknown.

## Provenance
- **Commit**: develop `39b207a2`; requests sent directly to the endpoint, then the change checked through Golem (below)
- **Endpoint**: `https://routellm.abacus.ai/v1/chat/completions`
- **Exact count**: a non-streaming response's `raw_input_tokens + cache_read_input_tokens` (#753), and a cold stream's `prompt_tokens` where nothing was cached
- **Design**: per prompt size, a fresh random nonce at the head of the system
  prompt, then four requests on the same prompt: a cold stream, a warm stream,
  a warm non-stream, a warm stream. Sizes from about 2k to 7.4k tokens, chosen
  so `cached * w` lands on a spread of fractional parts. A second run grew one
  conversation three turns (each adding about 1.2k tokens) and sent each turn
  streamed and non-streamed.

## Weight per model

| Model | Published rates (cached / input, per 1M) | `w` | Prompt sizes | Frames with cached tokens | Exact | Growing turns exact |
|---|---|---:|---:|---:|---:|---:|
| `gpt-5-mini` | $0.025 / $0.25 | 1/10 | 10 | 30 | 30 | 3 of 3 |
| `gemini-3.5-flash` | $0.15 / $1.50 | 1/10 | 6 | 12 | 12 | 3 of 3 |
| `grok-4.3` | $0.20 / $1.25 | 4/25 | 6 | 24 | 24 | 3 of 3 |
| `deepseek-ai/DeepSeek-V4-Flash-0731` | $0.028 / $0.44 | 7/110 | 6 | 18 | 18 | 3 of 3 |

"Frames" counts streamed `prompt_tokens` and non-streaming `input_tokens`,
which were equal for the same prompt in every case.

- **Floor, not round.** On `gpt-5-mini`, `cached * w` with fractional parts
  .6 and .8 was truncated every time (344 for 76 uncached plus 268.8). The
  DeepSeek and Gemini samples agree.
- **Exact products stay exact.** `gpt-5-mini` with 1,920 cached (192) and
  `grok-4.3` with 3,200 cached (512) recovered exactly, so Abacus's arithmetic
  does not fall one short at whole numbers.
- **The ratio comes from Abacus's listing.** `w` matched the listed rate ratio
  on all four models, including DeepSeek's 7/110. Golem's own `price` config is
  not used: a price is what the user expects to pay, and a typo in it should
  not change a reported token count.

## Models that cannot be inverted

Models that bill cache writes add the written tokens at a 1.25 premium that a
stream does not report. A follow-up run the same day (#759, branch
`fix/abacus-cache-write-usage` on #758's head `6e23c5ae`) measured them with
the same nonce design: per prompt size, a cold stream, a warm non-stream, a
grown turn (about 1,000 to 1,600 new tokens) streamed and then non-streamed,
and a cold non-stream and warm stream on a second fresh prompt. Every model
probed bills writes:

| Served model | Listed rates (cached / input, per 1M) | Read `w` | Prompt sizes | Cold stream, example (reported for exact) |
|---|---|---:|---:|---|
| `claude-haiku-4-5-20251001` | none / $1.00 | 1/10 | 6 | 5,691 for 4,554 |
| `claude-haiku-5-5` | $0.01 / $0.10 | 1/10 | 2 | 7,221 for 5,778 |
| `claude-sonnet-5-5` | $0.10 / $2.00 | 1/20 | 1 | 7,214 for 5,772 |
| `gpt-6-luna` | $0.01 / $0.10 | 1/10 | 6 | 4,730 for 3,785 |
| `gpt-6-luna` via `route-llm` | $0.10 / $2.00 (`route-llm`) | 1/10 | 2 | 4,729 for 3,784 |
| `gpt-5.6-luna` | $0.02 / $0.20 | 1/10 | 2 | 4,729 for 3,784 |
| `gpt-6-astra` | $1.00 / $10.00 | 1/10 | 1 | 4,728 for 3,783 |

All 20 prompt sizes followed the same rules, with `u` the uncached input,
`R` the tokens read and `W` the tokens written:

- **A cold stream reports `u + floor(1.25 * W)`** and carries no
  `prompt_tokens_details`, 20 of 20. The written count's remainder mod 4 was
  0, 1, 2 and 3 across the sizes, so the premium is exactly 1.25 and floored.
- **A cold non-streaming `input_tokens` is `u + floor(1.25 * W)` too**, next to
  `raw_input_tokens` (`u`) and `cache_creation_input_tokens` (`W`), 20 of 20.
  The exact input is `raw_input_tokens + cache_creation_input_tokens`.
- **A grown turn streams `u + floor(w * R + 1.25 * W)`**, one floor over the
  sum, 20 of 20; flooring each part separately fits 11. It reports `R` as
  `cached_tokens` and nothing for `W`, so inverting it as a read-only frame
  overshoots by a quarter of the new tokens (6,008 against an exact 5,666 in
  the first run's growing `gpt-6-luna` conversation).
- **A non-streaming grown turn reports both counters.** Four of four sent
  `cache_read_input_tokens` and `cache_creation_input_tokens` together, and
  `raw + read + creation` matched the exact count from repeating the request
  (5,802 = 3 + 4,550 + 1,249 on Claude Haiku 4.5).
- **A warm stream with nothing written** reports `u + floor(w * R)`, as on the
  models in the table, but a frame cannot show that nothing was written.

So no streamed frame from these models can be inverted. A frame with `R`
reported has two unknowns (`u` and `W`) in one equation, and a cold frame has
the same shape whether it wrote the cache or was too short to cache at all:
a 3,116-token Claude Haiku 4.5 prompt, which Abacus did not cache, streamed
its exact count in the same no-details shape as a 4,554-token prompt reported
as 5,691. The usage chunk of all 64 follow-up streams carried only `choices`,
`created`, `id`, `model`, `object` and `usage`.

`/v1/models` lists no cache-write rate, and write billing does not follow the
listed provider: the three GPT models are listed under `openai`. go-llm
therefore trusts an Abacus stream's count only for the four models in the
table above, measured not to write: on their cold streams the count matched
the exact input (22 of 22 on `gpt-5-mini`, `gemini-3.5-flash` and DeepSeek,
and 6 of 6 on `grok-4.3` once its 128-token preamble was inverted), and no
response from them carried `cache_creation_input_tokens`. A stream
from any other served model reports input and total as 0 (unknown), cold or
warm. A non-streaming response reports the exact input on every model, with
the written tokens as the cache-write count.

Several Claude models on Abacus list a cached rate, so a listed rate alone does
not mark a model as safe to invert.

## Cache counter when nothing is cached

Abacus leaves the cache counter out instead of sending 0. Of the streamed
frames above, all 33 with nothing cached (five models) had no
`prompt_tokens_details`, and all 81 with cached tokens (six models) had one
with `cached_tokens` above 0; none had `cached_tokens: 0` or a details object
without it. Four tiny cold streams (10 to 15 tokens) on `gpt-5-mini`,
`gemini-3.5-flash`, DeepSeek and Claude Haiku also had no details. Every
non-streaming response with nothing cached (Claude Haiku, `gemini-3.5-flash`
and `gpt-6-luna`) had no `cache_read_input_tokens`; a separate check on
`gpt-5-mini` found the same for a cold 1,985-token and a cold 27-token request.
`grok-4.3` reports 128 cached tokens even on a first request (a shared
preamble), so it always carries the counter. On an Abacus base URL go-llm
therefore reads an absent counter as 0 cached, reported.

## Through Golem

Same two goals (a ~3.3k-token goal that reads a file, then a follow-up), with
`gpt-5-mini` on Abacus priced at $0.25 input, $0.025 cached input and $2
output per million:

| Build | `usage (this process)` | `est. cost` |
|---|---|---|
| develop `39b207a2` | input 0 tok (0 cached, 0%), output 412 tok, 4 requests; unreliable usage on 4 requests | $0.0000 (excludes unreliable usage: abacus/gpt-5-mini) |
| this change | input 17376 tok (13440 cached, 77%), output 496 tok, 5 requests; cache reported on 4 of 5 requests | $0.0023 |

## Limits

- One day's measurement. If Abacus changes a model's rates, its `w` changes
  and the table entry must be re-measured. If Abacus starts streaming the
  exact count, the inversion would overcount by nearly the cached part (9,582
  for the 5,089-token request with 4,992 cached), and a single frame cannot
  tell the two apart; the guard catches only a negative result, which this
  never is.
- `route-llm` reports the served model in each chunk, and go-llm looks up that
  model, so a routed request is recovered only when the router picked a model
  in the table. Every follow-up `route-llm` request was served by
  `gpt-6-luna`, so in that sample a streamed `route-llm` request always
  reported unknown input.
- If a model in the table starts billing cache writes, its cold streams would
  overcount and pass through; only the four models were checked, on one day.
