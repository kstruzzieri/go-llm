# Child scope-denial reporting (#555): revised spec and TDD plan

> **Status:** Approved by the user on 2026-09-27; implemented, verified and independently reviewed.
>
> **For agentic workers:** After approval, use superpowers:executing-plans for native implementation in this chat. Complete each test-first task before continuing; independent reviews may use subagents.

**Goal:** Report actual scoped child request refusals in the parent's opt-in RiskReport without trusting model text or changing filesystem enforcement.

**Architecture:** Count refused requests at the existing native reader error boundary in a fresh per-child atomic counter, separate from scope evaluation counts. Dispatch snapshots those counters after its workers join and carries one native entry per affected child to the existing parent inspection pipeline. A stateless opt-in interceptor emits one capped finding per entry.

**Tech Stack:** Go 1.27.0 minimum, toolchain go1.27.1, standard library and existing test helpers; no new dependencies.

**Spec:** The Contract section below is the approved spec. This combined spec/TDD plan is the review deliverable required by the handoff.

## Baseline and ownership

- Live issue: https://github.com/kstruzzieri/go-llm/issues/555. Read on 2026-09-26 and independently rechecked during the 2026-09-27 review. Its opening scope is scoped children; older backlog wording does not defer the user's selected work.
- Worktree: `/Users/keith.struzzieri/.codex/worktrees/555-child-scope-denial-reporting/go-llm`.
- Branch: `feat/555-child-scope-denial-reporting`.
- Fetched and inspected base: `2d3fbd892e83a88fb3b050983b8960aae39e18f4`.
- Verified ancestors: #448/#556 `d4358ff`, #552/#588 `099007c`, #449/#595 `cb0d325`, #564/#565/#597 `1b58057`, #569/#596 `2d3fbd8`.
- Read the handoff, applicable AGENTS.md, CLAUDE.md, RTK.md, safety rules, live acceptance criteria, relevant #449 spec/plan and current local CI/changelog rules.
- Existing baseline passed on 2026-09-26: `rtk go test ./agent ./agent/tools` (2,650 tests), plus focused interceptor/Golem wiring tests (13). No code changed during this design revision; these are baseline results, not verification of the proposed feature.
- #501 owns provider destination parsing, CLI acceptance/help and admission documentation. Only narrowly related reporting paragraphs in docs/golem.md and docs/least-privilege.md overlap the documentation surface; recheck concurrent edits before publication.
- Leave unrelated worktrees, copilot.json, output/, CHANGELOG.md, shared Git settings and CI infrastructure untouched. No merge, tag or release.

## Review disposition

| Finding | Decision |
|---|---|
| F1: refusal counter | Accept. Record the fact where a native reader returns a typed denial; remove the child ToolResult/ToolCallRecord markers and child inspection gate. |
| F2: global cancellation behavior | Accept. Do not add a ctx.Err check to recordResult or change how any result drains. Parent reporting follows existing inspection behavior. |
| F3: call attribution/dedup | Accept task-level attribution. Dispatch produces exactly one entry per affected task; no call positions or dedup map. Retain empty/reused-ID tests because the live issue expressly requires them. |
| F4: aggregation | Accept one finding per child, Risk = 10 × min(requests, 10). Cap the int64 count before multiplication/conversion; normalization alone cannot repair overflow. The task limit bounds findings per dispatch observation, not across an arbitrary whole run. Three refusals still score 30: the cap limits repeated contribution rather than establishing intent. |
| F5: unscoped host guards | Explicitly exclude legacy unscoped children in this ticket. This matches the issue's scoped-child premise and preserves shared legacy tools. Document the limit and test it. |
| F6: symlinks | Count them when existing scopedPathError classifies them as a typed scope denial, including an in-scope symlink. No inference about intent or new filesystem policy. |
| F7: documentation | Update the existing reporting paragraphs in golem.md and least-privilege.md; skip a new standalone document. The old child risk_score remains unpropagated, so clarify the new separate evidence rather than claim child scores are relayed. |
| F8: chain/origin | Test that the reporter is inert inside ordinary scoped children. Findings have OriginModel from the parent dispatch observation; that origin describes observation provenance, not how the native evidence was authenticated. |
| F9: ignored draft | Keep this draft local pending approval. On approval, deliberately include this exact plan in the ticket commits with explicit force-add; never sweep ignored plans into staging. |

