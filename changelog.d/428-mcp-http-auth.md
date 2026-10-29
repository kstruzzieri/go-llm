### Security — Optional bearer authentication for the MCP HTTP transport (#428)

The MCP server's HTTP transport, h2c and TLS alike, can now require one shared
bearer token. Library callers pass `mcp.WithHTTPBearerToken(token)`; the
standalone `go-llm-mcp` binary reads `GO_LLM_MCP_TOKEN`, only with
`-transport http` (stdio ignores it). This is opt-in. When the option and the
variable are absent, nothing changes: loopback h2c and TLS on any permitted bind
stay unauthenticated, so an existing TLS-only remote deployment is still open to
every caller that can reach it until a token is set. TLS encrypts traffic, and
the loopback-only rule for h2c and the cross-origin check limit exposure, but
none of them authenticates the caller. Set a token and, once the server is
running at its deployed address, confirm that a request without it is refused
before clients use it. See `docs/GETTING_STARTED.md`, "HTTP authentication and trusted networks".

The token is 1-4096 bytes of RFC 6750 token characters (letters, digits, `-`,
`.`, `_`, `~`, `+`, `/`, then optional trailing `=`), compared by SHA-256
digest in constant time. Nothing is trimmed, so a value with a trailing newline
fails. `GO_LLM_MCP_TOKEN` set but empty fails startup rather than disabling
authentication. `WithHTTPBearerToken` with an empty or malformed token makes
`mcp.NewServer` return an error wrapping `mcp.ErrInvalidHTTPBearerToken`
before it loads configuration or opens any database. No error names the value.
The token is read once, so rotating it means restarting the server and
updating clients.

#### Behavior change

- With a token set, every request that reaches the MCP handler needs exactly one
  `Authorization: Bearer <token>` header: initialize, discovery, tool calls,
  GET/SSE, DELETE, and requests that carry an existing session ID. Any other
  request gets HTTP 401, body `unauthorized`, and
  `WWW-Authenticate: Bearer realm="go-llm-mcp"`, the same response for every
  credential failure. A request the body limit rejects first keeps the limit's
  status.
- `OPTIONS *` now returns 401 when a token is set. The listener sets
  `DisableGeneralOptionsHandler` so Go no longer answers it with an
  unauthenticated 200. Without a token it behaves as before.
- On h2c, an HTTP/1.1 `Upgrade: h2c` request must itself carry the token. One
  without it gets the 401 before the server reads its body or replies
  `101 Switching Protocols`, so an unauthenticated caller cannot make the
  server buffer an upgrade body. The upgraded request and every later stream
  are checked again.
- The gate strips `Authorization` before the MCP SDK and tools see the request,
  so tool handlers never receive the credential.
- Golem's MCP client cannot send an `Authorization` header yet (#578), so a
  token-protected `go-llm-mcp` answers Golem's `-mcp-http` with 401. Use stdio, or
  leave the token unset on loopback, until #578 lands.

#### Notes for library callers

- The token is one shared credential and carries no identity. No `TokenInfo`,
  `UserID` or principal is derived from it, and it grants no new tool
  permissions. A host-supplied `TokenInfo.UserID` and
  `WithRetrievalPrincipalResolver` keep their retrieval-policy meaning and cannot
  bypass the bearer check. A custom resolver cannot read `Authorization`,
  because the gate strips it, so it must use separate trusted context.
- `GODEBUG=http2debug=2` makes the HTTP/2 implementation log decoded request
  headers, `Authorization` included, before any handler runs. Do not use it on
  an authenticated server.
- The library never reads the process environment; only `go-llm-mcp` reads
  `GO_LLM_MCP_TOKEN`. There is no CLI flag, token file or `models.json` field.
