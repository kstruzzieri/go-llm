# Signed MutationReceipts (#445, ZT-302) — proposed spec and TDD plan

> **Status: approved by Keith on 2026-09-06; implementation authorized by "Begin execution in an isolated git worktree".**
> **Revision: Gemini feedback addressed.** Review recommendations are evaluated
> below; Keith subsequently approved execution of this revision.
> For agentic workers: after approval, use `superpowers:subagent-driven-development`
> or `superpowers:executing-plans`, with review after each independently testable task.

**Goal:** Give Golem's durable checkpoint mutations authenticated before/after
evidence, preserve that evidence through undo and checkpoint pruning, and expose
crash uncertainty honestly for #447.

**Architecture:** Keep the existing `runJournaledWrite` → `PreparingJournal`
protocol. The concrete checkpoint journal signs an intent before mutation and an
applied receipt after observing completion. Store both in the existing hardened
SQLite database; checkpoint rows reference this evidence without owning its lifetime.

**Tech stack:** Go 1.25.0, the shipped `signing/` package, stdlib, and the already
installed SQLite driver. No dependency, new signing interface, ledger file,
filesystem write primitive, or model-facing delete tool.

**Spec:** Part I of this document. Part II implements that spec.

**Planning baseline:** remote `origin/develop` is `b97613be5eca70205fb92b0cf114236f36e37fd4`
(verified against GitHub); the current checkout is older, at `8848d3a`. Relevant
mutation/checkpoint/signing files are identical between those revisions. Startup
and mount edits must preserve the newer Git-context composition on the baseline.
The current checkout has unrelated untracked `copilot.json` and `output/`.
This draft is outside the checkout; no branch, key, database, or source was changed.

## Approval decisions

Approve or amend these together before implementation:

| ID | Recommended decision | Consequence |
|---|---|---|
| D1 — coverage | Cover durable-checkpoint Golem writes: interactive, headless `-allow-tool`, late `/allow-write`, scratch promotion, and actual `/undo` restores/deletions. | AgentFlow task mode, parallel integration/rollback, direct embedders, and arbitrary exec writes remain uncovered. This is an explicit narrowing of #445's broad wording; do not close the issue as universal coverage without accepting that scope. |
| D2 — storage | Separate receipt table inside the existing checkpoint SQLite DB, with no cascade from checkpoint deletion. | Audit evidence survives `/undo` and the 50-checkpoint/64 MiB prior-content limits. Receipt metadata has no automatic pruning in v1, so total DB growth is no longer bounded by those limits. |
| D3 — identity | Persistent Ed25519 at `<dataDirBase>/golem/signing/agent-ed25519.pem`; enable automatically with checkpoint-backed writes. | No unsigned fallback or CLI algorithm toggle. AgentID is the key ID, not a model or session name. #446-specific identity configuration has not been established in the inspected sources. |
| D4 — failure claim | Success requires a durable applied receipt. Persist a signed intent first; post-write failures halt further writes and retain uncertainty. | A crash between filesystem mutation and receipt commit can leave an applied change with only an intent. It must never be reported as a verified applied receipt. |
| D5 — legacy history | Preserve legacy checkpoint bytes for listing, but refuse authenticated `/undo` of unsigned records. Refuse upgrade before schema migration if legacy open/undoing checkpoints exist. | Existing completed unsigned undo history becomes unavailable through `/undo`; finish interrupted legacy recovery/undo with the previous binary before upgrading. No automatic historical signing or unsigned fallback. |

D1 and D5 are product choices, not assumptions that implementation may quietly make.
If either is rejected, revise this plan before coding. Broader AgentFlow coverage
requires its separate journal and parallel promotion/rollback flow to be planned.

## Gemini review disposition

| Feedback | Disposition and specific change |
|---|---|
| D1–D5 and Tasks 1–6 recommended for approval | Keep the five decisions pending Keith's approval. The review is not evidence that the future implementation/tests pass. |
| Nullable `after_mode` and required fields | Accept explicit `null`, no `omitempty`, and forward-create-only mode validation. Correct the premise: `signing.Canonicalize` has no schema; receipt decoding must enforce required fields itself. |
| SQLite references, uniqueness, scan order | Specify child-to-ledger foreign keys and safe insertion/Abort ordering. Add nullable columns then ordinary unique indexes; partial indexes are unnecessary for nullable-reference uniqueness. Paginate by committed sequence, allowing gaps and non-monotonic timestamps. |
| Commit cancellation and Record bypass | Retain the existing decisions and tests. Clarify that signing and post-write persistence both use the non-canceled bookkeeping context. |
| A — `undo_of` lineage and replay | Accept lineage binding and retained-ledger replay checks. Amend the unconditional applied-forward/live-hash requirements: authenticated intent-only recovery stays supported, batch checks use simulated state, and completed inverse evidence permits bookkeeping reconciliation without replaying a filesystem operation. |
| B — canonical bytes and future chains | Store each complete envelope once as canonical JSON. Do not add `prev_receipt_hash`, a Merkle structure, or duplicate canonical-byte columns. Future whole-envelope hashing uses these exact stored bytes. |
| C and D5 — checkpoint status labels | Add `[unsigned]`, `[unconfirmed]`, `[receipts verified]`, and `[invalid receipts]` with precise meanings. Do not infer authentic legacy provenance from missing fields or claim that signature validation verifies live workspace contents. |
| D — key drift diagnostics | Report the receipt's claimed public key ID, actual loaded key ID, and escaped key path. Keep writes disabled and leave the key file untouched. Say receipt integrity, not audit-chain integrity: v1 has no chain. |

These changes refine the same six tasks; they do not authorize implementation or
broaden D1. No numerical receipt-size or cryptographic-completeness guarantee is
inferred from the review.

## Global constraints

- Spec + TDD plan approved by Keith BEFORE any implementation.
- Feature branch off fresh `origin/develop`, never commit to `develop`. Use a
  linked worktree and the established name `feat/445-mutation-receipts`.
