# Project-context trust gate (#431, ZT-602) — approved spec and TDD plan

> **Status: D1–D5 and Tasks 1–4 approved by Keith on 2026-09-06 (America/New_York).**
> **Revision: Gemini feedback addressed and approved. Execution authorized in this
> task with Astra/high orchestration, Sol/high implementation, and Terra/medium
> documentation preparation. Execution began in the isolated #431 worktree.**
> For agentic workers: after approval, use `superpowers:subagent-driven-development`
> or `superpowers:executing-plans`, with review after each independently testable task.

**Goal:** Inject AGENTS.md-style context only after the operator approves the exact
workspace document snapshot, and keep it visibly separate from operator instructions.

**Architecture:** Extend the existing loader with full-file hashes, expose a portable
content digest, and bind session grants separately to the canonical workspace and
source paths. Gate CLI execution/composition and publish project/Git context through
one helper using `promptfence` and the existing atomic `replSession.mount` seam.

**Tech stack:** Existing Go module, stdlib SHA-256 and binary encoding, existing
`internal/promptfence`; no dependencies, persistent trust store, or runtime API changes.

**Spec:** Part I of this document. Part II is its implementation plan.

**Verified baseline:** Local HEAD and GitHub `develop` both resolve to
`654f1ccb73f2ccf5db30bd24a7415ca9e14eb243`. Issue #431 is open and has no comments.
There were no open PRs when inspected; that does not establish that sibling lanes
are idle. The checkout has unrelated untracked `copilot.json` and `output/`.
This draft is outside the checkout. No source, test, branch, or Git state was changed.

## Approved decisions

| ID | Approved decision | Consequence |
|---|---|---|
| D1 — lifetime | Session-only trust, using existing grant reset semantics. | Reapprove after process restart, `/new`, `/clear`, successful `/resume`, or `/grants clear`. No disk trust store. |
| D2 — consent and portability | `/trust` inspects; `/trust sha256:<64 lowercase hex digits>` approves the complete logical document set. The displayed digest uses source-relative filenames; the private grant key binds canonical workspace/source paths. | Identical selected guidance has the same displayed digest across worktrees/runners, while an existing grant cannot transfer between workspaces or source locations. Show canonical paths, sizes, and full per-file hashes. |
| D3 — freshness | Rediscover and hash before each operator goal, `/trust`, and valid enabled `/git-context refresh`; freeze inputs within a running turn or AgentFlow invocation. | Any observed document-set/content change revokes prior trust, including A→B→A. No file watcher or mid-turn prompt replacement. |
| D4 — scripted use | `-trust-project-context sha256:<64 lowercase hex digits>` supplies exact consent. In `-p`, `-goal`, and `-plan`, it also requires that approved snapshot through invocation entry. | Omitted flag skips unapproved context visibly. Supplied but mismatched/unavailable/empty context exits 1 before execution; REPL startup retains skip-and-recover behavior. No blanket auto-trust flag. |
| D5 — framing | One combined publisher owns keyed PROJECT_CONTEXT/GIT_CONTEXT rendering, shared budgets, and atomic replacement. Retain the shared marker neutralizer. | One key per immutable combined snapshot; identical refreshes are no-ops. Tool results keep #430's separate per-request framing. No changes to `agent/` or `golem/runtime.go`. |

D1's session-only lifetime is approved. Persisted workspace trust would require
a different storage/security plan and is not included. Keith approved D1–D5 and
Tasks 1–4 together after the Gemini revisions.

## Gemini review disposition

| Feedback | Disposition |
|---|---|
| Absolute paths prevent sharing a digest across worktrees/CI | Accept portability, while retaining explicit local binding in a separate opaque grant key. The original digest was deterministic per checkout, but not portable. Use `(source, selected filename)` because current discovery selects direct children only; a redundant `golem/` prefix is unnecessary. |
| An explicit scripted digest mismatch should fail | Accept for `-p`, `-goal`, and `-plan`, regardless of TTY. Also fail on an empty/unavailable snapshot or loss of the approved snapshot before execution. Omitted consent still allows a context-free run. Preserve machine failure envelopes and exit 1. |
| Independent fence keys or one encapsulated atomic composer | Choose the shared composer alternative. A Git payload change can change the project budget even with unchanged files; one publisher owns that necessary coupling. Keep one snapshot key and semantic no-op detection rather than adding independent key caches. |
| Explicit cancellation checks inside streaming reads | Accept and make Task 1 concrete: pass `ctx` into `readCapped`, check between bounded chunks and before successful completion, and test mid-stream cancellation deterministically. |
| Broad security claims in the review | Keep the narrower contract: hashes identify selected guidance, not a repository tree/commit or safe instructions. Full-tail hashing binds approval to full file contents; bytes beyond the retained prefix were not injected by the old reader. Revocation happens at observation boundaries and cannot erase prior model influence. |