## Contract

### 1. Two different counters

Replace the existing private scoped counter carrier with `scopeCounters`, containing `evaluations atomic.Int64` and `requests atomic.Int64`. Workspace and dispatchChild retain a pointer to this structure. Allocate a fresh structure in newScopedWorkspace for each scoped task. Workspace snapshots copy only the pointer, never used atomics; constructing a nested scoped workspace allocates another fresh structure.

The existing evaluation counter keeps its exact behavior and remains the source of the existing scope_denials envelope field: repeated guard evaluations and quiet pruning still count there. Neither that value nor child risk_score is evidence of a refused request.

Implement `(w *Workspace) toolErrorResult(err error) agent.ToolResult` in tool_error.go. Increment requests exactly once when this is a scoped workspace and `errors.Is(err, errScopeDenied)`; then return the existing sanitized `errResult(toolErrMessage(err))`. Use it only at terminal error returns from read_file, search, glob and list. Route both direct glob pattern-denial branches through it before converting the error to text.

Do not increment requests in denyScope, checkScope, scopedPathError, toolErrMessage, preflight or enumeration callbacks. A single failed request may trigger several evaluations but reaches one terminal error return. Direct host Workspace operations and successful enumeration filtering cannot manufacture a refused model request.

Wrapped typed denials qualify. Same-text errors without typed identity do not. A ScopeGuard veto wrapped by Workspace is a genuine denial regardless of its original message. Returned scoped traversal, absolute/NUL-path and symlink denials count under the existing classifier; ordinary malformed arguments, missing files, unknown tools, success and quiet pruning do not. An in-scope symlink refusal can be an innocent mistake and still counts.

Legacy string tasks, including those whose shared parent tools have a host ScopeGuard, do not contribute this new evidence. Enforcement and sanitized errors still apply. Setup failures before any child starts do not count. Installed Go tools/interceptors remain trusted host code; no signing, persistent collector or wire authority is introduced.

### 2. Native parent carrier and scoring

Add `agent.ChildScopeDenial` in tool.go with exactly `Task int` (zero-based dispatch input position) and `Requests int64` (actual refused-request count). Add `ChildScopeDenials []ChildScopeDenial`, tagged `json:"-"`, to ToolResult and InspectedMessage. Do not add fields to ToolCallRecord, Result, State, Message or provider.ChatMessage.

Keep runChild's signature unchanged. Add private unexported `deniedRequests int64` to dispatchResult; snapshot requests after the child Run returns. The field never enters the JSON envelope. After all workers join and the ordinary envelope is successfully marshaled, iterate results in task input order and attach one entry for each positive count to the outer ToolResult. No metadata lives on the shared Dispatch. A successful child summary and an outer IsError=false can still carry refused requests.

Implement stateless `interceptor.ChildScopeDenials{}`:
- Name: child_scope_denials; Rule: child_scope_denied.
- Emit one finding per positive-count, nonnegative-task entry. Dispatch guarantees one entry per task; no dedup map or provider-ID association.
- `ChildScopeDenialRisk = 10`; calculate `10 * int(min(d.Requests, 10))` after skipping nonpositive counts. The count displayed remains the full int64 value.
- VerdictAllow: informational telemetry, no model-visible annotation, block, abort or grant revocation.
- TargetMessage at the inspected dispatch StateIndex. Normalization supplies parent Step, ToolCallID and OriginModel.
- Detail: `dispatch task N: R request(s) denied by workspace policy`, with N one-based. Include no paths, arguments, model summaries or private guard diagnostics.
- InspectOutput and InspectToolCall return no findings.

Two refusals in one child and one in another yield two findings and 30 points. Ten or more refusals in one child contribute 100 points. Counts never saturate for display merely because score saturates. Repeated dispatch invocations contribute independently, including identical tasks and empty/reused provider IDs. Overall risk uses the existing saturating sum and may exceed 100.

Identity is the parent inspection occurrence and task position within that invocation. The native producer emits each task once and the existing pipeline inspects each result once; that is the deduplication rule. No cross-invocation/run deduplication or identifier registry is added. Concurrent child completion order does not change output order.