- Start every worktree command with `cd <worktree> &&`; prefix its executable
  with `rtk`. Every Go command uses `env -u GOROOT go ...` through `rtk proxy`.
- Do not edit Lane A's `agent/interceptor` or `rag/indexer`, Lane B's `memory/`
  or `agent_memory`, or `signing/` implementation. Consume signing read-only.
- Keep `Journal`, `PreparingJournal`, `PreparedMutation`, and existing public
  tool constructor signatures unchanged. No global mutable signer state.
- Preserve approval keys, path containment, promotion mode checks, turn
  serialization, transactional admission, hardening errors, and undo refusal
  on content/type/mode drift.
- Every new assertion must fail under a corresponding mutated implementation.
  Pin independent canonical/framed bytes; never derive expectations from the
  production encoder/hash/framer being checked.
- Review after each task, fix findings until clean, then critically review the
  review. The handoff's literal `/code-review` and `/criticize-review` commands
  were not available here; use equivalent independent reviewer passes and
  report that substitution rather than claiming those commands ran.
- Add `changelog.d/445-mutation-receipts.md`; never edit `CHANGELOG.md`.
- No emojis in PR text, including generated footers.

## Part I — Spec

### 1. Current flow and coverage

The handoff's symbol claim is incorrect: `Workspace.WriteFileAtomic` and
`Workspace.RemoveFile` exist in `agent/tools/paths.go:529` and `:577`.
The integration point remains the existing journal protocol, not those low-level
primitives: they lack the approved before-image, durable owner, and actor identity.

| Operation | Existing flow | V1 behavior |
|---|---|---|
| Create/overwrite | `agent/tools/write.go:136` → `runJournaledWrite` | Signed forward intent and applied receipt. Preserve equal-before/after writes. |
| Edit, including truncate-to-empty | `agent/tools/edit.go:148` → same funnel | Same; an empty file is not absence. |
| Scratch promotion | `agent/tools/scratch_promote.go:193` → same funnel | Include its one regular-file create and authenticated tracked mode. |
| Durable `/undo` | `cmd/golem/checkpoint_journal.go:468` → direct write/remove | Signed inverse transition only when an actual filesystem operation occurs. |
| Headless and late write enablement | `buildWriteTools`, `main.go`, `mount.go` | Same signer/journal guarantees as startup `-allow-write`. |
| AgentFlow task tools/RAM undo and parallel canonical promotion | `agentflow_driver.go:420`, `journal.go:82`, `agentflow_parallel.go:280` | Explicitly deferred by D1. |
| `run_command`, `start_command`, external editors | Subprocess/external writes bypass file journal | No receipts or process-level attribution claim. |
| Scratch copies/cleanup and Golem metadata | Separate storage operations | Outside canonical user-file receipt scope. |

There is no current `delete_file` tool. `/undo` supplies the existing in-scope
deletion operation; adding a new tool is unnecessary for this proposal.
Scratch promotion availability remains frozen at startup: `/allow-write` does
not retroactively mount `promote_artifact` for an existing scratch runtime.

### 2. Signed record contract

Put the portable record and strict encoding/verification helpers in
`agent/tools/mutation_receipt.go`. Reuse `signing.Signer`, `signing.Verifier`,
`signing.Signature`, and `signing.MarshalCanonical`.

Each envelope is a typed **Body** and sibling **Signature**. Sign the entire Body;
do not maintain a second mirror struct containing selected signed fields.
Use the shipped domain convention `go-llm/mutation-receipt/v1`.

| Body field | Exact meaning |
|---|---|
| `kind` | `intent` or `applied`; signed, so an intent cannot verify as an applied receipt. |
| `mutation_id` | Host-generated `crypto/rand.Text()` identity, allocated inside Prepare before persistence or filesystem mutation and reused through that attempt's resolution; never a recyclable database ID. |
| `workspace_hash` | Full lowercase SHA-256 of the canonical absolute workspace root, using existing `ContentHash`. Keep the current checkpoint filename scheme unchanged. |
| `path` | Canonical workspace-relative path from `CanonicalPathForUndo`, slash-separated; never raw tool spelling or a path outside the workspace. |
| `before_hash` | Existing `ContentHash(prior bytes)`, or literal `absent` for a create. |
| `after_hash` | Existing `ContentHash(written bytes)`, or literal `absent` for an undo deletion. |
| `timestamp` | UTC RFC3339Nano. Intent records preparation time; applied records post-operation observation time. Neither claims trusted wall-clock time. |
| `agent_id` | `signer.KeyID()`, exactly matching `signature.kid`. |
| `after_mode` | Required JSON member with nullable permission bits, represented as `*uint32` with `json:"after_mode"` and no `omitempty`. Forward tracked scratch creates bind their mode; every untracked or inverse operation emits explicit `null`. Zero permission bits (`0`) are distinct from `null`. |
| `undo_of` | Empty string for forward changes; original mutation ID for an inverse. |

An applied body copies the intent's immutable facts; only `kind` and `timestamp`
change. The same signing identity is required for the two phases. Undo is a new
mutation with its own identity/times, reversed hashes, and `undo_of` lineage.
Operation kind is inferable from the two hashes; do not add redundant source,
model, session, or approval-key fields without a requirement.

Strict decoding must reject duplicate/unknown fields, malformed UTF-8/JSON,
invalid hashes, invalid path spellings, invalid kinds/modes/times, identity
mismatches, and invalid/unknown signatures. Canonicalize and validate stored JSON
before relying on decoded fields, so decoding cannot discard unsigned additions.
Absence and SHA-256 of empty bytes remain distinct. Both-absent is invalid.
Timestamp order is not assumed monotonic; the local clock can move backward.

