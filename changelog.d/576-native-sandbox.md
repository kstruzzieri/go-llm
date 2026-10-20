### Added — Native Seatbelt/Bubblewrap sandbox for Trusted commands and verification (#576)

Golem adds `-sandbox host|seatbelt|bwrap` and
`-sandbox-network denied|allowed`. A Trusted session still runs its commands
on the host by default. `-sandbox seatbelt` (macOS) or `-sandbox bwrap` (Linux) runs
foreground and background commands, scratch execution and post-write
verification under one frozen native policy, with the network denied unless
`-sandbox-network allowed`. That setting is permission, not a connectivity
guarantee. The backend is probed against the canonical root before any config,
store, session, MCP connection or provider is touched. An unavailable backend
stops startup, and Golem never falls back to the host. A native session mounts
no retrieval, auto-index or model memory tools. With it, `-rag-db`,
`-agent-memory` (with a session), `-delegate`, `-dispatch`, `-mcp-stdio`,
`-mcp-http`, `-progressive` and `-grounding` are usage errors. `-sandbox-network`
without a native `-sandbox` is a usage error, and AgentFlow modes reject both
flags. Every `permissions:` line, host and Inspect included, now carries
`scratch=` and `verification=` state, and a native backend's line adds its
`network=` policy and `limits=`. Seatbelt has no memory/CPU quota, and
Bubblewrap's `ip-denied` leaves pathname Unix sockets in the shared workspace
reachable. `-sandbox` does not
confine `/consult`, git-context helpers, MCP servers or the external editor.
Seatbelt may deny loader, SDK or cache access outside its reviewed roots. The
recovery is a new host Trusted session or a toolchain and caches inside the
workspace, never a broader policy. See `docs/golem.md`, "Native command
sandbox (-sandbox)".

#### Behavior change

- Post-write verification grant keys are `verify:v2` for every runtime, host
  included, bound to the full normalized sandbox policy and the prepared
  command. A changed runtime or network setting prompts again. Grants are
  session-local, so old `verify:v1` grants do not carry over and nothing
  migrates. The key still binds no script bytes (containment row V1 stays
  uncontained, owner #420).
- Scratch approval previews name the runtime. The host keeps
  `disposable cwd copy; host process remains unrestricted`; a native backend
  shows `disposable cwd copy under the <runtime> sandbox`.
- Seatbelt results report a failed private-temp cleanup as
  `note: sandbox temp cleanup: ...` on `run_command`, verification,
  `command_status`, `stop_command` and `/jobs`, without changing the exit code
  or error. A nonzero or failed-to-start Seatbelt foreground run adds a note
  that the profile may have denied loader, SDK or cache access and that the
  run is not a passing check.
- An explicit `-sandbox`, `host` included, selects the Trusted profile when
  `-profile` is omitted (`profile: trusted (selected by -sandbox)`).
  `-profile inspect` with either sandbox flag is a usage error.
- The verification approval preview and the model-visible verification result
  carry a `target:` line naming the canonical workspace and saying unpromoted
  scratch artifacts are not checked. A native verifier's preview warns that its
  writes inside the workspace and private temp are outside `/undo`.
- A native session marks every descriptor Golem inherited above stdio
  close-on-exec before the startup probe, so a descriptor a launcher left open
  (a CI runner's, a shell redirection) never reaches a sandboxed command.
  Host sessions keep their inherited descriptors.

#### Notes for library callers

- `golem.Options.Sandbox` selects the runtime. The zero value is the host and
  admits tools as before. A native runtime requires `ProfileTrusted` and fails
  `New` with an error wrapping `agenttools.ErrSandboxPolicy` when it cannot be
  honored. It admits only library-built tools bound to the session's root and
  policy (`agenttools.CheckSandboxBindings`), and its verifier must be nil or an
  `agenttools.VerifierSlot` bound to the same root and policy, checked at
  `New`, `ReplaceConfiguration` and every turn start. Each turn retains the
  verifier identity it validated, including an absent hook, across all callbacks.
  `agent.WithVerifier` is safe concurrently with `Run`; replacement affects
  subsequent runs. `agent.Orchestrator.RunWithVerifier` runs with an explicitly
  captured hook so a host can validate it without a check/use gap.
- `EffectivePermissions` adds `Command.Limits`, `Scratch` and
  `Verification{State, Target}`. `Command.Network` reports `denied`,
  `ip-denied` or `allowed` for a native runtime.
- `agenttools.JobStatus.Notes` and `agenttools.VerifyResult.Notes` carry
  backend diagnostics such as the Seatbelt cleanup note. They never change an
  exit code, a timeout or the error classification.
- The Seatbelt backend now refuses to spawn while any descriptor above stdio
  lacks FD_CLOEXEC, as Bubblewrap already did, naming the descriptor: a
  sandboxed command could otherwise write or connect through it whatever its
  profile says. Both native backends also run the audit at construction, so
  `agenttools.ValidateSandbox` (and with it `golem.New`) fails with an error
  wrapping `ErrSandboxPolicy` instead of advertising a backend that refuses
  every command. Mark your own descriptors close-on-exec before building a
  native session. On macOS, the audit marks libSystem's identified
  `com.apple.netsrc` resolver socket close-on-exec, retaining the parent's
  usable socket while preventing sandboxed children from inheriting it.
  Failure to set the flag refuses the spawn.
- Seatbelt execution is limited to the reviewed system roots, workspace,
  private temp and exact approved executable. An unreadable executable
  outside those allowances can no longer run as a child.
- `golem.New` validates every session's sandbox policy, the host included, so
  the host runtime with any constraint (network, memory, CPU or capabilities)
  fails `New` with an error wrapping `agenttools.ErrSandboxPolicy`.
- `golem.CommandPermissions` is no longer comparable with `==`: it gained
  `Limits []string`.
- `agenttools.VerifyCommand.Run` returns a `VerifyResult` alongside every
  error: the partial result, `Notes` included, once the command was launched,
  and a zero `VerifyResult` (exit code 0) for a refusal before launch, such
  as the descriptor refusal. Check the error before the exit code.
- Hosts that leave `Sandbox` zero keep host execution and the existing
  admission (Firn, which runs Inspect, is unaffected).
