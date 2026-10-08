### Changed — v0.5.0 consumer upgrade notes (#664)

Read this before upgrading from v0.4.0. Consumers on older pins must also apply
the v0.4.0 and v0.3.0 upgrade notes. The
[v0.5.0 upgrade guide](docs/releases/v0.5.0.md) has the details and the
consumer compile check.

#### Library consumers

- `mcpclient.Approve` takes `ApprovalDigests{Catalog, Connection}` and requires
  both (#578). `StdioServer` children receive only the platform baseline
  environment plus `Server.WithEnv` additions (`InheritEnv`, `SetEnv`), and
  `Manager.Close` returns fixed error text whose causes are reachable only
  through `errors.Is` and `errors.As`.
- `compat.ProviderStatus.Error` is replaced by `ErrorClass`, and `/v1/status`
  reports `providers[].error_class` (#637). `compat.New` no longer sends
  `Access-Control-Allow-Origin: *` and refuses cross-origin browser POSTs and
  unlisted `Host` headers with 403 (#633): pass the browser's exact origin to
  `WithCORS` and other host names to `WithAllowedHosts`.
- `route://breakers`, `route://warmth` and `route://sticky` emit flat
  snake_case objects; the nested `info`, `Key` and `Info` objects are gone
  (#634).
- `agent/tools` `search` skips credential files, and `dispatch` refuses scopes
  at or below `.git`, `.ssh`, `.gnupg`, `.aws` or `.kube`, for every consumer
  (#627). Custom `PathDeny` patterns are matched against normalized paths.
  This is a tripwire, not confinement: `glob` and `list` still show these
  names, and shell commands, `retrieve` and MCP tools can still reach them.
- On Linux and Darwin, Workspace reads fail closed when the guarded path no
  longer reaches the opened object (#613). Handle `path changed during access`
  and `ErrRootReplaced`, and build a new `Workspace` after the root is
  replaced. Scoped children stop reading a moved scope. This is decision
  integrity, not adversary resistance.
- SQLite stores fail to open when the WAL switch reports another journal mode,
  including `immutable=1` and `nolock=1` URIs (#619). Use
  `rag.OpenSQLiteStoreReadOnly` or your own handle for read-only access. UNC
  and device paths are rejected on Windows.
- A `localhost` base URL that resolves off-host matches
  `provider.ErrDestinationDenied` (#654). `golem.New`'s config-driven
  bootstrap, Golem and `go-llm-mcp` now fail at startup instead of starting
  without that provider's model list.
- `agentflow.PreflightP0` rejects plans whose schema major is not 1,
  `agentflow.Compile` emits `1.0.0` (#612), and `agentflow.NewSrcExecRunner`
  requires an absolute checkout. AgentFlow children receive an environment
  built from scratch; approve parent variables with
  `(*agentflow.ExecRunner).AllowEnv` (#577).

#### Golem operators

- Approve every MCP server once more: pins written by v0.4.0 or earlier report
  `connection_missing` until `golem mcp inspect` and
  `golem mcp approve -digest … -connection …` run with the startup `-root`,
  server and `-mcp-env` arguments (#578). Stdio servers start in `-root`
  without Golem's environment; name the variables they need with `-mcp-env`.
  A relative program path such as `./bin/server` resolves against `-root`.
  HTTP endpoints with userinfo, fragments, dot segments, backslashes, IPv6
  zone IDs or non-ASCII hosts stop startup as `invalid_config`, and every
  redirect is refused. A v0.4 binary blocks pins written by v0.5.0.
- Install AgentFlow 1.x. `-goal`, `-plan`, `-agentflow-resume` and
  `-agentflow-status` refuse other versions, 0.x plans and 0.x `.agent/` state
  (#612). Gates no longer inherit Golem's environment; approve variables with
  `-agentflow-env NAME` (#577).
- Deterministic tool guards are always on, with no opt-out (#575). Matching
  calls are refused before approval, and a `-p` run stopped by three
  consecutive refusals exits 1.
- Golem's own `git` calls no longer pass SSH agent, proxy,
  `GIT_CONFIG_COUNT`/`GIT_CONFIG_PARAMETERS` injection or other parent
  variables to hooks, filters and helpers (#623).
