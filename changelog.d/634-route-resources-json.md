### Fixed — `route://` resources emit a stable JSON shape (#634)

The `route://breakers`, `route://warmth` and `route://sticky` MCP resources
marshaled provider structs that have no JSON tags. Keys came out as Go field
names, unset timestamps as `0001-01-01T00:00:00Z`, and other times in the
server's local offset. `route://breakers` was the worst case: the breaker state
came out as an integer, and the last error as whatever exported fields its
concrete type had: `{}` for the built-in providers' wrapped errors, or the
endpoint URL, including any query-string credentials, for a provider that
returned a bare `*url.Error`.

Each resource keeps its URI and top-level shape: an array for breakers and
warmth, an object keyed by sticky-key hash for sticky, and `[]` or `{}` when
empty. Entries are now flat objects with snake_case keys and times in RFC 3339
UTC, omitted when unset:

- `route://breakers`: `provider`, `state` (`closed`, `open` or `half-open`),
  `failures`, `last_failure`, `recover_at`, and `last_error_class`, the
  bounded routing error class such as `network`, `5xx` or `rate_limit`
  (omitted when no error was recorded). Error text is never emitted.
- `route://warmth`: `provider`, `model`, `loaded`, `since`, `expires_at`, and
  `vram_gb`, omitted until the warmth source has measured it. Entries are
  sorted by provider, then model, so unchanged state reads back in the same
  order.
- `route://sticky`: `provider`, `model`, `score`, `reason`, `created_at`,
  `last_used_at`, `expires_at`.

Readers of the old nested objects (`info` in breakers, `Key` and `Info` in
warmth, `Key` in sticky) must move to the new keys. `provider.ErrorClassOf`
exposes the error classification to other callers.
