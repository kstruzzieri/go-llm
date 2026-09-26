# Child capability and budget limits (#449) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for native execution, or superpowers:subagent-driven-development if the user selects delegation. Implement task-by-task. Steps use checkbox syntax for tracking.

**Goal:** Attenuate nested runs to their parent's capacities and atomically enforce finite token allowances across the run tree.

**Architecture:** Keep the existing dispatch registry and single-subtree enforcement. Add a private context-carried budget tree at `Orchestrator.Run`; reserve the assembled prompt plus fixed generation cap immediately before each Chat and settle immediately afterward. Preserve local Usage and publish copied descendant telemetry separately.

**Tech Stack:** Go 1.25 minimum, repository toolchain go1.27.1, standard library, existing ContextManager accounting and test helpers; no new dependency.

**Spec:** [Approved design](../specs/2026-09-25-child-capability-limits-design.md), approved by the user on 2026-09-25.

## Global Constraints

- Work only in `/Users/keith.struzzieri/.codex/worktrees/449-child-capability-limits/go-llm`, branch `feat/449-child-capability-limits`, based on `099007ce4c761fa46f413ee0a26fbeed9f2336d0`.
- Prefix commands with `rtk`; host Go checks use `rtk proxy env -u GOROOT go ...`.
- Reuse defaults: input 8,192; parent MaxSteps 16; dispatch MaxSteps 6; dispatch generation 1,024 and total 32,768; otherwise finite generation fallback `provider.DefaultExpectedOutput("chat")` (2,048).
- Input/output capacities and configured step caps intersect; remaining credits never resize generation, static assembly, pressure budgets or step caps.
- TotalTokens zero stays unbounded at the root. Do not introduce a finite Golem aggregate pool.
- Admission credits are estimates, not billing tokens. No sentinel-based error refund or claim that router retries expose complete attempt usage.
- Preserve `Usage == sum(Steps.Response.Usage)` for its existing three counters. Add `DescendantUsage *provider.Usage` with `json:"DescendantUsage,omitempty"`.
- Native selected readers and stable truthful Effect metadata remain trusted. No wrappers, new capability framework, multi-root scope, scoped retrieval, provider changes or conversation CAS.
- Keep existing dispatch task/invocation/slot/timeout/result caps, Golem footer and machine output.
- No CHANGELOG.md edits, hook/config changes, merge, tag or release. Prepare and attach a reviewed PR to develop.

## Review Focus

1. Reentrant Run from an observer/interceptor/OnToken callback must inherit admission without deadlocking or borrowing an in-flight reservation (Task 2).
2. A retained context, including context.WithoutCancel, must not reopen a returned run; late settlements cannot mutate its published result (Tasks 1–2).
3. Total-only, overflowed, contradictory and failed-fallback reports cannot manufacture refunds or reported telemetry (Tasks 1–2).
4. Low spend balances must not alter assembly pressure, generation routing inputs or context-error classification, in either assembly mode (Task 2).
5. A successful overrun with tool calls must retain accepted text, invoke no tools and leave usable history; blocked/error output keeps its existing precedence (Task 2).

## File map and interfaces

| File | Responsibility |
|---|---|
| New `agent/run_budget.go` | Private run lifetime, immutable normalized capacities, atomic ancestor reservations/settlement and descendant snapshots |
| New `agent/run_budget_test.go` | Arithmetic, normalization, settlement and concurrent admission boundary tests |
| `agent/orchestrator.go` | Establish/close budget scope, use successful assembly cost, reserve/settle Chat, stop before tools |
| `agent/types.go` | Result.DescendantUsage and public Budget/Run contract comments; preserve Budget's field layout |
| New `agent/orchestrator_budget_test.go` | Behavioral tests through Run, callbacks, assembly and nested inference |
| `agent/governor_test.go` | Replace old post-call overshoot fixtures with admitted-overrun fixtures |
| `agent/tools/dispatch.go` | Document stable Effect metadata; only change behavior if an integration test proves a remaining gap |
| `agent/tools/dispatch_test.go` | Registry adversarial tests and corrected budget/step-cap fixtures |
| New `agent/tools/dispatch_budget_test.go` | Real dispatched siblings, repeated calls, independent parents, standalone calls |
| `cmd/golem/dispatch_wiring_test.go` and `cmd/golem/model_repl_test.go` | Startup and rebuilt/explicit child route attenuation through parent Run |
| `docs/least-privilege.md`, `changelog.d/449-child-capability-limits.md` | Delivered contract, compatibility changes and limits |

