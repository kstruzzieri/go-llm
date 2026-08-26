# Changelog

All notable changes to `go-llm` are documented here. Downstream consumers
(Firn IDE, Flux ML, Quantum Trader) should consult this before any
`go get -u github.com/kstruzzieri/go-llm`.

## [Unreleased]

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
