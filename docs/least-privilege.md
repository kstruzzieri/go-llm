# Least privilege roadmap

This roadmap extends the [zero-trust umbrella, #429](https://github.com/kstruzzieri/go-llm/issues/429), with operational controls and provenance-aware approval continuity. It separates the implementation baseline assessed on 2026-09-20 from planned work. Issue acceptance criteria own implementation details; this document records scope and dependencies.

The [Mnemoverse article on least privilege for AI agents](https://mnemoverse.com/docs/library/least-privilege-ai-agents) motivates the action boundary: restrict tools, resources, destinations and authority outside the model. An authorized read followed by an authorized send can still disclose data. The provenance work below adds approval evidence; it does not claim to infer intent or provide source-to-destination information-flow enforcement.

## Current boundaries

| Area | Shipped behavior | Limit |
|---|---|---|
| Tool execution | Read-only CLI default; explicit tool mounting; shared preparation, validation and approval before invocation. | Opting into exec permits host execution. Sanitized command environments do not restrict filesystem or network access. |
| Native sandboxes | Library Seatbelt and Bubblewrap backends fail closed when explicitly selected but unavailable. Sandbox policy participates in exec approval identity. | CLI exec and verification do not yet select these backends. |
| Interceptors | Optional deterministic injection/secret detectors, argument invariants and exec egress labels. Observation fencing is independent. | `-interceptors` is off by default. Labels and risk scores do not constrain network access or suspend grants. |
| Provider destinations | Model-provider requests made by config-driven Golem and the go-llm MCP server require admission for remote destinations; guarded transports check capabilities, origins and base paths and refuse redirects. Grants are revocable. | This provider boundary does not govern Golem's connections to external MCP tool servers, shell traffic, or consultant-process traffic. |
| MCP client | Workspace/alias catalog pins detect definition drift; tools require approval, have bounded execution/output, and produce foreign observations. | Pins do not attest endpoint/process identity. All admitted catalog tools are mounted. Local stdio servers run with host-user authority and inherit the parent environment; their HTTP counterparts are outside provider admission. |
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

Every nested `Orchestrator.Run` using its caller's context inherits that run's
effective input ceiling, fixed generation cap and configured step cap. Smaller
child settings remain smaller; child steps do not decrement the parent's loop
steps. An unset input ceiling resolves to 8,192, so even an explicit larger child
route is attenuated to an unset parent's 8,192 ceiling. A terse parent output cap
can shorten child summaries.

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
sentinels alone do not prove non-execution. An overrun is fully charged and stops
the current run before its returned tool calls execute; accepted answer text
survives unless a safety check or actual error takes precedence.

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
| [#577](https://github.com/kstruzzieri/go-llm/issues/577) — Agentflow environment allowlist | P1 | 2–3 | Replace ambient environment inheritance with a documented baseline plus explicit runner variables; prove unrelated sentinel secrets do not reach the child. |
| [#578](https://github.com/kstruzzieri/go-llm/issues/578) — MCP connection and environment boundaries | P1 | 5 | Explicit stdio environment allowlists and HTTP endpoint binding/redirect refusal for handshake, discovery, calls and cleanup. Environment allowlists limit inherited-secret exposure, not filesystem or network authority; local MCP retains host-user authority without an explicitly selected sandbox from [#580](https://github.com/kstruzzieri/go-llm/issues/580). Catalog pin identity remains workspace/alias/catalog unless explicitly amended. |
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

Points are provisional relative complexity, not engineer-day conversions. There is no inferred velocity, delivery date or fixed aggregate commitment. The existing [v0.4.0 scope](../README.md#roadmap) remains unchanged; new work has no v0.4.0 or v0.5.0 commitment. In particular, P1 [#575](https://github.com/kstruzzieri/go-llm/issues/575) is unmilestoned: priority is not a release assignment. Milestone placement requires a separate capacity decision.

A policy service, JIT credential broker or sender-constrained OAuth is deferred until a concrete consumer/integration needs it and supports the corresponding upstream scopes and token lifecycle. Reuse the current deterministic enforcement seams first. Local wrappers cannot manufacture upstream authorization restrictions.