Production interfaces are private and live together in run_budget.go:

```go
func newRunBudget(ctx context.Context, req Request) (context.Context, Request, *runBudget)
func (b *runBudget) reserve(ctx context.Context, promptTokens int) (*tokenReservation, error)
func (r *tokenReservation) settle(mr ModelResult, callErr error)
func (b *runBudget) stopped() bool
func (b *runBudget) close() *provider.Usage
```

`errRunBudgetExhausted` is a private sentinel from reserve; Orchestrator converts it to BudgetReached with nil error. Cancellation returns the context error. reserve returns a nonnil reservation only on success. settle is idempotent. close seals the node, copies descendant telemetry under the tree mutex, then cancels outside the lock. No callbacks or inference hold the mutex.

Use one typed context key, parent links and a shared mutex; no registry of all descendants is needed because admission checks ancestor closure. Nodes store effective capacities, finite allowance, charged/reserved credits, overrun/closed state, cancellation and descendant counters. Children inherit by context, never by mutable Orchestrator or Dispatch fields.

---

### Task 1: Implement the private admission and settlement ledger

**Files:** Create `agent/run_budget.go` and `agent/run_budget_test.go`.

**Interfaces:** Produce all five private functions above and errRunBudgetExhausted. Consume existing checkedTokenAdd, checkedTokenSub, saturatedTokenAdd, DefaultInputCeiling, defaultMaxSteps, provider.DefaultExpectedOutput and ModelResult.

- [x] **Step 1: Write normalization and lifetime tests.**

Name the table tests `TestRunBudgetCapacityIntersection` and `TestRunBudgetGenerationPrecedence`. Each row constructs a parent and child via newRunBudget, closes both using cleanup, and asserts the returned effective Request. Cover explicit smaller/larger child limits, default input/steps, OutputReserve precedence over NumPredict, positive NumPredict with zero reserve, finite fallback, inherited finite fallback, and fully unbounded NumPredict compatibility.

Representative row assertions (these are separate rows, not sequential mutations):

```go
// Parent: input=0, output=128, steps=2; child: input=16384, output=1024, steps=6.
if childReq.Budget.InputCeiling != 8192 || childReq.Budget.OutputReserve != 128 ||
    childReq.Options.NumPredict != 128 || childReq.MaxSteps != 2 { t.Fatal(childReq) }
// Finite root: total=10000, output=0, NumPredict=0.
if req.Options.NumPredict != 2048 || req.Budget.OutputReserve != 0 { t.Fatal(req) }
// Finite root: output=0, NumPredict=300.
if req.Options.NumPredict != 300 || req.Budget.OutputReserve != 0 { t.Fatal(req) }
// Unbounded root with NumPredict=-1.
if req.Options.NumPredict != -1 { t.Fatal(req) }
```

`TestRunBudgetClosedAncestor`: close a parent, create a child from context.WithoutCancel(parentCtx), and assert reserve returns errRunBudgetExhausted and no reservation. Also close an intermediate node while root remains open. `TestRunBudgetCanceledAdmission` asserts errors.Is(err, context.Canceled) before any reservation exists.

- [x] **Step 2: Write admission, settlement and telemetry tests.**

`TestRunBudgetConcurrentReservations`: four sibling nodes, E=1000, G=1024, parent total=8096, child input=100000. Use channels/barriers to hold all four reservations simultaneously; all fit, a fifth does not. Independent root with identical limits admits independently. Settle every admitted reservation.

