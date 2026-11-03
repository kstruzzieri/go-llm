### Fixed — openai-compat recovers Abacus streamed input counts (#758)

Streaming chat responses from Abacus RouteLLM (`routellm.abacus.ai`) that
reused cached input now report the exact input and total counts for
`gpt-5-mini`, `gemini-3.5-flash`, `grok-4.3` and
`deepseek-ai/DeepSeek-V4-Flash-0731`, instead of `0` (unknown). Abacus streams
the uncached tokens plus the cached tokens times the model's cached-to-input
rate ratio, rounded down; go-llm reverses that with the ratio measured for each
of these models. The served model decides, so a `route-llm` request is
recovered when it was routed to one of them.

- Golem now counts and prices these requests in `/context` instead of naming
  them under `excludes unreliable usage`.
- Any other Abacus model, and a count the reversal cannot explain, still
  reports input and total as `0` (unknown), as since #753.
