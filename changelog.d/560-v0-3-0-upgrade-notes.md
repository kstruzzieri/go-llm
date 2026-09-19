### Changed — v0.3.0 consumer upgrade notes (#560)

Read this before moving Firn IDE, Flux ML, or Quantum Trader past v0.2.0.
Each item summarizes a contract change; the full entry is further down in
this section under the referenced issue.

#### Library API

- `conversation.SQLiteStore.Save` is a compare-and-swap on
  `Conversation.Revision` (#473). Pass the `Revision` returned by `Load`; a
  revision-zero save over an existing ID is refused as a conflict. Existing
  databases migrate to revision 1 on open.
- Conversation and memory migrations claim each schema version before
  applying it (#543). Configure SQLite's busy timeout on every migrating
  connection and finish database and WAL setup before concurrent openers
  race.
- `memory.NewMemoryRecordStore` and `OpenRecordStore` require
  `RecordStoreConfig` (#446): set `KeyDir` or inject a signer and keyring.
  Agent-memory records are signed; legacy rows import once with
  `legacy-migration` provenance. Back up the database and its `.keys`
  directory together.
- `mcpclient.Connect` requires `ConnectOptions{Pins, RequirePinned}` (#432).
- Tool observations sent to a model are framed on the wire by
  `<<<TOOL_RESULT <key>` and `>>>TOOL_RESULT <key>` lines at the provider
  boundary (#430). `agent.State`, `Result.Messages`, observer events and
  stored transcripts keep raw bytes, but tests that assert on rendered
  prompts must expect the framing. `agent.Run` appends
  `agent.ToolTrustContract` to every effective system prompt.
- MCP agent-memory success results carry their JSON envelope inside matching
  `TOOL_RESULT` fences (#446); clients that decoded bare JSON must validate
  the fence and decode the enclosed object.
- The interceptor pipeline (#436, #514) is opt-in. Nothing changes unless a
  consumer installs interceptors.

#### Golem and go-llm-mcp command lines

- One-shot `-p` invocations exit 2 for caller errors and 1 for run failures
  (#352). Previously every failure exited 1.
- `-allow-destination` takes `"<provider>/<canonical base URL>"` in both
  binaries. The legacy `provider=URL` spelling still parses during a
  deprecation window; its removal is tracked in #501.
- `-p` runs require an existing matching MCP catalog pin for every alias
  (`golem mcp inspect`, `golem mcp approve`) (#432) and explicit
  `-trust-project-context` before AGENTS.md-style guidance is injected
  (#431).
- `-goal` routes through the new `planning` use case (#476). A `models.json`
  without `defaults.planning` degrades through `reasoning`, `analysis`, then
  `agent` before falling back to model recommendation.