```go
if admitted.Load() != 4 { t.Fatalf("admitted %d", admitted.Load()) }
if !errors.Is(fifthErr, errRunBudgetExhausted) { t.Fatal(fifthErr) }
```

`TestRunBudgetSettlement`: E=100, G=50, reservation=150. For each row, start a fresh root with total=300, settle once and twice, and probe remaining capacity using another reservation. Put a tiny child under the root so root.close also exposes raw descendant telemetry. These exact cases pin charge independently of Result.Usage:

| Usage P/C/T; other condition | Credits charged | Descendant P/C/T |
|---|---:|---|
| 80/20/100, success | 120 | 80/20/100 |
| 120/20/140, success | 140 | 120/20/140 |
| 100/20/0, success | 120 | 100/20/0 |
| 100/20/120, ReasoningTokens=10 | 120 | 100/20/120 |
| 0/0/100, success | 150 | 0/0/100 |
| all zero | 150 | nil |
| 100/20/120 plus provider or callback error | 150 | 100/20/120 |
| all zero plus provider.ErrRouterClosed, no outcome/chunks | 150 | nil |
| 100/20/120 plus RouteOutcome with two Attempts | 150 | 100/20/120 |
| 100/80/180, success | 180; stop current node | 100/80/180 |
| 100/20/119, contradictory total | at least 150 | nil |
| negative P, C or T | at least 150 | nil |
| overflowing P+C or E+G | fail closed; never wrap | nil for invalid usage |

Assert the next reservation fits exactly at the remaining-credit boundary and fails one credit above it; use G=1 on the probe child. Duplicate settlement must not change either boundary or telemetry. For safely known overages on failures/invalid reports, use T=500 with a contradictory decomposition and assert further parent admission fails. Do not use internal balance fields as the only proof.

`TestRunBudgetSnapshotAfterLateSettlement`: obtain a snapshot containing one descendant report, settle an already admitted second call after parent.close, and assert the first snapshot remains byte-for-byte unchanged. All subsequent admissions fail; the second call still releases its reservation exactly once.

- [x] **Step 3: Run the new tests and record the expected failure.**

`rtk proxy env -u GOROOT go test ./agent -run '^TestRunBudget' -count=1`

Expected initially: compile failure for the missing private interfaces. Implement the smallest declarations, rerun to observe failing behavior before filling in the logic.

- [x] **Step 4: Implement the ledger in run_budget.go.**

Normalize copies of Request; intersect input and configured effective step caps with the parent. Resolve fixed generation in the approved precedence order. Set NumPredict to the effective positive cap; clamp a configured positive OutputReserve to that cap, but never synthesize OutputReserve from the generation fallback. Reuse existing constants.

reserve checks context, closed/overrun/exhausted ancestors and checked E+G, then charges the same reservation to each node atomically if all finite balances fit. A failed fit must change no node. In-flight reservations cannot be borrowed; no waiting for refunds.

settle releases that reservation and adds the logical charge along the ancestor chain. Only successful, consistent decomposed reports with a positive prompt count and no known multi-attempt outcome can refund. Charge max(T, max(E,P)+C); all uncertain paths retain at least the reservation and larger safely known positive usage. Checked/saturating arithmetic must never wrap into credit. Mark a finite-governed current run stopped on an over-reservation report; preserve unlimited top-level behavior.

Aggregate valid raw descendant P/C/T once into ancestors only, including failed-call reports. A zero total with usable components remains raw zero in telemetry; derived logical charge stays private. Reject negative/contradictory/overflowing telemetry and saturate aggregate counters rather than wrapping. Do not aggregate reasoning separately. stopped checks actual exhausted/overrun/closed state, not temporary sibling reservations as permanent exhaustion.

- [x] **Step 5: Verify and commit.**