These changes revised the proposed design. Gemini's review was not implementation
approval; Keith subsequently approved the revised spec and plan. Neither is evidence
that the future code/tests pass.

## Global constraints

- Spec + TDD plan approved by Keith BEFORE any implementation.
- After approval, fetch `origin/develop`, revalidate the baseline, and create a
  linked worktree on `feat/431-project-context-trust`; never commit to `develop`.
- Start every worktree command with `cd <worktree> &&`; prefix the executable
  with `rtk`. Every Go command uses `rtk proxy env -u GOROOT go ...`.
- Respect the five-lane ownership map. Route shared-file hunks through their
  owner, or integrate only after that owner's work merges and this branch rebases.
- No persistent trust data, new dependency, recursive discovery, tool permissions,
  detector, or public runtime interface in this change.
- No fabricated production data, fallback digests, or silent read/refresh failures.
  Deterministic test fixtures and specified protocol constants are intentional.
- Every new behavioral assertion must fail under a corresponding implementation
  mutation. Pin independently calculated hashes/bytes; do not calculate expectations
  with the production digest/framing helper being tested.
- Review after each task, resolve defects and repeat until clean, then critically
  review that review. Use the repository's `.claude/commands/code-review.md` and
  `criticize-review.md` criteria through independent review passes; do not claim
  literal slash commands ran when this environment does not expose them.
- Add `changelog.d/431-project-context-trust.md`; never edit `CHANGELOG.md`.
- Any eventual PR targets `develop`, contains `Closes #431`, and has no emojis.
  This planning request does not authorize publication.

## Part I — Spec

### 1. Existing behavior and exact scope

`projectcontext.Loader.Load` currently discovers one selected global file and one
selected workspace-root file, in low-to-high precedence order. The first safe
existing candidate filename wins at each location. Preserve that discovery scope;
unused fallback filenames and recursive/subdirectory AGENTS.md discovery are outside it.

`cmd/golem/main.go:1465` injects loaded documents immediately. The 16 KiB per-file
read cap means their full content is currently unavailable for hashing.
`cmd/golem/gitcontext.go:570` later re-renders cached documents without a trust check.
Both paths must change. A hash of only the root document, retained prefix, or rendered
prompt would leave acceptance gaps.

The gated result supplies all existing CLI consumers: ordinary REPL turns, `/edit`
goals, `-p`, the `-goal` planner, and `-plan`/AgentFlow task prompts. Direct library
consumers remain responsible for their own trust decisions; the loader exposes
evidence and does not prompt or grant authority.

### 2. File evidence and digest contract

Add `Hash [32]byte` and `Size int64` to `projectcontext.Document`. Preserve
`Loader.Load(ctx) ([]Document, error)` and existing discovery ordering.

Read each selected regular file once through the existing identity-checked handle.
Hash its entire raw byte stream with SHA-256 while retaining only `MaxBytes` for
prompt content. `Size` is the full bytes read, not the retained prefix length.
Preserve final-component symlink/nonregular rejection, canonical directory handling,
safe filename checks, and opened-file identity checks. Never return a trustable
partial hash on read failure or cancellation. The retained bytes and full hash
come from the same captured stream; this does not claim an atomic filesystem
snapshot across concurrent external edits or across multiple documents.

Use one 32 KiB streaming buffer. Pass `ctx context.Context` into `readCapped` and
check `ctx.Err()` before every read, after every read, and before returning success,
including when the final read also reaches EOF. Golem
places a two-second deadline around the complete document load/hash operation, so
huge files do not add unbounded CPU scanning before each goal. This is a cooperative
deadline, not a promise to interrupt an OS filesystem read that is itself stuck.
Prompt retention remains bounded by the existing 16 KiB cap.