Append the reporter to interceptor.Defaults. Golem inherits it through existing interceptorsFor only when -interceptors is enabled. Custom chains without the reporter gain no automatic findings; they may opt in explicitly. Parent reporting works with child interceptors disabled. The same reporter installed in scoped children is inert because their native readers update counters and do not emit ChildScopeDenials. Preserve existing child risk_score and scope_denials semantics and all envelope omission rules.

### 3. Lifecycle: refusal first, parent inspection second

**Counted when a scoped reader returns a typed refusal; reported if the dispatch envelope reaches parent inspection under the existing runtime lifecycle.**

- A child result's subsequent truncation, interceptor rejection, observer error, cancellation or trailing-parallel discard does not undo an already-counted refusal. No child inspection/observation-admission gate is added.
- If Dispatch.Invoke hard-aborts on cancellation, a child construction/model-identity error or envelope marshaling error, it publishes no carrier. Parent findings from earlier inspections remain.
- An invocation deadline preserves existing partial-envelope behavior while the parent remains live: return actual refusals from started children, including completed results whose child observations were discarded. Unstarted tasks add none.
- Parent cancellation handled before result recording, mixed-context validation failure or a discarded trailing parent result prevents that result's inspection and contributes no finding.
- Once parent inspection runs, runHook retains findings even if another interceptor blocks/aborts/errors, OnInterception fails, OnToolResult fails/cancels, or the governor subsequently stops. No rollback.
- Do not promise a new cancellation boundary. In particular, when an earlier callback cancels during parallel result draining but returns nil and the existing runtime continues inspecting, the later result may still contribute. Keep and test that existing behavior; the reporter adds no ctx.Err gate.
- Reusing the dispatcher or orchestrator, overlapping invokes and a clean subsequent Run must not share counters, evidence or findings.

This remains a run-level report, not an audit of every access attempt. Counter evidence is lost if no dispatch envelope reaches parent inspection.

### 4. Ownership and wire exclusion

Clone the value slice on successful Tool.Invoke receipt, in each cloneInput and in the ToolResultObserver publication. recordResult only forwards this field into InspectedMessage and provides the observer's copy; it gains no child markers, cancellation checks or new admission rules. Dispatch creates a fresh metadata slice for every invocation. Shared-slice mutation cannot alter later inspections, outputs or another invocation.

Content capping and summary/envelope truncation leave native counts/task positions intact. Neither truncated nor complete text can create evidence. Metadata is never copied into conversation history.

Test actual encoding/json Marshal/Unmarshal of ToolResult and InspectedMessage/InputInspection, and marshaling of the private dispatch envelope. Native fields/private counts are absent on the wire; forged keys cannot populate freshly decoded carriers; round trips lose native authority. Decoding into already-populated trusted structs ignores json:"-" fields rather than clearing them; decoding is not a sanitizer for reused structs.

## Alternatives and review focus

- Child ToolCallRecord markers: rejected; their inspection gate and call-level metadata are unnecessary for the selected refusal-time contract.
- OnToolResult collector: rejected; it can miss a real refusal when child inspection aborts before that callback.
- General context collector or persistent audit: unnecessary; existing per-child ownership and worker joins provide the required lifetime.
- ToolResult.Findings with automatic insertion: rejected; would bypass the configured interceptor chain and change custom-chain behavior.
- Relaying child RiskReport findings: outside #555; would change score aggregation and risk double counting.
- Mandatory focus: terminal typed errors versus quiet pruning; parent inspection versus later rejection; shared-instance isolation; non-wire fields and owned slices; scoring arithmetic before normalization. Each has explicit tests below.

## TDD tasks

### Task 1: Count native scoped refusals and snapshot per-child results

**Files:** agent/tools/paths.go, dispatch_scope.go, tool_error.go, readfile.go, glob.go, search.go and dispatch.go; existing dispatch_scope_test.go, dispatch_scoped_invoke_test.go and any counter-type compile sites; new agent/tools/dispatch_denial_test.go.

**Interfaces:** Produce `scopeCounters{evaluations, requests atomic.Int64}`; replace private Workspace.scopeDenials with `scope *scopeCounters`; childTools/newScopedWorkspace and prepareChildTools return/pass *scopeCounters in the existing counter position; dispatchChild.counter uses *scopeCounters. Produce toolErrorResult(error) ToolResult and private dispatchResult.deniedRequests int64. No public function signature changes or filesystem enforcement changes.

