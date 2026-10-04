### Security — MCP connection policy (#578)

MCP pins now bind how Golem connects, not only the tool catalog. Each pin
stores keyed fingerprints of the stdio launch (resolved program path and its
symlink target, arguments, working directory, environment policy) or the exact
HTTP endpoint, and a changed connection is refused before anything is launched
or contacted (`connection_changed`, naming the changed fields), even when the
tool list is identical. Stdio servers run in the workspace root with a minimal
environment (`PATH`, `HOME`, `LANG`, `USER` and `TMPDIR`, plus `SYSTEMROOT`,
`TEMP`, `TMP`, `PATHEXT`, `USERPROFILE`, `COMSPEC`, `APPDATA` and
`LOCALAPPDATA` on Windows) and the variables named with the new repeatable
`-mcp-env 'alias=NAME,...'` flag; nothing else is inherited, and an unset named
variable blocks that server (`env_unset`). HTTP servers are pinned to one exact
endpoint and every redirect is refused, same-origin and session close
included. Headless `pin_missing` is now reported before the server is
launched. Tool-call errors no longer echo transport text that can contain
URLs. Same-path program updates and file or symlink swaps between the check
and the launch are not detected, and stdio servers keep host-user filesystem
and network authority (#580).

#### Upgrade notes

- Every server pinned by v0.4.0 or earlier reports `connection_missing`, in
  the REPL and with `-p`, until `golem mcp inspect` and
  `golem mcp approve -digest … -connection …` are run once with the same
  `-root`, server and `-mcp-env` arguments as startup.
- `golem mcp approve` requires `-connection` (the fingerprint `inspect` prints)
  in addition to `-digest`, and checks it before launching anything
  (`connection_mismatch`).
- Stdio servers no longer inherit Golem's environment. Servers that relied on
  inherited variables (exported tokens, `HTTP_PROXY`/`HTTPS_PROXY`,
  `NODE_EXTRA_CA_CERTS`, `VIRTUAL_ENV`, nvm or pyenv paths, `XDG_*`) need
  `-mcp-env 'alias=NAME,...'`. `env KEY=val command` still works for
  non-secret values.
- Stdio servers now start in `-root` instead of the current directory, and a
  relative program path containing a separator (`./bin/server`) resolves
  against `-root`, not Golem's current directory.
- Relative and empty `PATH` entries (`.`, `./node_modules/.bin`) are dropped
  from a stdio server's `PATH` (a quoted Windows entry is judged without its
  quotes and kept as written, but one holding a `;` only when the whole entry
  is quoted), and a `PATH` with no absolute entry is omitted (then programs use
  their own default search path). Servers that relied on a relative entry need
  absolute entries instead.
- On Windows a stdio server always gets `NoDefaultCurrentDirectoryInExePath=1`,
  so a bare program name is not looked up in the workspace root before `PATH`;
  `-mcp-env` may not name it.
- Windows stdio preparation rejects `PATHEXT` containing `/`, `\` or `:` as
  `launch_invalid` before executable lookup. These delimiters could make Go
  resolve a different executable when starting the already prepared launcher;
  the diagnostic never includes the environment value.
- `PATH` values are not part of the connection: a wrapper
  (`env KEY=val command`, `npx`, `uvx`, `sh -c`) or a script's `#!`
  interpreter binds only the launcher, so what it finds through the absolute
  `PATH` entries can change without `connection_changed`. Prefer absolute
  launcher paths to wrappers.
- HTTP endpoints with userinfo, a fragment (including a bare trailing `#`),
  `.` or `..` path segments (including percent-encoded `%2e%2e` and
  `%2F`-joined forms), a backslash, an IPv6 zone ID, or a non-ASCII host now
  fail as `invalid_config`, which stops Golem startup and names the alias and
  the rule on stderr; use the `xn--` form for internationalized hosts.
- Every HTTP redirect is refused, including on session close: a session
  `DELETE` answered with a redirect is never followed, and library callers see
  it only as `Manager.Close`'s fixed error text; startup, `golem mcp inspect`
  and `approve` are unaffected.
- The first run creates `<user data dir>/golem/mcp-pins/connection-hmac.pem`,
  so the `mcp-pins` directory must be writable. An unreadable or corrupt key,
  or on Unix one with group or other permission bits or another owner, makes
  the pin store unavailable and is never replaced. A lost key is recreated,
  and every pin then reports `connection_changed` (`key`) until approved once
  more.
- Failed MCP tool calls now report only `mcp call failed:` followed by
  `redirect refused`, `destination refused`, `canceled`, `timed out`, the
  server's own JSON-RPC error message, or `transport error`.
- A v0.4 binary reports pins written by this version as `pin_unavailable` and
  blocks those aliases.

#### Library changes

- `mcpclient.Approve` takes `ApprovalDigests{Catalog, Connection}`; both are
  required, and the connection fingerprint is checked before the server is
  launched or contacted.
- `mcpclient.Inspection` gains `CandidateConnection` (a new `ConnectionView`),
  `PinnedConnection` and `ConnectionChanges`; `AdmissionError` gains
  `ConnectionChanges`, and its `Names` also lists unset variables for
  `env_unset`. A `launch_invalid` rejection's text names the fixed rule that
  failed (for example `executable not found or not executable`), and per-alias
  rejections remain bare `*AdmissionError` values.
- `StdioServer` children receive only the platform baseline environment plus
  `Server.WithEnv` additions, built with `InheritEnv` (the parent's value,
  read at launch) or `SetEnv` (a host-supplied value, which may not name a
  baseline variable). `Server.WithDir` sets the absolute working directory;
  the default is the process working directory captured at preparation.
- `Server` and `EnvVar` implement `fmt.Formatter`: a `Server` formats as its
  kind and alias (`stdio:fs`) and an `EnvVar` as its source and name
  (`inherit:GITHUB_TOKEN`).
- `NewPinStore` creates or loads the per-user connection key and fails when
  an existing key is unreadable, insecure or corrupt.
- `Manager.Close` returns an error with the fixed text
  `mcpclient: closing MCP sessions failed`; causes are available only through
  `errors.Is` and `errors.As`.