Run `rtk proxy env -u GOROOT go test -race ./agent -run '^TestRunBudget' -count=1`; expect PASS, no race report. Review diff, run `rtk git diff --check`, then commit these two files as `feat(agent): add per-run token admission ledger`.

### Task 2: Enforce the ledger through the shared Orchestrator lifecycle

**Files:** Modify `agent/orchestrator.go`, `agent/types.go`, `agent/governor_test.go`; create `agent/orchestrator_budget_test.go`.

**Interfaces:** Consume Task 1's five functions. Extend the private method to `func (o *Orchestrator) run(ctx context.Context, req Request, obs Observer, ic *interceptorRun, budgetRun *runBudget) (Result, error)`; preserve the public Run signature. Produce Result.DescendantUsage with the exact type/tag in Global Constraints.

- [x] **Step 1: Write behavioral regressions through Run.**

Reuse scriptedCaller, newTestOrchestrator, pressureRec, echoTool, stepAbortObserver, tokenAbortObserver and stubInterceptor where they fit. For callback-controlled cases add only this test adapter in orchestrator_budget_test.go:

```go
type budgetModelFunc func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (ModelResult, error)
func (f budgetModelFunc) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
    return f(ctx, req, onToken)
}
```

Test names and assertions:
- `TestRunBudgetAdmissionAfterAssembly`, subtests legacy/mixed: total=1, ample static capacity, OutputReserve=64; assembly/pressure callbacks still run, Chat count=0, BudgetReached and nil error. Reuse a structured-anchor fixture for the mixed path. Compare assembled traces/pressure with an otherwise identical generous-total run.
- `TestRunBudgetKeepsContextExhaustion`: oversized pinned goal with total=1 preserves ErrContextExhausted and no Chat.
- `TestRunBudgetFixedGeneration`: enough credits for two turns but not a third, each successful response has a read tool call; capture all wire NumPredict values and pressure budgets. Test fallback 2048 and explicit Options.NumPredict=300 with OutputReserve=0. E comes from the actual fixture's recorded successful pressure; choose finite total to fit exactly two full reservations when usage is absent.

```go
if err != nil || res.StopReason != BudgetReached || calls != 0 { t.Fatalf("%+v %v; calls=%d", res, err, calls) }
if !reflect.DeepEqual(gotPressure, wantPressure) { t.Fatalf("%+v != %+v", gotPressure, wantPressure) }
// Fallback case; generous/limited runs must use the same static input budget.
if !reflect.DeepEqual(numPredicts, []int{2048, 2048}) { t.Fatal(numPredicts) }
if res.DescendantUsage != nil { t.Fatal(res.DescendantUsage) }
```

- `TestRunBudgetSettlesBeforeCallbacks`: table OnToken error, OnStep error, output block, provider error with usage, cancellation during Chat, and pre-inference observer failure. Run as a child so parent admission after its return demonstrates the charge. The first five retain the applicable charge exactly once; pre-inference failure leaves all credits available. Errors preserve errors.Is or errors.As classification.
- `TestRunBudgetNestedCallbacks`: launch nested Run using the callback's context at pre-inference pressure, output inspection/OnStep and inside Chat's OnToken path; use a bounded test context and channels, no sleeps. Assert the child sees parent capacity, consumes the same allowance and cannot borrow the parent's in-flight reservation. A nested grandchild's raw report reaches each ancestor exactly once.
- `TestRunBudgetOverrunStopsBeforeTools`: admit a response whose usage exceeds its reservation and which contains accepted text plus an echo tool call. Assert accepted text/StepRecord retained, BudgetReached, nil error, tool invocation count zero. Reuse Result.Messages as History in another run and require validation to pass. A blocking interceptor variant must redact content and return BlockedError instead.
- `TestRunBudgetReturnedResultIsSnapshot`: retain the supplied run context and one admitted child, return the root, try another child with context.WithoutCancel, then finish the admitted child. Assert the late child performs no Chat and root.DescendantUsage does not change.

