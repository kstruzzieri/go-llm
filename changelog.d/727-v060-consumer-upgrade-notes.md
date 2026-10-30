### Changed — v0.6.0 consumer upgrade notes (#727)

Read this before upgrading from v0.5.0. Consumers on older pins must also apply
the earlier upgrade notes. The
[v0.6.0 upgrade guide](docs/releases/v0.6.0.md) has the details, what a default
session does not get, the known issues and the consumer compile check.

#### Library consumers

- `golem.Options.Tools` requires `Profile: golem.ProfileTrusted` (#387). The
  zero value is Inspect, and `golem.New`, `Replace` and `ReplaceConfiguration`
  reject any caller tool under it with an error wrapping
  `golem.ErrProfileIncompatible`, at run time.
- `golem.CommandPermissions` is no longer comparable with `==`, `golem.New`
  rejects a host runtime with any sandbox constraint, and
  `agenttools.VerifyCommand.Run` returns a `VerifyResult` with every error
  (#576).
- A module that requires `modernc.org/libc` directly must set it to v1.70.0
  (#632). SQLite openers reject `file:` URIs with a `vfs` parameter, a repeated
  `mode`, a non-`localhost` host, user information or a fragment, and an
  agent-memory store opened by `file:` URI fails until its keys are moved
  beside the database (#648).
- A dispatched child blocked by `Secrets`, `configured_secrets` or a content
  denial reports a fixed marker instead of its last message, and a sensitive
  block drops free-text diagnostics from every interceptor in that hook
  (#424).
- Exact configured-secret checks and MCP HTTP authentication are opt-in:
  install `interceptor.NewConfiguredSecrets` and `openaicompat.WithContentGuard`
  (#424), and pass `mcp.WithHTTPBearerToken` (#428). Loading a config
  installs nothing.

#### Golem operators

- A session without `-profile` or a legacy opt-in flag is read-only (#387).
  Scripts that relied on retrieval, auto-indexing or `memory_search` need
  `-profile trusted` or one of the legacy opt-in flags.
- Install AgentFlow 1.1 or a later 1.x (#705). Failed step runs now block their
  attempt, so `-agentflow-resume` reruns the model, and a fresh `-plan` or
  `-goal` refuses a workspace with execution history (#642, #643).
- Exact configured-secret protection is available, opt-in through
  `-interceptors`; default sessions are not covered (#424). MCP HTTP
  authentication is optional: `go-llm-mcp` stays unauthenticated over HTTP,
  TLS without a token included, until `GO_LLM_MCP_TOKEN` is set (#428). The
  native sandbox needs `-sandbox seatbelt|bwrap` (#576), and sandboxed
  commands can still write `.git` metadata (#721).
