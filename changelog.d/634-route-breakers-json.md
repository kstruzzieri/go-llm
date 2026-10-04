### Fixed — `route://breakers` reports breaker state by name and the last error by class (#634)

The `route://breakers` MCP resource marshaled `provider.BreakerInfo` as is.
The state came out as an integer, unset timestamps as `0001-01-01T00:00:00Z`,
and the last error as whatever exported fields its concrete type had: `{}` for
the built-in providers' wrapped errors, or the endpoint URL, including any
query-string credentials, for a provider that returned a bare `*url.Error`.

Each entry now carries `provider`, `state` (`closed`, `open` or `half-open`),
`failures`, `lastFailure` and `recoverAt` (RFC 3339 in UTC, omitted when
unset), and `lastErrorClass`, the bounded routing error class such as
`network`, `5xx` or `rate_limit` (omitted when there is no error). Error text
is never emitted. The URI and the top-level array are unchanged; readers of the
old nested `info` object must move to the new keys.

`provider.ErrorClassOf` exposes the same classification to other callers.
