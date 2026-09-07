# #374 — Compact the active session: spec and TDD plan

**Status:** Approved by Keith after the Gemini-feedback revision and implemented on `feat/374-compact`. All implementation slices and the whole branch passed independent review. Integrated race tests, complete lint, and the required full Docker CI gate passed. PR preparation is complete; merge remains outside the approved scope.

**Goal:** Add `/compact` to compress the active persistent conversation on demand, report before/after stored-history token estimates and whether it changed, and preserve prior state on cancellation or failure before commit.

**Architecture:** Add one public Runtime thread operation over the existing durable compression and session-store path. Reuse thread reservations, `conversation.CompressMessages`, the progressive summarizer, SQLite snapshot replacement, and the CLI session reload. No new summarization algorithm, dependency, schema, or event protocol.

**Issue:** [#374](https://github.com/kstruzzieri/go-llm/issues/374), parent #343. Issue body verified directly through GitHub. Source inspected at `654f1ccb73f2ccf5db30bd24a7415ca9e14eb243`, also the current local `origin/develop` ref. Refresh the remote before implementation.

**Approved document destination:** `docs/plans/2026-09-06-golem-374-compact-spec-plan.md` in the feature worktree. This draft stays outside the checkout while approval is pending.

**Execution:** After approval, use `superpowers:subagent-driven-development` or `superpowers:executing-plans`; finish the red/green and review cycle for each task before advancing.

## Context and scope

The handoff's `golem.Session` reference is stale: there is no exported Session type at this base. The durable implementation is `golem/session.go:161`, called after successful turns, and it delegates to `conversation/compress.go:77`. `agent.RecencyCompactor` instead drops temporary request context and cannot implement this feature.

The repository has no `docs/ai/PROJECT-CONTEXT.md`, `docs/ai/ai-kit.yaml`, or local `docs/ai/safety-rules.md`. Planning uses root `CLAUDE.md`, the supplied AGENTS instructions, `.claude/workflow.md`, the handoff, and shared safety rules. No repository-specific sensitive-path map is available; the public API and durable-history/consent changes are explicit review items below.

Inspected entry points:

- `golem/session.go:95`: completed-turn persistence; compression failures remain warnings after that turn is durable.
- `golem/runtime.go:714`: same-thread reservation; `:764`: shutdown and cancellation.
- `conversation/compress.go:77`: rolling-summary consolidation and four-exchange retention via `conversation/trim.go:254`.
- `conversation/store.go:37`: atomic conversation and search-index replacement.
- `cmd/golem/repl.go:146`: slash dispatch; `:263`: scoped turn interrupts; `:520`: reload CLI history after Runtime persistence.
- `cmd/golem/main.go:1585`: summarizer construction and disabled-compression policy.
- `cmd/golem/destination_admission.go:101`: existing destination-consent gate.

## Proposed specification

### D1 — One public thread operation; refuse busy threads

Add `Runtime.CompactThread(ctx context.Context, threadID string) (CompactionReport, error)`.

The new `golem.CompactionReport` has only `TokensBefore int`, `TokensAfter int`, and `Changed bool`. It describes persisted session history, independently of the agent package's temporary-context report.

- Require a nonempty thread ID bounded by the existing 256-byte limit; malformed input matches `ErrInvalidRequest`.
- Use the existing `activeThreads`, `mu`, and wait group to reserve this thread for the complete load/summarize/save operation. Never hold the global mutex across store or summarizer calls.
- A turn or another compaction already reserved on the same thread returns `ErrRunConflict` immediately. Different threads remain independent.
- A turn attempting reservation during compaction also receives `ErrRunConflict`; every turn reserved after successful compaction reloads its saved state.
- Reuse the current active-operation cancellation shape for the thread reservation. Under `r.mu`, `Close` marks the runtime closed and calls each cancellation function in **both `r.active` and `r.activeThreads`**. It then unlocks, waits for the existing wait group, and closes owned resources. Stateful turns occur in both maps; canceling their context twice is safe, so no deduplication set is needed. Register each operation in the wait group exactly once under the same lock that checks closed state, and release it exactly once. A compaction does not invent a RunID or enter the public `Cancel(runID)` namespace.
- Closed runtimes return `ErrClosed`. Caller context and Runtime shutdown cancel work. No run events or new snapshot fields are introduced.
- Add `ErrCompressionUnavailable` for disabled compression or a missing summarizer. Respect the Runtime's existing `compress` decision, including `DisableCompression`, even if a summarizer was supplied.

Immediate refusal matches existing Run behavior and avoids a queue, queue cancellation rules, and hidden delayed mutations. A second summarization path would violate the issue and is excluded.

### D2 — Force the existing compressor to its retention floor

Extend the existing private `compressConversation` helper with an explicit force input. Automatic callers pass false; CompactThread passes true.

Automatic compression keeps its half-input-ceiling trigger, 512-token summary-content allowance, and four-exchange floor. The reserve also includes the rendered summary envelope and any larger known summary cost. If actual generated output, including quoting, still exceeds the history budget, fold additional evictable history into that summary before saving. Each additional pass must remove more raw messages; stop at the retention floor, which may itself exceed the budget. Failure during any pass preserves the original snapshot. Forced compression bypasses the outer threshold **and** passes a raw-history target of zero to `CompressMessages`. Changing only the outer threshold would still no-op for small histories.

The existing compressor remains responsible for retention and summarization:

- Keep the newest four completed user/assistant exchanges, including their complete tool chains; preserve system messages and any unresolved tool-call tail.
- Fold older messages into the existing bounded model-backed summary. Do not use `FallbackSummarizer` in production.
- Supply the prior summary and newly evicted messages separately; replace prior content and increment summary `MessageCount` only for newly evicted messages.
- If a prior summary exists but no raw messages can be evicted, allow its existing summary-only consolidation behavior.
- Consecutive `/compact` commands without an intervening turn can therefore each invoke the summarizer. The second call still invokes it when the result will be identical; equality is determined only after the response. Document that `(unchanged)` can still incur a model request. An identical result skips Save; do not add a last-compacted marker, cache, or separate repeat-suppression policy.
- A missing/empty conversation, or at most four complete exchanges without a prior summary, is an unchanged result with no summarizer call and no save. A missing ID is not inserted.
- Identical retained-message count, summary content, and summary message count is also unchanged, even if consolidation called the summarizer. Do not save or refresh CLI state for an unchanged result.
- Blank summary output is an error, as today. A successful changed summary can have equal or greater estimated size; `Changed` describes content replacement, not guaranteed token savings. Do not add a new rejection policy for such output.

### D3 — Report stored-history estimates accurately

Factor one private `estimateStoredHistory(current conversation.Conversation) int` helper in `golem/session.go`. Both the automatic trigger and compaction report use it:

- Use `conversation.CharRatioEstimator(4)` and `conversation.EstimateMessagesTokens` for non-system stored message contents and tool metadata.
- Add the estimated rendered `agent.DurableSummaryPrompt` only for a nonblank summary, trimming summary whitespace consistently with agent materialization.
- Empty history without a summary estimates zero.

This intentionally corrects the existing trigger's eight-token charge for an absent summary. With an 8192-token input ceiling and no summary, 4096 stored-history tokens do not trigger automatic compression; 4097 do. The policy and configured limits otherwise stay unchanged.

These are **stored-history token estimates**, not #63's full live input-pressure totals. Live pressure also prices the application/system prompt, tool schemas, transport framing, and current turn, and currently uses different message filtering/accounting. #375 can reuse this history helper inside golem when it adds inspection, but this issue does not add a second public pressure/inspection API or claim those totals are interchangeable. Keep the helper pure and package-private; extracting an internal package or promoting an API can wait for an actual consumer needing that boundary.

### D4 — Commit determines success; failures preserve prior state

Load the stored conversation, form a separate candidate through the existing compressor, and save only an actual change through the existing SessionStore.

- Check context before loading/work and after summarization, including when a summarizer ignores cancellation and returns valid text. Check again before saving.
- Use the cancellable operation context for Save. Do not claim an uncancellable finalization before this save.
- SQLite's existing transaction is the commit boundary for conversation, summary, metadata, and search index. A failed/canceled save leaves the old snapshot intact. No compensating rollback write or migration is added.
- Save returning nil means success. Do not inspect cancellation afterward and misreport a committed change as canceled.
- Return `Changed=true` and the candidate's after estimate only after Save succeeds. On an error after loading, keep the report's `Changed=false` and `TokensAfter=TokensBefore`; before a successful load, report fields are zero.
- Preserve underlying errors with wrapping, including context errors and `conversation.ErrEmptySummary`; save failures also match existing `ErrSessionPersistence`. Manual compression errors are returned to its caller. Automatic post-turn compression keeps its existing warning-only semantics.
- Preserve ID, title, creation metadata, retained message bytes, and summary accounting; successful Save updates normal store timestamps. Runtime's existing database-file hardening remains a post-commit warning if it fails.
- Document the injected SessionStore contract: Save must replace the full snapshot atomically; a returned error must leave the old snapshot intact, and a successful commit must return nil even if cancellation races afterward. Load results and summarizer inputs must be treated as read-only. The built-in SQLite path already has the required transaction; caller-owned stores are responsible for their implementation. Serialization across separate Runtime instances remains the caller's responsibility.

### D5 — REPL command behavior

`/compact` accepts no arguments and applies to `sess.session.id`. Keep it REPL-only: no one-shot flag, subcommand, background job, or new slash aliases.

Literal output contracts, including trailing newline:

| Situation | Output |
|---|---|
| Extra arguments | `usage: /compact` |
| Session disabled | `session disabled (--no-session)` |
| Runtime absent | `compact: runtime unavailable` |
| Compression disabled/unavailable | `compact: compression unavailable` |
| Success example | `compact: history token estimate 100 -> 89 (changed)` |
| No-op example | `compact: history token estimate 80 -> 80 (unchanged)` |
| Caller cancellation | `compact: canceled; session unchanged` |
| Other failure before commit | `compact failed: <redacted error>` |

The numeric examples are fixture values; production figures always come from the Runtime report. Reuse `runFailureMessage` to redact errors and do not print conversation/summary text.

The command handler has the existing `handleThink` signature shape. A private `replSession.interrupts` field carries the existing interrupt channel; `runREPL` initializes it on every entry, including nil. Direct handler tests can leave it nil. Preserve the existing `dispatchSlash` signature and its callers.

1. Validate arguments, session/runtime availability, and startup compression disablement before admission or model work. Retain `f.noCompress` as a private `replSession.noCompress` field, following the existing `noGitContext` pattern; Runtime still enforces the authoritative policy.
2. Create a child operation context through private `interruptContext(ctx context.Context, interrupts <-chan struct{}) (context.Context, context.CancelFunc)`, shared with `runOnce`. For a nil interrupt channel, return the child context and its original cancel function without a goroutine. Otherwise drain a stale interrupt, start one watcher selecting between the interrupt channel and `childCtx.Done()`, and have the watcher close a `stopped` acknowledgement when it exits. The watcher calls only the original context cancel function. The returned cancel/cleanup calls that same original cancel function, then waits for `stopped`; cancellation plus receiving from a closed acknowledgement is safe on repeated and concurrent calls. Do not add a separate close-on-cleanup `done` channel or a deduplication/once wrapper. `runOnce` calls cancel explicitly on an interrupted approval, passes it to the checkpoint journal, and also defers it: a cleanup that closes a channel on every call would panic. Cleanup must finish before the next prompt/turn starts; an interrupt racing with cleanup belongs to the finishing operation, and no old watcher may consume interrupts after cleanup returns.
3. Call existing `destAdmission.ensure` when present before CompactThread. This preserves the existing batch-consent behavior after `/grants clear`; denial/cancellation makes no compaction call or save. Eligible no-op histories may pass this gate before Runtime discovers no work. Correct the shared `lineSourcePromptYN` adapter to read consent with `ReadAnswer`, mapping `errInterrupted` directly to `context.Canceled`, as the existing tool approver does. Its current `ReadGoal` can reenter a blocking terminal read before the async watcher observes Ctrl-C. Retain existing yes/no/EOF semantics and use the answer reader's existing input protections; no editor internals change.
4. Print changed/unchanged using the returned report. After a durable change, reload CLI history/summary through `session.switchTo` with the parent REPL context. A fresh Ctrl-C can cancel the child context after Save commits but before reload; this is why reload uses the parent, even though deferred cleanup normally runs after reload. If reload fails, including because the parent itself was canceled, keep the successful report and add the existing `warning: session state not refreshed: ...` form, with redacted error text. A cache-refresh failure cannot undo the durable change or become a compaction failure.
5. Return to the prompt. `/compact` never becomes a model goal or input-history entry; `/edit` text that happens to equal `/compact` still follows the existing forced-goal behavior.

Compaction does not clear tool grants, reset model options, switch the session, or modify memories/checkpoints. Destination reapproval, when necessary, follows the existing consent scope.

## Implementation steps and dependencies

Every behavior task follows test-first red/green: write the specified failing check, run it and record the intended failure, implement the minimum change, run focused checks, prove new assertions with targeted mutations, then run the repository code-review and criticize-review cycle. Revert every mutation before proceeding. No test or implementation file is written before Keith approves this document.

### 1. Establish the feature worktree and approved artifact

- [x] Refresh `origin/develop`, verify #373 is present, and create linked worktree `.worktrees/374-compact` on `feat/374-compact` from that remote branch. Use the worktree skill and verify ignore rules first; never implement or commit on develop.
- [x] Copy this approved document to the destination above and point the worktree task checklist to it.
- [x] Confirm lane ordering: #374 follows #373 and precedes #375/#521. Lane 4 owns runtime.go/repl.go; Lane 2 canary and Lane 5 trust hunks must route through this owner or rebase after this merge.
- [x] Route the single `main.go` initializer hunk for `noCompress` through Lane 3, which owns its audit subcommand hunk, or apply it only after that lane merges and this branch rebases. Do not edit that shared file concurrently.

Dependency: approval. This is workspace setup only; no source changes.

### 2. Shared forced-compression policy and estimates

Files: modify `golem/session.go`; extend `golem/session_test.go`.

- [x] Write failing tests for the helper's forced-below-threshold behavior, normal-trigger boundaries, summary-inclusive estimates, retention floor, progressive replacement/counts, and no-ops.
- [x] Factor `estimateStoredHistory`, add the force argument, retain existing `CompressMessages`, and make `saveThread` explicitly use automatic mode.
- [x] Verify the existing `conversation/compress_test.go` and `golem/runtime_test.go` automatic-compression cases still pass. No production changes are planned in `conversation/` or `agent/`.

Produces: the shared private compression helper and history estimates consumed by Task 3.

### 3. Public Runtime operation and durable lifecycle

Files: modify `golem/runtime.go` (API contracts/reservation lifecycle) and `golem/session.go` (CompactThread orchestration); add `golem/compact_test.go` in the existing external test package.

- [x] Write failing public-API tests for persistence, cancellation and failure preservation, unavailable/invalid cases, conflicts in both directions, independent threads, shutdown, and next-turn visibility.
- [x] Implement CompactThread, CompactionReport, and ErrCompressionUnavailable; reserve through the existing thread map/mutex/wait group, and make Close cancel both active maps before unlocking and waiting. Do not introduce a RunID for compaction or double-count a stateful turn in the wait group.
- [x] Reuse the existing store and hardening paths, return success only after Save, and document injected-store atomicity and the method's context/report semantics.
- [x] Add a real SQLite close/reopen integration check and run targeted race tests with barrier-controlled concurrency. Reuse `mapSessionStore`, `cloneConversation`, and `captureCaller` from `golem/runtime_test.go` where suitable.

Depends on Task 2. Produces the public method and report consumed by the command.

### 4. REPL wiring, interrupts, admission, and documentation

Files: add `cmd/golem/compact.go` and `cmd/golem/compact_repl_test.go`; modify `cmd/golem/repl.go`, `cmd/golem/destination_admission.go`, `cmd/golem/destination_admission_test.go`, the coordinated one-line field initializer in `cmd/golem/main.go`, `README.md`, and add `changelog.d/374-compact.md`.

- [x] Write failing command tests for every literal output, help/argument validation, disabled startup wiring, cancellation, consent, cache refresh, and goal/history boundaries listed below.
- [x] Add `handleCompact`, register `/compact`, add its help entry, and initialize the private session interrupt channel at `runREPL` entry. Keep `dispatchSlash` and all existing caller signatures unchanged.
- [x] Extract/reuse `interruptContext` in `repl.go` using the child context as stop signal and one exit acknowledgement; prove returned cleanup remains safe when called repeatedly/concurrently and joins before the next prompt. Retain normal-turn and checkpoint cancellation semantics.
- [x] Add the private disabled flag and coordinated startup initializer, gate destination consent before compaction, switch its shared prompt adapter to `ReadAnswer` with synchronous interruption mapping, and reload the CLI cache only after a saved change.
- [x] Document the command, four-exchange retention, history-estimate scope, progressive-summary behavior (including repeated commands that make a model request yet report unchanged), disabled modes, and cancellation/commit behavior in the existing README section and the changelog fragment. Never edit CHANGELOG.md.

Depends on Task 3 and coordination of the main.go hunk. This is the complete user-visible deliverable.

### 5. Final verification and review-ready branch

- [x] Complete code-review/fix cycles until clean and criticize-review after each implementation task. Review the final diff against every decision and test below.
- [x] Run the focused race command, complete lint, then required full container gate; capture each actual exit code directly.
- [x] Check only intended files changed, all targeted mutations were reverted, and shared lane hunks remain owned and reviewed.
- [x] Prepare the PR with `Closes #374`, validation evidence, and no emojis. Apply the repository's ordinary integration workflow after implementation; do not merge as part of this planning approval.

Depends on Tasks 2–4 and clean review.

## Tests and explicit expectations

Test cases belong in the task that introduces the behavior; they are not postponed to Task 5. Use literal expected output, model request strings, and stored snapshots rather than calling the production estimator/formatter to build expectations. Use deterministic channels and bounded waits rather than sleeps for concurrency tests. A test summarizer signals `started`, then selects between `release` and `ctx.Done()` so Close can cancel it without a test-side release. Use existing callback seams and small closures instead of adding a production barrier abstraction. When testing shutdown's wait boundary, acknowledge cancellation and then hold callback exit behind a second test barrier to prove resources stay open until the operation returns.

| Area / test file | Required behavior and mutation it detects |
|---|---|
| Force and report — `golem/session_test.go`, `golem/compact_test.go` | Five exchanges, ten 40-character contents, summary `SUM`: exact **100 -> 89**, changed true; summarizer gets oldest two messages, store gets newest eight and summary count 2. Ordinary mode does nothing with the same small fixture. Detect bypassing only the outer trigger or changing floor/estimator. |
| Estimate boundaries — `golem/session_test.go` | Empty history **0**; eight 40-character messages **80**; those messages plus `SUM` summary **89**. With ceiling 8192, raw **4096** does not trigger, **4097** does. Also test summary-inclusive exact threshold and a multibyte fixture to distinguish character from byte accounting. Detect missing/phantom summary cost or changed threshold comparator. |
| Retention — `golem/session_test.go` | Pin exact fields/bytes for the newest four exchanges, system message, tool arguments/results, and unresolved tail; evict an older complete tool exchange together. Detect a one-exchange floor shift or tool-chain split. |
| Progressive summaries — `golem/session_test.go`, `golem/compact_test.go` | Prior `EARLIER`, count 12, one newly evicted pair: exact callback prior/pair and count **14**, replacement content. Summary-only rewrite receives no new messages and keeps count. Different equal-token summaries still produce changed true and one save. Detect losing prior/counts or equating changed with token savings. |
| No-ops — `golem/compact_test.go` | Missing/empty ID state and one-to-four complete exchanges without summary: zero summarizer calls, zero saves, no inserted row. Identical summary-only rewrite: one summarizer call, zero saves; before=after and changed false. Detect unconditional Save or forced model work at the floor. |
| Consecutive commands — `golem/compact_test.go`, `cmd/golem/compact_repl_test.go` | Execute `/compact` twice with no intervening goal on the five-exchange `SUM` fixture. First report is **100 -> 89 (changed)**, second **89 -> 89 (unchanged)**. Across both commands assert two summarizer invocations and one Save; the second callback receives prior `SUM` and zero newly evicted messages. Detect an unintended repeat-suppression shortcut or saving unchanged state. |
| Unavailable/validation — `golem/compact_test.go` | Empty and oversized IDs, closed runtime, absent summarizer, and DisableCompression with a supplied summarizer return the specified errors; no model/save work. Detect bypassing authoritative disablement. |
| Failure preservation — `golem/compact_test.go` | Load failure, malformed Load result, summarizer error, whitespace-only summary, and atomic Save failure retain exact prior data; error identity remains discoverable. Save failure never reports changed. Detect swallowed errors, premature publication, or loss of summary. |
| Cancellation and commit — `golem/compact_test.go` | Already canceled request; cancellation during blocked summarization; valid summary returned after cancellation; cancellation during context-aware Save: all preserve prior data. A successful Save followed by cancellation still returns changed success. Detect missing post-summary checks or a post-commit cancellation override. |
| Concurrency and Close — `golem/compact_test.go` | Active turn rejects CompactThread; blocked compaction rejects same-thread Run and CompactThread; different thread progresses; canceled/failed compaction releases reservation. Close must cancel a blocked thread-only compaction, an ordinary stateful turn on another thread, and a stateless turn, then join all before closing resources and reject later calls. Separately exercise a blocked compaction Save. Deleting either map's cancel loop must fail the corresponding test; removing the wait must fail the resource-lifetime assertion. No manual summarizer release may be needed to observe cancellation. |
| Durable integration — `golem/compact_test.go` | Default SQLite store close/reopen and injected store reuse preserve exact messages, title, summary/count; next Run contains literal `Previous conversation summary:\nSUM` plus retained history. Check existing search-index behavior through reopened store. Detect wrong store path or non-durable state. |
| CLI outputs and disabled startup — `cmd/golem/compact_repl_test.go` | Exact output table, `/help` entry, invalid arguments, --no-session, runtime unavailable, -no-compress skips admission/model/save, and startup actually wires the flag. Detect invented hardcoded counts and flag/wiring omissions. |
| CLI lifecycle — `cmd/golem/compact_repl_test.go` | Successful change refreshes cache; next goal and `/resume` see saved history, ID stays fixed. `/compact` creates no goal/history entry; `/edit` yielding `/compact` remains a forced goal. A cache reload error retains successful output plus redacted warning. |
| CLI cancellation after commit — `cmd/golem/compact_repl_test.go` | Inject a Runtime SessionStore using the CLI fixture's same underlying SQLite store. Its Save commits, signals `committed`, waits for the child context's cancellation, then returns nil. Send Ctrl-C only after the commit signal. Assert changed-success output, cache refresh using the still-live parent context, no canceled/failed message, and a usable next prompt. This injection is test-only; do not change production store wiring. |
| CLI interrupts — `cmd/golem/compact_repl_test.go` | Fresh Ctrl-C during summarization cancels and returns to prompt; next goal works, stale interrupt does not cancel compact, watcher cannot steal the next turn's interrupt. Existing normal-turn cancellation tests still pass. |
| Interrupt cleanup contract — `cmd/golem/compact_repl_test.go` | With a nonnil interrupt channel, call cleanup twice and from concurrent goroutines; every call returns without panic/deadlock. After cleanup returns, send an interrupt and prove no retired watcher receives it. Also cover nil channel and parent cancellation. Preserve the existing interrupted-approval and checkpoint callback paths, which may cancel explicitly before deferred cleanup. Detect Gemini's unconditional channel-close failure and a missing watcher join. |
| CLI consent — `cmd/golem/compact_repl_test.go`, `cmd/golem/destination_admission_test.go` | `/grants clear` then `/compact` reapproves before summarizer work; denial/canceled prompt makes zero summarizer calls/writes. An actual editorSource fixture receiving Ctrl-C must synchronously abort consent without needing another input byte; fake cancellation callbacks alone are insufficient. Pin unchanged yes/no/EOF behavior and the answer reader's rejection of invalid approval input. Grant/model/session settings unrelated to compaction remain intact. No external provider calls. |

Targeted mutations must make every new behavioral assertion fail in its relevant check: disable force, alter floor, remove prior/count accumulation or summary cost, bypass cancellation/reservation/consent, omit save, save no-ops, swallow errors, falsify report, skip reload, or leave an interrupt watcher alive. Reuse the repository's test framework; no new framework or mutation runner.

Commands below run only after approval, with every worktree command beginning with explicit `cd`. Preserve the `env -u GOROOT` prefix for every Go invocation, including any additional focused `-run` checks:

- Focused suites: `cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/374-compact && rtk proxy env -u GOROOT go test ./conversation ./golem ./cmd/golem`
- Focused race/lifecycle: `cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/374-compact && rtk proxy env -u GOROOT go test -race ./golem ./cmd/golem`
- Lint: `cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/374-compact && rtk proxy golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`
- Full gate: `cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/374-compact && rtk proxy docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`

Do not pipe tests to tail or infer success from filtered output. A test suite has not been run by this task during planning; the pasted Gemini review mentions a test command but supplies no final test output or exit code, so it is not implementation-validation evidence.

## Gemini feedback disposition

- **Close cancellation:** accepted and made concrete in D1/Task 3: cancel both maps, tolerate duplicate context cancellation, wait once per registered operation, and test stateful/stateless/thread-only lifetimes.
- **Watcher join:** accepted with a correction in D5/Task 4. Gemini's sample closes `done` on every cleanup call, which conflicts with runOnce's repeated cancellation calls. Use child-context cancellation as the stop signal plus one exit acknowledgement instead; no extra synchronization wrapper is needed.
- **Post-commit reload:** retained and tested explicitly. The relevant race is Ctrl-C after a successful Save and before reload, rather than the ordinary order of deferred cleanup.
- **Repeated compaction:** document and test that identical summary-only output still required invoking the summarizer; unchanged means no saved replacement, not necessarily no model work.
- **#375 reuse:** name the pure private helper now; defer a new package or exported inspection API until an actual consumer needs it.
- **Deterministic concurrency:** use the existing injectable summarizer/store seams with cancellation-aware channel barriers. No new production abstraction or test framework.

The estimator correction and consent-reader fix were already in the draft and remain. These revisions clarified implementation and regression checks without broadening the public feature; Keith subsequently approved the revised document.

## Risks and human sign-off required

| Task | Risk | Approved boundary |
|---|---|---|
| 1 | Shared-file conflicts | Preserve lane ownership; route the main.go initializer through Lane 3 or rebase after its merge; serialize Lane 2/5 runtime/repl hunks through Lane 4. Existing unrelated `copilot.json` and `output/` remain untouched. |
| 2 | Blast radius: shared automatic compression arithmetic | Approve the explicit eight-token absent-summary correction and summary whitespace consistency. Trigger policy/floor/reserve stay the same; boundary tests are required. |
| 3 | Blast radius and data: public API, Runtime shutdown, durable-history replacement | Additive method/result/error only; no protocol/schema migration. Caller-owned SessionStore implementations must meet the stated atomic Save contract. Runtime concurrency remains per instance. |
| 4 | Blast radius and data: interrupt reuse, shared destination-consent reader, CLI cache | No new authority or providers; retain disabled-compression routing and existing consent. Use the existing answer reader for all callers of the consent adapter and verify its Ctrl-C/yes/no/EOF semantics. Cache-refresh failure remains a warning after successful commit. |
| 5 | Integration | Review and full CI are required before a PR is ready. Merge is outside this planning approval. |

Compaction intentionally replaces older raw history with a lossy summary. It has no new undo/archive mechanism; this is the existing durable-compression behavior made explicit. There is no database migration or infrastructure operation to roll back. Cancellation/failure before commit retains the old snapshot; success does not promise recoverability of evicted raw text.

**Approved scope:** D1–D5 and Tasks 1–5, including immediate busy-thread refusal, the four-exchange forced target, summary-only rewrites, honoring -no-compress, history-only estimates with the small trigger correction, commit-based cancellation semantics, and using the existing answer reader to make consent cancellation reliable. Implementation is authorized; merge remains outside this approval.

## Execution results

Source and tests were verified at `d1fb71f`; the subsequent plan update changes documentation only.

- Integrated host race command: `env -u GOROOT go test -race ./golem ./cmd/golem`, exit 0 (golem 20.153s; cmd/golem 237.578s).
- Complete host lint: `env -u GOROOT golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`, exit 0, zero issues.
- Required full gate: `docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`, exit 0. Formatting, lint, repository-wide race tests, and compile smoke checks passed.
- All 49 targeted mutations were detected and restored: 13 shared-policy, 18 Runtime, 4 consent-reader, and 14 CLI mutations.
- Independent scoped reviews and final Astra XHigh whole-branch review passed with no outstanding findings. The consent test review added bounded waits before its clean re-review.

Mutation limits: the immediate pre-Save cancellation guard overlaps the post-summary guard and was source-reviewed rather than independently mutated. The missing-watcher-join check uses 64 bounded scheduling attempts; correct cleanup cannot consume the next interrupt, while detection of the broken select path is probabilistic (the mutation failed at attempt 2).

Execution decisions, in order:

1. Run the user-approved disjoint consent/docs workers alongside core work despite the skill's generic serial-implementation guidance. Exclusive file ownership controlled the risk of overlapping edits.
2. Keep staging, commits, and mutation-window coordination with the orchestrator because workers share one checkout. A coordination mistake could let tests observe intentionally broken source or stage another worker's changes.
3. Extract briefs directly because the bundled brief script invoked a non-executable sibling. Binding spec content was retained; the risk was omitting a requirement, checked by independent reviews.
4. Add one internal compaction resource-lifetime test in `golem/session_test.go` using the existing private `closeOwned` seam. Public tests cannot directly observe resource closure; the cost is one extra test hunk, with no production hook or API.
