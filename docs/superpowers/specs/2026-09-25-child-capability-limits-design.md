# Child capability and budget limits (#449)

Status: proposed for review; implementation has not started.

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
   host code. This filter is not process isolation and cannot constrain a Go
   implementation that lies about its effect. The selected retrieve adapter's
   configured backend remains trusted. The child's model transport is still
   network-capable; the restriction applies to child tool authority.
4. Scoped calls still require native readers sharing one Workspace. Do not wrap
   or replace those readers in a way that defeats native scope construction.

## Budget contract

### Limits and lifetime

- Normalize the dispatch's existing defaults, then intersect child input
  ceiling, finite output cap and step cap with the active parent's effective
  limits. A smaller explicitly configured child limit remains smaller.
- InputCeiling zero continues to mean the existing runtime default (8,192).
  A positive OutputReserve is authoritative; otherwise a positive
  Options.NumPredict supplies the parent's finite generation cap. No finite
  parent generation cap is inferred from a zero value.
- TotalTokens zero remains unbounded for a top-level Request. A child retains
  its dispatch default of 32,768 when no smaller explicit child cap is set,
  and must also fit every finite ancestor balance. Do not silently introduce
  an aggregate Golem token cap where the caller supplied none.
- Unbounded top-level inference keeps its existing assembly/generation
  behavior. It still publishes the parent limits and collects descendant usage;
  reservations apply when the local run or an ancestor has a finite total.
- A top-level Run owns the budget lifetime. All dispatch invocations and
  sibling children within it share that balance. Reusing a dispatcher for a
  later or concurrent independent top-level Run does not share balances.
- Direct `Dispatch.Invoke` without a parent Run keeps standalone child behavior
  and its local caps; it cannot infer an absent parent. Child tokens consume
  the parent total, but child step counts do not consume the parent's loop
  steps. Existing task, invocation, slot, timeout and result caps remain.

### Admission and settlement

1. Before each inference turn, atomically reserve at most the effective input
   ceiling, reduced by the available balance of every finite ancestor and the
   local run. No reservation may spend credits held by another active turn.
2. Use that allowance as the effective turn ceiling. Reserve space for a
   finite configured answer; if no answer limit exists, leave at least one
   token for output during assembly and cap generation to the remaining
   allowance after the assembled prompt estimate. Configured output limits may
   be lowered to fit the allowance and must never be raised.
3. Assemble with the existing ContextManager and include its schema, framing
   and advisory charges. An allowance too small for mandatory input plus any
   output produces BudgetReached before ModelCaller.Chat. Release reservations
   on paths that never call the model. Ordinary context exhaustion when the
   static input ceiling is insufficient retains ErrContextExhausted.
4. Once Chat starts, settlement is guaranteed on success, provider error,
   callback error and cancellation. Successful requests with usable reported
   usage return unused reserved credits. Missing, invalid or incomplete usage,
   and failed requests, retain at least the full reservation; failure must not
   create refundable credit. Use checked/saturating arithmetic.
   A positive total-only report is usable for compatibility with ModelCaller
   implementations. Otherwise use at least the nonnegative sum of prompt and
   completion tokens; negative fields or overflow are invalid and cannot
   manufacture credit. Do not propagate negative usage into parent aggregates.
5. Charge a report above the reservation in full, stop admitting further calls
   against an exhausted balance, and surface the overrun as a provider/budget
   failure. Already consumed provider tokens cannot be undone. Do not invoke
   model-selected tools from a response that exhausted or violated the run's
   allowance.
6. Parent Result.Usage includes known provider usage from descendants once,
   including usage returned with errors. Conservative reservation charges are
   accounting state, not fabricated provider Usage. Step records stay local to
   their run. BudgetReached can therefore occur with less reported Usage than
   the configured cap when usage is unknown.

Admission is conservative: if other in-flight reservations leave too little
capacity, a child may stop for budget rather than wait for a speculative refund.
This keeps cancellation and concurrency simple; no scheduling fairness promise
is introduced.

### Exact guarantee and trust boundary

For compliant usage and model transport, the ledger enforces
`charged + in-flight reservations <= each finite ancestor cap` at admission.
Child per-turn limits never exceed the parent limits, including after default
normalization. Concurrent and repeated dispatch cannot multiply available credit.

Input fitting currently uses an estimator (len/4 by default), not the selected
model's exact tokenizer. Generation limits rely on the ModelCaller/provider
honoring NumPredict, and settlement relies on accurate usage. Consequently this
is strict runtime admission/accounting, not an unconditional upper bound on
provider-billed tokens. Documentation and the PR must state that distinction.
Host callbacks, custom compactors and trusted tool-internal inference that call
a provider outside Orchestrator.Chat are not newly metered by this change.
An exact billing-token guarantee would require a separately scoped tokenizer
and transport contract; do not claim it shipped in #449.

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
  when mandatory input cannot fit remaining total credits.
- Concurrent siblings, repeated dispatch calls and multiple dispatch calls in
  one model response; aggregate admission and exactly-once parent usage.
- Reused dispatcher across independent concurrent parent runs, with no budget
  contamination; standalone invocation and unbounded-parent compatibility.
- Cancellation before admission, during inference and while queued; provider
  errors with and without usage; zero/negative/inconsistent/overflowing usage;
  a provider report above its reservation; no returned credit from a failed
  call. Preserve per-child completion and permit-release behavior.
- Both context assembly modes and a tiny total limit; original scope tests
  continue to exercise typed guard denials, traversal/symlinks, pinned roots,
  independent tasks and cleanup.

Write failing regressions for new behavior before implementation. Update tests
that intentionally asserted the old post-call overshoot behavior to assert the
new admission contract, rather than loosening them to permit either outcome.

Final checks:

```sh
rtk go test ./agent/tools -run 'Test(NewDispatch|Dispatch|Scoped)' -count=1
rtk go test ./cmd/golem -run 'Test(NewDispatchTool|OrchestratorFactory_Dispatch|RebuildDispatch)' -count=1
rtk go test -race ./agent ./agent/tools ./cmd/golem
rtk docker compose -p go-llm-449 -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

Run new regression names explicitly when selectors do not include them. Review
the whole diff and add `changelog.d/449-child-capability-limits.md`; never edit
CHANGELOG.md. Document the delivered contract in `docs/least-privilege.md`.
The existing pre-push hook uses shared project `go-llm` without `--build`; do not
change it or shared Docker configuration to address #564 here. Recheck develop
before publishing and rerun affected checks after any update. Attach the PR to
this task. No merge, tag or release.