- [x] Write `TestScopedRequestDenials` with table subtests for typed/wrapped errors, unrelated same-text errors, private guard errors, real read/list/glob terminal refusals, in-scope symlinks, malformed arguments, unknown requests, missing files, denial-looking file contents, quiet search/list/glob pruning and skipped unreadable files. Assert exactly one request per refused tool Invoke, unchanged sanitized text/evaluation count and zero new requests for direct Workspace access or pure toolErrMessage formatting. Follow existing Darwin/Linux scoped test build tags; keep platform-neutral classifier cases portable.
- [x] Add runChild snapshot subtests to `TestScopedRequestDenials`: prepare a real scoped dispatchChild and run serial/parallel distinct native readers and repeated requests. Inspect the returned private dispatchResult directly: deniedRequests equals terminal refusals, ScopeDenials keeps its existing evaluation meaning and RiskScore stays unchanged. Unscoped guarded tools retain zero deniedRequests. Task 2 owns outer Invoke ordering and metadata tests.
- [x] Run `rtk go test ./agent/tools -run '^TestScopedRequestDenials$' -count=1`; record RED before adding implementation. Add the pointer-owned counter/helper/snapshot, then run GREEN and existing scoped/error/cleanup tests.
- [x] Review every changed error path and commit the tested ticket code only.

### Task 2: Transport owned metadata and add the opt-in reporter

**Files:** agent/tool.go, dispatch.go and interceptor.go; agent/tools/dispatch.go and dispatch_denial_test.go; new agent/child_scope_denial_test.go and agent/interceptor/child_scope_denials.go/child_scope_denials_test.go; agent/interceptor/defaults.go/defaults_test.go; cmd/golem/interceptors_test.go/dispatch_wiring_test.go.

**Interfaces:** Produce ChildScopeDenial{Task int, Requests int64}, the two json:"-" slice fields, ChildScopeDenials' four Interceptor methods and ChildScopeDenialRisk=10. Consume Task 1's snapshot; only attach positive per-task counts after a successful join/marshal.

- [x] Write `TestChildScopeDenialReporter`: counts 1, 2, 10, 11 and MaxInt64 score 10, 20, 100, 100 and 100; nonpositive counts/negative task positions yield none. Assert exact detail and Allow verdict, normalized TargetMessage/OriginModel, two children contribute two findings, and identical positions in a later inspection count again. No text/JSON inference.
- [x] Write `TestChildScopeDenialCarrier` with JSON encode/decode/round-trip subtests, no private envelope count, output truncation, owned tool/interceptor/observer copies and no State/history carrier. Assert mutations by an earlier callback cannot affect later callbacks or a captured output.
- [x] Write `TestDispatchRequestDenials` through outer Invoke and parent Run: actual denial then innocent summary scores 10; counts two/one yield 30 in two findings; >10 refusals cap per child while displaying full count. Include serial/parallel distinct native readers, mixed scoped/legacy tasks, reverse child completion order, empty/reused provider IDs, repeated dispatch calls, positive pruning counts with no requests, unrelated positive child risk, forged summaries/files/JSON/provenance and replayed history. Use enough varying refused arguments/steps to avoid the existing repeat governor terminating cap fixtures early.
- [x] Write `TestChildScopeDenialLifecycle`: child pre/post-refusal cancellation, Block/Abort/interceptor/observer failures, discarded child parallel results, dispatch own deadline, parent deadline/cancel, hard envelope errors, parent validation/discard and later rejection. Pin existing cancellation-during-drain behavior with read-only native-carrier fixtures; real dispatch is Read|Network and cannot run on the parent's parallel path. Use barriers, not timing sleeps; cover both assembly modes.
- [x] Write `TestDispatchDenialIsolation`: overlap independent parents sharing a dispatcher and order child completion with channels; verify exact independent findings, a clean next Run and detached returned slices. No shared mutable test callback state.
- [x] Write `TestChildScopeDenialOptIn` and `TestGolemChildScopeDenialReporting`: no chain/custom chain without reporter produces no scope findings; reporter-only parent works with child chain disabled; the reporter in children contributes no child risk itself. Update the exact default-chain/notice assertions and verify Golem factory/dispatch flag-on/off wiring.
- [x] Run focused tests RED before implementation, then GREEN: `rtk go test ./agent ./agent/interceptor -run '^TestChildScopeDenial' -count=1`; `rtk go test ./agent/tools -run 'Test(DispatchRequestDenials|DispatchDenialIsolation)$' -count=1`; `rtk go test ./cmd/golem -run 'Test(GolemChildScopeDenialReporting|InterceptorsFor|StartupNotices_Interceptors|RunWiresInterceptors)$' -count=1`. Place lifecycle/opt-in integration subtests in dispatch_denial_test.go where native scope construction is required and run those actual names explicitly too.
- [x] Review and commit. No global cancellation fix, call-level dedup machinery or CLI parsing edits.

