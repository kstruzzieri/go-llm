# Conversation CAS Save (#473) — Spec and Implementation Plan

**Status:** Approved by Keith on 2026-09-10 ("begin execution"). S1–S5, including the documented compatibility limits, are authorized; implementation is in progress.

**Review update (2026-09-10):** Gemini's feedback has been checked against the pinned source and the SQLite documentation. Accepted clarifications and item-by-item dispositions appear below. The reviewer's recommendation to execute is not Keith's implementation approval.

**Goal:** Prevent competing saves of the same existing conversation from silently replacing one another's transcripts, and make the rejected save observable to Golem callers and users.

**Architecture:** Add an integer revision to the existing conversation row and compare it inside the existing save transaction. Keep the value-based Save signature; a successful save commits exactly the submitted revision plus one. Reuse the existing migration tracker, search projection transaction, error propagation, and compression pipeline.

**Tech stack:** Go 1.25, database/sql, existing modernc.org/sqlite dependency; no new dependencies.

**Spec:** The proposed spec below is binding on the implementation steps in this document.

For implementation agents: use the approved spec and execute each task through red/green testing and review, using superpowers:subagent-driven-development or superpowers:executing-plans. This document contains no implementation or test code, as requested.

## Evidence and corrections to the handoff

- Issue: [#473 — compare-and-swap save with revision](https://github.com/kstruzzieri/go-llm/issues/473), open; its body and comments were read through GitHub. It blocks #474.
- Source baseline: origin/develop at `14af019b8fdeda251ad1a25a84393a1c90fe25b8`, verified against GitHub. The main local checkout is older (`654f1cc`); implementation must start from refreshed origin/develop.
- `conversation/store.go:37-99`: Save currently replaces the full snapshot unconditionally; transcript and search changes already share one transaction.
- `conversation/store.go:216-242`: the second upsert is the search metadata projection, not an independent conversation writer. It needs the successful main-row CAS as its gate, not a separate revision.
- `conversation/migration.go:15-31,111-166`: migrations already use `conversation_schema_version`, currently v3. Use migration v4; introducing PRAGMA user_version would create a second version authority.
- `golem/session.go:162,190,209`: explicit compaction, raw-turn persistence, and automatic compression all save. #374 is already merged at the baseline.
- `golem/runtime.go:488-493`: raw persistence errors already propagate to Run and run.failed. The CLI demotes them to success at `cmd/golem/repl.go:428-436`, so a store-only change is insufficient for explicit refusal semantics.
- `cmd/golem/session.go:191`: recordMessages is another save helper, currently used only by tests. Its cached revision still needs to obey the same contract.
- Guidance loaded: repository CLAUDE.md, the supplied handoff, RTK.md, and the referenced safety rules. This repository has no docs/ai/PROJECT-CONTEXT.md, docs/ai/ai-kit.yaml, or docs/ai/safety-rules.md at the baseline; no repository-specific sensitive-path classification is available. Tests will use synthetic data in temporary databases.

## Approved spec

### S1. Revision and API contract

Add exported `Conversation.Revision int64`. Keep `Save(context.Context, Conversation) error` in both conversation.Store and golem.SessionStore.

| Submitted revision | Database state | Result |
|---|---|---|
| 0 | ID absent | Create revision 1 |
| 0 | ID present | Typed conflict; preserve stored snapshot |
| Positive r | ID present at r | Replace snapshot and commit r+1 |
| Positive r | ID absent or at another revision | Typed conflict; do not insert or replace |
| Negative or maximum int64 | Any | Validation error; no write |

Successful Save means exactly r+1 is durable. Its input is a value and remains unchanged. A caller retaining that snapshot must advance its local revision by one only after Save returns nil; it must not fetch a newer revision and attach it to stale content. Load returns the persisted revision. List/Search remain read-only projections and do not become save tokens.

Use exported `ErrConflict` plus `*ConflictError` containing `ID string` and `ExpectedRevision int64`; the type unwraps to ErrConflict. Both errors.Is and errors.As must survive store, runtime, and compaction wrapping. The error contains no transcript/summary text and states that the snapshot was not saved. No current-revision lookup is needed to construct it.

Keep these errors in conversation; do not add golem aliases. Keep invalid-revision failures as contextual validation errors, consistent with the existing missing-ID validation. Invalid-input tests assert an error, no ErrConflict match, and unchanged storage; they do not need to parse error messages or introduce a new ErrInvalidRevision sentinel.

SQLite busy, cancellation, encoding, I/O, and commit failures remain their original errors, wrapped with context. Only a valid attempted CAS that affects zero rows becomes a conflict.

### S2. Atomic save and migration

Migration v4 adds a non-null INTEGER revision column with default 1 through the existing migration registry. Existing rows load at revision 1. The migration does not rewrite titles, message JSON, summary content/count, timestamps, search metadata, or FTS contents. Fresh databases apply v1 through v4; repeated opening is idempotent.

Within the existing Save transaction:

1. A revision-zero request performs insert-only with conflict-do-nothing and stores revision 1.
2. A positive revision performs an update guarded by both ID and expected revision and advances revision by exactly one. A missing row must never be resurrected through the update path.
3. Check RowsAffected before any search-index mutation. Zero returns ConflictError through rollback.
4. Update the existing search metadata/FTS projection only after CAS succeeds, then commit all changes together.

The conditional INSERT or UPDATE must be the first data statement after BeginTx. Save must not query the revision or existence first, inside or outside the transaction. The pinned SQLite driver starts a plain BEGIN by default. In WAL mode, a preliminary read can establish a snapshot that becomes unwritable after another connection commits, producing SQLITE_BUSY_SNAPSHOT. Starting with the CAS write avoids that read-to-write upgrade; no transaction-mode change or retry loop is needed. This does not guarantee every lock resolves within the configured timeout: an actual SQLite busy/timeout error remains an error, not a fabricated CAS conflict. [SQLite isolation](https://www.sqlite.org/isolation.html), [busy timeout](https://www.sqlite.org/c3ref/busy_timeout.html).

Preserve CreatedAt on updates; keep existing UpdatedAt and nil-message normalization behavior. Identical-content saves still advance the revision. An index failure must roll back the transcript, revision, timestamps, and index together.

Keep creation timestamps owned by the store. Both existing update clauses omit created_at, so they already preserve their stored values. Do not change the search helper to trust caller-supplied CreatedAt for a speculative schema refactor, and do not add a timestamp SELECT before CAS. Pin the stored main-row and search-row timestamps in update/conflict tests.

Use separate database handles to one stable temporary file for concurrency tests, each with one open connection, WAL, and busy_timeout=5000 configured using Exec. Initialize/migrate first. Each worker must finish its Load and signal readiness before the coordinator releases a shared start channel; then both may Save. Use bounded waits and cleanup that releases/drains workers even on failure; never time.Sleep for synchronization. This guarantees both snapshots start at the same revision, not a particular scheduler ordering or simultaneous acquisition of SQLite's single writer lock. No process-local mutex can substitute for the SQL predicate.

### S3. Runtime and compaction

The runtime loads a full Conversation before each independent turn, so loaded revision already follows the snapshot into saveThread. Compression already copies the full Conversation; retain that behavior.

- After raw Save succeeds, advance the candidate revision before retaining it or passing it to automatic compression. After the compressed Save succeeds, advance the retained compressed candidate too.
- If raw Save conflicts, return the completed agent.Result plus an error matching both ErrSessionPersistence and ErrConflict, preserving ConflictError through errors.As. Emit one terminal run.failed with code `session_conflict`, not run.finished. Keep the answer in the returned result; persistence failure cannot undo work already performed during the turn.
- If automatic compression conflicts after the raw turn committed, keep the successful run and emit the existing OnWarning callback carrying the typed conflict. Do not replay the completed turn or overwrite the competing writer. Hosts without an OnWarning callback retain the existing quiet best-effort compression behavior.
- If explicit CompactThread conflicts, return the typed error wrapped with ErrSessionPersistence, report Changed=false and TokensAfter=TokensBefore, and preserve the winning snapshot and search index.
- Preserve existing cancellation/finalization and post-commit hardening semantics. A durable raw turn must not become a failed turn because later compression lost its CAS.

Update SessionStore documentation to require the revision contract for injected implementations. Strengthen the existing mapSessionStore test fake so it also checks and advances revisions; an unconditional fake would hide bugs in the raw-save-to-compression handoff.

Retain the existing failure payload shape: its code and message fields, using session_conflict as the code for this new failure category. Do not add expected_revision event metadata: the Go error already exposes ExpectedRevision, and no current event consumer requires another field. A revision counts saves, including compaction; it is not a turn number.

### S4. User-visible refusal

Use refuse-and-notice. No automatic reload-and-save, merge, retry, or model/tool replay.

- Exclude ErrConflict from the CLI branch that demotes ordinary persistence failures to success. Reuse the existing error rendering/redaction and the conflict error's explicit unsaved-snapshot message.
- Interactive use returns to the prompt after showing the error. The losing snapshot is not persisted. An explicit next turn follows the existing fresh-load behavior; it does not replay the rejected turn.
- One-shot and machine output retain the error outcome; machine output carries the runtime's `session_conflict` code. Returned/previously streamed answer content is not represented as a successfully saved transcript. Keep existing output-format rules for error results.
- Ordinary non-conflict disk failures retain their current CLI behavior; this change does not redesign every persistence error.
- /compact already renders returned errors and refreshes its cached state only after a successful change. Verify its conflict notice without adding a second recovery mechanism.
- Add cached revision to the CLI session: load it in openSession/switchTo; submit and increment it in recordMessages; reset it after successful clear and on renew. Failed saves, loads, and deletes leave the prior cached state intact.

### S5. Compatibility and explicit limits requiring approval

The guarantee covers CAS-capable writers saving a continuously existing conversation row. It does not yet cover every lifecycle operation:

1. **Delete/recreate can reuse a revision.** /clear currently deletes a row and retains the same ID. If it is recreated and reaches an old snapshot's revision, that stale snapshot can pass the numeric CAS. The shortest counterexample is A loads revision 1; B deletes and recreates the ID at revision 1; A's stale Save is accepted. No additional updates or unusually long-running turn are necessary. This proposal preserves Delete and /clear semantics and does not claim safety across that lifecycle; deferral is an explicit scope decision, not an evidence-backed claim that this is rare. Closing this gap would require a separate incarnation token or retained revision history and a revised API/plan. Merely checking CreatedAt is not reliable: it is millisecond resolution, caller-modifiable, and Save does not return its generated creation timestamp to callers retaining a newly created value.
2. **Old binaries can bypass CAS.** The additive schema preserves data/read compatibility, but an old unconditional upsert can still replace content without advancing the revision. All processes writing the shared sessions database must be upgraded and restarted together for this guarantee. Preventing old writes with database triggers is additional scope, not part of this migration.
3. **Injected stores must adopt the new behavioral contract.** Signatures stay compatible, but implementations must check and increment revisions. The adjacent Firn MemorySessionStore currently saves submitted values unconditionally; its dependency upgrade needs a corresponding consumer change. No Firn source changes are included in #473.
4. **Simultaneous migration startup is separate.** Existing runMigrations reads the version before its transaction and concurrent initializers can race, failing one startup with a visible error. This plan tests concurrent saves after migration, not a migration-runner redesign.

Integer revisions are recommended over content hashes because they fit the existing API without a returned token or pointer mutation. A pointer Save or revision-returning Save would make advancement explicit but change every store implementation/caller; neither fixes delete/recreate identity by itself. Automatic merge/retry is rejected because independently generated turns and tool effects cannot safely be combined by replacing a snapshot.

No append-only history, force-save endpoint, lock service, new package/dependency, search schema redesign, or work on #474 is included.

## Gemini feedback disposition — 2026-09-10

| Feedback | Decision | Verified reason / plan change |
|---|---|---|
| Never SELECT before the CAS write | Accepted; S2 and Step 2 strengthened | A read-first deferred transaction can fail with a stale WAL snapshot. Keep the CAS as the first data statement and preserve real busy failures. |
| Two-phase concurrency barrier | Accepted; S2 and Step 2 made explicit | Both loads complete before a shared release; bounded waits and no sleep synchronization. This controls starting state, not exact lock interleaving. |
| Re-export ErrConflict and ConflictError from golem | Deferred | SessionStore already exposes conversation.Conversation and documents conversation.ErrNotFound; Firn's concrete store already imports conversation. Keep one error API until a concrete host need justifies aliases. |
| Pass conv.CreatedAt to the search helper on updates | Not adopted | Existing main-row and search-row SQL already preserve created_at. The supplied value can be zero or caller-modified, so it is not a better authority than stored metadata. Add timestamp assertions, not a speculative input change. |
| Add ErrInvalidRevision | Deferred | No current caller has distinct recovery behavior for invalid revisions. Test rejection, non-conflict classification, and unchanged storage directly; no message parsing is required. |
| Add expected_revision to failure-event metadata | Deferred | Typed Go errors already carry it; session_conflict supplies the machine outcome. Keep the event shape unchanged. Revisions do not identify turn numbers because compression also saves. |
| Leave legacy writers outside #473 | Retained as proposed scope, awaiting Keith's approval | A trigger would reject legacy updates while installed; describing that as permanent database breakage overstates the effect. Coordinated writer upgrades are a precondition here, not a general promise of safe downgrades or mixed-version use. |
| Leave delete/recreate outside #473 | Retained as proposed scope, with corrected risk | Revision 1 is reused immediately on recreation. The feedback's claim that ABA needs a hung turn plus exactly R subsequent turns is incorrect. S5 now gives the shortest counterexample; fixing the identity contract still needs a revised plan and approval. |

The source claims above were rechecked against origin/develop `14af019`, including conversation/store.go, golem/runtime.go, cmd/golem/session.go, and the local modernc.org/sqlite v1.46.1 transaction implementation. No implementation or tests were run for this document review.

## Global execution constraints

- Await Keith's approval of this spec and TDD plan before implementation.
- Refresh origin/develop and create an isolated linked worktree at `.worktrees/473-cas-save` on `feat/473-cas-save`; never commit to develop and never use a codex/ branch prefix.
- Start worktree shell commands with `cd <absolute-worktree-path> &&`; prefix executable commands with rtk, using rtk proxy where needed.
- Every Go invocation uses `env -u GOROOT go ...` behind rtk proxy.
- Lane 4 owns golem/runtime.go and cmd/golem/repl.go. Integrate their small contract/classification/error-demotion hunks through the owner, or land after that lane merges and this branch is rebased. Do not send external messages without authorization.
- Use only synthetic test data and temporary databases; do not open or migrate the user's live sessions database during development.
- Add `changelog.d/473-cas-save.md`; do not edit CHANGELOG.md.
- Review each task, challenge the review, fix findings, and repeat until clean. Each new behavioral assertion needs a targeted mutation that makes it fail; do not derive expected revisions or payload bytes from implementation helpers.
- Before a PR, run lint with unlimited issue reporting and the required full Docker gate. PR body includes `Closes #473`; no emojis. This approval does not authorize merge.

## Steps, tests, and dependencies

Each behavior task below includes its own red/green cycle. No tests have been run or claimed to pass during planning.

### Task 1: Establish the revision schema and loaded contract

**Files:** conversation/message.go, conversation/migration.go, conversation/migration_test.go, conversation/store.go, conversation/store_test.go; create conversation/testdata/schema-v3.sql. After approval, copy this document into docs/plans/2026-09-09-conversation-473-cas-save-spec-plan.md in the isolated worktree.

- [ ] Write a literal prior-v3 SQL fixture independently of migrateV1/migrateV2/migrateV3: version table, conversations, search metadata and FTS, fixed timestamps, a tool-call message, and a durable summary with known text/count.
- [ ] Add migration tests that build a temporary file from that fixture, open NewStore, assert schema version 4 and loaded revision 1, and compare raw stored fields and search results to literal expectations. Close/reopen and assert no data/version changes. Update existing fresh/idempotent/v2-upgrade version assertions without weakening their old-content checks.
- [ ] Run the focused migration/load cases and record the expected failure; a missing Revision field may initially produce a compile failure. Then add the field, v4 migration, and Load projection so the cases pass.
- [ ] Keep the fixture immutable; in Step 2 extend its round trip through a real CAS update from revision 1 to 2.
- [ ] Review the diff and mutation-check the migration default, loaded revision scan, and exact preserved fixture fields before committing the task.

**Risk:** Data/schema change; fixture must not be produced by the migrations being tested. Migration is additive but no down-migration is proposed. Requires approval of S1/S2/S5 before execution.

### Task 2: Enforce transactional CAS and prove two-handle conflicts

**Files:** conversation/message.go, conversation/store.go, conversation/store_test.go, conversation/migration_test.go.

- [ ] Add the conflict type/sentinel contract tests and behavioral cases listed below; run them red against the old unconditional Save.
- [ ] Implement separate insert-only and revision-guarded update paths inside the current transaction, validation, RowsAffected classification, and gated search projection writes. The CAS must be the first data statement after BeginTx; no preliminary revision, existence, or timestamp SELECT. Verify that constraint during source review. Update the Store/Save API comments.
- [ ] Adapt existing update tests to use loaded snapshots or explicitly advance the successfully submitted revision, then rerun all conversation tests.
- [ ] Prove: create 0→1; loaded updates 1→2→3; identical-content update advances; input value remains unchanged; both stored main-row and search-row CreatedAt stay fixed even if the submitted CreatedAt is zero or modified. Duplicate create, stale positive revision, positive revision for an absent row, and save-after-delete-before-recreate all conflict.
- [ ] Prove errors.Is and errors.As through a wrapping conflict error, including exact ID/ExpectedRevision and a literal error string. For negative/max revisions require a non-nil validation error that does not match ErrConflict, with no writes; do not parse validation text. Permit max-minus-one→max and refuse the subsequent save.
- [ ] Race two independent handles: each worker finishes loading revision 1, reports readiness, then waits on a shared release channel. The coordinator verifies both ready outcomes before release; use bounded waits and release/drain cleanup, with no sleep synchronization. Require exactly one success and one typed conflict, persisted revision exactly 2, and one of two literal complete expected winner transcripts. Assert the losing message, summary, title, and search terms did not replace the winner. Repeat for two creates after both workers observe ErrNotFound. These cases prove the stale-save outcome regardless of which writer acquires the lock first; do not claim the barrier forces overlapping lock acquisition.
- [ ] Force a search metadata write failure after a valid main-row CAS using a test-only trigger in the temporary DB. Assert revision, transcript, timestamps, search metadata, and FTS all roll back. Keep cancellation/busy/SQL failures distinguishable from ErrConflict.
- [ ] Finish the v3 fixture round trip with loaded revision 1→save→reload revision 2, pinned content/summary/tool-call values, preserved creation timestamp, and correct search results.
- [ ] Run focused tests under the race detector; mutate the SQL revision predicate, duplicate-create handling, revision increment, zero-row return, and index transaction ordering/rollback one at a time. Each relevant assertion must detect its broken implementation; restore before review/commit.

**Dependency:** Step 1. **Risk:** Shared Save behavior changes; stale inputs that previously overwrote data now fail. Tests must contend on separate handles, not merely goroutines serialized by a single sql.DB.

### Task 3: Carry revisions through runtime, compaction, and CLI

**Files:** golem/session.go, golem/runtime.go, golem/runtime_test.go, golem/session_test.go, golem/compact_test.go, conversation/compress_test.go, cmd/golem/session.go, cmd/golem/session_test.go, cmd/golem/repl.go, cmd/golem/repl_test.go, cmd/golem/compact_repl_test.go, cmd/golem/machineout_test.go. Modify only the relevant existing tests; no parallel event or persistence framework.

- [ ] Update mapSessionStore to enforce the proposed contract under its existing mutex and adjust preseeded persisted fixtures to revision 1. Run existing sequential-turn/compression tests red to expose missing caller advancement.
- [ ] Add runtime regression cases before changing production callers: consecutive turns; a raw save immediately followed by compression; two independent runtime instances losing/winning a shared revision; and underlying typed conflict retained with the completed answer and one terminal run.failed payload using `session_conflict`.
- [ ] Add explicit compaction and post-raw automatic compaction races. Block the existing summarizer seam while a second handle commits, then release it. Require exact winner content/index preservation. Manual compaction reports unchanged plus typed error; automatic compaction remains a successful durable turn with a typed warning.
- [ ] Add a compression value-preservation assertion with a fixed nonzero revision, including a real changed-summary result. Add CLI session load/switch/save/reset/failure cases with fixed expected revisions.
- [ ] Add CLI conflict cases: interactive error and prompt continuation; one-shot error; machine result/event conflict code; /compact failure without cache refresh. Preserve the existing test proving ordinary persistence failures still use their established warning behavior.
- [ ] Run the new tests red, then add post-success revision advancement, CLI cached revision handling, SessionStore contract documentation, the runtime conflict classifier, and the single ErrConflict exclusion in CLI persistence-error demotion. Use existing returned errors and renderers.
- [ ] Run conversation, golem, and cmd/golem tests with the race detector. Mutate omission/premature placement of revision advancement, loss of error wrapping, terminal code, CLI demotion exclusion, and manual/automatic compaction conflict branches; assert each required behavior detects the defect.
- [ ] Review runtime and CLI changes together and integrate Lane 4's shared-file hunks under its ownership rule before committing the combined behavior.

**Dependency:** Step 2; shared-file integration depends on Lane 4. **Risk:** Public injected-store contract and event classification; raw-turn and post-commit compression failures must retain their different outcomes. Preserve secret/canary error rendering precedence. No changes to tool authority or redaction policy are required.

### Task 4: Document compatibility, complete review, and run the required gates

**Files:** docs/library.md, changelog.d/473-cas-save.md, approved plan document; any specific fixes identified by review remain within the approved behavior.

- [ ] Document the expected-revision Save contract, success advancement, typed conflicts, machine code, compaction behavior, synchronized binary upgrade requirement, injected-store migration requirement, and explicit deletion/recreation limit.
- [ ] Add the changelog fragment. Read the complete diff; perform code-review cycles and a separate challenge of each review until findings are resolved. Use the available review skills; locate the handoff's named code-review/criticize-review workflows before claiming those exact workflows ran.
- [ ] In the worktree, run `rtk proxy env -u GOROOT go test -race ./conversation ./golem ./cmd/golem` after integrated changes, unless the same unchanged revision already passed this focused check.
- [ ] Run `rtk proxy env -u GOROOT golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`.
- [ ] Run `rtk proxy docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full` and capture its direct exit code without piping through tail.
- [ ] Record actual test, mutation, review, and gate results in the approved plan. Rebase after Lane 4 if needed and rerun checks justified by changed integration code.
- [ ] Prepare a reviewable PR containing `Closes #473`, the behavioral contract, compatibility limits, and actual validation evidence. Follow repository authorization rules for PR publication; do not merge as part of this approval.

**Dependency:** Steps 1–3. **Risk:** Full gate availability and integration drift; report a failed/unavailable gate rather than representing it as passed. #474 remains blocked until #473 lands.

## Human sign-off required

Approve S1–S5 and Steps 1–4 together before code. Approval specifically covers the schema/Save contract, refuse-and-notice UX with error status for a lost raw save, warning-only automatic compaction after a durable turn, and the stated boundaries for delete/recreate, older writers, external injected stores, and migration startup.

If deletion/recreation or legacy-writer protection must be guaranteed in #473, revise this spec and plan before implementation; the present numeric revision design must not be described as covering those cases.
