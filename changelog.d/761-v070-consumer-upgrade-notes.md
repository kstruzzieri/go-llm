### Changed — v0.7.0 consumer upgrade notes (#761)

Read this before upgrading from v0.6.0. Consumers on older pins must also apply
the earlier upgrade notes. The
[v0.7.0 upgrade guide](docs/releases/v0.7.0.md) has the details, the known
issues and the consumer compile check.

- `apidiff` reports no incompatible change to an exported package since v0.6.0.
- Use AgentFlow 1.2.2 or 1.2.0. Golem refuses 1.2.1 for recovery (#756).
- `openaicompat.NewClient` against an OpenCode mirror on another host, or over
  `http`, must pass `WithCacheAffinity("opencode")` to keep its header (#749).
- `PromptTokens` of 0 means unknown. Streamed requests on most Abacus RouteLLM
  models now report it (#759). Ollama embeddings report the server's count, not
  the number of input texts (#751).
- With a durable summary, the legacy compactor now drops prior-turn history
  before current-run tool results (#748).
