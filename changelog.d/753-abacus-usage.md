### Fixed — openai-compat reads Abacus RouteLLM token usage (#753)

Abacus RouteLLM reports token usage in its own shape, and the `openai-compat`
client read most of it as zero or as a discounted count.

- Non-streaming chat responses now report the full input count (uncached plus
  cached tokens), the cached count, completion and reasoning tokens, and a
  total. Abacus's billing-weighted `input_tokens` is not used.
- OpenAI-shaped usage keeps its existing decoding behavior, including ignoring
  unrelated vendor extensions with different JSON types.
- Streaming chat responses from an Abacus base URL (`routellm.abacus.ai`) that
  include cached tokens now report the input and total counts as `0`
  (unknown). Abacus streams a count that discounts cached tokens and can be
  smaller than the cached count itself, and it sends no exact input count. A
  stream with nothing cached, and every other server, reports as before.