For every completed/error test with recorded steps, sum P/C/T from Steps and assert equality with res.Usage. Failed Chat reports without StepRecords must not appear in local Usage. Marshal results and assert DescendantUsage absent when nil, present under precisely that casing otherwise.

- [x] **Step 2: Run the new regressions before integration.**

`rtk proxy env -u GOROOT go test ./agent -run '^TestRunBudget' -count=1`

Expected: behavioral failures for missing runtime admission/lifetime/telemetry (plus the missing Result field until declared). Record failures; do not loosen old tests to accept both semantics.

- [x] **Step 3: Integrate admission and settlement.**

In Run establish the derived context/effective Request before any run-scoped callbacks; ensure close executes for every return and assign its copied telemetry after o.run completes. Keep Risk publication.

Within o.run preserve static turnBudget and existing validation/observer order. Reuse successful `pressure.InputTokens` as E: both legacy and mixed assembly already calculate this checked cost including schemas, frames and advisory. Build chatReq/session identity, then reserve immediately before Chat; there must be no callback or other fallible operation in between. Convert only errRunBudgetExhausted to BudgetReached; propagate cancellation normally. Denial returns the current messages/partial evidence.

Call settle immediately after Chat returns, before output inspection, OnStep, partial-error handling or any return. Keep current recorded-step Usage updates, redaction and error precedence. After successful safety and observer processing, check stopped before running tool calls; retain nonempty accepted Content as a plain assistant Message without unexecuted ToolCalls, leaving the StepRecord as the actual response. Do not append an empty assistant message when only tool calls were returned. Check again after tools because nested runs may have exhausted the allowance. Replace the two Result.Usage-based budgetExceeded calls and remove the now-unused helper.

Update Budget/Run comments with inherited context, capacity/spend distinction and stable caps. Add DescendantUsage to Result without changing Budget's positional field layout.

- [x] **Step 4: Update old budget fixtures, verify and commit.**

Update `TestBudgetCapStop` and `TestBudgetCapStopOnFinalAnswer` to use a small explicit generation cap and a total that admits the first request, then a report that demonstrably overruns it. Assert calls, accepted answer and no post-overrun tool execution; retain separately the new tiny-total no-Chat test.

Run `rtk proxy env -u GOROOT go test ./agent -count=1` and `rtk proxy env -u GOROOT go test -race ./agent -run '^TestRunBudget' -count=1`; expect PASS. Fix actual regressions without changing unrelated usage/output contracts. Review diff, run diff --check and commit as `feat(agent): enforce inherited token limits before inference`.

### Task 3: Prove dispatch/Golem behavior and document the delivered boundary

**Files:** The dispatch, Golem, least-privilege and changelog files in the file map. Existing scope/path implementation stays intact unless a test demonstrates a defect caused by this change.

**Interfaces:** Consume unchanged NewDispatch/Dispatch.Invoke/Orchestrator.Run and Result.DescendantUsage. Use existing dispatchModelFunc, dispatchNamedTool, gatedDispatchCaller, validDispatchAvailable, specRecordingCaller and newModelSwitchFixture; extend those small test fixtures as needed, not production APIs.

- [x] **Step 1: Add/adjust integration regressions before any dispatch changes.**

`TestDispatchRejectsUnsafeSelectedTools`: table selected names with Write, Exec, Network, mixed Read combinations, approval-requiring and PlanningTool entries; all constructor calls fail. Retain missing/duplicate/nil and native scoped-reader tests.

`TestDispatchCannotInvokeOmittedParentTools`: full parent registry includes write, exec, network, planning, dispatch and MCP-like names; construction succeeds. Child model requests each omitted name, receives the existing unknown-tool error and cannot increment any host tool's invocation counter. Optional retrieve still works unscoped and is absent scoped.