Add `Loader.Strict bool`, default false, to preserve the library's documented
optional-global-error behavior for existing consumers. Golem sets it true: global
or workspace discovery/read failures reject the entire candidate snapshot. Always
propagate cancellation, even in non-strict mode. A Golem config-directory resolution
failure is also explicit snapshot unavailability, rather than a hidden global skip.
Missing files and existing safely excluded symlink/nonregular entries remain absent;
their appearance/disappearance in the selected document set invalidates prior trust.

Keep both hashes private to the CLI. For all fields below, a length prefix is an
unsigned 64-bit big-endian byte length. Counts use the same integer encoding.

The portable displayed digest is
`projectContextDigest(docs []projectcontext.Document) [32]byte`, over:

1. ASCII bytes `golem-project-context-content-v1` followed by one NUL byte.
2. Document count.
3. Each document in loader order: length-prefixed raw `Source`, length-prefixed raw
   selected filename relative to that source directory, then fixed 32 raw `Hash` bytes.

Today each selected file is a direct child, so the relative filename is
`filepath.Base(d.Path)`: for example `(global, AGENTS.md)` and `(workspace, AGENTS.md)`.
Keep `Document.Path` absolute for inspection and local identity; no extra public
relative-path field is needed. Discovery remains non-recursive. A future discovery
expansion must define relative paths explicitly rather than discard directory names.

The private grant key is `projectContextGrantKey(root string, docs
[]projectcontext.Document) string`: lowercase hex SHA-256 over:

1. ASCII bytes `golem-project-context-grant-v1` followed by one NUL byte.
2. Length-prefixed raw canonical workspace root.
3. Document count.
4. Each document in loader order: length-prefixed raw `Source`, length-prefixed raw
   canonical absolute `Path`, then fixed 32 raw `Hash` bytes.

Use the private key for grant lookup, snapshot equality, and revocation. A workspace
move or changed canonical global-file location changes local identity even when the
portable digest is unchanged. Canonical aliases resolving to the same directories
retain identity. This preserves path-change and A→B→A revocation without relying
on the current one-store-per-session wiring alone.

Both hashes include every selected document before prompt budgeting, even one
entirely omitted from the prompt. Hash raw filename/path bytes, not UTF-8 replacement,
Unicode normalization, or escaped display strings. No timestamps, prompt budget,
fence keys, truncation labels, or newline normalization enter either hash. `Size`
is displayed evidence; the full content hash already binds its bytes.

Display only the portable aggregate digest and per-file hashes, as `sha256:` plus
64 lowercase hex digits. The local grant key stays opaque inside the grant store.
Portability requires the same complete logical set, including global content and
presence/absence; identical Git commits alone are insufficient. Different repositories
with identical selected guidance intentionally have the same portable digest.
Supplying it is explicit consent for those bytes in the current invocation, not a
persisted or automatically shared trust decision. CI must control its global config.
No documents means no injection or grant, and fails a scripted invocation that
explicitly required a digest under D4.

### 3. Approval and revocation

Use a dedicated `project-context` scope in `approvalGrants`, keyed by the private
grant key that binds canonical workspace and source paths. Do not add a tool-name mapping
to `grantScope`. Keep at most the current snapshot's project-context grant; revoke
the previous one when the private key changes or the snapshot becomes unavailable.

`/trust` reloads and reports the current manifest and trust state without approving.
`/trust <digest>` reloads before comparison, so an edit between inspection and
approval rejects the stale digest. Exact matching consent installs the captured
snapshot; record and report success only after runtime composition succeeds.
Unchanged already-approved consent is idempotent. Empty sets cannot mint grants.
A malformed or mismatched `/trust` argument does not revoke an unchanged valid
grant. Its mandatory reload still revokes trust if the underlying snapshot changed
or became unavailable. Startup flag mismatch has no existing consent to preserve:
REPL startup skips injection; `-p`, `-goal`, and `-plan` fail under D4.

An explicit `/trust <digest>` entered through the REPL is sufficient operator
consent with either terminal or scanner input. It does not require a second stdin
reader or a TTY. Text inside a `-p` prompt, model output, tools, files, or restored
history never reaches this approval route.

Follow existing grant reset semantics exactly: `/new` and `/clear` revoke even
with `-no-session` or a failed conversation clear; successful `/resume` revokes;
failed `/resume` preserves; `/grants clear` revokes. Grant revocation is immediate.
The next context refresh must remove the project fragment before any further
provider invocation. Slash commands that make no provider request need no extra
runtime replacement solely to remove an inactive cached string.

