# Changelog

All notable changes to `go-llm` are documented here. Downstream consumers
(Firn IDE, Flux ML, Quantum Trader) should consult this before any
`go get -u github.com/kstruzzieri/go-llm`.

## [Unreleased]

### Added — agent interceptor and gateway pipeline (#436)

`agent.Orchestrator` gains an opt-in deterministic middleware seam. Nothing
is active unless a consumer installs interceptors; Golem wiring lands in a
follow-up after #372. The types below are public contract for #438, #451
and #452.

- `agent.Interceptor` (`InspectInput` / `InspectOutput` / `InspectToolCall`),
  installed with `agent.New(..., agent.WithInterceptors(...))`. Every
  interceptor runs on every hook in registration order, even after a block
  or an earlier error; findings carry a verdict (`VerdictAllow` /
  `VerdictTag` / `VerdictBlock` / `VerdictAbort`), a 0-100 risk contribution,
  the provenance (`agent.Origin`) of the inspected content, and a validated
  target (`agent.TargetKind` plus state index, tool-call id, and context-set
  alternative).
- Inspection at ingress on frozen values: the initial input before assembly;
  the collected model response (content, thinking, tool calls) before it is
  recorded or reaches `OnStep` (streamed `OnToken`/`OnThinking` deltas
  precede inspection and cannot be retracted), including the partial output
  of a failed stream;
  each tool call before `Plan` and approval on both dispatch paths, with
  `Plan`, the approver, `OnToolCall` and `OnStep` receiving their own copies;
  each tool result before the observer, State and the governor, with
  `OnToolResult` receiving a clone of the final observation; verifier
  output before it is appended.
- Fail closed: a terminal block or an abort returns `*agent.BlockedError`
  with the partial `Result` and a `"blocked"` event, joined with any
  interceptor errors; a blocked tool call or tool result becomes a fixed
  model-visible error observation (`ToolCallRecord.Blocked`). A malformed
  `ContextSet` is now rejected before any consumer sees it.
- Tags append a fixed trailer to the observation and every mixed
  alternative (widening `OutputCap` by the trailer bytes per group) and emit
  `InterceptionEvent` to an `InterceptionObserver`.
