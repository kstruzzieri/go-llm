# Child capability and budget limits (#449)

Status: revised after code-grounded review and approved by the user on 2026-09-25.
Implementation has not started. This revision supersedes the budget contract in
`93f1622`.

## Goal and baseline

Complete the amended [#449](https://github.com/kstruzzieri/go-llm/issues/449):
children receive only the existing read-only registry and cannot enlarge their
parent's token allowances. Preserve #448's optional single-subtree contract and
#552's filesystem boundaries. Prepare a reviewed PR to `develop`; do not merge.

Isolated worktree:
`/Users/keith.struzzieri/.codex/worktrees/449-child-capability-limits/go-llm`.
Branch: `feat/449-child-capability-limits`.
Fetched base: `099007ce4c761fa46f413ee0a26fbeed9f2336d0`, containing #587
(terminal sanitization) and #588 (workspace mutation hardening).

The primary checkout and other worktrees remain untouched. #549 owns the
feedback/fingerprint/provider migrations; #542 owns conversation CAS.

## Gap analysis

- `NewDispatch` already selects `read_file`, `search`, `glob`, `list`, and
  optional `retrieve`; rejects selected tools whose effect is not exactly
  Read/ApprovalNever; rejects PlanningTool; omits every other parent tool.
  Keep this boundary and expand its behavioral regression coverage.
- `childTools` already rebuilds native readers against a pinned descendant,
  excludes retrieval, snapshots the caller guard, and translates child paths
  into that guard. Existing tests exercise real denied observations, guard
  composition, symlinks, independent counters, cancellation and cleanup.
- Child input/output settings are supplied at dispatch construction. They are
  not intersected with the active parent's request. A separately configured
  child route can therefore receive a larger input ceiling than its parent.
- Each child has an independent default 32,768-token run allowance. Parent
  usage excludes child inference. Concurrent tasks and repeated dispatch calls
  can consume the same apparent parent balance.
- `Orchestrator` currently checks TotalTokens after a successful model response
  and tool execution. It neither reserves before inference nor charges failed
  requests with unknown usage. Fixing only Golem constructor values would leave
  these runtime gaps and library callers unchanged.

Baseline: 885 agent tests, 109 dispatch/scope tests and 16 Golem wiring tests
passed.

## Chosen approach

Use one internal per-run budget tree carried through the existing context.
Nested `Orchestrator.Run` calls inherit their parent's node; independent top-level
runs create independent trees. A tree uses one mutex for atomic admission and
settlement, without holding it across assembly, callbacks, tools or inference.
No global pool, new dependency, public capability framework or model-facing
budget argument is needed.

A static child-budget clamp alone cannot cover concurrency or repeated calls.
Reserving a child's entire multi-turn allowance at launch would unnecessarily
starve siblings; instead reserve one inference turn at a time.

## Review disposition

| Finding | Decision and verified basis |
|---|---|
| P1-1: reserve before assembly | Accept the correction. Assemble under static capacity, then reserve the assembled prompt estimate plus a fixed generation cap. No spend-driven compaction, pressure drift or counterfactual context-error classification. |
| P1-2: generation cap changes with balance | Accept fit-or-stop and a fixed default for otherwise uncapped finite runs. Both model callers forward NumPredict as ExpectedOutput. Qualify the routing claim: StickyKey includes output class, but nonempty PreferredChain suppresses sticky routing, so lost sticky affinity is not a universal Golem consequence. |
| P2-3: descendant usage changes Usage | Add DescendantUsage; preserve local recorded-step Usage and current Golem rendering. Charge all calls in the ledger independently of those result fields. |
| P2-4: refund routing sentinels | Do not use the proposed sentinel/empty-outcome/empty-stream heuristic. ExecuteChatStream can execute a primary, fail its fallback admission, and return bare ErrRouterClosed without emitting a chunk. It publishes RouteOutcome to the caller only on terminal Done chunks. Defer refunds until there is explicit pre-execution evidence. |
| P2-5: Ollama counts uncached prompt only | Not true of the current documented API: it has separate prompt and cached-prompt counts, and its throughput calculation subtracts cached from prompt. Choose logical admission credits with a sent-prompt estimate floor regardless of backend/version. |
| P2-6: Golem has no finite parent total | Document the limitation. Do not add a new arbitrary dispatch pool or present capacity intersection as a finite aggregate allowance. |
| P2-7: capacity versus privilege | Keep the requested intersection and explicitly document/test the 8,192 parent default, larger child routes and terse parent output caps. |
| P3-1: inherited context and late goroutines | Document all nested Run callers, trusted-host escape and direct Chat exclusions; seal nodes/tree on return and deny late admissions. |
| P3-2: overrun outcome | BudgetReached with accepted text retained and no tools, unless a real provider/callback/security/cancellation error takes precedence. |
| P3-3: concurrency coverage | Multiple dispatch calls in one response test sequential accounting; race tests exercise siblings and a shared dispatcher in independent concurrent runs. |
| P3-4: step cap | Minimum of child and parent's configured effective step cap, not remaining steps. |
| P3-5: settlement placement | Immediately after Chat returns, before any output processing or observer error path; explicitly test OnToken, OnStep and interceptor failures. |
| P3-6: changing effects | Require constant Effect metadata; retain concrete native readers and their optional interfaces. |
| P3-7: split Go toolchain | GOROOT is unset in this Codex environment; `env -u GOROOT go env` resolves go1.27.1 from the toolchain module. Use env -u GOROOT explicitly in host checks for portability. |

Local evidence: `agent/model_caller.go`, `cmd/golem/modelcaller.go`,
`provider/routing_request.go`, `provider/route_plan.go`,
`provider/route_plan_admission_test.go`, `agent/types.go`,
`agent/dispatch_parallel.go`, `agent/tools/delegate.go`,
`cmd/golem/input_ceiling.go`, `cmd/golem/model.go` and `cmd/golem/main.go`,
inspected at base `099007c`. External Ollama evidence is pinned below.

## Registry contract

1. Keep the five canonical names and current required/optional split. Arbitrary
   additional read tools are outside this ticket. Legitimate parent registries
   may contain write, exec, network, planning, dispatch and MCP tools: those
   entries are omitted rather than making the parent registry unusable.
2. An unsafe entry under a selected name is a constructor error, including
   mixed Read|Write, Read|Exec or Read|Network effects, approval-requiring
   entries, and PlanningTool implementations. Preserve duplicate/missing/nil
   checks. A child that nevertheless requests an omitted name receives the
   existing unknown-tool result; the host tool is never invoked.
3. Installed tool implementations and stable, truthful metadata remain trusted
   host code. `Effect()` must be constant after construction. This filter is
   not process isolation and cannot constrain a Go implementation that lies
   about its effect. The selected retrieve adapter's
   configured backend remains trusted. The child's model transport is still
   network-capable; the restriction applies to child tool authority.
4. Scoped calls still require native readers sharing one Workspace. Do not wrap
   or replace those readers in a way that defeats native scope construction.

## Budget contract

### Capacity, allowance and fixed generation cap

InputCeiling and OutputReserve are per-turn capacity settings. TotalTokens is
the spend allowance; capacity intersection alone cannot bound aggregate spend.
Normalize child defaults once, then intersect the child input ceiling, finite
generation cap and step cap with the parent's effective limits. Keep smaller
child limits. The step cap is `min(child, parent's configured effective MaxSteps)`,
not the parent's remaining steps; zero MaxSteps resolves to the runtime default.

InputCeiling zero still resolves to 8,192. Thus an unset library parent ceiling
attenuates a larger explicit child ceiling to 8,192. A terse parent's positive
OutputReserve can shorten child summaries. A Golem child route with a larger
context window than the parent is also attenuated. These capacity changes must
appear in documentation, tests and the changelog, without presenting them as a
new aggregate spending limit.

Resolve generation once per Run: positive OutputReserve, otherwise positive
Options.NumPredict, otherwise `provider.DefaultExpectedOutput("chat")` (currently
2,048) when any finite total applies. Intersect with the parent's finite cap.
Dispatch's existing positive default of 1,024 remains its default. The resulting
cap is stable across turns: fit the whole request or stop; never shrink a cap
because the balance is low. A finite run cannot retain an unlimited NumPredict.

Keep the existing static turnBudget semantics, including configured
OutputReserve subtraction. A generation default chosen only for total-token
admission is a fixed NumPredict; do not also subtract it from InputCeiling as a
new OutputReserve. Golem already accounts for its implicit router output
reservation when deriving a ceiling. Remaining spend never changes assembly,
compaction thresholds or PressureEvent.InputBudget.

### Lifetime and Golem scope

- TotalTokens zero remains unbounded for a top-level Request. A dispatch child
  retains its default 32,768-token allowance unless explicitly configured
  otherwise, while also fitting every finite ancestor balance.
- All nested Orchestrator.Run calls using the supplied context inherit this
  tree, including calls from host tools, observers and interceptors. This is
  a library-wide context contract, not a dispatch-name special case. Host code
  starting from context.Background can intentionally escape inheritance;
  installed code is trusted and models cannot choose the context.
- Repeated dispatch calls share the top-level Run's allowance. Independent
  concurrent top-level runs have separate trees even when they reuse the same
  dispatcher. Direct Dispatch.Invoke without an enclosing Run has only its
  children's local caps; it cannot infer a parent or shared cross-call pool.
- On Run return, seal its node before publishing the final usage snapshot.
  Descendant admission checks every ancestor for closure; a goroutine that
  outlives any enclosing Run must fail closed. Seal the whole tree when the
  root returns and cancel the run-derived context. Already admitted calls
  still settle; a returned Result is an immutable snapshot, not a live counter.
  Hosts must join nested work to obtain complete descendant telemetry.
- Unbounded top-level inference retains current generation and assembly
  behavior. The tree still carries capacity limits and descendant telemetry.
  Child steps do not consume the parent's loop steps. Existing dispatch task,
  invocation, slot, timeout and result caps remain independent controls.

**Golem currently supplies no parent TotalTokens. Do not add an arbitrary
per-run dispatch pool in #449, or claim a new finite parent spend bound for
Golem.** The configured product remains four dispatch invocations times four
children times 32,768 tokens: 524,288 admission credits under the proposed
enforcement, subject to the estimator/provider qualifications below. Existing
post-call checks allow overshoot; the new per-child admission improves that
behavior without introducing a smaller aggregate pool. Library users
who supply a finite parent allowance get shared enforcement across parent and
children. A finite Golem pool or operator budget setting is a separate product
decision; it is not implied by this change or claimed implemented.

### Assemble, then reserve

1. Assemble with the effective static ContextManager budget. Preserve normal
   pressure reporting and all existing observer/interceptor checks before
   inference. Assembly failures retain their existing classification, including
   ErrContextExhausted. Do not retry assembly under a smaller spend balance.
2. Price the assembled prompt with the manager's checked accounting, including
   tool schemas, observation frames and advisory text. Call that estimate E;
   call the fixed generation cap G. Checked `need = E + G` is the reservation.
   Invalid estimates or arithmetic overflow fail closed without Chat.
3. Immediately before Chat, atomically reserve exactly need against the local
   node and every finite ancestor. Check cancellation and sealed nodes. If it
   does not fit, return BudgetReached without Chat. Do not wait for possible
   refunds, trim the output cap or evict more evidence. No user callbacks or
   other fallible work belong between successful admission and Chat.
4. Settle immediately after Chat returns, before inspectOutput, OnStep, partial
   stream processing or any early return. Every successful reservation settles
   exactly once, including OnToken errors, provider errors and cancellation.
   Pre-admission observer/interceptor errors hold no reservation.

For example, four prompts estimated at 1,000 tokens with G=1,024 reserve 8,096
credits in total, irrespective of whether a child's context ceiling is 8k or
100k. The final turn stops if its whole request does not fit. Existing partial
evidence fallback remains; a tools-disabled finalization turn is outside #449.

### Logical admission credits and settlement

The ledger counts logical prompt estimates plus generation, not just tokens
freshly computed after a cache hit. Keep the accounting unit separate from raw
provider telemetry and from an exact tokenizer or billing-token claim.

- For successful calls with usable decomposed usage, charge at least
  `max(reported.TotalTokens, max(E, reported.PromptTokens) + reported.CompletionTokens)`.
  A cache-related lower prompt report cannot refund the sent-prompt floor E.
  Completion counts already include any reported reasoning subset; do not
  add ReasoningTokens a second time.
- Negative fields, overflowing arithmetic, contradictory totals and missing
  usage cannot create credit. A total-only report is accepted for compatibility
  but cannot separate input from output, so it does not establish a refund.
  For incomplete/invalid usage, failures and known multi-attempt routed calls,
  retain at least the full reservation and any larger safely known usage.
  Decomposed usage is usable when nonnegative, with positive prompt count and
  a total that is zero (derive from the components) or equals their sum.
- No sentinel-based failure refunds in this version. ErrRouterClosed can follow
  a real primary attempt and a denied fallback while both RouteOutcome and the
  chunk stream are empty. Other arbitrary ModelCallers likewise provide no
  proof of non-execution. A future refund needs explicit evidence from the
  pre-execution boundary, not errors.Is plus absent telemetry.
- Charge reported overages in full. Exhaustion or an overrun stops inference
  and tool execution with `StopReason=BudgetReached` and nil error when Chat and
  subsequent safety/observer checks succeed. Preserve accepted answer text;
  do not run its tool calls. A security block, callback error, provider error
  or cancellation keeps its existing error precedence and disclosure rules.
- Budget decisions read the ledger, never Result.Usage. A successful call may
  refund unused generation credits only after the logical prompt floor and
  failure/usage checks above. Once an ancestor is exhausted or sealed, further
  admission fails; a child cannot borrow its sibling's in-flight reservation.

For Ollama specifically, the current [chat API](https://docs.ollama.com/api/chat)
describes prompt_eval_count as the prompt count and separately exposes
prompt_eval_cached_count. The [current API implementation at 7af3931](https://github.com/ollama/ollama/blob/7af393188defd52d370464de0d2064649cab9b41/api/types.go#L1005)
subtracts the cached count to compute uncached throughput. This contradicts the
review's uncached-only premise for the current API; it does not establish the
semantics of every old server or third-party compatible backend. The E floor
keeps this design independent of that variation. No Ollama adapter change is
part of this ticket.

### Result compatibility

Keep Result.Usage local to the run's recorded model steps, preserving
`Usage == sum(Steps[i].Response.Usage)` for PromptTokens, CompletionTokens and
TotalTokens, leaving
Golem's footer meaning unchanged. Failed Chat usage is charged by the ledger
even when the existing partial-error path records no StepRecord; do not quietly
add it to Usage and break that invariant.

Add `Result.DescendantUsage *provider.Usage` with
`json:"DescendantUsage,omitempty"`, matching existing Result field casing.
It sums validated reported usage from descendants, including known usage on
failed calls, exactly once; do not re-add a child's aggregate when it returns.
Keep it nil when no descendant reports valid nonnegative nonzero usage; a
total-only report is still telemetry even though it cannot justify a refund.
Aggregate the same three counters as Usage, without adding a new reasoning-token
aggregation policy. Copy the snapshot
on return. The ledger's prompt floors and unknown-usage charges are not inserted
into either telemetry field. BudgetReached may occur below the configured cap
in reported telemetry. Do not change Golem's machine-output schema or footer
to combine these fields in this ticket.

### Exact guarantee and trust boundary

At admission, charged credits plus in-flight reservations must fit each finite
ancestor allowance. Concurrent and repeated dispatch cannot multiply credits.
An unexpected report over the reservation is fully charged and stops the run;
settlement is not claimed to prevent an already consumed overage.

Input fitting remains estimated (len/4 by default), generation limits require a
compliant ModelCaller/provider, and reported usage must be truthful. RoutePlan
can retry/fall back within a single Chat and does not expose complete per-attempt
usage to this ledger; retaining a full reservation when multiple attempts are
known is conservative accounting, not a proof of their combined actual cost.
This is strict runtime admission, not an unconditional provider-billing bound.
Exact tokenization and per-attempt transport admission require separate scope.

Direct ModelCaller.Chat calls outside Orchestrator are not metered, including
delegate_code, custom compactor inference and tool-internal retrieval inference.
No new claim of process isolation, token billing enforcement or protection
against malicious installed Go code is made.

## Scope contract

Keep a single existing descendant directory per scoped task. The caller guard
and the model-selected scope must both allow a read; typed denials and sanitized
tool output must reveal no denied contents or private guard diagnostics.
Legacy string tasks remain unscoped read-only tasks. Multiple independent tasks
may already choose different individual scopes; one child spanning several
disjoint roots is not implemented.

No demonstrated requirement needs a multi-subtree extension for this ticket.
Record that decision in delivered documentation. Any future disjoint-root
request needs separate discovery of native-reader composition, guard mapping
and containment tests. Scoped semantic retrieval remains #554; runtime profiles
remain #387. Do not alter `paths*` or implement the newer least-privilege roadmap.

## Files and focused verification

Implementation ownership: an internal budget helper and focused tests in
`agent/`, integration in `agent/orchestrator.go`, contract comments in
`agent/types.go`, dispatch tests and any narrowly necessary dispatch adjustment
in `agent/tools/dispatch.go`. Exercise Golem startup, explicit child routes and
`/model` rebuilding through `cmd/golem/dispatch_wiring_test.go`; constructor
changes are needed only if runtime inheritance leaves a proven gap.

Behavioral regressions must cover:

- Selected write/exec/network/mixed/approval/planning tools rejected; full
  parent registries accepted; adversarial child calls cannot invoke omitted
  tools. Preserve optional retrieval and real scoped reader behavior.
- Child defaults under smaller parent input/output/step limits; smaller child
  limits retained; finite NumPredict with zero OutputReserve; no model call
  when the full prompt-plus-generation reservation cannot fit remaining credits.
  Pin unchanged static pressure/compaction and generation cap near exhaustion,
  including the fixed 2,048 fallback and the 1,024 dispatch default.
- Concurrent siblings whose combined request costs fit despite much larger
  context ceilings; repeated dispatch calls; sequential multiple dispatch calls
  in one model response; aggregate admission and exactly-once descendant usage.
  Do not label same-response dispatch calls concurrent: Read|Network is not
  parallelSafe and Golem's invocation cap also serializes them.
- Reused dispatcher across independent concurrent parent runs, with no budget
  contamination; standalone invocation and unbounded-parent compatibility;
  nested Run calls in a host callback; sealed-tree late admission and immutable
  returned usage snapshots.
- Cancellation before admission, during inference and while queued; provider
  errors with and without usage; zero/negative/inconsistent/overflowing usage;
  a provider report above its reservation; no returned credit from a failed
  call, including a sentinel with no chunks/outcome after a prior provider
  attempt. Preserve per-child completion and permit-release behavior.
- OnToken/OnStep failures and an output-interceptor block cannot skip or repeat
  settlement. Accepted overrun text survives with BudgetReached and no invoked
  tools; blocked content stays blocked. Result.Usage retains its recorded-step
  sum, DescendantUsage omits absent telemetry, and logical accounting floors
  never appear as fabricated reported usage. Test a cached/underreported prompt,
  total-only usage and known fallback attempts.
- Both context assembly modes and a tiny total limit; original scope tests
  continue to exercise typed guard denials, traversal/symlinks, pinned roots,
  independent tasks and cleanup.

Write failing regressions for new behavior before implementation. Update tests
that intentionally asserted the old post-call overshoot behavior to assert the
new admission contract, rather than loosening them to permit either outcome.

Final checks:

```sh
rtk proxy env -u GOROOT go test ./agent/tools -run 'Test(NewDispatch|Dispatch|Scoped)' -count=1
rtk proxy env -u GOROOT go test ./cmd/golem -run 'Test(NewDispatchTool|OrchestratorFactory_Dispatch|ModelSet.*Dispatch|ModelSetAfterAllowWrite)' -count=1
rtk proxy env -u GOROOT go test -race ./agent ./agent/tools ./cmd/golem
rtk docker compose -p go-llm-449 -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

Run new regression names explicitly when selectors do not include them. Review
the whole diff and add `changelog.d/449-child-capability-limits.md`; never edit
CHANGELOG.md. Document the delivered contract in `docs/least-privilege.md`.
The existing pre-push hook uses shared project `go-llm` without `--build`; do not
change it or shared Docker configuration to address #564 here. Recheck develop
before publishing and rerun affected checks after any update. Attach the PR to
this task. No merge, tag or release.

## Review validation (2026-09-25)

This is a design-only revision; the new enforcement regressions are still to
be written during implementation. `git diff --check` passed. The following
existing behavior checks passed in all three packages with GOROOT cleared:

```sh
rtk proxy env -u GOROOT go test ./agent/tools ./cmd/golem ./provider -run 'Test(NewDispatch|Dispatch|Scoped|OrchestratorFactory_Dispatch|RebuildDispatch|FallbackAdmissionFailureIsTerminalButKeepsPriorAttempts|FallbackAdmissionCancelRecordsNoWarmthForPriorProvider)' -count=1
```