Tool grants, `/auto-edits`, `-allow-tool`, destination consent, plan-lock approval,
plan edit/gate approval, and checkpoint restore never confer document trust.
The startup digest flag is consumed once, not automatically reapplied after a
reset or an observed change. Scripted modes retain its required-snapshot condition
until invocation entry; that condition checks a live local grant and cannot recreate
one. If local paths change while portable content stays identical, the old grant is
still revoked and the pending scripted invocation fails. Trust is never serialized
into sessions or checkpoints.

### 4. Refresh, failure, and visibility

Capture a fresh candidate before every `runOnce` request and before entering an
AgentFlow author/task invocation. Perform initial discovery/hash and explicit-flag
validation after output-format validation and canonical-root resolution, before
provider setup, destination prompts, backend discovery, bootstrap, or probing.
Digest calculation does not need Git capture or prompt budgeting. Retain matching
consent as pending until initial composition succeeds, then record the local grant.
`/trust` and valid enabled `/git-context refresh` also reload.
Refresh does not recapture Git before ordinary goals: retain the existing explicit
Git-refresh policy while validating project documents.

| Observation | Required behavior before another provider call |
|---|---|
| Same local document snapshot with a live grant | Keep injecting the approved immutable bytes. |
| First load or same unapproved snapshot | Inject no project block; show skip and approval affordance. |
| Changed content/path/source/order, addition, removal, or restored earlier version | Revoke prior trust; remove the whole project block; report the new manifest and required approval. |
| Read/discovery error or hash deadline | Revoke prior trust; remove the whole project block; report the escaped cause. No partial snapshot or stale fallback. |
| Empty document set | Revoke any previous trust and clear the block; report removal if it had existed. Explicit scripted requirement fails. |
| `-no-project-context` | Perform no project discovery, hashing, approval, or injection. `/trust` reports disabled. |
| Runtime replacement failure during removal | Abort the pending goal; do not call the provider with the old block. Retry removal on the next attempt. |
| Runtime replacement failure during approval | No new grant or success notice; the next goal must still pass the gate. |
| Explicit digest required by `-p`, `-goal`, or `-plan`, but snapshot mismatches, is absent/unavailable, or loses its local grant | Abort with exit 1 before invocation. Do not downgrade to context-free execution. |

Reuse `sess.mount(sess.mountAt, nil, next)` and live `sysInputs` for atomic replacement,
preserving tools, capabilities, Git context, memory, model options, and history.
Apply trust validation before journaling/provider invocation. A canceled parent
context aborts the pending operation. Without an explicit scripted requirement,
a project-only timeout may continue without project context once removal succeeds.
With that requirement, it fails. An initial rejection makes zero setup/probe/model
calls. A later rejection prevents subsequent model/planner/worker execution, but
cannot undo bootstrap probes that completed before the files changed.

For Git refresh, project invalidation must still be processed if Git capture fails:
the existing Git failure policy may retain its previous Git snapshot, but must not
retain a now-unapproved project fragment. Never render cached documents without
consulting the live project-context grant.

Initial/changed manifests show source, control-safe quoted canonical path, full
byte size, full file hash, and whether content is retained/truncated/omitted by the
prompt budget. Show the full aggregate digest and the exact approval command.
An early rejected snapshot is labeled `not injected`; do not invent budget-dependent
retention information before Git capture and composition have occurred.
Repeated skipped goals may use one concise line with the aggregate digest and
`/trust` affordance; `/trust` always prints the complete current manifest.
Do not print file bodies as part of trust inspection.

Human notices go only to stderr in `-p`, `-goal`, and `-plan`. Use the exact digest
flag affordance in those modes. The D4 predicate is `f.promptSet || f.goalSet ||
f.planPath != ""`; TTY availability does not change it, including interactive plan-lock
approval in `-goal`. REPL mode, including scanner input, keeps skip-and-recover behavior.

Omitted flag: skip unapproved context and proceed. Explicit flag in those three
modes: mismatched digest, no documents, discovery/read failure, or hashing deadline
is fatal with exit 1. Report expected and actual portable digests when available;
otherwise report `no documents` or `snapshot unavailable` plus the escaped cause.
Never fabricate an actual digest for a failed load. A later local-identity change
may have equal expected/actual portable digests; name the changed source identity
as the reason consent no longer holds. Malformed digest syntax and the flag combined
with `-no-project-context` remain usage errors under the existing mode-specific exit
taxonomy; a valid but unsatisfied trust requirement is an ordinary failure, not a
`usageError`.