`TestDispatchInheritsParentCapacities`: invoke dispatch through a parent Run, not standalone. Capture child wire NumPredict/input pressure and count model calls with parent MaxSteps=2 versus child 6; repeat with smaller child limits. Test parent input zero against explicit larger child ceiling, parent output 128 against dispatch default 1024, and defaults under an unbounded parent.

`TestDispatchSharesParentAllowance`: channel-held siblings fit their actual E+G reservations even when their context ceilings far exceed remaining credits. Exhaust the parent allowance using multiple dispatch calls across turns and multiple calls in one response; assert total admitted calls and no duplicated descendant telemetry. Same-response calls are sequential, not a concurrency claim.

`TestDispatchIndependentParentAllowances`: reuse one dispatcher from simultaneous independent parent Runs; each admits its own expected calls and gets only its own descendant usage. `TestDispatchStandaloneLocalBudget`: direct Invoke enforces each child's default 32768 independently, with no invented parent pool. Test queued cancellation/slot release and an admitted child failing with unknown usage without unblocking sibling credit.

Core assertions for these fixtures:

```go
if unsafeInvocations.Load() != 0 { t.Fatal(unsafeInvocations.Load()) }
if !reflect.DeepEqual(childCaps, []int{128, 128}) { t.Fatal(childCaps) }
if parentRes.Usage != ownStepUsage { t.Fatalf("%+v != %+v", parentRes.Usage, ownStepUsage) }
if parentRes.DescendantUsage == nil || *parentRes.DescendantUsage != knownChildUsage { t.Fatal(parentRes.DescendantUsage) }
```

Replace `TestDispatchReportsPerChildBudgetStopAndModel`'s impossible total 10/output 123 fixture with two explicit cases: tiny total calls no model and returns budget_reached with unavailable-model/no-summary error; admitted overrun preserves actual model identity and accepted/partial evidence. Raise only the budget in `TestDispatchReportsChildStepCap` to 10000 so its two-turn step-cap assertion exercises steps rather than admission.

Add `TestNewDispatchTool_AttenuatesAtParentRun` and extend existing model-switch fixtures to dispatch through parent Run after startup, parent-following rebuild and an explicitly pinned larger child route. Assert runtime cap attenuation while retaining the selected child model/route. Keep existing standalone constructor tests proving output777 passes through, read-only tool lists, shared retrieve and tiny-ceiling rejection.

- [x] **Step 2: Run targeted tests and inspect failures.**

```sh
rtk proxy env -u GOROOT go test ./agent/tools -run 'Test(NewDispatch|Dispatch|Scoped)' -count=1
rtk proxy env -u GOROOT go test ./cmd/golem -run 'Test(NewDispatchTool|OrchestratorFactory_Dispatch|ModelSet.*Dispatch|ModelSetAfterAllowWrite)' -count=1
```

Expect the new runtime cases to pass with Task 2, and the deliberately corrected old fixtures to pass. If a remaining enforcement gap fails, record it before making the smallest shared-path fix. No redundant dispatch clamp when Run already enforces it.

- [x] **Step 3: Document and verify the contract.**

Document constant Effect metadata, exact selected registry, caller guard AND one selected subtree, omitted scoped retrieval, inherited capacities/defaults, logical reservation/settlement, fixed generation, telemetry split and sealed lifetime in docs/least-privilege.md. Include the unbounded Golem parent and unchanged configured product 4×4×32768=524288 admission credits, without promising exact billed-token bounds. Name direct Chat, estimator/provider compliance and hidden router attempt limitations.

Create changelog.d/449-child-capability-limits.md headed `### Security — Child capability and budget limits (#449)`. Explain new pre-admission stops, parent-capacity intersection including default8192 and potentially shorter summaries, separate descendant telemetry, single-subtree scope and no new finite Golem pool.

Run `rtk proxy env -u GOROOT go test -race ./agent ./agent/tools ./cmd/golem`; expect PASS with no race reports. Review the whole diff and commit as `test(dispatch): verify child limits across runtime and Golem`, including documentation.

## Final review and publication