- Per-run `RiskReport` on `Result.Risk` (nil when nothing was found); an
  optional `RiskApprover` receives the cumulative snapshot with each
  approval; `RunScopedInterceptor.ForRun(ctx, RunScope)` returns a per-run
  instance and a system-prompt addendum (a canary nonce's seam).
- Provenance: `ToolResult.Origin` per invocation, else the tool's
  `OriginTool`, else unknown; invalid values normalize to unknown, which
  detectors treat as foreign. Every built-in in `agent/tools` declares:
  workspace for file, search, exec, background, scratch, retrieve and memory
  tools; model for `delegate_code` and `dispatch`; MCP observations are
  foreign. Golem's own tool wrappers follow in the wiring follow-up.
- `tools.NewDispatch(..., interceptors ...agent.Interceptor)` installs them
  on every child; child envelopes report `risk_score` when non-zero.
- New `agent/interceptor` package: `ZeroWidth`, `Encoding`, `Typoglycemia`
  (`Defaults()`); strong phrases block foreign/unknown content and tag the
  rest, weak indicators only tag, a strong phrase dominates a weak one.

### Added — golem mid-session write/exec tools and the runtime replacement seam (#372)

- `/allow-write` and `/allow-exec` enable the approval-gated write and exec
  tools in a running REPL session when stdin is a terminal; scripted input
  must opt in with the startup flags. They mount exactly what those flags mount
  (same guards, approver, undo journal, post-write verification, tool order),
  recompose the system prompt in the same operation, are one-way for the
  session, are idempotent, and never grant approval by themselves.
  With `-scratch`, promotion stays as it was at startup and the command says so.
  Disabled-state messages (`/undo`, `/checkpoints`, `/auto-edits`, `/jobs`)
  now name the command as well as the flag.
- Library: `(*golem.Runtime).Replace(system, tools)` atomically replaces the
  runtime's `{System, Tools}`. A turn's pair is fixed when the run is
  reserved; turns reserved after `Replace` returns see the new pair until a
  later `Replace` supersedes it. Validation matches `New`, a rejected
  replacement changes nothing, and `ErrClosed` dominates even when `Close`
  completes during validation.

### Added — `signing` package: Signer/Verifier interfaces and key management (#444)

New top-level `signing/` package, the ZT-301 seam the Phase 4 ledgers build
on (#445 receipts, #446 memory provenance, #447 `golem audit`, #450 delegate
proposals). No new module dependency and no consumer wiring in this change.

- Sibling least-authority `Signer`/`Verifier` interfaces over
  `(ctx, domain, payload)`; every
  backend signs a length-prefixed frame `"go-llm-signing-v1\0" ||
  len(domain) || domain || payload`, so a signature over one record kind
  never verifies as another. Empty domain is rejected.
- `Canonicalize` / `MarshalCanonical`: canonical form v1 (sorted keys,
  compact, verbatim numbers, HTML escaping off), rejecting invalid UTF-8,
  unpaired surrogate escapes, duplicate keys, and trailing data, including
  invalid Go strings before encoding replacement. Exact `json:"-"` and
  non-anonymous unexported fields remain omitted by `encoding/json`, while the
  defensive prewalk conservatively visits fields that may serialize through
  embedding. Not RFC 8785; divergences documented and golden-pinned.
- Backends: `Ed25519Signer`/`Ed25519Verifier` and `HMACSigner`
  (HMAC-SHA256, `hmac.Equal` only, pinned by a source-level gate). Signature
  JSON shape `{"alg","kid","sig"}` is public contract. Concrete zero values
  fail closed with `ErrUninitializedKey`.
- Algorithm-bound key IDs and purpose-scoped `Keyring` verification support
  rotation. `LoadOrCreateEd25519` (PKCS#8 PEM) and `LoadOrCreateHMAC`
  (typed HMAC PEM) report identity creation, require writable storage with
  same-directory hard links and directory-sync support, and atomically publish
  synced 0600 keys below a validated owner-only directory; unsupported
  environments fail closed. The parent is synced when the dedicated key
  directory was initially missing or a key must be published, including after
  a failed creation retry; the ordinary existing-key path re-syncs only the key
  directory before trusting it. The file loaders refuse symlinks, swaps, loose
  unix ownership/modes, and foreign key types. Pure must-exist
  `LoadEd25519` and `LoadHMAC` loaders never create or fsync and support
  preprovisioned/read-only storage. Pure `LoadEd25519Verifier` reads canonical
  PKIX/RFC 8410 `PUBLIC KEY` PEM.
- Review hardening before merge: the PEM block type is checked, not only the
  first line; no path-based chmod after key-directory creation;
  `fmt.Formatter` on value receivers so signers held by value never print
  key material; `encoding.TextMarshaler` output is UTF-8 validated;
  `Signature` JSON decodes with strict base64; a nil context or nil
  `*Keyring` fails closed.

### Added — golem headless integration surface (#352)

One-shot mode (`-p`) gains a machine surface for scripting consumers. The
tool-name set and the machine output shapes below are public contract.

- `golem -p -` reads the one-shot prompt from stdin to EOF, bounded at 1 MiB
  (the same ceiling the runtime already enforces on a turn message); a
  terminal stdin fails fast with usage guidance.
- `golem -output-format text|json|stream-json`. `json` emits one
  `golem.result.v1` record; `stream-json` emits the protocol-v1 events one per
  line followed by the same record as the final line. Protocol events are
  never modified; the record is a separate versioned contract carrying the
  exact `Result.Answer`, a bounded failure code, and the same `-grounding`
  report object field for field, with no size cap and every key always present.
  A protocol event carries `protocol` and never `schema`; the record carries
  `schema` and never `protocol`, and the record completes the stream. Diagnostics stay on
  stderr in every format, and `text` is byte-identical to before. Early flag,
  argument, prompt, and configuration parse/validation errors leave stdout
  empty and exit 2. Among pre-run failures, exactly `destination_denied`
  (exit 2) and `provider_unavailable` (exit 1) emit a result record; all other
  pre-run failures leave stdout empty. Both shapes are frozen by golden
  fixtures. Protocol v1 reports execution progress only —
  a tool call rejected before invocation emits no event; denial observability
  is follow-up work.
- `golem -allow-tool NAME` (repeatable) mounts and non-interactively approves
  one exact built-in gated tool for a one-shot run: `write_file`, `edit_file`,
  `run_command`, `start_command`, `stop_command`. Naming `start_command` also
  mounts its ungated `command_status`/`command_tail` readers (a dependency
  closure, not an authorization expansion). Authorization is by exact tool
  name only — previews and approval keys are never parsed — no session grants
  are created, and MCP tools and `submit_plan` are never eligible. The system
  prompt for such a run is built from the exact mounted tool set instead of
  the interactive group prompt, and never describes per-call approval.

### Changed — one-shot exit codes (#352)

- One-shot (`-p`) invocations now exit 2 for caller errors (flag, input,
  configuration, and destination-admission failures) and 1 for run failures,
  including a provider failure during startup probing; previously every
  failure exited 1. Non-`-p` invocations are unchanged, including
  `-agentflow-status`'s exit 2/3 semantics.

### Amended — #348 grounding delivery mechanism (#352)

- The #348 entry below states that #352 would buffer the terminal protocol
  event at the CLI adapter and add the grounding object to its protocol-v1
  payload. That mechanism is superseded: the 128 KiB protocol event cap cannot
  carry an unbounded report, so the report object is serialized
  inside the `golem.result.v1` record instead — which keeps the promise's
  substance (the frozen report shape ships field for field, pinned against
  the trace by test). `golem/runtime.go` remains unchanged either way.

### Added — ephemeral scratch workspaces for approved commands (#443, ZT-204)

Approved build/test commands can run against a disposable copy-on-write
snapshot of the workspace, closing the execution-sandboxing mini-epic
(#440–#443): #441/#442 are the syscall layer, this is the filesystem layer,
and they compose without either knowing about the other.

- `agent/tools`: `ScratchConfig` + `ExecToolsOptions` with additive
  constructors (`NewExecToolsWithOptions`,
  `NewSandboxedExecToolsWithOptions`, `NewExecToolsWithBackgroundOptions`);
  zero options stay byte-identical to the legacy constructors. A session
  snapshots the canonical tree twice (CoW via `clonefile`/`FICLONE`, exact
  plain-copy fallback only for enumerated unsupported/cross-device errnos)
  into a pristine reference root and an execution root holding `workspace/`
  and `tmp/`, rewrites the approved spec's workspace root, cwd,
  workspace-local executable, and `TMPDIR`, runs the command, stream-diffs
  the two private trees into a bounded in-RAM outcome, and removes both
  roots. Foreground and background share one runtime; background capture is
  owned by the process `Wait` wrapper. Cleanup gets its own bounded phase and
  one fixed deferred-reaper grace window; a persistent filesystem failure is
  reported and quarantines that admission slot, bounding live-process
  residue without hanging manager `Shutdown`.
- Threat model, stated plainly: on the host runtime this is accident
  isolation (cwd-relative build droppings, `rm` in scripts) — a malicious
  process can still address the canonical tree by absolute path. Composed
  with Seatbelt or bwrap the rewritten root becomes an enforced write
  boundary, proven behaviorally on both platforms. `.git` is omitted at
  every depth (no `git describe`/VCS stamping inside scratch); the
  file-by-file clone is not a point-in-time filesystem snapshot, and a
  drifting canonical source retries once, then fails closed. Crash/SIGKILL
  orphans under the platform temp base are an accepted limitation (0700,
  OS-reaped; no shared startup sweep), and a crash before promotion's rename
  can leave one reserved 0700 `.golem-scratch-promote-*` staging directory in
  the canonical parent. Its staged file may already have the approved final
  mode but remains protected by that directory. A background
  scratch job holds its session slot (default 2) for its whole
  manager-owned lifetime, so long-lived scratched jobs can defer new
  scratched commands until one finishes.
- Approval identity: an enabled scratch policy inserts a versioned
  `scr:<digest>:` component after the `exec:v3:`/`exec-bg:v2:` prefixes (the
  `sb:` precedent — recipes unchanged, no version bump), so a host grant
  never authorizes a scratch run or vice versa; the ephemeral path is never
  identity. The outer effect budget is setup + command + capture + cleanup +
  a fixed 5s grace, each phase under its own child context.
- `scratch_changes` (read-only, approval-free) reports per-change metadata in
  byte-budgeted continuation pages, never artifact bytes. Symlink aliases back
  into the canonical tree are rewritten into each clone; external directories,
  unresolvable targets, and regular hard links not proven to be canonical are
  rejected rather than preserving a possible write path. `promote_artifact`
  applies exactly one captured
  create per call: always-prompting (empty structural key), create-only
  (updates, deletes, modes, links, binary, and preview-oversize content are
  report-only), fully previewed (complete escaped additions, 64 KiB cap),
  journaled with a write-ahead intent and a tracked after-mode, and
  installed descriptor-anchored with `renameatx_np(RENAME_EXCL)` /
  `renameat2(RENAME_NOREPLACE)` — no overwrite, no fallback. Checkpoint
  schema v2 adds a nullable `after_mode` column (v1 migrates additively);
  `/undo` refuses to delete a promoted create whose complete mode drifted
  even with identical bytes. Promotion-enabled construction fails on
  platforms without the tested no-replace install; capture/query still work.
- `cmd/golem -scratch` (requires interactive `-allow-exec`; one-shot drops
  it with a warning): registers the scratch tools, passes the checkpoint
  journal only when `-allow-write` built it, and prints one startup notice
  naming the accident-vs-enforced split and whether promotion is armed.

### Added — phase-based model routing: the planning use case (#476)

Golem's plan-authoring mode (`-goal`) now routes through its own `planning`
use case instead of `agent`, so a config can author plans with a different
model than the one that executes them.

- A `models.json` authoring `defaults.planning` sends plan authoring to that
  role. One that does not degrades in order through `reasoning`, `analysis`,
  and `agent` — deliberately behavior-changing: a config that never mentions
  planning can author plans through an existing reasoning or analysis role.
  Only when none of those is configured does planning fall back to model
  recommendation, and the startup notice names exactly what was absent.
- The planning route is goal mode's single active route: destination
  admission (#477) consents it, tool-capability preflight proves it, the
  input ceiling is sized from it under the `planning` use case, and the
  orchestrator caller routes it. Goal mode performs no discovery, refresh,
  probe, or inference for the inactive agent, embedding, or summarize routes,
  and a remote planning route — including one reached through the fallbacks —
  fails closed without `-allow-destination`.
- `RouteOutcome` records the requesting use case (`use_case`, omitted when
  empty), so route telemetry can attribute a route to the phase that asked
  for it. `resolveInputCeiling` and `plannerBudget` now take the caller's use
  case instead of hard-coding `agent`.
- Golem's config view declares the `planning` requirement
  (`chat|stream|tool_call`); an authored `defaults.planning` binding projects
  with eligibility, an absent one is not synthesized.
- Execution seams are unchanged: REPL and one-shot turns, task execution,
  parallel workers, and dispatch children keep the `agent` use case;
  `delegate_code` keeps `coding`; summarization keeps `summarize`.

### Changed

- The repeatable `-allow-destination` flag now takes the same syntax in both
  binaries: the canonical `"<provider>/<canonical base URL>"` grant form
  (`provider.ParseDestination` identity). `go-llm-mcp` previously required
  `"provider=https://host/base"`; that legacy `=` spelling now parses in both
  binaries during a deprecation window via the shared
  `provider.ParseDestinationFlag` parser, normalizing to the same canonical
  destination identity.

## [0.2.0] - 2026-08-30

### Added — destination admission before discovery, probe, or inference (#477)

A consent boundary between chain resolution and any outbound byte, part of
the zero-trust epic (#57). Previously, a config whose side-task use cases
fell back onto a hosted role (for example `summarize -> analysis` with
`analysis` on a remote provider) sent conversation-derived content — with
the provider credential attached — to that endpoint with no prompt, and
provider bootstrap refreshed every configured provider before any consent
surface existed.

- One canonical endpoint identity per provider: `{provider, canonical base
  URL}` under the `destination/v1` normalization (credentials, models, and
  use cases excluded; userinfo/query/fragment-bearing base URLs are rejected
  rather than stripped). Literal loopback auto-admits; everything else is
  remote.
- The frozen network plan resolves every enabled route once, before any
  I/O, and derives the manifest the user consents to: deduplicated
  destinations, every use-case edge visible with primary/fallback marking.
- Enforcement is structural, not enumerative: model-runtime HTTP clients
  constructed by the migrated Golem, bootstrap, and MCP paths are wrapped by
  a guard bound to one destination, and requests must carry a purpose
  capability issued by the current admission generation.
  Redirects are refused (same-origin included); loopback bypasses proxies
  and validates `localhost` resolution at dial time; admitted remote
  traffic keeps configured proxies.
- `golem`: renders the destination manifest at startup and collects one
  batch consent for the remote set on a TTY. Noninteractive runs (`-p`,
  piped stdin) and the `models`/`index`/`source` subcommands never prompt:
  remote destinations require the repeatable
  `-allow-destination "<provider>/<canonical base URL>"` flag or fail
  closed naming the destination, a use case that reaches it, and the exact
  flag value. `/grants clear` revokes destination grants and re-runs the
  batch gate before the next goal; `/new`, `/clear`, and `/resume` keep
  clearing tool grants but leave destination authority standing. Backend
  discovery runs strictly after admission, guarded per loopback candidate,
  and pins the discovered URL into the admitted manifest without adding
  authority.
- `golem.New` (library): new `Options.DestinationPolicy`. BREAKING for
  config-driven callers whose resolved routes reach a remote destination:
  the zero value fails closed with an error matching
  `provider.ErrDestinationDenied` before any outbound byte. Local-only
  configurations are unaffected. Opt in with an exact
  `provider.NewDestinationPolicy(...)` or an explicit
  `provider.AllowAllDestinations()`. A nonzero policy with a
  caller-supplied `Orchestrator` is refused
  (`golem.ErrDestinationPolicyIneffective`) — the host owns those
  transports.
- Standalone MCP: new `mcp.WithDestinationPolicy` and repeatable
  `-allow-destination "provider=https://host/base"` grants. Same zero-value
  fail-closed contract; the server never prompts. Health, warmth polling,
  model listing/pull, and the resolution sweep all run through guarded,
  capability-bound clients; a destination denial at startup is fatal rather
  than a degraded start.
- Bootstrap: the Ollama URL override now lands in the effective config
  (previously it was applied only to the constructed client, so
  `Bundle.Config` reported a URL the client was not dialing), refresh runs
  only for providers on the active plan, and slot probes, capability
  probes, and registry-initiated model queries bind their own metadata
  purposes.

### Added — agent/tools: Linux Bubblewrap sandbox backend (#441)

A `SandboxRuntimeBwrap` execution backend behind the #440 exec-backend
seam, part of the zero-trust epic (#429, Phase 3). On a capable Linux
host it runs every approved command — and its descendants, foreground and
background alike — inside per-invocation Bubblewrap namespaces:

- Deny-default by construction: an empty mount namespace populated only
  with reviewed read-only system roots (`/usr/{bin,sbin,lib,lib64,
  libexec,share}`), typed top-level layout binds/symlinks, exec-critical
  `/etc` literals (linker cache, alternatives; resolver and narrow trust
  stores only when `AllowNetwork` is true), a minimal `/dev` and a
  PID-namespace-private `/proc`, the workspace as the only host-visible
  writable mount, and a final read-only remount of the base root.
- Zero IP egress unless `AllowNetwork` is true: a fresh network namespace
  denies outbound TCP/UDP and host abstract Unix sockets. A pathname Unix
  socket inside the writable workspace deliberately remains a host
  channel; stdio and inherited descriptors are TCB channels. No broader
  "all egress" claim is made.
- Fresh user namespace with nested creation disabled
  (`--disable-userns`), PID/IPC/UTS isolation, all capabilities dropped,
  `--die-with-parent`, and a new session; the existing timeout plus group
  kill plus PID-namespace teardown removes the whole tree, including
  session-detached descendants.
- Private temp is a namespace-local tmpfs (`/tmp`, plus a private
  `/dev/shm`): born empty, invisible to the host and other invocations,
  destroyed with the namespace — no cleanup machinery exists. Inherited
  `TMPDIR` stays command input; the payload sees exactly the approved
  allowlist environment (carried as `--clearenv`/`--setenv` policy
  arguments; the outer prlimit/bwrap chain runs with an empty
  environment).
- `MemoryCapMB` maps to a per-process `RLIMIT_AS` soft+hard ceiling via a
  `/usr/bin/prlimit` exec chain, plus `--size` quotas on both private tmpfs
  mounts. These are three independent, additive budgets rather than one
  ceiling: a single invocation can hold roughly three times the configured
  value of host memory (address space plus each tmpfs). It is not an
  aggregate tree cap either — the figure is per process, so a fork bomb
  splits allocations across children — and `RLIMIT_AS` counts virtual
  reservations, so VM-heavy runtimes need generous caps. True aggregate
  enforcement requires delegated cgroup v2 and is deliberately out of scope.
  `/dev` is remounted read-only because `--dev` creates its own tmpfs that
  neither `RLIMIT_AS` nor either quota would otherwise bound. Because a bare
  number would read as a total ceiling the backend does not enforce, the
  approval preview renders the scope: `memory_cap=512MiB/process`. `CPULimit`
  is rejected; requested `DropCaps` are subsumed by the unconditional
  drop-ALL.
- Fail closed, never a host fallback: missing or unsafely packaged
  `/usr/bin/bwrap` (and, for capped configs, `/usr/bin/prlimit`), or a
  failed active capability probe of the production namespace prefix,
  fails construction with a remediating error. Docker's default seccomp
  blocks user namespaces, so confinement is exercised by the pinned
  `Linux Sandbox (bwrap)` CI job (`GO_LLM_REQUIRE_BWRAP=1`), not by the
  Docker-based local CI.
- Known limitations, unchanged from the pathname threat model: the
  workspace bind shares inodes with the host (externally hard-linked
  workspace files are rejected at prepare time; an unsandboxed same-UID
  process can still race pathname replacement before bind resolution),
  and the pre-spawn inheritable-FD audit is not kernel-atomic with the
  spawn. That audit is also process-global: a descriptor leaked in by
  whatever launched the agent — which go-llm neither created nor may close —
  fails every sandboxed command until the launcher sets `FD_CLOEXEC` on its
  own descriptors, which the error message says explicitly.
- Scope boundary: this backend governs the exec tools. The post-write
  verification command (#347) deliberately runs on the host runtime, so a
  session using the bwrap runtime still has one unsandboxed exec path, as
  that command's own approval preview states.

Approval identity: bwrap grants live in their own `sb:<digest>:` key
namespace derived from the approved `SandboxConfig`; the `exec:v3` /
`exec-bg:v2` fingerprint recipes are unchanged. The approval preview
renders `runtime="bwrap"` with the `temp=private` marker and, when a cap is
set, the `/process` memory scope. Preview text is presentation only and is
not part of the key, so no existing grant changes meaning.

### Added — golem: grounding verification for retrieval-backed answers (#348)

`-grounding` (default off) runs claim-support verification over a completed
turn's final answer. When the turn used `retrieve` and the answering prompt
carried retrieval attribution, `analysis.SupportJudge` judges the answer
against exactly that evidence and one dim line is printed:

```text
grounding · partial · 3/4 claims · 5 evidence · 1.2s · 850 tok
```

Evidence is captured from the retriever's own results and joined to the
post-assembly `RetrievalPresentationObserver` attribution, so the judge sees
what the model actually read rather than the raw retrieval set. Both the
legacy and progressive retrieval paths are eligible. The capture is
turn-scoped and byte-bounded; anything it cannot reconstruct exactly - a
capped chunk, or an identity resolving to two different chunks - is reported
as `evidence_incomplete` with no model call, because a verdict over a
silently reduced evidence subset would mark supported claims unsupported.

The verdict is scoped: it measures support against the retrieval evidence in
that prompt, not overall correctness, so claims drawn from ordinary language or
standard-library knowledge read as unsupported. It costs two sequential model
calls per retrieval-backed turn and prints a notice while running. The judge
prompt fences evidence as untrusted data and echoes evidence ids rather than
accepting them, but per-claim verdict fields still come back from the model, so
`supported` is evidence of grounding rather than a security boundary over an
untrusted corpus.

Fail-open throughout. A routing failure, malformed verifier output, the
60-second ceiling, or Ctrl-C during the judge changes nothing about the
answer, `agent.Result`, the session, the recorded run status, or the exit
code. The two verifier stages route by their own `extract`/`verify`
side-task use-cases at background priority, so verification never displaces
the primary agent model. Verifier tokens and latency are reported only in
the grounding payload: they are absent from the run's usage footer, from
`agent.Result`, and from telemetry.

Frozen payload for #352. The report object is fixed by an exact-bytes golden
test and #352 will serialize the same fields:

```json
{
  "status": "supported|partial|unsupported|skipped|error",
  "reason": "no_final_evidence|evidence_incomplete|canceled|timeout|judge_failed|evidence_truncated",
  "tokens": 0,
  "duration_ms": 0,
  "report": { "status": "...", "claims": [], "evidence": [],
              "missing_evidence": [], "missing_evidence_queries": [] }
}
```

`reason` is omitted for a verdict and required for every `skipped` or
`error`; `report` is present only for a verdict. Raw provider and router
error text never enters the payload - it is terminal diagnostic output only.
#348 freezes this shape but emits no runtime event: `golemruntime.Run`
emits its terminal `run.finished` before post-run grounding exists, so #352
buffers that terminal event at the CLI adapter, runs grounding, and adds the
object to the protocol-v1 payload before writing it. `golem/runtime.go` is
unchanged.

Consumer notes: `internal/agenttrace.TraceRecord` gains an optional raw
`grounding` object, additive within schema version 2, so a trace without one
is byte-identical to a pre-#348 trace. `-telemetry` receives no grounding
field. `-grounding` is unrelated to the `.golem.json` `verify` command
(#347), which checks the workspace after a write; task and planning modes
ignore the flag with a warning because neither runs an answer turn.

### Added — agent/tools: macOS Seatbelt sandbox backend (#442)

A `SandboxRuntimeSeatbelt` execution backend behind the #440 exec-backend
seam, part of the zero-trust epic (#429, Phase 3). On a capable, unsandboxed
macOS host it runs every approved command — and its descendants, foreground
and background alike — under a per-invocation `sandbox-exec` (Seatbelt)
profile:

- Writes are confined to the canonical workspace and one fresh `0700`
  private temp directory created per invocation. Before each spawn, the
  backend rejects any workspace inode with a hard-link name outside that
  root while allowing hard links whose names are all internal. The inherited
  `TMPDIR` is never trusted as policy: it is command input, replaced in the
  child environment with the private directory.
- Reads outside the workspace/private-temp are limited to the exact
  executable target, a minimal reviewed set of read-only system runtime
  subtrees (loaders, system libraries), and the fixed device literals
  `/dev/null`, `/dev/random`, `/dev/urandom`. `$HOME` and other user data
  are unreadable — so, for example, a sandboxed `git` cannot read
  `~/.gitconfig`; that is the boundary working as intended. Broad or
  data-bearing roots (`/System`, `/usr`, the `/System/Volumes/Data`
  firmlink) are never granted.
- File metadata access is scoped to allowed subtrees plus the exact
  traversal ancestors, never globally.
- Outbound TCP, UDP, and Unix-domain sockets are denied unless
  `AllowNetwork` is set; `AllowNetwork` is an all-or-nothing network-class
  relaxation, not an egress firewall.

The backend fails closed with no host fallback: off macOS, when
`sandbox-exec` is missing, when an active capability probe cannot apply a
profile (a nested-sandbox host returns `sandbox_apply` EPERM — an existence
check alone is a false positive), and for the `MemoryCapMB`, `CPULimit`, and
`DropCaps` ceilings Seatbelt cannot enforce.

Limitations, disclosed deliberately: `sandbox-exec` and SBPL are deprecated
by Apple (active probing turns removal or disablement into a clear
unavailable-runtime error, not a replacement); the pre-spawn hard-link audit
walks all workspace entries, and a same-UID unsandboxed process can still race
filesystem mutation after that final host check; and Homebrew/MacPorts/custom-
dylib tools whose libraries live outside the reviewed roots may fail rather
than run with a widened profile. Runtime selection is a library capability
until #347 wires it into `cmd/golem`.

As part of this change the exec approval-key recipe gained the canonical
workspace root (foreground `exec:v2:` → `exec:v3:`, background
`exec-bg:v1:` → `exec-bg:v2:`) so a grant cannot cross workspace
boundaries. Grants are session-scoped, so there is no persisted migration.
### Added — golem, agent, agent/tools: post-write verification hook (#347)

An optional, workspace-declared command that runs after any tool-call batch
which successfully ran `write_file` or `edit_file`, before that batch's
result returns to the model, so a break is visible in the same turn instead
of surfacing to the user turns later.

`agent` gains a hidden `Verifier` seam installed with `agent.WithVerifier`.
It is not a model-visible tool and is absent from the tool schema. The hook
runs at the shared `runToolCalls` boundary, and eligibility is the exact tool
names `write_file`/`edit_file` rather than `EffectClass`: `Write` also covers
agent-memory writes, and `IsMutating` covers the exec tools, which already
report their own outcome. `Verify` returns `(string, error)`; every outcome of
the check itself is data on the string return and can never fail a run, while
the error channel carries only an interrupted approval, a control-plane
failure, or cancellation.

`agent/tools` gains `VerifyCommand`, which reuses `run_command`'s bounded
preparation — argv validation, workspace-contained cwd, the fixed environment
allowlist, executable identity re-check at spawn, process-group kill, output
caps — through the #440 backend seam at the host runtime.

Golem reads `.golem.json` at the workspace root, only under `-allow-write` and
with no ancestor search, accepting `argv`, a relative `dir` and
`timeout_seconds` and nothing else. The resolved command is approved once per
session under its own grant namespace, so a verification grant can never
authorize `run_command`/`start_command` or the reverse. Failures are appended
to the batch's last successful write under a separate 4 KiB model-visible cap.

Behavior is byte-for-byte unchanged when no `.golem.json` declares a verifier,
which includes one-shot, task, planning and Agentflow modes: all of them
either clear or reject `-allow-write`. Verification runs on the host with no
isolation, so a verifier that writes produces changes #355 checkpoints did
not capture and `/undo` will not restore.

### Added — config: role lifecycle mutations and atomic credential scrub (#462)

Six narrow `*config.Document` operations unblocking the Firn config
workspace (firn-ide#263 Slice B): `AddRoleModel` (atomic complete-role
creation with the SetRoleModel eligibility semantics), `ForkRoleModel`
(lossless copy of the source's raw authored subtree — unknown/future JSON
members survive — with exact drop confirmation for the projection-hidden
think_tags/slots, refused before mutation; required set exposed via
`DropSetOf`), `UnbindUseCase`, guarded `RemoveRole` (refuses while routed
or fallback-referenced; generalized tombstones prevent stale raw members
from resurrecting on re-add), `SetRoleOverrides` (selector-wide explicit
capabilities/think-mode edits preserving every omitted authored field,
ThinkTags and Slots included), and `ClearAllProviderAPIKeys` (atomically
clears every authored provider api_key — literal and ${ENV} forms —
returning no names, values, or Config). Four new diagnostic codes extend
the closed vocabulary to 31: `role_exists`, `role_in_use`,
`use_case_not_found`, `drop_confirmation_required`. `SetRoleModel` and
all existing signatures are unchanged.

### Added — golem, agent/tools: multi-step persisted mutation checkpoints (#355)

Golem's `/undo` is now durable and turn-scoped. Every interactive write is
journaled write-ahead into a per-workspace hardened SQLite store
(`<data>/golem/checkpoints/<workspace-hash>.db`, 0700/0600, WAL,
single-connection) guarded by an exclusive OS file lease, and all mutations
from one REPL turn group into one checkpoint.

- **Write-ahead protocol.** `agent/tools` gains an optional
  `PreparingJournal` capability: `write_file`/`edit_file` persist a
  mutation intent BEFORE the workspace rename and mark it applied after,
  so a crash can never leave an applied change the journal never saw.
  Plain `Journal` consumers (AgentFlow's composite journal) keep the
  post-write `Record` path unchanged.
- **`/undo [n]` and `/checkpoints [list]`.** `/undo` reverts whole turns
  (default 1), across restarts, with an all-file chain-simulated hash
  preflight: any divergent, removed, symlink- or directory-replaced file
  refuses with the existing message and changes nothing. Restores apply in
  reverse mutation order with persisted per-file progress; a crash mid-undo
  resumes idempotently on the next `/undo`, and new model turns are blocked
  until the interrupted undo resolves. `/checkpoints` lists turns newest
  first with control-safe single-line labels.
- **Crash recovery.** Startup reconciles a crashed turn's intents by live
  file state (never-landed intents are dropped; landed ones become an
  undoable checkpoint) and reports one notice. Recovery refuses to guess on
  unclassifiable paths and `-allow-write` fails closed on any store, lease,
  migration, recovery, or hardening failure.
- **Strict retention.** At most 50 completed checkpoints and 64 MiB of
  prior content per workspace, enforced by pre-write admission (oldest
  completed checkpoints are pruned; a mutation that cannot fit is refused
  before touching the workspace). Open and undoing checkpoints are never
  pruned.
- **Process model.** One write-enabled golem per workspace: a second
  process fails closed on the checkpoint lease instead of last-writer-wins.
  Durability covers process crashes; host/power-loss fsync hardening
  remains out of scope.

### Added — tools: sandbox backend seam (#440)

Added `SandboxConfig` and fail-closed sandbox backend construction for the
foreground and background exec paths. Host remains the only implemented
runtime and preserves existing execution, approval keys, and previews;
non-host implementations land in #389, #441, and #442.

### Added — golem, tools: background command execution (#346)

Golem can now start long-running commands in the background and keep
working. The `-allow-exec` tool set gains four model-facing tools:
`start_command` launches a detached argv-first command (no shell) and
returns an opaque handle immediately; `command_status` reports one job's
state; `command_tail` reads stdout/stderr incrementally with resumable
cursors; `stop_command` kills the job and reports its final state.

- **Approval.** `start_command` is approval-gated under a distinct
  background key namespace (`exec-bg:v1:` vs foreground `exec:v2:`), so a
  foreground "don't ask again" grant never authorizes a background start
  of the same command, and vice versa. `stop_command` always prompts (its
  approval key is deliberately empty); `command_status` and `command_tail`
  are read-only and never prompt.
- **Output caps and retention.** Each job retains the newest 64 KiB per
  stream in a tail ring; evictions are reported explicitly as
  `dropped_bytes`, never as a silent gap. Tail reads return 8 KiB by
  default, capped at 16 KiB per call. At most 4 jobs run concurrently and
  the newest 8 finished jobs are retained; running jobs are never evicted.
- **Interactive-process scope.** Background jobs run with stdin wired to
  `/dev/null`: a child that prompts for input reads immediate EOF instead
  of hanging. Interactive processes are out of scope.
- **`/jobs` REPL command.** `/jobs` lists the session's background jobs
  (one control-safe line per job); `/jobs stop <handle>` stops one
  directly with no model call and no approval prompt. Jobs survive
  `/new`, `/clear`, and `/resume` while session approval grants still
  clear.
- **Linux/macOS managed-process-group guarantee.** Every job is the leader of its
  own process group; stop and shutdown SIGKILL the whole group and the
  REPL does not return before killed groups are reaped, and when the
  command exits on its own, background processes it left running in the
  group are also killed. Descendants that escape the managed group (e.g.
  via setsid) are unsupported. Other platforms fail closed:
  `start_command` reports exec unsupported.

### Added — golem,rag: managed source lifecycle in the CLI (#349)

`golem source add|list|rm|reindex` manages ad-hoc documents in the workspace
index over immutable index generations: mutations acquire the writer lease,
copy the active generation, apply the managed operation to the staging copy,
and atomically publish; the active index is never modified in place. `list`
reads the published generation read-only with non-mutating freshness
reconciliation (`rag`: immutable stores now reconcile listing snapshots in
memory instead of failing on the write). `rm` and `list` need no provider
backend; `-json` list output is machine-clean (`[]`, never `null`; notices on
stderr).

### Added — configio: explicit inventory refresh and tool_call probe (#456)

New leaf package `configio` implements the two explicit I/O operations of
the role-config stack (v4 spec slice 4), consumed by the Firn config panel
(firn-ide#263) and the upcoming golem CLI surfaces (slice 5):

- `RefreshInventory(ctx, providers, models)` — explicit provider model
  listing returning a new `configview.Inventory` value. Deterministic
  ordering; a provider whose listing fails is reported in-value as
  `Reachable: false` (provider reachability only). Persisted tool_call
  probe verdicts (yes AND no) surface through `KnownMask`, validated
  against the listing identity, so they stick across sessions (digestless
  negatives bounded by a 7-day TTL). Refresh performs exactly one listing
  call per provider and NO other provider I/O — no fingerprint probes, no
  tool-call probes, no re-queries — and a cancelled refresh publishes
  nothing. Wall-clock is bounded per provider by the models.json
  `timeout` and overall by the caller's context.
- `ProbeToolCall(ctx, resolver, key)` — explicit, per-model, consumer
  consent-gated probe wrapping `ModelRegistry.ResolveToolCall`.
  Returns `ProbeOutcome{State, Persisted}`: the verdict is valid for the
  session immediately; `Persisted` is true only when the verdict is
  durable (a currently-VALID probe row, or explicit/catalog/profile
  knowledge that re-derives next session) — `Persisted=false` warns it
  may not survive a restart, as a warning, never an error, and never raw
  I/O text. A model no path can answer for yields `probe_unavailable`.
  A cancelled caller receives nothing.
- `provider.ModelRegistry.ProjectListedModels(ctx, provider, infos)` —
  the read-only, list-fed fact projection behind refresh: reuses the
  registry's merge layering with the runtime layer taken from the
  supplied listing and the fingerprint layer forced read-only; never
  probes, never re-queries, never writes the profile cache; total over
  the listing (its only error is cancellation).
- Errors carry bounded codes via `configio.CodeOf`: `invalid_argument`,
  `probe_unavailable`, `probe_failed`. Caller cancellation is
  UNCLASSIFIED (raw context error, no code). Firn mapping: add all three
  codes to both closed allowlists; unknown future configio codes fall
  back to the generic diagnostic with a cleared subject; configio errors
  carry no subject. Consumers must forward only the bounded code across
  projection boundaries — configio error messages deliberately retain
  the wrapped cause text for CLI and log surfaces and are not
  boundary-safe.

### Changed — provider: bounded parallel capability resolution (#401)

`EnsureToolCallResolved` now resolves distinct unresolved model keys
concurrently (bounded at 4 per invocation) instead of serially, so a cold
route over U unknown models costs roughly `ceil(U/4) x ~30s` instead of
`U x ~30s`. Chain routing batches every chain entry's candidates into one
resolution wave. Live probes acquire through the Router's slot-admission
gate (#400) where a backend is governed; cached verdicts, overrides, and
merged capabilities never touch admission.

- **Duplicate keys share one probe.** Within one call, unresolved
  occurrences of the same key receive one resolution attempt and the same
  result; transient failures are not retried until a later call.
  Candidate order, pointer behavior, input immutability, and diagnostic
  ordering are unchanged from the serial implementation.
- **Cancellation now surfaces raw.** A route that dies with the caller's
  context returns `ctx.Err()` (`context.Canceled` /
  `context.DeadlineExceeded`) instead of burying it inside
  `ErrNoViableCandidate` — Golem classifies cancellation correctly and
  `compat` can answer 499/504 instead of 400. Ordinary probe failures
  still classify as `ErrNoViableCandidate` with stringified diagnostics;
  router closure remains a terminal `ErrRouterClosed`.
- **Probe ordering and expiry use separate timestamps.** `TestedAt` is
  captured before the cache read so an older slow probe cannot overwrite
  a newer verdict; equal persisted timestamps keep the first verdict.
  TTLs remain anchored after the probe returns.
- **Ownership invariant.** At most one live `Router` may attach to a
  `ModelRegistry`; `NewRouter` installs itself as the registry's probe
  admission gate.

### Added — governed dispatch fan-out and per-child progress (#403)

Golem's `-dispatch` tool now runs children concurrently when every backend
in the child chain is slot-governed, sized per invocation from the chain
head's discovered slot capacity (`min(capacity, 4, tasks)`); ungoverned or
unknown routing stays serial, and the existing validated
`models.<role>.slots` override feeds sizing through `Router.SlotCapacity`.
Router admission (#400) remains the oversubscription guard — fan-out sizing
is quality of service only. Each completed child now emits a display-only
`dispatch: task #<index> finished (<count> total)` notice (stderr mid-turn),
where the index identifies the input task and notices may arrive out of order.

Library API (`agent/tools`): `DispatchLimits` gains two optional function
fields — `Concurrency func() int`, read once per Invoke and clamped to
`[1, MaxConcurrent]`, and `OnChildComplete func(index, total int)`, called
from child goroutines after both dispatch permits release. The struct is
therefore no longer comparable. `MaxDispatchTasks` (4) is now exported. The
instance-wide `MaxConcurrent` bound across overlapping invocations is
unchanged; the dispatch envelope and `agent.Observer` contract are
unchanged.

### Added — golem: REPL line editing, goal history, and multiline input (#340)

The interactive `golem` prompt is now a real line editor
(`golang.org/x/term`) instead of a `bufio.Scanner` read. On a terminal you
get arrow-key cursor movement and in-line editing, up/down recall of previous
goals, and multiline goals.

- **Per-workspace goal history.** Accepted goals persist under
  `$XDG_DATA_HOME/golem/history/<workspace>` (directory `0700`, file `0600`),
  keyed by workspace so one project's goals never surface in another. Only
  accepted goals are recorded: blank lines, slash commands, and approval
  answers never reach the store. Entries that cannot be safely re-edited in a
  single-line editor are stored in full but excluded from arrow-key recall —
  that is any entry longer than 4096 runes, any entry containing DEL or **any**
  control character below `0x20` (which covers multiline text (LF), CR, ESC,
  BEL, and tab), and any entry that is not valid UTF-8. The last case exists
  for entries written before the encoding boundary below: recall replays them
  through the editor rune by rune, so pressing Enter would submit different
  bytes than were stored.
- **Goals must be valid UTF-8.** A goal containing malformed bytes is refused
  with a warning and neither recorded nor sent to the model, from every input
  source: the line editor rejects it at the terminal, and the scanner and
  `/edit` are checked immediately before the goal is recorded and run. This is
  a behavior change — such input previously reached the provider, where the
  JSON transport substituted U+FFFD silently, so the model answered a question
  the user had not typed. A correctly encoded U+FFFD is still accepted outside
  the line editor, which is the only place it cannot be represented.
- **A pasted block is one goal.** Bracketed paste is detected below the
  editor, so pasting several lines composes a single goal and runs one turn
  rather than submitting each line separately.
- **Explicit continuation.** A line ending in an odd number of backslashes
  continues the goal on a `...> ` prompt. A trailing run of `n` backslashes
  emits `n/2` literal backslashes.
- **`/edit [seed]`** composes a goal in `$VISUAL`, else `$EDITOR`, else `vi`
  (`notepad.exe` on Windows). The editor value may carry arguments
  (`code -w`); it is split on whitespace into argv with **no shell
  interpretation**, so quoting and shell syntax are unsupported. The result
  runs as a goal even if it begins with `/`. `/edit` is refused when stdin or
  stdout is not a terminal, so a piped script cannot spawn an editor.
  The editor **must block until the edit is finished**: configure `code --wait`
  or `subl -w`, since an editor that forks and returns immediately hands back
  the unmodified seed, which then runs as a goal. Each edit gets its own
  directory outside the workspace, removed whole afterwards so editor backup
  and swap files cannot outlive it, and the draft is read back through a
  confined root that will not follow a symlink planted while the editor ran.
  Asynchronous notices are held while the editor owns the screen and flushed
  when it exits.
- **Ctrl-C at an idle prompt** discards the partially typed line and hints;
  a second press with no input between them exits. Ctrl-D on an empty line
  still exits, and Ctrl-C during a turn or an approval still cancels it.
- **`-no-editor`** forces the previous scanner behavior on a terminal. It
  disables inline editing only: `/edit` remains available.
- **Input ceilings.** A single line is limited to 4096 runes (x/term's
  bound; golem warns instead of dropping the keystroke silently), and a
  composed goal, a single paste, or an `/edit` result is limited to 1 MiB —
  the same ceiling the scanner path always had.

**Non-interactive behavior is unchanged.** Piped stdin, `-p`, `-plan`, and
`-goal` keep the scanner and produce byte-identical output.

**Limitation — terminals without bracketed paste.** Paste-as-one-goal
requires the terminal to bracket pasted text (`ESC[200~` / `ESC[201~`), which
golem enables for the duration of each read. On a terminal that does not
support it, a multiline paste arrives as ordinary Enter presses and is
indistinguishable from typing: each line submits as its own goal and starts
its own turn. The workarounds are `/edit` for anything long and a trailing
`\` for explicit continuation. Shift+Enter is not a solution: terminals do
not portably distinguish it from Enter, so golem cannot bind it.

**Windows keeps the scanner in this release.** Selection declines the editor
on Windows before any descriptor is probed, because `x/term`'s Windows
`MakeRaw` enables virtual-terminal processing on the input handle only, and a
correct editor there additionally needs console-output setup plus a real
Windows test runner. Windows is compile-verified in CI; enabling the editor
there is a follow-up. `/edit` works on Windows.

### Changed — golem: interrupted approvals record `canceled`, not `error`

A Ctrl-C during an interactive approval prompt now always records the run's
trace and telemetry status as `canceled` and renders `canceled`. Previously
the recorded status raced the interrupt watcher's context cancellation and
could land as `error` with an `error: interrupted` line. `runOnce` now
synchronizes the run context whenever the run returns `context.Canceled`, so
the classification no longer depends on scheduler order. Telemetry consumers
that keyed on `status == "error"` for interrupted approvals will see those
runs as `canceled` from this version on.

### Added — mixed-domain context assembly, slice 3b of #331

The agent runtime can now assemble model context from RAG results,
conversation spans and agent-memory records at MIXED fidelity under one global
token budget, instead of retaining or dropping each tool result whole. Tools
declare what they COULD contribute (a `ContextSet` of per-subject
alternatives); `ContextManager` picks at most one alternative per subject and
reports every choice in a content-free trace.

Opt-in via `ContextManager.Mixed`. Off, model-visible messages stay
byte-identical and the new trace is the zero value.

Mixed assembly preserves `RecencyCompactor`'s recency semantics: it replaces
the compactor rather than layering on it, and under pressure it retains the
same messages the compactor would have. Two orderings are involved. Retention
PRIORITY by kind is the reverse of the compactor's drop order (current-run
plain exchanges, then completed tool chains, then prior history). WITHIN each
of those, the NEWEST members are retained, exactly as dropping oldest-first
does. A consumer switching a pressured session to `Mixed` therefore does not
have to re-discover which turns the model still sees.

**Two behavior changes reach consumers who do not opt in:**

- `agent/tools.Retrieve.Progressive` is now a HARD CONTRACT on `R`. Setting it
  with a retriever that does not implement `RenderProgressiveWithGroups` fails
  every call instead of silently serving the legacy `BuildContext` path with
  its over-crediting attribution and no `ContextSet`. `*rag.Retriever`
  satisfies it; only a consumer-supplied retriever is affected.
- `agent/tools.Retrieve` now clamps the model-supplied `k` to 20 on the
  LEGACY path too, before the backend call. A consumer whose model asks for
  50 results silently gets 20, and the legacy attribution set (which credits
  every retrieved result) shrinks with it. Unbounded model-supplied `k` is a
  resource vector in flat mode as well; the new `Retrieve.MaxK` field is the
  escape hatch. In progressive mode `MaxK` is additionally capped at 20 and a
  larger value is rejected per call, because the capability projection emits
  3(k+1) alternatives per fresh source and 21 would exceed the carrier bound.
- `golem -progressive` now also sets `ContextManager.Mixed`, not only the
  summary generation described below. That rewrites the model-visible bytes of
  every tool anchor and shifts which `Pressure.Cause` bucket a pressured run
  reports. The library-level `golem.Options.Progressive` sets ONLY the mixed
  flag; a library host wires the tool's own `Progressive` field itself.

New public API:

- `agent`: `ContextManager.AssembleWithTrace`, `ContextManager.Mixed`,
  `ContextSet` / `ContextGroup` / `ContextAlternative`,
  `ContextAssemblyTrace` / `ContextSubjectTrace`,
  `ContextAssemblyObserver` / `ContextAssemblyEvent`, `ErrMixedCompactor`,
  and the `Decision*` / `Omit*` trace vocabulary.
- `agent/tools`: `Retrieve.MaxK`.
- `golem`: `Options.Progressive`.
- `rag`: `Retriever.RenderProgressiveWithGroups`, `ProgressiveGroup`,
  `ProgressiveAlternative`.
- `contextdepth`: the descriptor vocabulary these carry (`SubjectRef`,
  `GroupDesc`, `AlternativeDesc`, `RepresentationDesc`).

`Retriever.RenderProgressiveWithGroups` returns the same output, trace and
error as `RenderProgressive` for the same request, on every path. A blank
`Chunk.Source` on any result yields NO groups for that call rather than
failing it — such a result has no subject id, and a partial projection would
lose its blocks under mixed assembly, which replaces the anchor's flat content
with the selected alternatives.

Under mixed assembly a fresh source is offered the deterministic metadata
overview as its cheapest alternative, matching the flat renderer's own
budget fallback, so a source that does not fit at summary depth still
contributes a short block instead of vanishing. Its note line reads
`summary omitted: budget` — never `no summary`, which would be false.

**`Pressure` gains a field, and one existing field changes meaning.** Read
this before upgrading a telemetry consumer.

- NEW `Pressure.AnchorOmissions` counts subjects mixed assembly dropped from a
  RETAINED structured anchor. The usual cause is a full anchor byte cap
  (`Message.OutputCap`, 64 KB for `retrieve`), which a large retrieval hits
  routinely — a 20-source projection is far more alternative text than the cap
  can hold. Before this counter such a drop was invisible: `Evicted` stayed 0,
  `UsedPct` stayed low, `Level` stayed `ok`, and `ToolResult.Truncated`
  describes the DISCARDED flat rendering, not the mixed one. Always 0 with
  `Mixed` off.
- `Pressure.Evicted` is unchanged and still counts WHOLE groups (spans and
  chains). Within-anchor omissions are deliberately NOT folded into it: five
  sources shed from one retained anchor is not five evicted groups.
- CHANGED `Pressure.Compactions` and `Pressure.Mitigation`: under mixed
  assembly a turn that only shed subjects from a retained anchor now reports
  `Compactions: 1` / `MitigationEvict` (previously `0` / `MitigationNone`).
  The orchestrator emits its `compaction` `EventRecord` for such a turn, and a
  consumer counting compactions will see turns it did not see before. Legacy
  (`Mixed` off) values are byte-for-byte unchanged.
- `Pressure.Level` deliberately does NOT react. The bands measure TOKEN-budget
  usage; a byte-cap omission is orthogonal (a turn can shed a quarter of its
  retrieval at 8% of budget), and promoting `Level` would make the bands mean
  two different things and break existing level histograms.
- `internal/agenttrace` carries the count as `anchor_omissions` on the
  `model_step` and `runtime_stage` spans. Additive within `SchemaVersion` 2:
  the key is omitted when zero, so legacy and lossless turns emit the same
  bytes as before.

`golem -telemetry` now emits a `context_assembly` span per mixed assembly,
pairing with `anchor_omissions`: token totals, subject counts,
`verbatim_shortfalls`, rendered bytes, and `by_decision` / `by_omission_reason`
breakdowns keyed on agent's fixed vocabulary. It is a NEW span kind, so it is
additive within `SchemaVersion` 2 and only mixed turns emit it. The breakdowns
are counts only — persisted telemetry does not retain the per-subject rows that
carry source paths, memory record IDs, and tool call IDs. `-trace` likewise
does not serialize structured `ContextAssemblyTrace` rows or row fields, though
its content-full model-visible messages can independently contain those
identifiers. The rows themselves are available only to a live
`ContextAssemblyObserver`.

`golem` no longer prints `(truncated)` on a tool-result line when mixed
assembly replaced that result's content. `ToolResult.Truncated` describes the
DISCARDED flat rendering, and the flag cannot be recomputed at that point:
assembly runs against a global budget before the next step's model call. Plain
tools under mixed, and every tool with `Mixed` off, are unaffected.

MEMORY: with `Mixed` on, each tool result's projection is cloned onto its
anchor message and retained for the rest of the run. For `Retrieve` that is
quadratic in `k` — 1.35 MB per call at `MaxK` 20 over 2 KB chunks, so a
20-step run holds roughly 27 MB. That worst case needs every one of a call's
`k` results to land on ONE source; results spread over 4 or more sources cost
under 0.4 MB per call. With `Mixed` off nothing is cloned.

### Added — model-backed progressive source summaries, slice 2 of #189

Golem can now generate and serve the existing L0 abstract/L1 overview ladder
with the explicit `-progressive` flag. Generation runs only in an unpublished
index generation, routes through the configured `summarize` role (including
its existing `analysis`/`chat` fallback), and records the model that actually
served the request. Default indexing and retrieval still make zero summary
model calls.

`SQLiteStore.GenerateSourceSummaries` refreshes missing or stale summaries
from stored indexed chunks. It copies `ContentHash` and `VectorSpaceID`
byte-for-byte from `SourceProvenanceBatch` and compare-and-swaps the write
against both values, so a concurrent reindex cannot publish stale model text.
Rows below `SourceSummaryFormatVersion` regenerate; rows above it remain
unreadable and are not overwritten by an older binary. A per-source model or
validation failure leaves that source on the deterministic metadata fallback,
continues the remaining summaries, and warns without blocking index publication.
That warning leads with a `N of M sources failed` tally, because callers print
only its first line.

Degradation rules, so `-progressive` fails visibly rather than quietly:

- A source larger than the prompt budget is summarized from its leading chunks
  instead of being refused, and the model is told outside the fence how much it
  is seeing. Refusing would leave large sources permanently unsummarizable and
  re-erroring on every index run.
- Model output wrapped in a single Markdown code fence is accepted, since local
  models emit that despite instructions. The rest of the contract stays strict:
  unknown fields, trailing objects, blank fields, and a multi-line abstract are
  all still rejected.
- `-progressive` now warns when no `summarize`/`analysis`/`chat` default
  resolves. Previously the flag was accepted and did nothing at all — including
  on the zero-config path where no `models.json` is discovered.

`SourceProvenanceBatch` and `SourceSummaryBatch` now read in bounded batches.
They were introduced for retrieval-result-sized inputs;
`GenerateSourceSummaries` passes every source in the index, which would exceed
SQLite's 32766-variable ceiling on a large enough workspace and fail the whole
read rather than degrade.

`internal/promptfence.FlattenLine` and `internal/modeltext.StripCodeFence` hold
the single copy of two rules that now have multiple callers. No public API
change: `analysis` and `agent/tools` forward to them.

### Added — `rag` progressive source summaries, slice 1 of #189

A store and renderer for per-source L0/L1 summaries, so retrieval context can
mix short orientation text with full chunk evidence under hard token and byte
budgets instead of concatenating whole chunks until a limit is hit.

**Nothing here runs unless you opt in.** `Retriever.BuildContext` is unchanged
and remains the default path; Golem's slice-2 opt-in is described above.

New exported surface in `rag`, all additive:

- `Retriever.RenderProgressive`, with `ProgressiveRenderRequest`,
  `ProgressiveTrace`, `ProgressiveSourceTrace`, `RenderedEvidence`, `PinRef`,
  and the `Depth*` and `Decision*` constants.
- `SourceSummary`, with `SQLiteStore.UpsertSourceSummary`,
  `SourceSummaryBatch`, and `DeleteSourceSummary`. Writers MUST take
  `ContentHash` and `VectorSpaceID` from `SQLiteStore.SourceProvenanceBatch`
  for the same source; any other value stores a row that permanently derives
  stale and never renders, with no error reported.
- `SourceProvenance`, with `SQLiteStore.SourceProvenanceBatch` and
  `SQLiteStore.ChunkContentDigestBatch`.
- `ValidityReason` and its nine `ValidityReason*` constants.
- Schema migration **v8** adds the `source_summaries` table. Writable
  databases migrate on open; a v7 database opened read-only keeps working and
  degrades to summary-missing rather than failing retrieval.

Rendered source paths and managed document titles are untrusted text that
reaches the model. Newline-based forgery of a whole block is blocked;
same-line forgery of a block's own label is not — see the security note on
`RenderProgressive`.

## [0.1.0] - 2026-07-22

First tagged release of `go-llm`. Prior to this tag, downstream consumers
(Firn IDE, Flux ML, Quantum Trader) tracked `develop` via pseudo-versions;
`v0.1.0` is the first stable ref to pin. Semantic versioning applies from
here — `0.x` means the public API may still change between minor versions.

### Initial surface

- **Local LLM backends** — llama.cpp via its OpenAI-compatible server
  (`provider/openaicompat`, the primary/recommended backend) and native
  Ollama (`ollama/`), selected per-provider by `models.json` `api_format`.
- **Use-case-aware routing** (`provider/`) — chat/FIM/embedding/reasoning/
  analysis/code-review/agent profiles, circuit breakers, warmth, sticky
  routing, scoring, and fallback chains.
- **RAG** (`rag/`) — chunking, SQLite-backed vector store, indexing,
  scored/hybrid retrieval, and a managed document registry with stable IDs
  and freshness tracking.
- **Golem CLI** (`cmd/golem`) — local-first terminal coding agent: read/
  write/exec tools, RAG retrieval, project-context (`AGENTS.md`) loading,
  persistent sessions, conversation compression, explicit and agent-authored
  memory, MCP client attachment, and AgentFlow plan/task execution.
- **MCP** — server (`mcp/`, `cmd/go-llm-mcp`) exposing tools/prompts/
  resources over stdio and HTTP/2, and a client (`mcpclient/`) adapting
  external MCP servers' tools for the agent.
- **Memory** (`memory/`) and **conversation** (`conversation/`) — SQLite
  persistence with scope-filtered FTS5/bm25 search.
- **Supporting packages** — `completion/` (FIM), `analysis/`, `feedback/`,
  `fingerprint/`, `prefetch/`, `compat/` (OpenAI-compatible shim),
  `projectcontext/`, and the `cmd/llm-bench` evaluation harness.
- **Distribution** — `go install github.com/kstruzzieri/go-llm/cmd/golem@v0.1.0`,
  or prebuilt `golem` / `go-llm-mcp` binaries (darwin/linux/windows,
  amd64/arm64) attached to the GitHub release. `golem -version` reports the
  build identity.

The consumer-facing notes below describe the state of the `models.json`
defaults and the router API as shipped in this release.

### Breaking for consumers of `models.json` defaults

The root `models.json` lineup has been retargeted for 2026 model releases.
**Consumers that read `models.json` via `config.Load` and expect specific
model names will observe different models at runtime.**

| Role          | Was                    | Now                       |
|---------------|------------------------|---------------------------|
| `general`     | `qwen3.5:27b`          | `gemma4:31b`              |
| `analysis`    | `qwen3.5:27b`          | `gemma4:31b`              |
| `fast`        | `qwen3.5:35b-a3b`      | `qwen3.6:35b-a3b`         |
| `agent` (new) | —                      | `gemma4:31b`              |
| `coding`      | `qwen3-coder-next:latest` | unchanged              |
| `lightweight` | `qwen3:8b`             | unchanged                 |
| `embedding`   | `qwen3-embedding:8b`   | unchanged                 |

**Before upgrading, consumers MUST:**

1. `ollama pull gemma4:31b` and `ollama pull qwen3.6:35b-a3b` on every
   deployment host. Absent pulls will produce 404s from Ollama at first
   use (circuit breaker will trip and fall back per the `fallbacks`
   chain, so behavior degrades rather than crashes — but quality will
   drop).
2. If you pinned to `qwen3.5:27b` or `qwen3.5:35b-a3b` in app-level
   code, that pinning is unaffected by this change (the library only
   reads from `models.json`). You can keep pinning explicitly.
3. Review `docs/llm/recommendation.md` for the rationale and the
   GLM-5.1 parallel experiment plan.

### Added

- **`agent` role** in `models.json` defaults, pointing at `gemma4:31b`.
- **`agent` / `tool-use` weight profiles** in
  `provider/router_score.go:defaultWeightProfiles` so the new role
  gets meaningful scoring instead of falling back to `chat`.
- **`ollama.ChatRequest.KeepAlive`** field exposing Ollama's
  `keep_alive` directive. Useful for benchmark runs that want a model
  to stay warm longer than the 5-minute default.
- **`cmd/llm-bench`** — scaffold for model A/B comparison on captured
  traces. See `docs/llm/benchmark-plan.md`.
- **`qwen3.6`, `qwen3-coder-next`, `gemma4`** families in
  `provider/catalog.json`.
- **`docs/llm/`** — 2026 local-LLM analysis, three candidate setups,
  recommendation, and benchmark plan.

### Changed

- Catalog: `qwen3-coder-next` gains a `latest` variant alias kept in
  sync with `80b` (guarded by test).

### Router: provider-instance pinning (#81)

- **`provider.RoutingRequest.Provider`** — new optional `string` field that
  hard-scopes routing to a specific provider *instance* (the config-time
  name, e.g. `ollama-local-a`, `vllm-prod-1`). Acts as a pre-score filter:
  empty `Model` + `Provider` scopes `Recommend`; unqualified `Model` +
  `Provider` pins `ModelKey{Provider, Model}` via `Lookup`; qualified
  `Model` (`provider/model`) + non-empty `Provider` must agree on identity
  or `Router.Route` returns the new `ErrProviderMismatch` sentinel before
  candidate resolution. `PreferredChain` is authoritative when set —
  chain selectors carry their own provider identity and the per-request
  `Provider` hint is ignored under chain routing.
- **`provider.ChatRequest.Provider`, `GenerateRequest.Provider`,
  `EmbedRequest.Provider`** — optional `string` fields (`json:"provider,omitempty"`)
  forwarded by `Router.Chat / ChatStream / Generate / GenerateStream / Embed`
  into `RoutingRequest.Provider`. Router selection metadata only; not
  forwarded to the concrete provider's execution call (the provider already
  knows its own identity).
- **`provider.RecommendOpts.RestrictToProvider`** — single-string hard
  filter on the recommendation path. Distinct from the still-unused soft
  `PreferredProviders`. An unknown provider name surfaces as a provider
  resolution error rather than degrading to a silent empty result.
- **Sticky-key derivation** — `RoutingRequest.Provider` participates in
  `StickyKey` so two scoped requests with identical affinity/model/use-case
  keep independent sticky entries. Empty `Provider` produces byte-identical
  keys to pre-change behavior, preserving existing affinity warmth.
- **JSON wire / Go literals** — unset request-level `provider` fields are
  omitted on the wire (`omitempty`). Keyed Go struct literals
  (`provider.ChatRequest{Model: ..., Messages: ...}`) are
  additive-compatible. Unkeyed composite literals for the changed exported
  structs (`provider.RoutingRequest{...}`, `provider.ChatRequest{...}`,
  `provider.GenerateRequest{...}`, `provider.EmbedRequest{...}`) will fail
  to compile because positional arguments now shift by one slot; convert
  to keyed literals (recommended) or insert an explicit empty `Provider`
  positional value. All call sites within this repo (`analysis/*`,
  `provider/route_plan.go`, `provider/router.go`, etc.) use keyed
  literals and are unaffected. External consumers — Firn IDE, Flux ML,
  Quantum Trader — live in separate repos and should audit their own
  call sites; they will get a compile error rather than silent
  misbehavior on `go get -u`.
