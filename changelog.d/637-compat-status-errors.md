### Security — `/v1/status` no longer returns raw provider health-error text (#637)

**Breaking:** `GET /v1/status` replaces `providers[].error` with
`error_class`, and the Go type `compat.ProviderStatus` replaces `Error` with
`ErrorClass`.

A failed health check used to report the error's text. For OpenAI-compatible
providers (`api_format: openai-compat`) that text included the request URL,
with any credentials in its query string, or up to 64 KiB of the upstream error
body. The provider now reports the bounded routing error class (`network`,
`timeout`, `4xx`, `5xx`, `rate_limit` or `unknown`), and the error text goes to
the server log, truncated to 512 characters. Ollama's health check keeps no
cause, so an unhealthy Ollama provider reports `unknown`.

Providers are now sorted by name, and warm models by provider, then model, so
repeated reads list them in the same order. `expires_at` is RFC 3339 UTC with
fractional seconds, matching `route://warmth`; before, it used the server's
local offset and dropped fractional seconds.
