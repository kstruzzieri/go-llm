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
count. Models that bill cache writes (Claude, `gpt-6-luna`) cannot be inverted
this way and stay unknown.

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

Models that bill cache writes add the written tokens at a premium the stream
does not report:

- `claude-haiku-4-5-20251001` (no cached rate listed): a cold stream reported
  `prompt_tokens` 5,691 for a 4,554-token prompt (3 uncached plus 4,551
  written at 1.25). Non-streaming, the same request reported
  `cache_creation_input_tokens: 4552` and `raw_input_tokens: 3`.
- `gpt-6-luna` (listed $0.01 / $0.10, the model `route-llm` served): a cold
  stream reported 3,455 for a 2,765-token prompt. In the growing conversation
  the inversion overshot by a quarter of each turn's new tokens (6,008 against
  an exact 5,666). A cold non-streaming request reported
  `cache_creation_input_tokens: 3779` and `raw_input_tokens: 3`.

For both models a cold request is affected outside this change too: the
stream's count passes through with written tokens at 1.25, and #753's
non-streaming parser reads `raw_input_tokens + cache_read_input_tokens`, which
leaves out `cache_creation_input_tokens` (3 tokens for a 3,782-token prompt).

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
  exact count, the inversion would undercount, and a single frame cannot tell
  the two apart; only a negative result is caught.
- `route-llm` reports the served model in each chunk, and go-llm looks up that
  model, so a routed request is recovered only when the router picked a model
  in the table.