Preserve machine output explicitly. Add bounded CLI result code
`project_context_untrusted` and a private typed trust-precondition error. An initial
rejection in `main.run` calls existing `reportPreRunFailure` once and returns the
ordinary error. A later rejection returned by `runOnce` is recognized in
`machineWriter.buildResult`'s no-terminal-event branch; `runOneShot` writes its one
final result. Do not also call `reportPreRunFailure` there. JSON and stream-JSON each
produce exactly one `golem.result.v1` error record, null answer/model/stopReason/grounding,
and no fabricated runtime events; text-mode stdout remains empty. Human manifests
stay on stderr. Update documented result codes without changing the result schema.

A running goal keeps its captured inputs: external edits cannot replace bytes
mid-turn. An AgentFlow invocation similarly freezes the approved project snapshot
for its planner/tasks. The next invocation or explicit refresh detects later edits.
This does not erase earlier conversation content or undo earlier model influence.

### 5. Shared framing and budgets

Keep a fixed operator-authored explanation outside the project frame: this is
operator-approved advisory project guidance, subordinate to operator instructions,
and it cannot grant permissions. Approval admits evidence; it does not turn file
text into operator authority.

Render PROJECT_CONTEXT and GIT_CONTEXT with one fresh `promptfence.Fence` when a
new combined context snapshot is published. All raw inputs are fixed before
minting the key. Reuse `Open`, `Close`, keyed document `Lead`, and `FlattenLine` for
metadata. Include controlled provenance (`global`/`workspace`) and source path in
each project document header; Git remains labeled repository snapshot data.
Retain `neutralizeFence` for both sources so marker-looking content remains data,
including copied old keys. Terminal output uses existing control-safe escaping.

Put combined rendering in one private `projectContextInputs` function and live
publication in `publishProjectContext`, both in `projectcontext_trust.go`. Startup
uses the same renderer before constructing the runtime; `refreshProjectContext`,
`handleTrust`, and `handleGitContext` share the live publisher, which calls that
renderer and publishes both fragments in one existing `sess.mount` call. Do not
add a nil-runtime success path to the live publisher. Rendering consumes already
captured inputs and performs no discovery or subprocess calls. Handlers decide
refresh/consent and pass candidates; they do not independently rewrite each other's
fragments. The existing cached Git state is sufficient.

This is snapshot-scoped source rendering, not per-provider-request system rewriting.
Reusing exactly the same immutable snapshot within or between unchanged goals is
safe under the retained neutralization policy. Never frame newly read bytes with
an old snapshot key; when either source changes, reframe both together. Compare
semantic inputs before minting keys, so randomized strings do not break idempotence.
An identical Git refresh or repeated unchanged consent does not mint a key or call
Replace. A changed Git payload may change the remaining project budget, so paired
rendering is intentional; independent fence-key caches would not remove that coupling.
Tool-result rendering remains #430's independent per-request contract.

Preserve the 16 KiB aggregate project/Git body budget and 4 KiB Git component cap.
Git reserves its payload first; project documents spend the remainder in reverse
precedence order. Include keyed headers in body accounting, retain complete outer
fences, and truncate content only on UTF-8 boundaries. If a complete provenance/path
header cannot fit, omit that document rather than rendering an incomplete header.
Document omission/truncation notices count in the body budget. Omitted bytes and
documents still participate in trust hashing. Keep existing Git capture controls.

### 6. File ownership and integration

| Surface | Planned ownership/use |
|---|---|
| `projectcontext/projectcontext.go`, `read.go`, their tests/doc.go | Lane 5: full hashes, strict loading, cancellation, retained-prefix semantics. |
| `cmd/golem/projectcontext.go`, new `projectcontext_trust.go`, related tests | Lane 5: aggregate digest, manifest, state, gate, `/trust` handler, combined context composition. |
| `cmd/golem/grants.go`, tests | Lane 5: separate scope; reuse existing grant methods/lifetime. |
| `cmd/golem/gitcontext.go`, renderer/refresh tests | Necessary adjacent change: shared framing and trust-aware refresh. Reconfirm owner before editing. |
| `cmd/golem/main.go`, relevant tests | Shared startup/flags/session wiring and pre-AgentFlow gate; coordinate with Lane 3's audit subcommand and Lane 2's composition work. |
| `cmd/golem/repl.go`, related tests | Lane 4: session state, pre-`runOnce` gate, `/trust` dispatch/help. These are several small hooks, not merely the registration hunk described in the handoff. |
| `cmd/golem/machineout.go`, `machineout_test.go`, `exitcode_test.go` | Necessary adjacent integration: trust failure code and one-result machine/exit behavior. Preserve the existing result schema and ordinary usage classification. |
| `cmd/golem/mount.go`, tests | Consume existing seam; change comments only if needed. No replacement API redesign. |
| `agent/`, `agent/interceptor/`, `golem/runtime.go` | No production edits in this proposal. |

