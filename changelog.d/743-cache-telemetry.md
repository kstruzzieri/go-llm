### Added — Prompt-cache telemetry and the `llm-bench -cache-probe` mode (#743)

`provider.Usage` gains `CachedPromptTokens`, `CachedPromptTokensReported`,
`CacheWriteTokens` and `CacheWriteTokensReported`. `PromptTokens` still
includes cached tokens; the Reported flags tell a measured zero apart from a
backend that does not report the counter. The openai-compat provider fills
them from `prompt_tokens_details` (OpenAI, llama.cpp's llama-server, and other
servers that send it), in both non-streaming and streaming responses. The
values are passed through as the server sent them.

The agent carries the counters through `Result.Usage` and nested-run
`DescendantUsage`. A negative cache counter is treated like a negative
reasoning count: the run budget charges that call's full reservation and
leaves its usage out of the descendant totals.

Golem's end-of-run footer reads `… · T tok (C cached)` when the backend
reports the counter, and is unchanged otherwise. `-telemetry` step spans gain
`cached`, `cached_reported`, `cache_write` and `cache_write_reported` inside
`usage`; the schema version stays 2.

`llm-bench -cache-probe -candidate-base-url <url> -models openai-compat/<model>
-cache-probe-out <file>` runs a scripted Golem session (eight file reads, then
three follow-up turns) against one openai-compat server and writes a markdown
table of each request's prompt tokens and cached tokens, beside a
prefix-stability ratio that is labeled as a serialization proxy.
`-cache-probe-scenario pressure` uses Golem's default 8192-token input ceiling
so compaction runs; the default `uncompacted` uses 131072. A bad output path
fails before any request, and a failed run leaves an earlier table in place.