- [x] Re-read approved spec and map every requirement to implementation/tests; independently review the whole branch using the selected execution method and applicable review skill. Fix actionable findings and rerun affected checks.
- [x] Fetch origin/develop; compare against base 099007c. If it advanced, rebase/update the isolated branch safely, resolve conflicts, and rerun affected checks before publication.
- [x] Run `rtk docker compose -p go-llm-449 -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full`; require successful security contracts, formatting, lint, repository-wide race tests and compile smoke. Inspect hook/shared-image state; do not modify hooks, bypass their gate or rebuild shared images concurrently with another task.
- [x] Verify `rtk git diff --check`, intended branch and no unrelated files. Include exact validation results and material limits in the PR description.
Publication: push the feature branch with its normal pre-push gate and create a PR to develop with `Closes #449`. Use a body file for gh, attach the created PR using the Codex artifact tool, and report its URL. Do not merge.

## Plan review

Self-review completed against the approved spec: registry/scope, capacity/defaults, atomic admission, conservative settlement, cancellation/lifetime, Usage compatibility, Golem wiring and publication each have an owning task. Test selectors include the actual ModelSet rebuild tests. No provider migration or new dependency is planned. Native execution proceeds under the user's approved design and original implementation request; progress is recorded in this plan's execution ledger.

## Independent review fixes

The final branch review found two important defects, both reproduced before the
fix: exhaustion during a tool batch or preparation callback still permitted
later invocations, and cancellation during OnStep could be hidden by the new
overrun return. The shared invocation boundary now checks the run budget after
callbacks, serial preparation stops after exhaustion, queued parallel calls are
not invoked, and cancellation precedes a successful budget stop. Completed tool
observations remain paired; unexecuted calls and empty assistant residue are
removed. Existing History still accepts plain chat only.

Regressions cover dispatch followed by a mutation, serial/parallel callbacks,
queued parallel work, and cancellation after an overrun. No minor findings or
additional product-scope changes were raised. Final verification and publication
outcomes are recorded in the execution ledger and attached PR.

## Verification record

Final isolated full Docker gate passed after the review fixes: security
contracts, formatting, lint (zero issues), repository-wide race tests (including
Golem, 281.904s), and compile smoke. Native agent/tool race tests passed
(13.822s/55.416s), as did native Golem dispatch/model-switch race tests (3.528s),
vet for all three packages, changelog validation, and diff whitespace checks.
The unchanged pre-push hook runs with the per-command Compose project
`go-llm-449`; shared hooks, credentials and Docker configuration are untouched.
Publication outcome belongs to the PR/task attachment rather than this
pre-publication commit.

## Recorded rulings

- Ruling: proceed from approved design to native implementation without another approval round — developer autonomy instruction and user's carry-it-out request authorize this reversible work — user may prefer a different execution method, which remains easy to switch.
- Task 2: Ruling: nested output/step callback test expects BudgetReached for an admitted child that exactly exhausts the shared allowance — the fixture has 2E+65 credits, parent charges E+1 and child E+64 — expecting Completed would contradict the approved exhaustion contract; cost if wrong is stop classification, not admission.
- Final: Ruling: exact billing, hidden attempt costs and dishonest installed tools stay outside the guarantee — users get conservative logical admission and explicit trust/provider limits, matching the approved spec — cost if wrong: treating credits as billed-token proof would overstate the boundary.
- Final: Ruling: no finite aggregate Golem pool — configured parent TotalTokens remains zero; finite caller budgets are enforced when supplied, as approved — cost if wrong: Golem users expecting a new global spend cap would still lack one.
- Final: Ruling: pending Docker gate is execution status, not a code defect — publication waits for the completed full gate rather than inventing a review finding — cost if wrong: publication delay or an unvalidated regression if completion were misread.
- Final: Ruling: absent repository-specific review escalation files — use the supplied safety contract and CLAUDE.md under the user's approved security-boundary task — cost if wrong: unavailable local review requirements could be missed.
