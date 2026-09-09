### Added — openai-compat sends a client User-Agent and an opencode session id (#533)

The openai-compat provider set no `User-Agent`, so `net/http` supplied its
generic `Go-http-client/1.1`, and the provider had no way to carry a
per-conversation identity. opencode asks clients for both: an agent that
names the client rather than its HTTP library, and a stable
`x-opencode-session` it uses for request routing and prompt caching. Every
go-llm consumer routing to opencode was affected, so the fix lives here
rather than in `cmd/golem` and `cmd/llm-bench` separately.

#### Added

- `openaicompat.WithUserAgent` overrides the new `go-llm/<version>` default,
  letting an embedder identify as itself (Firn IDE, say) instead of as the
  shared module. The version is read from the build info the toolchain
  stamps in. Only this module's own version is reported: when go-llm is
  imported, `info.Main` describes the consumer, so the dependency entry is
  authoritative.
- `provider.ChatRequest.SessionID`, tagged `json:"-"`, is emitted as the
  `x-opencode-session` header when non-empty.
- `provider.RoutingRequest.SessionID` carries the id across routing, so it
  survives the ChatRequest → RoutingRequest → ChatRequest round trip that
  `Router.Chat`, `Router.ChatStream`, and `RoutePlan.buildChatRequest`
  perform. Unlike `AffinityKey` it never influences model selection; it only
  travels with the request.
- `agent.Request.SessionID` forwards the id to every model call in a run,
  through both `agent.NewRouterModelCaller` and golem's chain caller. The
  golem runtime fills it from the turn's thread id, so all turns of one
  conversation share a session id and multi-turn prompt caching can hit.

#### Changed

- `golem.Turn.ThreadID` is now rejected with `ErrInvalidRequest` when it
  contains an ASCII control character. The thread id becomes a header value,
  and Go's Transport refuses to send one containing a control character —
  without this check a bad id would fail every turn of the thread with an
  opaque transport error instead of a clear validation error. Length limits
  are unchanged, and `net/http` already prevented header injection.

#### Notes

Sending the session header only when the field is non-empty leaves existing
callers and non-opencode endpoints byte-identical on the wire. The
`User-Agent` default changes for every openai-compat request, which is the
intended fix: identifying as `Go-http-client/1.1` was the reported problem.