All ten Body members in the table, both envelope members (`body`, `signature`),
and all three signature members (`alg`, `kid`, `sig`) must be present with their
exact case-sensitive names. `after_mode` is the only member that may be JSON
null. An absent `after_mode` is invalid, not equivalent to explicit null. Reject
unknown/case-variant names rather than relying on encoding/json's case-insensitive
struct matching. The canonicalizer handles lexical JSON validity/duplicate
decoded names; it does not enforce required fields. Neither ordinary struct
decoding nor DisallowUnknownFields alone checks required-member presence.
The receipt decoder must check membership and types before interpreting defaults.

A non-null mode is an integer in 0..511 (octal 0000..0777), allowed only when
`before_hash` is `absent`, `after_hash` is a real content hash, and `undo_of` is
empty. The shared kind validator separately requires intent/applied. Overwrites
and all inverses require null. Construction must additionally match the existing
MutationRecord.TrackedMode/AfterMode contract: ordinary untracked creates remain
null, and a promoted create whose tracked mode is 0000 retains numeric zero.

Bookkeeping (`applied`, `restored`, checkpoint status, goals, summaries, retention)
and prior-content blobs are outside the Body. Before restoration, authenticate
the Body, compare it with every checkpoint field used to choose a path/transition,
and hash the actual prior-content blob against signed `before_hash`.

### 3. Journal and filesystem sequence

**Forward mutation:** retain `Prepare → write → Commit/Abort` in
`runJournaledWrite`. `checkpointJournal.Prepare` canonicalizes the path, derives
the immutable transition, signs its intent, and stores intent plus checkpoint row
atomically while retaining the existing serialization slot. Failed signing or
Prepare changes no workspace file. The existing write closure runs once.

After the write closure reports a landed operation, `checkpointPrepared.Commit`
checks the live after-state against the expected hash and tracked mode, signs
the applied record, and atomically persists it with `applied=1`. Use the existing
non-canceled local bookkeeping context so user cancellation after a landed write
does not prevent either signing or receipt persistence (`context.Background()`,
as in existing commit bookkeeping). A detected after-state mismatch, signing
failure, transaction error, or hardening error latches/cancels the journal and
reports an indeterminate change. Never retry the filesystem operation implicitly.

An ordinary failed write uses Abort and discards its unused signed intent in the
same transaction as the checkpoint intent. If Abort fails, preserve uncertainty
and existing failure behavior. A post-transaction hardening error can mean the
receipt already committed; report the failure without duplicate receipt creation.

The concrete `checkpointJournal.Record` compatibility method must fail closed
and latch: it cannot directly manufacture an observed applied receipt from an
arbitrary caller-provided record. Production tools already use PreparingJournal;
keep the interface method solely to satisfy the existing Journal contract.

For scratch promotion, preserve the distinction between a landed create and an
overall tool success. A landed file may have valid evidence even when later
cleanup/verification reports an error. Receipt creation must not turn that tool
error into success. Shared commit verification must still honor the tracked mode.

**Undo:** before any first restore in either normal undo or interrupted resume,
authenticate selected evidence and pending before-image blobs, then scan retained
inverse evidence as described below. Classify completed inverses before live-file
preflight or progress updates; only genuinely pending operations enter the current
chain simulation, reverse restore ordering, and per-file drift checks.

Authenticate the referenced forward intent in the retained ledger and require
its `undo_of` to be empty. If its applied receipt exists, authenticate it and its
agreement with the intent; otherwise retain the existing unconfirmed-recovery
behavior. Bind every inverse to that forward mutation ID, the same workspace/path,
and exactly reversed signed hashes. Reject missing/mismatched originals and
inverse-to-inverse references. Never require applied-forward evidence where this
spec expressly permits intent-only recovery.

For normal multi-record preflight, compare expected content with the existing
simulated per-path state: after A0→A1→A2, the older record is checked against
simulated A1, not the initial live A2. Preserve the live guard immediately before
each actual operation and the existing already-target behavior on resume. Rows
classified as completed inverses are bookkeeping-only: skip their drift check and
leave simulated state unchanged rather than pretending their old target was
restored now. Older genuinely pending rows still check the actual/simulated state
and may refuse after later user edits.

Before starting a physical inverse, inspect retained evidence for completed
inverses of that exact forward mutation ID, including rows surviving checkpoint
deletion. Use the receipt scan in bounded pages, authenticate before relying on
`undo_of` for matching, and retain only matches for the selected forward IDs.
If that pass encounters invalid/unverifiable evidence, refuse before any write.
No unsigned lookup field or mutable restored flag is proof that no inverse exists.
This scan is O(retained history); if it becomes measured undo latency, add a
validated index then. Mark that implementation ceiling with a `ponytail:` comment.

An existing valid applied inverse means that physical undo already completed.
Reuse its evidence and reconcile progress without another signature or filesystem
call, even if a user later edited the target or recreated the original after-state.
Refuse a second physical inverse for that forward ID. Multiple intent-only inverse
attempts remain allowed; do not impose uniqueness on every `undo_of`. A valid
completed inverse is found by its lineage, not merely by the checkpoint's current
inverse reference. This protects replay only while the evidence remains available;
it does not extend the whole-ledger deletion/rollback guarantees in section 5.

After ruling out valid completed inverse evidence, classify remaining rows:

- If target state is already reached and no inverse intent exists, persist
  progress without a new intent or receipt. Do not attribute an external deletion
  or a skipped same-byte restore to Golem.
- If an inverse intent exists and target is reached after restart, preserve that
  intent without inventing an applied signature; mark progress under the existing
  idempotent rule and expose the unresolved evidence. Print an explicit
  `undo target reached; interrupted attempt has no applied receipt` notice and
  label checkpoint completion as unconfirmed; do not print only ordinary success.
- If restoration is needed, persist a new signed inverse intent first, call the
  existing WriteFileAtomic/RemoveFile once, verify the resulting state, then
  persist its applied receipt and `restored=1` in the same transaction.
- A prior failed attempt that left the original after-state may be retried with
  a fresh mutation ID; keep its unresolved evidence. Never reuse an identity for
  a second physical mutation attempt. A committed receipt is never signed twice.