Default integration is to prepare Lane 5's isolated changes, wait for shared-file
owners to merge, rebase, and then apply the listed integration hunks. If owners
accept and land the exact hooks themselves, consume those instead. Do not message
other task owners without user authorization or edit their files opportunistically.

## Part II — TDD implementation plan

No implementation/test code is included or written at this approval stage.
Each task below defines its failing checks and mutation obligations. After approval,
write those tests first, run them red, implement the minimum change, run green,
perform the review/critique cycle, and commit only the reviewed task in the worktree.

### Task 1 — Full-file evidence with bounded retention

**Files:** `projectcontext/projectcontext.go`, `read.go`, `projectcontext_test.go`,
`read_test.go`, `doc.go`.

**Interfaces:** additive `Document.Hash`, `Document.Size`, `Loader.Strict`; unchanged
public `Load(ctx)` shape. `readCapped` takes `ctx context.Context` and forwards it
to the internal streaming helper; check cancellation before/after each 32 KiB
chunk read and before returning a successful result.

- [ ] Write `TestLoadFullContentHash`: file `abc`, retained cap 1, exact content `a`,
  size 3, truncation true, hash
  `ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad`.
- [ ] Cover empty content, equal-size different tails after the retained prefix,
  and both selected global/workspace files. Preserve first-safe-candidate ordering,
  filename rejection, symlink/nonregular exclusion, and opened identity checks.
- [ ] Test strict global failure versus preserved non-strict optional behavior;
  cancellation/global-only deadline cannot become successful absence. Interrupted
  reads produce no usable document/hash. Exercise stream failure/cancellation with
  a deterministic callback reader around the internal streaming helper, rather
  than scheduling concurrent filesystem writes and hoping to hit a race window.
  Cancel after a known chunk while unread data remains and separately during a
  final read that returns bytes plus EOF; neither case may publish a hash. Record
  read calls to prove cancellation does not wait for the rest of the file.
- [ ] Run the focused failing checks; implement full streaming SHA-256 and strict
  behavior using stdlib, retaining the existing prefix cap; rerun them green.
- [ ] Mutate full hashing to prefix hashing or suppress strict-global/cancellation
  errors; the corresponding tests fail. Preserve and inspect the existing opened
  identity checks; do not claim existing static symlink tests detect removal of
  `os.SameFile`, or add a timing-dependent test to make that claim.
- [ ] Run the whole `./projectcontext` suite, review, critique, and commit.

### Task 2 — Exact-digest trust and shared context rendering

**Files:** `cmd/golem/projectcontext.go`, new `projectcontext_trust.go`,
`projectcontext_test.go`, new `projectcontext_trust_test.go`, `grants.go`,
`grants_test.go`, `gitcontext.go`, and its renderer tests. Respect adjacent ownership.

**Interfaces:** private `projectContextDigest(docs) [32]byte` and
`projectContextGrantKey(root, docs) string`. One concrete `projectContextState` holds
current documents, portable digest, local grant key, disabled state, and a required
digest only for explicitly pinned scripted invocations. Existing grant methods
store/revoke the local key in `project-context` scope. `projectContextInputs(in
systemInputs, docs []projectcontext.Document, gitSnap gitContextSnapshot, trusted
bool) systemInputs` renders the combined candidate without I/O; callers decide
semantic equality before rendering. Keep raw Git state for paired rendering with
`promptfence.Fence`; avoid independent key caches or a public registry.

- [ ] Pin canonical preimage bytes and independently calculated outputs for both
  the portable digest and local key. Identical logical documents in different
  workspaces have equal portable digests and unequal local keys; one shared grant
  store must refuse cross-workspace reuse. Canonical aliases resolving to the same
  locations preserve both identities.
