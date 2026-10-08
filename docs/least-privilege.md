# Least privilege roadmap

This roadmap extends the [zero-trust umbrella, #429](https://github.com/kstruzzieri/go-llm/issues/429), with operational controls and provenance-aware approval continuity. It separates the implementation baseline assessed on 2026-09-20 from planned work. Issue acceptance criteria own implementation details; this document records scope and dependencies.

The [Mnemoverse article on least privilege for AI agents](https://mnemoverse.com/docs/library/least-privilege-ai-agents) motivates the action boundary: restrict tools, resources, destinations and authority outside the model. An authorized read followed by an authorized send can still disclose data. The provenance work below adds approval evidence; it does not claim to infer intent or provide source-to-destination information-flow enforcement.

## Current boundaries

| Area | Shipped behavior | Limit |
|---|---|---|
| Tool execution | Read-only CLI default; explicit tool mounting; shared preparation, validation and approval before invocation. | Opting into exec permits host execution. Sanitized command environments do not restrict filesystem or network access. The executable, cwd and root are identity-checked before launch (#553), but launch is by pathname, not a pinned descriptor. |
| Agentflow subprocesses | Agentflow, its gates and its `git` calls receive a documented baseline plus operator-approved names (`-agentflow-env`); provider keys and other parent variables are dropped unless explicitly approved. | Environment narrowing only: gates keep host-user filesystem and network authority, and an approved value reaches every gate. |
| Golem's own `git` calls | Parallel-mode worktree operations and the git context snapshot receive a fixed baseline plus Golem's settings; provider keys and other parent variables never reach `git` or the hooks, filters and helpers it runs. | Environment narrowing only: repository hooks, filters and `core.fsmonitor` helpers still run as you with the baseline, `HOME` and your trusted global and system git config. |
| Native sandboxes | Library Seatbelt and Bubblewrap backends fail closed when explicitly selected but unavailable. Sandbox policy participates in exec approval identity. | CLI exec and verification do not yet select these backends. |
| Interceptors | Always-on argument invariants, exec-class egress labels and native scoped-child refusal reporting in the Golem CLI (#575); optional deterministic injection/secret detectors and canary (`-interceptors`). In every configuration, `search` skips the read rule's credential files and scoped `dispatch` refuses protected directories (#627). Observation fencing is independent. | Content detectors and Secrets are off by default. Invariants are lexical checks on named tool arguments, not confinement. Labels and risk scores do not constrain network access or suspend grants. Library consumers opt in to interceptor installation, on the orchestrator and on `NewDispatch`. |
| Provider destinations | Model-provider requests made by config-driven Golem and the go-llm MCP server require admission for remote destinations; guarded transports check capabilities, origins and base paths and refuse redirects. Grants are revocable. | This provider boundary does not govern Golem's connections to external MCP tool servers, shell traffic, or consultant-process traffic. |
| MCP client | Workspace/alias catalog pins detect definition drift and bind keyed fingerprints of the stdio launch (resolved program path, symlink target, argv, working directory, environment policy) or the exact HTTP endpoint, checked before launch or contact (#578); tools require approval, have bounded execution/output, and produce foreign observations. Stdio servers run in the workspace root with a documented baseline environment plus operator-named variables (`-mcp-env`); HTTP servers are pinned to one exact endpoint and every redirect is refused. | Same-path program updates, changed packages behind an unchanged launcher, and file or symlink swaps between check and launch are not detected. Relative and empty `PATH` entries are dropped from the server's `PATH` (a `PATH` with no absolute entry is omitted, leaving programs their own default search path), and on Windows the server always gets `NoDefaultCurrentDirectoryInExePath=1`, so bare names are not looked up in the workspace root first; but `PATH` values are not identity: a wrapper (`env`, `npx`, `uvx`, `sh -c`) or a script's `#!` interpreter binds only the launcher, and what it finds through the absolute `PATH` entries can change undetected. All admitted catalog tools are mounted unless the host selects exact tools per alias (`-mcp-tools`, #579); selection narrows exposure, not catalog verification. Local stdio servers keep host-user filesystem and network authority (confinement is #580), and a named variable's value reaches that server. HTTP servers remain outside provider admission. |
| Grants | Exec grants bind command/environment/runtime details; grants can be cleared. | Edit grants cover the write class; changed script contents and foreign content do not automatically invalidate reuse. |
| Provenance and audit | Observation fences, project-context trust, signed mutation receipts, signed memory records and offline integrity checks. | Observation fences are model-facing wire conventions, not deterministic enforcement barriers. A signature is not authorization. Persistent origin/exposure continuity and complete policy-decision telemetry remain planned. |

Provider clients currently use fixed inference, embedding and model-listing paths. The article's Files API example is not a demonstrated Golem execution path. The destination guard itself does not restrict service operations within an admitted base path.

The per-user signing key identifies the author of receipts, not a separate upstream service principal. Provider API keys are configuration credentials; no task-scoped credential broker is present. `/consult` rebuilds its child environment and explicitly trusts vendor-process egress. Golem does not independently confine the consultant process; Codex's requested read-only sandbox is a vendor-runtime control, not whole-process isolation supplied by Golem. See [grant security](golem.md#grant-security-and-observation-fencing), [MCP trust](golem.md#external-mcp-catalog-trust), and [consult boundaries](consult.md).

## Workspace mutation boundary (#552)

On Linux and Darwin, Workspace writes, deletes and temporary cleanup act relative
to one validated parent directory descriptor. Root replacement and observed
component identity changes fail closed. `write_file`, `edit_file`, RAM undo and
checkpoint undo verify the expected content hash (and recorded mode when tracked)
through that same parent. A separate directory substituted at the old pathname
cannot inherit the earlier check. Conditional operations require content-read
permission; unconditional replacement/deletion does not.

The admitted directory remains authoritative for that operation even if another
process moves it outside the root or under a denied spelling. This is directory
capability confinement, not continuous pathname confinement or process isolation.
Overwrite/delete are not atomic compare-and-swap of the checked leaf or bytes:
a last-instant replacement entry may be replaced/unlinked, but the namespace
operation never follows its final symlink or removes a directory. Temporary
identity checks also have this final-check race. Foreign temp entries observed
before installation/cleanup are left alone; externally moved temps are not
searched for, and cleanup failures are returned with the original error.

Targets admitted absent use atomic no-replace installation, including legacy
unconditional `WriteFileAtomic` calls. Concurrent creation is refused; unsupported
no-replace operations fail closed without a plain-rename fallback. Existing
targets retain permission bits; new files use 0600. There is no stronger crash
durability promise or automatic stale-temp sweeping.

Existing names are checked against canonical on-disk spelling. With a ScopeGuard,
unverifiable names under an unenumerable directory fail closed. Without a guard,
search-only parents remain usable. A missing name has no canonical spelling:
hosts reserving names must cover the filesystem's case/normalization-equivalent
spellings or conservatively restrict accepted names. Workspace imposes no global
normalization. Journals retain logical paths and do not follow directory renames;
checkpoint after-state failures retain pending intent and refuse success. Other
platforms keep the checked-path backend without these concurrent mutation
guarantees.

## Exec launch boundary (#553)

Exec approval covers argv, the resolved executable spelling, working directory,
canonical workspace root, sanitized environment values, timeout and the selected
sandbox/scratch policy. Each plan records filesystem identities for its
executable, cwd and workspace root and rechecks them before invocation. On Linux
and Darwin, scratch setup checks its source snapshot manifest against those
objects (the executable only when it is inside the workspace) before running in
the clone, and native sandbox preparation checks that the executable target it
resolves is the approved object.

These checks do not bind file bytes: an in-place rewrite keeps the identity.
They do not bind a script's interpreter, loader inputs or dependencies, or the
kernel's pathname resolution at launch; commands are started by pathname, so a
change after the final check can still alter what runs or where. Seatbelt
launches the approved spelling, which keeps invocation-name (`argv[0]`)
behavior, and that spelling is resolved again at launch; the result still runs
inside the profile. Session grants identify the command recipe, not file
identity, so a fresh plan may reuse a grant after a same-path update.

Concurrent same-UID host mutation remains an accepted residual. That covers
in-place rewrites, changes after validation, and changes to the scratch
reference or execution trees. Snapshot drift detection is not a point-in-time
coherence proof. Protection against these races requires a coordinated launch
redesign with [#484](https://github.com/kstruzzieri/go-llm/issues/484).

## Child capability and budget boundary (#449)

Dispatch selects only `read_file`, `search`, `glob`, `list` and optional
`retrieve`. Other parent tools, including write, exec, network, planning,
nested dispatch and MCP tools, are omitted. A selected canonical entry must
declare exactly Read, ApprovalNever and no PlanningTool interface; unsafe
selected entries fail construction. A child requesting an omitted tool receives
the ordinary unknown-tool denial. Installed implementations are trusted host
code: their Effect metadata must remain constant and truthful. This is not
process isolation, and the child's model transport and configured retrieval
backend can still use the network.

Scoped tasks retain #448's single pinned descendant directory. Native readers
must share one Workspace, and every read must satisfy both the caller's guard
and the selected subtree. Typed, sanitized denials disclose neither denied
contents nor private guard diagnostics. Legacy string tasks remain unscoped.
Separate tasks may select different subtrees; a single child spanning disjoint
roots is not implemented. Scoped retrieval remains excluded and belongs to
[#554](https://github.com/kstruzzieri/go-llm/issues/554). #552's filesystem
boundaries remain unchanged.

With the parent reporter enabled (always in Golem since #575), actual scoped native-reader refusals contribute
to its run-level risk report as described in
[interceptors and secret detection](golem.md#interceptors-and-secret-detection).
A refusal is counted when the reader returns it, even if the child observation
is later rejected, canceled or discarded. Reporting requires the dispatch
envelope to reach parent inspection under the existing runtime lifecycle.
Hard-aborted dispatches publish no evidence; a dispatch deadline may return a
partial envelope. Parent results rejected or discarded before inspection
contribute none. Once inspection runs, later interceptor, observer or governor
rejection does not remove its findings. Cancellation during result draining
follows the existing behavior and can still allow later inspection. This is not
a complete access audit: no envelope means no parent evidence. Native metadata
is excluded from JSON and conversation history; output truncation does not erase
it. Filesystem enforcement is unchanged.

Every nested `Orchestrator.Run` using its caller's context inherits that run's
effective input ceiling, fixed generation cap and configured step cap. Smaller
child settings remain smaller; child steps do not decrement the parent's loop
steps. An unset input ceiling resolves to 8,192, so even an explicit larger child
route is attenuated to an unset parent's 8,192 ceiling. A terse parent output cap
can shorten child summaries. If intersection leaves a child's output reserve at
or above its inherited input ceiling, the child fails with ErrContextExhausted
before any model call rather than silently shrinking its generation cap.

When configuring a research child behind a terse parent, size the parent's input
and generation caps for the child's work too. For example, a parent
OutputReserve of 64 also limits a child's 1,024-token summary to 64, even with a
separate dispatch route. Use prompting for shorter parent answers when children
need more output capacity. In Golem, review `-input-ceiling` and
`-output-reserve` alongside `-dispatch-role`: a larger child route cannot raise
inherited capacities, and the parent limits must still fit its own model.

Generation uses positive OutputReserve, otherwise positive Options.NumPredict,
otherwise the fixed chat default (2,048) when any finite total allowance applies,
then intersects with the parent's cap. Dispatch keeps its 1,024 generation
default, six-step cap and 32,768-token local allowance. The generation cap never
shrinks to fit remaining credits. The fallback sets NumPredict without creating
an extra OutputReserve subtraction; normal assembly and pressure reporting use
static capacity.

A positive TotalTokens applies to the run's own inference and all descendants.
After normal context assembly and pre-inference callbacks, the runtime atomically
reserves the checked prompt estimate plus fixed generation allowance against
every finite ancestor. Concurrent siblings and repeated dispatch calls cannot
reserve the same credits. A request that does not fit stops with BudgetReached
before Chat, without extra compaction or a shorter output cap.

Settlement happens immediately after Chat, including errors and cancellation.
Consistent decomposed usage on a successful single-attempt call may refund
unused generation, but its prompt charge is at least the assembled estimate.
Total-only, missing, invalid, failed or known multi-attempt reports retain at
least the full reservation and larger safely known usage. Routing error
sentinels alone do not prove non-execution. Reported usage above the
reservation is fully charged. A prompt count above the estimate is expected
estimator error and does not stop the run by itself. Output beyond the fixed
generation cap, or excess that cannot be attributed to the prompt, is an overrun:
it stops the current run before its returned tool calls execute; accepted answer
text survives unless a safety check or actual error takes precedence.
Tool invocation rechecks the allowance after callbacks and before queued work
starts. If a nested run exhausts it, later tools cannot execute; already admitted
invocations may finish. Returned transcripts omit unexecuted tool calls. A
restraint-governor stop recorded in the same batch keeps its stop reason, and a
cancellation keeps precedence over the budget stop.

`Result.Usage` remains the raw usage of that run's recorded steps.
`Result.DescendantUsage` separately snapshots valid reported descendant usage,
including known failed-call usage, once per model call. Neither field fabricates
reported tokens from admission estimates. Golem's footer and machine output keep
their existing meaning. A run seals its scope and cancels its context on return:
late nested admission fails even through context.WithoutCancel. Admitted calls
still settle, but returned Results do not change; hosts must join nested work
for complete telemetry.

TotalTokens zero adds no local allowance. **Golem currently supplies no finite
parent TotalTokens; #449 introduces no new aggregate Golem spend pool.** Its
configured product remains four dispatch invocations × four children × 32,768 =
524,288 admission credits, with per-child admission replacing the older
post-call-only checks. Standalone Dispatch.Invoke has independent child limits,
without an inferred parent or cross-invocation pool.

These are logical admission credits, not exact billing tokens. The default
prompt estimate is approximate, providers must honor generation caps and report
usage truthfully, and internal router retries/fallbacks do not expose complete
per-attempt usage. Direct Chat calls outside Orchestrator—such as delegate_code,
custom compactor inference or retrieval-internal inference—are not metered here.
All nested Runs in tools, observers and interceptors inherit the supplied
context; trusted host code can deliberately start an independent context.

## ZT-700: operational least privilege and observability

This workstream belongs to [#429](https://github.com/kstruzzieri/go-llm/issues/429). It connects existing controls and makes effective authority and decisions inspectable.

| Work | Priority | Provisional points | Scope and acceptance boundary |
|---|---|---|---|
| [#575](https://github.com/kstruzzieri/go-llm/issues/575) — default deterministic tool guards | P1 | 2–3 | Install argument invariants and egress labels by default. Specify compatibility for `-interceptors`, startup reporting and machine output; preserve the existing canary behavior. Invariants enforce argument denials; egress labels inform previews/telemetry and neither authorize calls nor prove they are offline. |
| [#576](https://github.com/kstruzzieri/go-llm/issues/576) — native sandbox selection | P1 | 5–8 | Wire explicit native-runtime selection through foreground/background exec, scratch execution and verification. Fail closed if required isolation is unavailable. Include runtime/network policy in verifier approval keys and test the CLI path. |
| [#577](https://github.com/kstruzzieri/go-llm/issues/577) — Agentflow environment allowlist | P1 | 2–3 | Shipped: Agentflow children get a documented baseline plus `-agentflow-env` names; sentinel secrets are proven absent from Agentflow and executed gates, in installed and source modes, by a pinned real-Agentflow CI job. |
| [#578](https://github.com/kstruzzieri/go-llm/issues/578) — MCP connection and environment boundaries | P1 | 5 | Shipped: stdio servers get a documented baseline plus `-mcp-env` names and run in the workspace root; HTTP endpoints are bound exactly, with every redirect refused, for handshake, discovery, calls and cleanup. Environment allowlists limit inherited-secret exposure, not filesystem or network authority; local MCP retains host-user authority without an explicitly selected sandbox from [#580](https://github.com/kstruzzieri/go-llm/issues/580). Pin identity is amended: each workspace/alias pin now also binds keyed fingerprints of the connection (launch or endpoint), checked before launch, and pins from earlier versions need one re-approval. |
| [#579](https://github.com/kstruzzieri/go-llm/issues/579) — per-server MCP tool selection | P1 | 3 | Exact tool allowlists affect advertisement and invocation. Preserve full-catalog drift checks and existing mandatory MCP approval. |
| [#580](https://github.com/kstruzzieri/go-llm/issues/580) — local MCP subprocess confinement | P2 | 8, provisional | Independently integrate a sandboxed stdio launch path with lifecycle cleanup. Validate feasibility before committing the implementation scope; CLI sandbox-selector availability is not a prerequisite. |

[#576](https://github.com/kstruzzieri/go-llm/issues/576) requires a documented contract amendment coordinated with [runtime profiles, #387](https://github.com/kstruzzieri/go-llm/issues/387): **Trusted remains host execution by default**, with an explicit optional native sandbox and honest effective-permissions reporting. It adds no fourth profile and does not redefine Preview's Docker isolation. Runtime selection must apply equally to verification; wiring exec tools alone is insufficient. Trusted names the operator's trust in host authority, not isolation; the effective-runtime display must say when execution is on the unsandboxed host. Changing that default requires native-toolchain compatibility evidence and a separate runtime-contract decision.

Existing work is reused, not refiled:

- [#420](https://github.com/kstruzzieri/go-llm/issues/420) owns narrower reusable grants. Coordinate path scope, changed-workspace tripwires, and any bounded lifetime/use-count extension with strict-mode suspension.
- [#406](https://github.com/kstruzzieri/go-llm/issues/406) owns MCP child lifecycle cleanup; coordinate it with [#578](https://github.com/kstruzzieri/go-llm/issues/578) and [#580](https://github.com/kstruzzieri/go-llm/issues/580). Environment filtering alone is not confinement.
- [#428](https://github.com/kstruzzieri/go-llm/issues/428) owns inbound MCP HTTP authentication. This is distinct from the outbound MCP-client boundary.
- [#449](https://github.com/kstruzzieri/go-llm/issues/449), [#552](https://github.com/kstruzzieri/go-llm/issues/552), and [#433](https://github.com/kstruzzieri/go-llm/issues/433) retain child capability attenuation, write/delete pathname hardening, and streamed ANSI sanitization respectively.
- [#517](https://github.com/kstruzzieri/go-llm/issues/517) retains the default-on decision for model-visible detectors and secret screening, with quality/false-positive evidence. [#575](https://github.com/kstruzzieri/go-llm/issues/575) is a separately scoped change; it does not silently change all interceptor defaults.
- [#424](https://github.com/kstruzzieri/go-llm/issues/424) remains open for its unmet exact-secret-literal requirement. Existing pattern detection and canaries do not make that requirement obsolete. Narrow the residual scope before sizing it.

Backlog consolidation closes [#425](https://github.com/kstruzzieri/go-llm/issues/425) in favor of shipped [#448](https://github.com/kstruzzieri/go-llm/issues/448) plus remaining caller-policy/ceiling decisions in [#449](https://github.com/kstruzzieri/go-llm/issues/449) and scoped retrieval in [#554](https://github.com/kstruzzieri/go-llm/issues/554). [#426](https://github.com/kstruzzieri/go-llm/issues/426) and [#427](https://github.com/kstruzzieri/go-llm/issues/427) transfer their complete remaining acceptance to [#452](https://github.com/kstruzzieri/go-llm/issues/452), reusing [#451](https://github.com/kstruzzieri/go-llm/issues/451). These closures transfer ownership; they do not claim all original work has shipped.

The observability cluster remains six individually scoped issues: [#519](https://github.com/kstruzzieri/go-llm/issues/519) interceptor findings in telemetry, [#518](https://github.com/kstruzzieri/go-llm/issues/518) headless risk/blocked outcomes, [#506](https://github.com/kstruzzieri/go-llm/issues/506) rejected-tool events, [#393](https://github.com/kstruzzieri/go-llm/issues/393) tool arguments in events, [#521](https://github.com/kstruzzieri/go-llm/issues/521) active interceptor-chain visibility, and [#412](https://github.com/kstruzzieri/go-llm/issues/412) grant labels. Each needs explicit field, sanitization and compatibility decisions. This is not a blanket promise that arbitrary content can be logged safely; size each issue after confirming its scope.

## ZT-800: provenance and approval continuity

[#581](https://github.com/kstruzzieri/go-llm/issues/581) tracks continuity of exposure information and the authority it affects. It does not label generated text as confidential or claim data-loss prevention.

| Work | Priority | Provisional points | Scope and acceptance boundary |
|---|---|---|---|
| [#582](https://github.com/kstruzzieri/go-llm/issues/582) — runtime exposure state | P2 | 3 | Host-assigned provenance/exposure state across ingress and child results. Model or tool text cannot promote itself to trusted. Define unknown/mixed inputs explicitly. |
| [#583](https://github.com/kstruzzieri/go-llm/issues/583) — persistence and summary continuity | P2 | 5–8 | Preserve exposure through saved/resumed sessions, compaction and summaries. Legacy records are unknown, not implicitly trusted. Define and test migration behavior. |
| [#584](https://github.com/kstruzzieri/go-llm/issues/584) — suspend reusable grants after exposure | P2 | 3–5 | After [#582](https://github.com/kstruzzieri/go-llm/issues/582) and [#583](https://github.com/kstruzzieri/go-llm/issues/583), strict mode requires fresh approval for all exec, verifier and write actions affected by foreign/unknown exposure. It must cover automatic edit grants and reusable exec/verifier grants; exec egress labels are not an enforcement classifier. MCP already requires approval. |
| [#585](https://github.com/kstruzzieri/go-llm/issues/585) — source-to-destination policy discovery | P3, deferred | Unestimated | Start discovery when a concrete confidentiality requirement or demonstrated residual gap justifies it. Split any proposed implementation before sizing; no export-control feature is committed here. |

Strict-mode rollout must include runtime and persisted continuity together. Otherwise resume or compaction could clear the signal and permit grant reuse or creation without the inherited restriction. Grants themselves remain ephemeral. Initial opt-in behavior, default changes, and compatibility require explicit acceptance criteria and evidence; there is no scheduled automatic default flip.

The planned exposure trigger is admitted foreign or unknown content under a host-owned source policy, not every tool observation. Workspace and command output are not automatically foreign, and workspace origin is not proof that content is safe. [#581](https://github.com/kstruzzieri/go-llm/issues/581)/[#584](https://github.com/kstruzzieri/go-llm/issues/584) must specify source classification, expected approval prompts, and a representative workflow evaluation before strict-mode implementation; prompt counts and task completion must be measured for clean edit/test loops, foreign reads, and legacy versus metadata-aware resume.

The planned first version has no in-place declassification or automatic exposure expiry. Compaction, quarantine summaries, one-time approvals, and `/grants clear` do not clear exposure. A successful new/clear operation starts a fresh active conversation context and resets its prior exposure; reintroduced content is classified again. Resume restores valid stored exposure, while missing or invalid legacy metadata is unknown. Resuming a clean conversation with valid metadata does not itself mark it foreign. Strict mode remains opt-in; a future declassification mechanism needs its own scoped design and evidence.

Reuse [#434](https://github.com/kstruzzieri/go-llm/issues/434) quarantined ingestion, [#435](https://github.com/kstruzzieri/go-llm/issues/435) retrieval screening, [#471](https://github.com/kstruzzieri/go-llm/issues/471) memory-promotion approval, and [#452](https://github.com/kstruzzieri/go-llm/issues/452) adversarial regression cases. Compound cases should include edit-then-granted-exec, foreign read-then-send, summary/resume continuity, memory reuse, and background execution. Assertions must test containment even when the simulated model follows an injected instruction.

## Dependency order and release scope

1. Start the bounded operational work: [#575](https://github.com/kstruzzieri/go-llm/issues/575), [#577](https://github.com/kstruzzieri/go-llm/issues/577), [#578](https://github.com/kstruzzieri/go-llm/issues/578), and [#579](https://github.com/kstruzzieri/go-llm/issues/579). Coordinate [#576](https://github.com/kstruzzieri/go-llm/issues/576) with [#387](https://github.com/kstruzzieri/go-llm/issues/387)'s contract/reporting and verification approval identity; preserve existing [#449](https://github.com/kstruzzieri/go-llm/issues/449)/[#552](https://github.com/kstruzzieri/go-llm/issues/552)/[#433](https://github.com/kstruzzieri/go-llm/issues/433) hardening scope.
2. Narrow grants with [#420](https://github.com/kstruzzieri/go-llm/issues/420) and complete the individually sized observability issues. Develop [#580](https://github.com/kstruzzieri/go-llm/issues/580) against its own stdio launch seam, coordinated with [#578](https://github.com/kstruzzieri/go-llm/issues/578) and [#406](https://github.com/kstruzzieri/go-llm/issues/406).
3. Implement [#582](https://github.com/kstruzzieri/go-llm/issues/582), then [#583](https://github.com/kstruzzieri/go-llm/issues/583). Enable [#584](https://github.com/kstruzzieri/go-llm/issues/584) only with both continuity layers and regression evidence. Feed remaining demonstrated gaps into deferred [#585](https://github.com/kstruzzieri/go-llm/issues/585) discovery.

Points are provisional relative complexity, not engineer-day conversions. There is no inferred velocity, delivery date or fixed aggregate commitment. The [release roadmap](../README.md#roadmap) records milestone scope; new work here has no release commitment until explicitly assigned. Priority is not a release assignment; placement is a separate capacity decision. The 2026-09-30 re-plan assigned the step 1 work ([#575](https://github.com/kstruzzieri/go-llm/issues/575), [#577](https://github.com/kstruzzieri/go-llm/issues/577), [#578](https://github.com/kstruzzieri/go-llm/issues/578), [#579](https://github.com/kstruzzieri/go-llm/issues/579), with [#576](https://github.com/kstruzzieri/go-llm/issues/576) as a stretch item) to v0.5.0 and [#582](https://github.com/kstruzzieri/go-llm/issues/582)–[#584](https://github.com/kstruzzieri/go-llm/issues/584) to the v0.6.0 outline.

A policy service, JIT credential broker or sender-constrained OAuth is deferred until a concrete consumer/integration needs it and supports the corresponding upstream scopes and token lifecycle. Reuse the current deterministic enforcement seams first. Local wrappers cannot manufacture upstream authorization restrictions.