Mutable progress flags control traversal, but cannot be presented as signature
evidence. Already-restored rows continue protecting subsequent user edits by
skipping filesystem operations; audit exposes missing inverse evidence where
applicable instead of interpreting a flag as proof.

### 4. Persistence and migration

Increment the internal checkpoint schema from v2 to v3 additively. Preserve
`prior_content`, hashes, modes, IDs, applied/restored flags, and state-machine
guards; do not rebuild the existing tables.

Add a `mutation_receipts` table with a monotonic scan sequence, unique mutation
ID, signed intent JSON, and nullable signed applied-receipt JSON. Add nullable,
unique forward/inverse mutation references to `checkpoint_files`, so evidence
cannot be attached to two different checkpoint operations. The ledger owns its
immutable workspace/path/transition facts inside the signed bodies. It must not
have a cascade dependency on checkpoint rows. New journal-managed records always
have a forward intent; null references are legacy/unverifiable, never silently
upgraded into signed records.

The foreign keys point from `checkpoint_files.forward_mutation_id` and
`checkpoint_files.inverse_mutation_id` to `mutation_receipts.mutation_id`, with
no cascade into the ledger. Create the ledger first, add nullable reference columns
with default NULL, then create separate ordinary UNIQUE indexes on those columns
inside the migration transaction. SQLite ADD COLUMN cannot add UNIQUE inline;
multiple NULLs are already permitted by an ordinary unique index, so a partial
index is unnecessary for this contract. [SQLite ADD COLUMN](https://www.sqlite.org/lang_altertable.html#alter_table_add_column),
[SQLite unique indexes](https://www.sqlite.org/lang_createindex.html#uniqueidx).

Insert a signed intent before inserting its referencing checkpoint row, in the
same transaction. On a definite Abort, remove the checkpoint reference before
deleting its unused intent. Preserve the existing explicit checkpoint-file cleanup
and application-level binding checks; foreign keys alone are not authentication.
Forward references may only name a forward intent; inverse references must name
an inverse of that row's forward ID. Per-column uniqueness is not a substitute
for those cross-column/semantic checks. Do not denormalize `undo_of` or introduce
another replay table for v1; the signed lineage already lives in the ledger.

Store `intent_json` and `applied_json` as `signing.MarshalCanonical` of the whole
envelope, including its sibling signature, with no trailing newline. The signature
still covers only the entire Body under its domain. These TEXT values are the
single stored representation; do not add duplicate canonical-byte columns or a
previous-receipt hash. At the storage boundary, reject noncanonical stored envelope
bytes instead of silently rewriting them. Portable decoding may canonicalize
equivalent whitespace/member order; stored bytes have the stricter invariant.
Future whole-envelope hashing uses exactly these bytes, and body-signature
verification continues to canonicalize/verify the Body separately.

Normal commit and undo progress each write their evidence and checkpoint flags
in one existing `execTx` transaction. Reuse conditional updates and exact-row
checks. Completed evidence is immutable; receipt absence is never verified success.

Provide a bounded, ordered, metadata-only scan starting from `mutation_receipts`,
returning sequence/intent/receipt without selecting `prior_content`. It must still
return entries after checkpoint pruning or successful undo; use no inner join to
checkpoint rows. Signature verification and unresolved-gap reporting are distinct
from raw storage scanning. #447 supplies its own audit command later.
Use `sequence INTEGER PRIMARY KEY AUTOINCREMENT` and a `sequence > cursor` scan
ordered ascending, never a timestamp cursor. Committed sequence values increase
but need not be gapless; they do not themselves prove ledger completeness.
[SQLite AUTOINCREMENT](https://www.sqlite.org/autoinc.html).

Keep the current 50 completed checkpoint and 64 MiB prior-content admission
policies. Those limits apply to undo snapshots, not receipt metadata. Preserve
unresolved ledger entries even if their checkpoint blob is later pruned; evidence
of uncertainty may outlive the ability to undo. No archive/compaction or log-chain
framework in v1. Document indefinite receipt growth and leave retention for an
explicit follow-up when required.

For v1/v2 databases, check for legacy `open` or `undoing` states **before** changing
the schema. Refuse upgrade with guidance to finish recovery/undo with the old
binary; leave its usable schema and data unchanged. Completed legacy rows migrate
intact and remain listable but fail strict `/undo` preflight. Missing or invalid
forward intents/references and invalid present applied receipts fail closed in v3;
never infer legacy permission from a null field. A valid intent with no applied
receipt remains explicitly unconfirmed and may support guarded undo as specified
below; that exception does not authenticate the original operation as applied.
Older binaries already reject the newer schema; downgrading requires restoring a
pre-upgrade DB backup. No in-place destructive downgrade or fake backfill.

### 5. Crash recovery and exact limits

Keep existing conservative filesystem classification for undo safety, while
separating it from evidence:

| Crash state | Recovery and evidence |
|---|---|
| Intent only; live state equals prior | Existing checkpoint row may be dropped. Retain the signed intent as unconfirmed: a same-byte write could have occurred. |
| Intent only; live state equals expected after | Keep checkpoint as undoable using authenticated intent and live guards. Preserve missing applied receipt; do not sign one during recovery. |
| Intent only; live state diverges from both | Keep conservative checkpoint state; `/undo` refuses divergence. Preserve unconfirmed evidence. |
| Live state unreadable/type-invalid | Refuse recovery and retain the open state. |
| Applied receipt and checkpoint commit both persisted | Verify and reuse them; no new signature or duplicate entry on restart. |
| Undo filesystem change landed before receipt/progress commit | Resume by live-state rules, retain inverse intent-only evidence, and do not claim observed execution retrospectively. |

The authenticity claim is that the host possessing the key signed a particular
transition/observation. It is not user approval, proof of which model/process
caused every byte, trusted time, or power-loss durability. Existing external-writer
TOCTOU windows and best-effort file fsync remain. Receipts do not by themselves
detect whole-ledger deletion, reordering, rollback, or truncation; no completeness
or external-anchor claim is made for #445. #447 must not treat an intent-only
entry as a clean successful audit.

Recovery uses a separate store transition from normal receipt commit: it can
retain `applied=1` solely for conservative undo bookkeeping while the ledger's
applied receipt stays absent. Recovery notices report the count of unconfirmed
attempts. D4's success guarantee applies to operations observed and completed in
the running process; reaching a target during recovery is reported as such, with
the evidence gap still visible.

### 6. Signer lifecycle and host wiring

Resolve the key path using `dataDirBase`; validate it is outside the workspace.
Reuse the signing package's hardened load/create behavior (owner-only key directory
and file, no symlink/permission bypass). Load once per write-enabled runtime,
including late mounting; never load/create from a model prompt or file tool call.

If this workspace has retained intents/receipts, call `LoadEd25519` (must exist)
and validate their key binding before allowing writes. Do not first create a
replacement and then discover the mismatch. If it has no such history, use
`LoadOrCreateEd25519`; make `created=true` visible as a new signing identity.
The key is shared per user across workspaces, but loss detection here is bounded
to retained current-workspace history: a new workspace cannot prove the previous
global identity existed. Do not claim otherwise. No automatic rotation or key
repair; unknown/mismatched identities fail write enablement.

For a parseable key-ID mismatch, report the first mismatch in scan order as:
`golem: signing key mismatch: receipt <mutation-id> names kid <receipt-kid>, but key file <path> has kid <loaded-kid>; writes disabled to preserve receipt integrity`.
IDs are public, validated fixed-format metadata; receipt identity is described
as a claim until authenticated, not as an independently trusted expectation.
Escape the key path with the existing control-safe diagnostic formatter. For a
missing required key, report the required path and historical claimed key ID,
with guidance to restore the matching key from backup; never automatically
generate or overwrite it. Malformed IDs/records receive an invalid-record error
without echoing unchecked bytes. Do not log PEM, private material, or signer values.

The same initialization path covers startup, headless writes, and `/allow-write`.
Read-only sessions do not touch signing keys. Late-mount failure preserves the
active runtime and releases the checkpoint lease. Use the signer-provided public
verifier for local verification; independent public-key export/retention is not
part of this ticket. HMAC is supported by the portable receipt helper through the
existing interface, but the Golem path always selects Ed25519.

No #446 implementation or agreed key configuration was found in local branches,
worktrees, or its issue discussion. This proposal aligns with shipped signing
discipline only. Recheck #446 before implementation; if it establishes a different
shared identity convention, resolve that specific conflict before key wiring.

### 7. Checkpoint evidence labels

Append an evidence label to the existing `/checkpoints` row; preserve checkpoint
numbering, goal escaping, and `[in progress]`/`[interrupted undo]` lifecycle markers.
Use the existing command, not a new UI or audit command:

- `[unsigned]`: at least one forward reference is null; `/undo` is unavailable.
  This includes migrated legacy rows without claiming missing metadata proves
  legacy origin. A non-null reference to missing evidence is invalid instead.
- `[unconfirmed]`: all required present evidence authenticates, but at least one
  forward attempt or retained inverse attempt for these rows lacks applied evidence.
  Completed retries do not erase earlier uncertain attempts.
- `[receipts verified]`: all forward receipts and any retained inverse-attempt
  evidence relevant to the checkpoint are complete, cryptographically verified,
  and bound to the checkpoint transition metadata.
- `[invalid receipts]`: present evidence, linkage, key identity, or signed-to-row
  metadata binding is invalid; never relabel tampering as legacy or unconfirmed.

For a mixed checkpoint, show the most restrictive label in this order:
invalid → unsigned → unconfirmed → receipts verified. The last label means only
receipt authenticity and metadata binding; it does not verify live workspace
content or stored prior-content blobs. Undo still hashes the actual restore blob
and checks filesystem state before writing. Reuse strict receipt verification and
metadata projections; listing must not fetch full prior-content blobs. One short
legend beside `/checkpoints` in the existing `/help` text explains these meanings.
Preserve read-only listing
behavior: it never signs, rewrites receipts, or repairs data.

If the retained-history scan encounters evidence it cannot authenticate, its
lineage cannot be trusted even when it appears unrelated to current checkpoints.
Fail the command with `receipt history unverifiable; evidence labels unavailable`
and do not emit checkpoint rows with inferred evidence labels. This command-level
failure takes precedence over per-row rendering. `[invalid receipts]` still covers
attributable linkage or checkpoint-metadata faults when the ledger evidence itself
authenticates; an unverifiable history entry must never be skipped to produce a
`[receipts verified]` label elsewhere.

## Part II — Implementation plan

Each task follows: write the named tests → run them red → implement only the
specified behavior → run green → check distinguishing mutants → independent
review and critique → fix to clean. Do not run implementation steps before approval.

### Task 1 — Portable intent/receipt contract

**Files:** create `agent/tools/mutation_receipt.go` and
`agent/tools/mutation_receipt_test.go`; update `agent/tools/mutate.go` comments
only as needed to describe its journal contract accurately.

**Interfaces:** produce concrete `MutationReceiptBody` and `MutationReceipt`
types, plus `SignMutationReceipt`, `DecodeMutationReceipt`, and
`VerifyMutationReceipt` helpers using the existing signing interfaces and context.
Sign consumes one complete typed Body; decode strictly validates the wire form;
verify authenticates that complete body and enforces its semantic/key binding.
Do not add another signer, sink, or journal interface.

- [ ] Pin one public fixed-key Ed25519 fixture with literal UTC body bytes,
  literal full framed bytes, and literal signature. Use stdlib Ed25519 verification
  over the literal frame as an independent check, without private production keys.
- [ ] Add table cases for field tampering, intent/applied substitution, wrong
  domain, wrong key/algorithm, unknown and duplicate fields, path/hash/mode/time
  validation, empty-file versus absent, and nil/canceled/failing signer. Remove
  each required member separately; test exact-case names, null versus omitted
  after_mode, tracked mode 0000 versus null, invalid bits/numeric types, and
  non-null mode on overwrite/inverse versus a valid tracked forward create.
- [ ] Implement the types and helpers; reuse the existing canonicalizer and
  signature frame unchanged. Exercise HMAC through the same portable helper.
- [ ] Verify each assertion is distinguishing: mutate every signed field, bypass
  identity validation, omit `kind`, and substitute empty-hash for absence one at
  a time; the relevant test must fail before restoring the implementation.

**Risk:** Blast radius — additive public record API used by #447. Data — signed
path and key identity metadata. Human approval covers the wire contract.

### Task 2 — Atomic evidence persistence and legacy migration

**Files:** modify `cmd/golem/checkpoint_store.go` and
`cmd/golem/checkpoint_store_test.go`.

**Interfaces:** store Prepare accepts the signed forward intent and stores its
mutation reference with the checkpoint row; commit accepts matching applied
evidence; undo preparation/progress accept inverse evidence. Add a receipt-only
scan by sequence/limit. Every method uses existing `execTx`, hardening, and
conditional-row state guards; no filesystem or signing work inside SQL helpers.

- [ ] Add fresh/v1/v2-to-v3 fixtures. Assert exact old IDs/blobs/modes/states
  survive, multiple legacy NULL references are allowed, duplicate non-null or
  dangling references refuse, and legacy interrupted states refuse before changing
  user_version. Exercise actual additive migration and unique-index creation.
- [ ] Test atomic forward and inverse evidence transactions, duplicate identity
  refusal, matching-intent enforcement, Abort, transaction rollback, and the
  post-commit hardening-error case.
- [ ] Test scan order/pagination and exact envelopes after checkpoint count/byte
  pruning and completed undo. Include unresolved entries after snapshot deletion.
  Use equal/backward timestamps and gapped sequences. Pin canonical whole-envelope
  bytes, reject noncanonical stored bytes, and allow equivalent whitespace/order
  only at the portable decoder boundary. Test intent-before-reference insertion
  and reference-before-intent Abort deletion with foreign keys enabled.
  Inspect the scan query to confirm it never selects prior-content blobs.
- [ ] Implement additive schema and concrete store operations. Update bounded
  growth documentation to distinguish snapshots from receipt metadata.
- [ ] Mutate the cascade/scan join, remove either half of atomic updates, remove
  uniqueness/matching checks, and allow interrupted migration; corresponding
  tests must fail. Preserve the existing quota and privacy tests.

**Risk:** Data and irreversibility — schema v3 blocks old binaries; legacy undo
compatibility changes and audit metadata grows. No destructive table migration.

### Task 3 — Forward mutation receipts and truthful recovery

**Files:** modify `cmd/golem/checkpoint_journal.go`,
`cmd/golem/checkpoint_journal_test.go`, and `cmd/golem/checkpoint_crash_test.go`.

**Interfaces:** inject the existing signer/verifier into the concrete checkpoint
journal; keep public Journal/Prepare/Commit interfaces unchanged. Prepare captures
the canonical transition; the prepared handle owns its signed intent and signs
applied evidence only after the real mutation path reaches Commit.

- [ ] Use real write/edit/promotion tools to prove signed create, overwrite,
  truncate, same-byte write, and tracked-mode promotion behavior.
- [ ] Test sign/Prepare failure before any write, post-write signer failure,
  live after-state mismatch, failed Commit/hardening, cancellation after landing,
  Record compatibility refusal, and preserved promotion error reporting.
- [ ] Extend subprocess crash fixtures at intent persistence, filesystem landing,
  and receipt commit. Include prior/expected/divergent/unreadable live states and
  equal before/after content. Repeated recovery produces no applied signature and
  no duplicate ledger evidence for the same attempt.
- [ ] Implement concrete Prepare/Commit/Abort evidence handling and recovery
  verification/classification. Keep unresolved evidence when dropping a
  never-observed checkpoint row; never sign historical success in recovery.
- [ ] Mutate signing order, remove latching, sign inside recovery, skip live
  verification, or admit Record as witnessed success; the named tests must fail.

**Risk:** Blast radius — all durable checkpoint callers. Data — cryptographic
evidence and restore blobs. Existing persistence failures must keep their exact
halt/refusal implications.

### Task 4 — Authenticated inverse receipts and undo/resume

**Files:** continue in `cmd/golem/checkpoint_journal.go`,
`cmd/golem/checkpoint_journal_test.go`, and `cmd/golem/checkpoint_crash_test.go`;
extend `cmd/golem/checkpoint_store.go` and its existing tests for the metadata
projection used by listing, plus `cmd/golem/repl.go` for the help legend and
`cmd/golem/repl_test.go` for command output.

**Interfaces:** ordinary undo and resume share authenticity preflight before
any pending restore. `restoreFile` adds inverse evidence around its existing
WriteFileAtomic/RemoveFile calls. It does not recursively create undo checkpoints.
Use the existing paged ledger scan for lineage and replay checks. Listing reuses
the receipt verifier and returns evidence labels independently of lifecycle state.

- [ ] Assert exact inverse hashes, new mutation ID, original `undo_of`, and
  applied receipt for overwrite restoration and created-file deletion.
- [ ] Reject missing/wrong forward ID, inverse-to-inverse lineage, different
  workspace/path, non-reversed hashes, and an inverse reference attached to another
  forward row, including a bad later record before any batch write. Keep successful
  intent-only forward recovery and A0→A1→A2 simulated undo tests.
- [ ] Preserve a completed inverse after deleting its checkpoint, recreate the
  reference and original after-state in a fixture, and prove no second filesystem
  inverse occurs. Reconcile the same completed attempt without signing again,
  preserving later user edits; distinguish separate forward IDs with identical
  transitions. Mix a completed inverse with an older pending write to the same
  path: skipping the completed row must leave simulated state unchanged and the
  older pending write must still refuse drift before any mutation. Keep fresh-ID
  retries for earlier intent-only inverse attempts.
- [ ] Tamper the signature, signed fields, checkpoint path/hash/mode, and prior
  blob in a later record of a multi-file undo; ordinary undo and resume must
  refuse before their first filesystem change. Legacy unsigned rows refuse too.
- [ ] Cover already-absent create, same-byte restore, interrupted inverse before
  mutation, interrupted inverse after mutation, committed progress, and a fresh
  retry following an uncertain failed attempt. No fabricated or duplicate receipt;
  assert the explicit unconfirmed notice and checkpoint completion label when
  recovery reaches the target without applied evidence.
- [ ] Preserve the existing behavior that resumed already-restored paths retain
  subsequent external edits, while missing inverse evidence stays unconfirmed.
- [ ] Implement inverse Prepare/apply/Commit and transactional progress. Verify
  signatures, lineage, retained completed-inverse evidence, and signed-to-checkpoint
  field binding before using restore data. Do not treat missing forward applied
  evidence or an unresolved prior inverse as a blanket replay refusal.
- [ ] Test each evidence label, mixed-state precedence, retained uncertain
  inverse attempts, invalid-signature handling, and lifecycle-marker coexistence
  through `/checkpoints`. Preserve escaped goals and numbering. A changed live file
  does not change `[receipts verified]` into a claim of live-state validity; undo
  still refuses a pending restore of that changed file. Pin the help legend and
  prior-blob-free projection, including invalid dangling versus null references.
  An unverifiable history entry outside current checkpoint references must produce
  the command-level labels-unavailable error, never verified rows or silent omission.
- [ ] Mutate preflight ordering, skip prior-blob hashing, sign an already-target
  file, reuse an ID on retry, bypass lineage/replay checks, assign a verified label
  from non-null JSON alone, or trust unsigned legacy data; tests must fail.

**Risk:** Data — restores overwrite/remove user files. Compatibility — D5 changes
old unsigned undo availability. Existing all-file preflight and crash resume
protections must remain intact.

### Task 5 — Enable the signing identity on every covered host path

**Files:** create `cmd/golem/mutation_signing.go` and
`cmd/golem/mutation_signing_test.go`; modify `cmd/golem/tools.go`,
`cmd/golem/main.go`, `cmd/golem/mount.go`, and their relevant existing tests
(`tools_test.go`, `mount_test.go`, `mount_run_test.go`, `main_test.go`,
`headless_test.go`). Use the existing run/headless fixtures rather than making a
second harness.

**Interfaces:** one concrete key-loading helper returns signer/verifier and a
new-identity notice; `buildWriteTools` supplies them to the concrete journal.
The helper takes explicit root/data-dir context. No process-global state or new
CLI flag. Callers preserve their current resource ownership and mount ordering.

- [ ] Use temporary data directories to test first creation, stable reload,
  owner-only files, insecure/inside-workspace paths, missing historical key,
  changed identity, and failure before replacement creation. Pin mismatch/missing-key
  diagnostics including public IDs and an escaped control-bearing path; assert no
  private PEM/material is emitted and the key file is not created or overwritten.
- [ ] Exercise startup, headless write/edit, late `/allow-write`, and scratch
  promotion through real construction paths. Verify read-only startup creates
  no key, and failed late signing initialization leaves the runtime unchanged
  and releases its lease. Pin that `/allow-write` leaves startup-frozen scratch
  promotion unavailable rather than rebuilding its runtime or approval identity.
- [ ] Implement shared initialization and visible new-identity notice. Fail
  write enablement on key/integrity errors; never fall back to unsigned writes.
- [ ] Mutate one mount path to omit signing, silently rotate a missing historical
  key, or swallow initialization errors; corresponding host tests must fail.

**Risk:** Data/cryptography — persistent per-user key identity. Blast radius —
startup/headless/late-mount lifecycle shared with #354/#372. Recheck #446 convention
before this task; resolve an actual conflicting convention before editing.

### Task 6 — Documentation, final review, and release gates

**Files:** `README.md`, `cmd/golem/doc.go`,
`changelog.d/445-mutation-receipts.md`; copy the approved spec/plan into the feature
worktree's `docs/plans/2026-09-05-mutation-receipts-445-spec-plan.md`.

- [ ] Document the exact D1 scope, signing identity/path, D5 upgrade impact,
  receipt/intent distinction, retention behavior, and missing applied-receipt
  failure semantics, including evidence labels and key recovery diagnostics.
  Explicitly distinguish AgentFlow's existing proof receipts from these
  MutationReceipts and keep the no-chain/no-completeness claim explicit.
- [ ] Run the focused package suites and race checks for touched packages.
- [ ] Run `golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`.
- [ ] Run `docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`.
  Capture the exit code directly; never pipe test output to `tail`.
- [ ] Complete independent defect review and critical review of those findings;
  resolve findings and rerun checks only where new changes require them.
- [ ] Review the complete diff, confirm lane boundaries and unchanged approval
  behavior, add the changelog fragment, and prepare the PR against `develop`.
  PR creation/publication follows the user's session authorization; do not imply
  the current request authorizes publishing before implementation approval.

**Risk:** Unknowns — current baseline and sibling work may change during approval.
Revalidate before creating the worktree; no timing assumptions justify overwriting
another lane's work or unrelated current checkout files.

## Dependencies and validation commands

Task order is 1 → 2 → 3 → 4 → 5 → 6. Task 1 establishes the wire contract; Task 2
establishes atomic persistence; Tasks 3–4 attach it to real filesystem operations;
Task 5 makes it reachable through the product. Task 6 does not count as complete
until the required full gate and review cycle pass.

After approval, create the linked worktree from freshly fetched origin/develop.
Use its absolute path for every command. The focused verification commands are
`rtk proxy env -u GOROOT go test ./agent/tools ./cmd/golem` and
`rtk proxy env -u GOROOT go test -race ./agent/tools ./cmd/golem`, after the required
`cd <worktree> &&` prefix. Run failing targeted tests first within each task; do
not rerun unrelated suites merely to produce more output.

## Human sign-off and remaining context

Approve D1–D5 and Tasks 1–6 together before code. This is the user's explicit gate,
also consistent with the external safety contract's cryptography escalation and
the planning workflow's public API/schema/data-risk review requirements.

There is no repository-local `docs/ai/PROJECT-CONTEXT.md`, `docs/ai/ai-kit.yaml`,
or `docs/ai/safety-rules.md`. This draft uses `CLAUDE.md`, inspected code/tests,
the supplied handoff, GitHub issues, and the externally referenced safety rules.
It does not invent missing sensitive-path rules or an approved #446 convention.

Sources: [#445](https://github.com/kstruzzieri/go-llm/issues/445),
[#446](https://github.com/kstruzzieri/go-llm/issues/446),
[#447](https://github.com/kstruzzieri/go-llm/issues/447), and the shipped
`signing/doc.go`/`signing/signing.go` contract at the verified baseline.

## Execution appendix — 2026-09-06

The approved specification and its dated discovery/approval record above are
preserved as the historical contract. Execution used the isolated
`feat/445-mutation-receipts` worktree from fresh `origin/develop` at
`b97613be5eca70205fb92b0cf114236f36e37fd4`. Tasks 1–5 passed their independent task
reviews and controller critiques. Task 6 documentation and release evidence are
recorded here; the controller still owns the independent final whole-branch
review and explicit Task 6 spec/quality assessment. The unavailable literal
`/code-review` and `/criticize-review` commands were replaced by independent
reviewer passes and controller critical review, not claimed as executed commands.
No push, PR creation, merge, or external publication is authorized by this work.

The fresh #446 convention check before Task 5 inspected sibling commit `7a474a5`:
`memory/open.go` and `cmd/golem/memory_records.go` use `<database path>.keys`, with
`memory/record_keys.go` loading `current.pem` inside that per-database directory.
That memory-store identity is not a competing shared per-user Golem mutation
identity convention. D3 remained unchanged; the inspection was read-only and
neither sibling lane nor `signing/` implementation was edited.

Three implementation rulings recorded during execution (including their costs):

1. Cap each portable receipt envelope at 32 KiB before canonicalization — the
   signing package documents approximately 100x transient amplification and asks
   consumers to bound untrusted input — cost if wrong: unusually long external
   receipt paths are rejected; real workspace paths fit comfortably. The bound
   applies to Sign output, Decode input, and typed Verify input; ordinary bounded
   encoding precedes generic canonicalization.
2. Validate mutation IDs and nonempty undo_of as at least 26 ASCII base32
   characters (A-Z, 2-7) — matches crypto/rand.Text while tolerating future length
   increases and keeping diagnostic metadata safe — cost if wrong: opaque
   external IDs outside that spelling require a later schema revision.
3. Move the minimal Task 5 key helper and constructor wiring needed to activate
   signing into Task 3 — the journal cannot require a signer while keeping real
   host paths and existing tests functional without it; avoids temporary
   ephemeral keys or a broken write path — cost if wrong: Task 3 review spans
   more files, while Task 5 remains the dedicated end-to-end lifecycle gate.

Release commands below all start with
`cd /Users/keith.struzzieri/projects/go-llm/github/go-llm/.worktrees/445-mutation-receipts &&`.
Logs are retained locally in
`.superpowers/sdd/2026-09-05-mutation-receipts-445-spec-plan/`; each command's stdout
and stderr were redirected directly to the named log and its actual process exit
was captured separately, without a pipeline or `tail`.

| Command after the worktree prefix | Actual result | Log |
|---|---|---|
| `rtk proxy env -u GOROOT go test ./agent/tools ./cmd/golem` | Exit 0; 17.807s / 86.563s. Initial run before lint fixes. | `task-6-focused.log` |
| `rtk proxy golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...` | Exit 1; eight unchecked test Close calls and two equivalent De Morgan simplifications, all introduced by this branch. | `task-6-lint.log` |
| `rtk proxy env -u GOROOT go test ./agent/tools ./cmd/golem` | Exit 0; 16.131s / 67.803s after those minimal lint fixes. | `task-6-focused-final.log` |
| `rtk proxy golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...` | Exit 0; 0 issues after fixes. | `task-6-lint-final.log` |
| `rtk proxy env -u GOROOT go test -race ./agent/tools ./cmd/golem` | Exit 0; 38.652s / 160.771s. | `task-6-race.log` |
| `rtk proxy docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full` | Exit 0; format, lint (0 issues), repository-wide race tests, and compile smoke passed. | `task-6-docker-full.log` |
| `rtk proxy scripts/check-changelog b97613b` | Exit 0. | `task-6-changelog.log` |
| `rtk proxy git diff --check b97613b` | Exit 0; no whitespace errors. | Tool output |

The full Docker gate runs formatting, lint, repository-wide race tests, and
compile smoke. The Task 6 lint fixes only spell the same character predicates
differently and explicitly ignore deferred test cleanup errors using the existing
repository pattern. No behavior assertions were weakened or new behavior added;
existing package/race suites verify the amended code. Documentation-only changes
need no additional behavior tests.

Scope review confirmed unchanged public `Journal`, `PreparingJournal`,
`PreparedMutation`, and tool constructor signatures, no dependency additions,
no excluded-lane or signing-implementation edits, and no `CHANGELOG.md` edit.
Host changes supply the signer and identity notice through existing startup/late
write construction; approval keys, prompts, path containment, scratch promotion
availability/mode guards, turn serialization, transactional admission, hardening
errors, and pending-undo drift checks retain their existing boundaries.
The approved D1/D5 scope and the no-chain/no-completeness limits remain explicit.
Local PR title/body for `develop`: `/private/tmp/go-llm-445-pr.md`.