- [ ] Table-test source/relative filename/order/content changes and global
  addition/removal, including a changed global document fully omitted by rendering:
  both hashes change. Relocating identical global bytes changes only the local key,
  revokes trust, and updates canonical path headers. Check A→B→A location changes.
  Distinct raw non-UTF-8 filenames change both hashes; different absolute parent
  bytes change only the local key. Budget/fence/display changes affect neither hash.
- [ ] Test grant isolation from every tool scope, exact/full lowercase digest
  parsing, nil-store behavior, idempotent consent, and A→B→A revocation.
- [ ] Pin the operator advisory wording and source metadata with only the random
  key normalized. Prove project/Git use matching keyed outer boundaries, both
  sources neutralize forged marker text, and control-bearing paths stay one line.
- [ ] Test exact body budgets at boundaries, complete provenance headers,
  workspace-first retention, omitted-document evidence, and semantic refresh
  idempotence. New raw source input must trigger a new combined frame key. Assert
  identical Git refresh/consent makes no Replace call; changed Git and project
  candidates publish exactly once through the shared helper. Startup and live
  composition use the same body/budget rules with the random key normalized.
- [ ] Run targeted tests red, implement the digest/state/render helpers and CLI
  two-second strict loader policy, then run the affected suites green.
- [ ] Mutate one manifest field out of hashing, replace a local grant key with the
  portable digest, use portable digest alone for equality/revocation, permit prefix
  consent, cross grant scopes, reuse a key with changed input, remove neutralization,
  or stop counting metadata/omission notices; each corresponding check must fail.
- [ ] Review/critique this deliverable and commit. Unwired helpers do not count as
  completed feature behavior; Task 3 must verify actual provider requests.

### Task 3 — Integrate every CLI path and lifecycle boundary

**Files:** shared `cmd/golem/main.go`, `repl.go`; `projectcontext_trust.go`,
`gitcontext.go`, `machineout.go`; `main_test.go`, `repl_test.go`, `headless_test.go`,
`machineout_test.go`, `exitcode_test.go`,
`projectcontext_trust_test.go`, `gitcontext_refresh_test.go`,
`gitcontext_wiring_test.go`, `fence_wire_test.go`, and existing AgentFlow tests.

**Interfaces:** `handleTrust(ctx, out, sess, fields)` handles slash consent;
`refreshProjectContext(ctx, out, sess) error` validates and publishes the permitted
fragment, returning an error when another provider call must be blocked. It uses
the common `publishProjectContext` path and existing `sess.mount` seam. Session state
replaces the old ungated document cache. A private `projectContextTrustError` carries
an unsatisfied explicit scripted requirement; it remains an ordinary exit-1 error.
`machineWriter.buildResult` recognizes it when there is no terminal event and emits
`project_context_untrusted`. Do not emit a second pre-run result on that path.

- [ ] Revalidate shared-file ownership and baseline. Land owner-supplied hooks or
  rebase after owners merge before changing shared production files.
  Re-inventory provider entry points after Lane 4 lands, including compaction;
  apply the gate to any new path consuming cached project context. A path that
  provably consumes history only does not need project-file discovery.
- [ ] Add provider-capture tests: unapproved startup and bare `/trust` exclude exact
  fixture bytes; explicit matching consent includes them in the expected frame;
  mismatch, stale approval, failed reload, and failed Replace never grant. A bad
  `/trust` argument preserves an unchanged existing grant; a changed/unavailable
  reload revokes it even when the supplied argument is bad.
- [ ] Test two successive goals with same-size content edits, a tail-only change,
  an omitted-global change, additions/removals, and `/undo` restoring an old version.
  The next captured provider system must contain no PROJECT_CONTEXT frame,
  neither the old approved bytes nor the new unapproved bytes, while preserving
  Git and other fragments.
- [ ] Test `/git-context refresh` cannot resurrect revoked cached docs, including
  when Git capture fails. Failed removal Replace yields zero provider calls; retry
  can recover. Tool/capability/memory/model settings survive a successful replacement.
- [ ] Cover `/new`, `/clear`, successful/failed `/resume`, `/grants clear`,
  `-no-session`, and a new process using the same saved session. Tool and destination
  grant behavior stays unchanged, while document grants follow D1.
- [ ] Test flag validation, exact match/mismatch, `-no-project-context`, and startup
  flag consent not reapplied after reset. In REPL mode, mismatch skips and allows
  `/trust` recovery. In each of `-p`, `-goal`, and `-plan`, an explicit mismatch,
  empty set, strict read failure, or deadline returns exit 1 and zero initial
  bootstrap/probe/model/worker calls. Include `-goal` with terminal plan-lock input.
  Omitted-flag cases continue with context excluded; matching consent executes.