### Task 3: Mutation evidence, existing docs, review and publication

**Files:** reporting paragraphs in docs/golem.md and docs/least-privilege.md; changelog.d/555-child-scope-denial-reporting.md; this exact approved plan and its evidence. No standalone reporting document or unrelated roadmap/release rewrite.

- [x] Perform each mutation separately, record its exact failing test/assertion/exit status, restore it and rerun the affected test. Keep the live issue's mandatory aggregation/isolation evidence even though the implementation has no dedup map.

| Mutation | Required failing coverage |
|---|---|
| Bypass request increment, dispatch snapshot or parent carrier (separately) | TestScopedRequestDenials / TestDispatchRequestDenials |
| Replace typed identity with string matching, or infer parent evidence from text/JSON | TestScopedRequestDenials / TestDispatchRequestDenials negative subtests |
| Use evaluation scope_denials or child risk_score as refused-request evidence | TestDispatchRequestDenials pruning/other-risk subtests |
| Lose repeated attempts/children, associate by provider ID, or misapply the score cap | TestDispatchRequestDenials / TestChildScopeDenialReporter |
| Reuse a counter/evidence slice across tasks, invokes or runs | TestDispatchDenialIsolation |
| Remove a copy or expose parent/private metadata to JSON | TestChildScopeDenialCarrier |

- [x] Update golem.md to distinguish unchanged child risk_score from separately reported native refusals. Update the relevant child/interceptor boundaries in least-privilege.md. Document per-child capped scoring, scope-only and symlink limits, OriginModel meaning, existing cancellation semantics, opt-in/custom chains and no stronger enforcement.
- [x] Add the valid fragment `### Added — Child scope-denial reporting (#555)`; never edit CHANGELOG.md.
- [x] Run `rtk go test -race ./agent ./agent/tools ./agent/interceptor ./cmd/golem`; run new tests explicitly by their actual names.
- [x] Run `rtk docker compose -p go-llm-555 -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full`. Preserve the current Go 1.27 security/lint/race/smoke gates. Coordinate a shared hook-image rebuild if another branch's Dockerfile differs; do not change shared infrastructure.
- [x] Complete independent whole-diff review using the applicable review skill, fix verified findings and record revision/evidence.
- [x] Fetch develop before publication. If #501 lands first, incorporate it and rerun affected checks, resolving only narrow reporting-document overlaps.
- [x] Review the final diff; explicitly add selected ticket files and force-add only this approved plan in the ignored docs/superpowers directory. Never use broad git add -A or force-add the directory. Commit, push through the normal hook and prepare/attach a PR to develop with contract, RED/GREEN, mutation, race and full-gate evidence. Do not merge, tag or release.

## Approval and execution evidence

User approved the revised contract on 2026-09-27: refusal-time counting; one finding per scoped child with 10 points per request capped at 100; legacy unscoped children excluded; existing typed symlink refusals included. The original two-stage inspection design is superseded.

Review on 2026-09-27 checked the supplied F1–F9 feedback against the isolated base. An independent reviewer confirmed the counter design, existing cancellation semantics, scoped-only boundary, overflow-safe scoring and retained acceptance matrix. This is design review, not implementation verification.

Task 1 RED: missing counter/helper interfaces; GREEN: 108 focused scoped/error tests.
Task 2 RED: missing carrier/reporter interfaces, missing dispatch evidence, and
missing default/Golem parent findings. GREEN: 87 focused tests across agent,
tools, interceptor, and Golem. Commits: efaeeb2 and 6e3699d.

