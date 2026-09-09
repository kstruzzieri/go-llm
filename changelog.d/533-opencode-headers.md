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

Both headers change what every openai-compat request looks like, not only
requests to opencode.

`User-Agent` is the intended case: identifying as `Go-http-client/1.1` to any
provider was the reported problem.

`x-opencode-session` is emitted whenever `SessionID` is set, and the provider
has no way to tell an opencode endpoint from llama.cpp, vLLM or LM Studio. A
direct caller that leaves `SessionID` empty is unaffected, but golem always
supplies a thread id, so golem runs now send this header to whichever
openai-compat provider they route to. The value is a session id, not content:
by default `workspace:<sha256 prefix>` (a hash, not a path), or `golem:<uuid>`
for a fresh session. `-session <name>` makes it a user-chosen string, which
then reaches any configured endpoint including remote ones. Servers ignore
unknown headers, so this is a disclosure question rather than a compatibility
one; gating emission per provider would need the provider identity plumbed
into the client, which this change does not do.