- [ ] Match startup consent, then deterministically change/delete/unreadably replace
  a document before invocation through existing run hooks. Require exit 1 and no
  subsequent model/planner/worker calls. Also relocate unchanged global content:
  equal portable digest does not restore the revoked local grant.
- [ ] Test both early `main.run` rejection and later pre-`Runtime.Run` rejection:
  text stdout is empty; JSON/stream-JSON each contain exactly one existing-schema
  error result with code `project_context_untrusted`, null optional result fields,
  and no runtime events. Pin expected/actual digest diagnostics on stderr and the
  explicit unavailable cause when no actual digest exists. Malformed syntax keeps
  existing mode-specific usage exit classification; it is distinct from mismatch.
- [ ] Capture `-goal` planner and `-plan` task prompts, with their ordinary plan
  approvals enabled: unapproved context stays absent, digest-approved context is
  present. Freeze snapshots during an invocation and refresh before the next one.
- [ ] Prove `-p` text, model output, and tool output containing `/trust` cannot grant;
  explicit scanner REPL `/trust <digest>` can. No extra stdin reader is created.
- [ ] Run these checks red; wire grant construction before startup composition,
  initial preflight before provider setup, the flag, common publisher, handlers,
  typed error/result code, and pre-invocation gate; run the affected CLI suites green.
- [ ] Remove each gate in turn (startup, runOnce, planner/task entry, Git refresh),
  retain stale A after otherwise successful invalidation, retain stale context
  after Replace failure, downgrade explicit scripted mismatch to a warning,
  misclassify it as usage, double-write its machine result, or reapply startup consent:
  the relevant integration assertion must fail under each mutation.
- [ ] Complete review/critique cycles and commit the integrated behavior.

### Task 4 — Documentation and release verification

**Files:** `README.md`, `cmd/golem/doc.go`,
`changelog.d/431-project-context-trust.md`; copy the approved document into
`docs/plans/2026-09-06-project-context-trust-431-spec-plan.md` in the feature worktree.

- [ ] Document exact inspection/approval/flag syntax, full-file versus aggregate
  digest meaning, portability and global-configuration requirements, local grant
  binding, session resets, explicit-scripted-requirement failures, and the
  per-invocation freshness boundary. Add `project_context_untrusted` to the documented
  result codes and distinguish stderr diagnostics from the single machine error
  record. Replace the
  current README claim that AGENTS.md reload requires restarting Golem.
- [ ] Document snapshot-scoped project/Git framing versus per-request tool framing;
  approval does not grant capabilities or sanitize prior conversation influence.
- [ ] Run focused package/race checks, the required lint gate, and full Docker CI.
  Capture actual exit codes directly; never pipe test output to `tail`.
- [ ] Complete independent whole-diff review and critical review, resolve defects,
  and rerun checks only when changes or failures warrant it.
- [ ] Confirm no unintended lane edits, dependencies, persistent trust data,
  `CHANGELOG.md` edits, or unrelated worktree files. Prepare PR text containing
  `Closes #431`; publication remains a separate authorized action.

## Verification commands after approval

Every command below also receives the required `cd <worktree> &&` prefix:

- `rtk proxy env -u GOROOT go test ./projectcontext ./cmd/golem ./internal/promptfence`
- `rtk proxy env -u GOROOT go test -race ./projectcontext ./cmd/golem`
- `rtk proxy golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`
- `rtk proxy docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`
- `rtk proxy git diff --check <implementation-base-commit>`

Within each task, run the newly named tests first to establish red/green evidence.
Mutations are temporary local verification edits and must be reverted before the
task commit. No implementation tests were run during this planning-only pass.

## Approval and sources

Keith approved D1–D5 and Tasks 1–4 together on 2026-09-06, saying: "approved. which
model/reasoning should we use for execution? can any of the tasks be parallelized
by subagents?" The requested design approval gate is satisfied. No additional
permission gate has been inferred from the skills or external safety contract.

Primary requirement: [GitHub issue #431](https://github.com/kstruzzieri/go-llm/issues/431).
Implementation evidence: the verified baseline files cited above and the supplied
handoff `handoff-431-agents-md-trust.md`. The baseline already contains #430's shared
fencing, #341's session grants, and #354's Git refresh/composition behavior.