Task 3 mutation run on 2026-09-27: all 21 mutations below failed behavioral
assertions (exit 1, no compile failures or panics); after restoring each mutation,
the identical test selector passed (exit 0). Tests ran with
`rtk proxy go test <package> -run <selector> -count=1 -timeout=30s`.
Counter/evidence authority, task aggregation, provider-ID independence, ownership,
and wire exclusion were each bypassed separately. Final verification:
- `rtk go test -race ./agent ./agent/tools ./agent/interceptor ./cmd/golem`:
  passed on the final code (RTK reports 7,000 passed).
- `rtk docker compose -p go-llm-555 -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full`:
  exit 0; security contracts, format, lint (0 issues), repository-wide race tests
  and compile smoke passed. Existing platform-boundary skips are unchanged.
- `scripts/check-changelog 2d3fbd892e83a88fb3b050983b8960aae39e18f4` and
  `git diff --check`: passed.
- Independent fresh-context review of `2d3fbd8..1a4c3e2`: APPROVE after
  broad, adversarial and security passes; no actionable findings, deferred
  minors or declined-to-judge items. Repository AI-kit manifests were absent;
  review used CLAUDE.md and the supplied safety rules.
- Fresh `origin/develop` remains `2d3fbd892e83a88fb3b050983b8960aae39e18f4`;
  #501 has not landed. CI Dockerfile and compose definitions match develop.

The first broad gate caught one additional existing exact default-chain
expectation in `TestHardeningContracts`; updating its expected list to include
the seventh interceptor produced a passing focused check and the final gates
above. No enforcement or lifecycle change was needed.

Published through the normal pre-push hook as PR #599 to develop; no merge, tag
or release is authorized.


### Mutation evidence


- **drop-request-increment** (agent/tools/tool_error.go): `rtk proxy go test ./agent/tools -run TestScopedRequestDenials -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:91: requests=0, want 1`

- **drop-child-snapshot** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/innocent -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:195: native=[] want [{Task:0 Requests:1}]`

- **drop-dispatch-carrier** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/innocent -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:195: native=[] want [{Task:0 Requests:1}]`

