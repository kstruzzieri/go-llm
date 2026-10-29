### Security — Exact configured-credential checks on outbound model requests (#424)

With `-interceptors`, Golem now also refuses to send a request body to a
non-loopback OpenAI-compatible provider when the body carries a credential
go-llm resolved from `models.json`, the same set the `configured_secrets`
tool-call guard uses. The check covers chat (`/v1/chat/completions`),
completion (`/v1/completions`) and embedding (`/v1/embeddings`) bodies: the raw
text, every decoded JSON key and string value, and each tool call's argument
document inside its string wrapper. A refused body is never sent: no
connection, dial or proxy request is made, Golem does not fall back to another
provider, and the circuit breaker does not count it. A refused embedding input
never joins a shared in-flight request or takes a slot. The run ends with
`run blocked: sensitive content detected`, the goal is kept out of CLI history,
and neither the trace nor a session row is written; a dispatch child refused
this way reports `Partial result unavailable: blocked: sensitive content`.

Providers at `localhost` or a literal loopback IP are exempt; a LAN, private or
link-local address, or a DNS name that resolves to loopback, counts as remote.
Headers are never inspected, so a provider's own key still authenticates in
its `Authorization` header; the same key in the body is refused, as is one
provider's key in a request to another. The request URL, other headers,
cookies, proxy credentials and responses are outside this check.

This protection is available, not on by default. Only the provider clients an
`-interceptors` session builds participate. The MCP server, the `golem`
library's automatic provider bootstrap, `golem index` and `golem source`,
`llm-bench`, native Ollama clients (including an Ollama provider at a remote
address) and independently built providers are not covered.

#### Library

- `openaicompat.WithContentGuard(match)` installs the check on one client; a
  refused request returns an error wrapping the new
  `provider.ErrContentDenied`, which names neither the credential nor the URL.
  `provider.IsInfrastructureError` reports false for it, so the router neither
  falls back nor counts it against a breaker. Install it with
  `cfg.SecretMatcher()` on each client whose `provider.Destination` is not
  `IsLocal()`, and build that client's HTTP client with
  `provider.GuardHTTPClient` for origin binding and redirect refusal; the
  matcher alone provides neither.
- `interceptor.SensitiveBlocked` now also reports `provider.ErrContentDenied`,
  so a dispatched child refused this way reports the fixed marker.
- When a run's model call fails with `provider.ErrContentDenied`,
  `Result.Messages` replaces each field an `agent.SensitiveTextInterceptor` in
  the chain matches (message content, tool name and call ID, and each tool
  call's ID, type, name and arguments) with `[...]`. Caller-owned inputs and
  earlier published step records are not rewritten.
