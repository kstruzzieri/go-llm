### Changed — Golem starts in the read-only Inspect profile (#387)

Golem and the `golem` library now grant the model one of three runtime
profiles. `golem.Profile` selects it through `Options.Profile`. The zero value
is Inspect: the four workspace reads (`list`, `glob`, `search`, `read_file`)
and nothing else. Trusted is the explicit opt-in that admits caller tools,
writes, commands and the rest. Preview is reserved for the staged-workspace
runtime (#388, #389) and is always rejected, with
`Preview unavailable: staged-workspace runtime requires #388/#389`. Admission
runs at `New`, `Replace` and `ReplaceConfiguration`, before any tool's
`Effect`, `Plan` or `Invoke` runs and before model bootstrap. `Options.ToolAllowlist` is a frozen
ceiling that no replacement can widen, and it may name tools that are not
mounted yet. `Runtime.EffectivePermissions()` returns a copied snapshot of the
profile, root, ceiling, mounted tools (each with its effect class, approval
policy and provenance: `runtime`, `library` or `host-extension`) and command
runtime. Native command sandboxes (`-sandbox`) are not part of this change;
they come with #576, and without one Trusted commands run on the host.

The CLI adds `-profile inspect|trusted`. With no `-profile`, a session is
Inspect unless a legacy opt-in flag survives one-shot normalization
(`-allow-write`, `-allow-exec`, `-scratch`, `-allow-tool`, `-rag-db`,
`-agent-memory`, `-delegate`, `-dispatch`, `-mcp-stdio`, `-mcp-http`,
`-progressive`, `-grounding`); it is then Trusted and startup prints
`profile: trusted (selected by <flag>)`. An explicit `-profile inspect` with
one of those flags is a usage error. AgentFlow modes stay Trusted and print no
profile notice. Startup and successful late mounts print a `permissions:` line
to stderr, and result JSON and stream events keep their schemas.

#### Behavior change

- The default CLI mounts no retrieval, auto-index, embedding route or model
  memory tools (`memory_search`, `agent_memory_*`). Human `/remember`,
  `/forget` and `/memories` still work. Existing indexes and memory databases
  are not migrated or deleted.
- In an Inspect session `/allow-write` and `/allow-exec` are refused before any
  resource opens, and the hints for `/undo`, `/checkpoints`, `/auto-edits` and
  `/jobs` say to restart with `-profile trusted` and the flag. Trusted sessions
  keep the existing hints. `/help` marks the two commands Trusted-only.
- `golem.Options.Tools` requires `Profile: golem.ProfileTrusted`; Inspect
  rejects any caller tool, including one that declares only a `Read` effect,
  with `ErrProfileIncompatible`.
- Trusted raises an approval floor under every extra tool. Non-library tools
  that mutate or use the network always prompt, even if they or their plans ask
  for `ApprovalNever`. Four library types keep `ApprovalNever`:
  `agent_memory_create` and `agent_memory_promote` (with the real memory store
  they write only the agent-memory database under `-agent-memory`'s own
  guards) and `delegate_code` and `dispatch` (model-provider traffic under
  destination admission). The exemption is by concrete type and trusts the
  store, model caller and hooks the host injects into those types: the floor
  guards against a tool that declares `ApprovalNever` for itself, not against
  host code. Non-nil pointers to value-receiver library tools retain their
  library provenance and the same approval policy as the equivalent values.
- `dispatch` is rejected alongside a `ScopeGuard`, and an allowlist that admits
  `dispatch` must name all four reads and every other child tool it carries
  (an optional `retrieve`). The new `agenttools.Dispatch.ChildToolNames`
  reports those children.
- Approval previews pass through `agent.SanitizePreview`, which renders
  terminal controls as visible escapes, and diff headers quote paths that
  contain whitespace, controls, quotes or backslashes, so literal escapes
  cannot impersonate another filename.

#### Consumers

Firn's two `golem.New` sites (`internal/ai/runtime.go` and
`internal/git/generator.go`) pass no `Options.Tools` and never call `Replace`,
so they already run as Inspect with no behavior change. Setting
`Profile: golem.ProfileInspect` there explicitly is recommended for clarity
when Firn bumps go-llm. Callers that pass `Options.Tools` must add
`Profile: golem.ProfileTrusted`.