- **drop-parent-forwarding** (agent/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/innocent -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:210: risk=<nil> want score 10 / 1 findings`

- **match-error-text** (agent/tools/tool_error.go): `rtk proxy go test ./agent/tools -run ^TestScopedRequestDenialsTypedIdentity$ -count=1 -timeout=30s`

  Failure: `tool_error_test.go:118: result={Content:path denied by workspace policy IsError:true Preview: Truncated:false Provenance:null Attrib:<nil> Context:<nil> RouteOutcome:<nil> Origin:unknown ChildScopeDenials:[]} requests=0, want "path denied by workspace policy" / 1`

- **infer-text** (agent/interceptor/child_scope_denials.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/forged -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:296: risk=&{Score:40 Findings:[{Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:10 Detail:dispatch task 1: 1 request(s) denied by workspace policy Origin:model Hook:input Step:0 Target:message StateIndex:0 ToolCallID: Group:-1 Alternative:-1} {Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:10 Detail:dispatch task 1: 1 request(s) denied by workspace policy Origin:user Hook:input Step:0 Target:message StateIndex:1 ToolCallID: Group:-1 Alternative:-1} {Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:10 Detail:dispatch task 1: 1 request(s) denied by workspace policy Origin:user Hook:input Step:0 Target:message StateIndex:2 ToolCallID: Group:-1 Alternative:-1} {Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:10 Detail:dispatch task 1: 1 request(s) denied by workspace policy Origin:model Hook:input Step:0 Target:message StateIndex:4 ToolCallID:reused Group:-1 Alternative:-1}] CurrentToolCallFindings:[]} want score 0 / 0 findings`

- **trust-evaluations** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/quiet -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:195: native=[{Task:0 Requests:3}] want []`

- **trust-child-risk** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/forged -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:289: [{0 23}]`

- **lose-repeated-requests** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/serial -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:195: native=[{Task:0 Requests:1}] want [{Task:0 Requests:2}]`

- **lose-later-children** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/multiple -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:243: id="" ordered carrier=[{Task:0 Requests:2}]`

- **require-provider-id** (agent/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/multiple -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:249: risk=<nil> want score 60 / 4 findings`

- **deduplicate-provider-id** (agent/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/multiple -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:249: risk=&{Score:30 Findings:[{Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:20 Detail:dispatch task 1: 2 request(s) denied by workspace policy Origin:model Hook:input Step:0 Target:message StateIndex:2 ToolCallID: Group:-1 Alternative:-1} {Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:10 Detail:dispatch task 3: 1 request(s) denied by workspace policy Origin:model Hook:input Step:0 Target:message StateIndex:2 ToolCallID: Group:-1 Alternative:-1}] CurrentToolCallFindings:[]} want score 60 / 4 findings`

- **remove-score-cap** (agent/interceptor/child_scope_denials.go): `rtk proxy go test ./agent/interceptor -run ^TestChildScopeDenialReporter$ -count=1 -timeout=30s`

  Failure: `child_scope_denials_test.go:42: finding={Interceptor: Rule:child_scope_denied Verdict:allow Risk:110 Detail:dispatch task 1: 11 request(s) denied by workspace policy Origin:model Hook:unknown Step:0 Target:message StateIndex:3 ToolCallID:reused Group:-1 Alternative:-1}`

- **shared-child-counter** (agent/tools/dispatch_scope.go): `rtk proxy go test ./agent/tools -run ^TestDispatchDenialIsolation$ -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:519: risk=&{Score:30 Findings:[{Interceptor:child_scope_denials Rule:child_scope_denied Verdict:allow Risk:30 Detail:dispatch task 1: 3 request(s) denied by workspace policy Origin:model Hook:input Step:0 Target:message StateIndex:2 ToolCallID: Group:-1 Alternative:-1}] CurrentToolCallFindings:[]} want score 20 / 1 findings`

- **shared-invoke-slice** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchDenialIsolation$ -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:543: [{0 999}]`

- **omit-invoke-copy** (agent/dispatch.go): `rtk proxy go test ./agent -run ^TestChildScopeDenialCarrier$/owned_receipt -count=1 -timeout=30s`

  Failure: `child_scope_denial_test.go:92: receipt={Content:xxxxxxxx IsError:false Preview: Truncated:true Provenance:null Attrib:<nil> Context:<nil> RouteOutcome:<nil> Origin:model ChildScopeDenials:[{Task:2 Requests:999}]}`

- **omit-interceptor-copy** (agent/interceptor.go): `rtk proxy go test ./agent -run ^TestChildScopeDenialCarrier$/owned_callbacks -count=1 -timeout=30s`

  Failure: `child_scope_denial_test.go:123: aliased source=[{Task:2 Requests:7}] inspection=[{Task:2 Requests:999}]`

- **omit-observer-copy** (agent/dispatch.go): `rtk proxy go test ./agent -run ^TestChildScopeDenialCarrier$/owned_callbacks -count=1 -timeout=30s`

  Failure: `child_scope_denial_test.go:133: observer mutated canonical result`

- **wire-agent-tool.go** (agent/tool.go): `rtk proxy go test ./agent -run ^TestChildScopeDenialCarrier$/JSON -count=1 -timeout=30s`

  Failure: `child_scope_denial_test.go:39: wire evidence={"Content":"innocent","IsError":false,"Preview":"","Truncated":false,"Attrib":null,"Context":null,"RouteOutcome":null,"ChildScopeDenials":[{"Task":2,"Requests":7}]}`

- **wire-agent-interceptor.go** (agent/interceptor.go): `rtk proxy go test ./agent -run ^TestChildScopeDenialCarrier$/JSON -count=1 -timeout=30s`

  Failure: `child_scope_denial_test.go:39: wire evidence={"StateIndex":0,"Role":"","Origin":0,"ToolName":"","ToolCallID":"","Content":"innocent","Alternatives":null,"ChildScopeDenials":[{"Task":2,"Requests":7}]}`

- **wire-private-count** (agent/tools/dispatch.go): `rtk proxy go test ./agent/tools -run ^TestDispatchRequestDenials$/private -count=1 -timeout=30s`

  Failure: `dispatch_denial_test.go:313: private wire={"results":[{"summary":"","stop_reason":"","model":"","LeakedDenials":17}]}`
