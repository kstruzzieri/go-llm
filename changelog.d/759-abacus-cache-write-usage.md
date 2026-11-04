### Fixed — openai-compat counts Abacus cache writes and stops trusting their streamed input (#759)

Abacus RouteLLM (`routellm.abacus.ai`) bills the tokens a request writes to
its prompt cache at 1.25 times the input rate on every model measured outside
the four below (Claude Haiku 4.5, Haiku 5.5 and Sonnet 5.5, `gpt-6-luna`,
`gpt-6-astra` and `gpt-5.6-luna`, and `route-llm` when it serves one of them),
and counted them wrong:

- A non-streaming response now includes the written tokens in the input count
  and reports them as the cache-write count. Before, a cold request read as a
  handful of tokens (3 for a 3,782-token prompt), and Golem priced it as
  reliable.
- A streamed response from any served model other than `gpt-5-mini`,
  `gemini-3.5-flash`, `grok-4.3` and `deepseek-ai/DeepSeek-V4-Flash-0731` now
  reports input and total as `0` (unknown), the first request included. Before,
  a request with nothing cached passed its count through, which on these
  models includes the written tokens at 1.25 (5,691 for a 4,554-token prompt).
  A stream reports no written count and cannot tell a request that wrote the
  cache from one too short to cache, so the input cannot be recovered. Golem
  names such requests under `excludes unreliable usage` instead of pricing
  them.
- The four models above, measured not to bill writes, are unchanged.

The [v0.7.0 upgrade guide](docs/releases/v0.7.0.md) has the net Abacus behavior
against v0.6.0.
