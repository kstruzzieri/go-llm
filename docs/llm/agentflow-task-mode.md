# AgentFlow planning and task modes

Golem planning mode (`-goal`) authors a traceable AgentFlow plan, previews it,
and locks it only after explicit human approval, then stops.
Task mode (`-plan`) separately runs that locked plan step-by-step in a
proof-carrying loop. It is serial by default, with an optional isolated initial
cohort before dependent work continues serially. AgentFlow owns durable proof
state on disk; Golem owns the model loop and the in-process guards that keep the
model inside what the plan allows. The two are separate, cooperating processes:
AgentFlow never runs a model, and Golem never writes proof state directly.

## Ownership

| AgentFlow owns | Golem owns |
|---|---|
| Plan lock and validation (`lock-plan`) | Reading the plan document |
| Workflow recommendation and contract policy (`recommend-workflow`, `workflow-contract`) | Projecting explicit task facts and showing the selected route |
| Per-step claim and attempt tracking (`claim-step`) | Driving the state machine (the `driver`) |
| File-change receipts (`record-file-change`) | Running the model per step |
| Review evidence and finding-linked amendments (`record-review`, `amend-step`) | Selecting AgentFlow's amendment-ready projection and driving each attempt |
| Command-gate execution and receipts (`run`) | The pre-write scope guard mirroring the step's effective scope |
| Drift detection (`finish-run` audit) | The fatal-on-unreceipted-edit journal |
| The proof pack — durable, model-opaque `.agent/` state | Recovery reporting |

## Planning mode

Run `golem -goal "<text>" -root <workspace>` to have the local model inspect the
workspace with read-only file tools and submit one typed aggregate containing
the task type, objective, non-goals, invariants, requirements, acceptance
criteria, steps, and criterion mappings. The required `task_type` is one of
`docs`, `bugfix`, `feature`, or `refactor`. The planner may also provide
`security_sensitive`, `blast_radius`, and `declared_size`, but is told to omit
them when repository evidence or the user's request does not make them explicit.
An omitted signal stays unknown; Golem never converts absence into a safe value.
Golem compiles the aggregate directly into AgentFlow's
optional `requirements[].acceptance_criteria`, `steps[].criterion_ids`, and
`gates[].criterion_ids` fields; there is no second spec file or proof ledger.

Before any AgentFlow state is initialized or locked, Golem rejects duplicate or
malformed stable IDs, dangling mappings, criteria without an implementing step,
proving gates mapped outside their parent step, criteria without a proving gate
or review floor, and the existing dependency, path, scope, and argv errors.
Criterion IDs share one plan-wide namespace: reusing an id across requirements
is a duplicate.

For a locally valid submission, Golem sends AgentFlow task-brief schema `0.1.0`
to the read-only `recommend-workflow --stdin --json` command. Existing plan facts
are reused exactly: `risk_level` becomes `declared_risk`, the first-seen union of
step file scopes becomes `candidate_files`, and structured validation labels
become `validation_needs`. Golem does not scan the repository, keyword-match
prose, infer task type, or implement a second risk classifier. It fails before
model work when the installed AgentFlow lacks the stable recommendation or
contract commands and flags.

Golem then renders a deterministic, control-safe preview with
scope, risk, file boundaries, rollback, the compiled schema version and drift
budget, the requirement-to-step-to-gate mapping, exact validation argv, and the
AgentFlow recommendation. The route section shows recommended and selected
pack/profile, signals and rationale, alternatives, selection/override reason,
review depth, whether a review run is required, required capabilities, required
gates, and hunk-attribution policy. Golem asks `Lock this plan? [y/N]` only after
that route is visible. When a rejected lock leads to a repaired
resubmission, the second preview appends a line-level delta against the previous
one. A denial, EOF, or interruption before any approved submission leaves
AgentFlow proof state unchanged; a denial also saves the compiled plan to a temp
file named in the error so the authoring work can be inspected or adapted.
Approval initializes AgentFlow, locks `.agent/plan.lock.json`, and asks
AgentFlow to materialize the exact validated candidate through
`workflow-contract --from-json`. Golem never writes
`.agent/workflow.contract.json` itself. It also saves the approved task brief in
a mode-0600 `.agent/golem-task-brief-*.json` file. A second mode-0600
`.agent/golem-workflow-handoff-*.json` file contains the validated
recommendation plus SHA-256 digests of the canonical approved plan and task
brief. The separate `-plan` command includes both paths. At task startup, Golem
revalidates the handoff, matches both digests, and verifies its candidate against
AgentFlow's already-materialized contract. The task driver then reuses that
approved route without recommending or materializing it a second time. This
prevents a stale or edited plan/brief, or a changed AgentFlow version, from
silently replacing or inheriting the human-approved route. For scripted or CI
use,
`-approve-plan-lock` prints the same preview and approves the lock without
prompting. `-goal` never executes the plan or edits source files, and it refuses
to replace a locked plan, a non-empty draft, or an unrecognized plan file.

The AgentFlow v0.4 runtime still uses plan schema `0.3.0`. Plans that omit
`requirements` remain valid for existing `-plan` users and do not gain criterion
coverage. A review-backed criterion may declare `spec_quality` or `deep`; Golem
authors that floor into the lock, while AgentFlow remains responsible for later
review evidence and proof projection.

The #277 local-model spike locked 48/48 toy plans against AgentFlow 0.3.0 on the
first try with both a 9B dense model and a 35B-A3B MoE model. The load-bearing
request settings were thinking disabled and at least about 3,500 output tokens;
a worked example was not needed. Planning mode applies those settings only to
its model request, preserves a larger caller-provided output budget (including
a `-output-reserve` above the floor; a smaller nonzero reserve is raised to
it), and keeps one bounded repair submission after a rejected first plan.
Re-run the spike if the AgentFlow validator contract tightens.

## Enabling task mode

- `-plan <plan.json>` — required; the path to the plan document to lock and
  execute. Passing it turns on task mode.
- `-plan-workers N` — optional, task-mode-only worker bound. The default is `1`
  (serial), and `N` must be positive. Values above `1` attempt the bounded
  initial cohort described below and fall back to the unchanged serial loop
  when fewer than two safe steps qualify.
- `-approve-plan-edits` and `-approve-plan-gates` — both required in headless
  task mode. Task mode has no TTY approver, so the run refuses to start
  unless both approval classes are opted in up front: one for step-scoped
  write/edit tool calls, one for running the plan's declared validation
  gates.
- `-agentflow-src <checkout>` — run `python3 -m agentflow` from a source
  checkout (`PYTHONPATH=<checkout>/src`) instead of the installed `agentflow`
  binary. Use this when the CLI isn't installed on PATH.
- `-evidence <sidecar.json>` — optional. Records evidence with AgentFlow
  before the plan is locked. Accepts one JSON object or an array of objects;
  each entry needs `id`, `claim`, and `source`.
- `-review-manifest <review-manifest.json>` — optional and valid only with
  `-plan`. Records the manifest through AgentFlow, then opens explicit
  finding-linked amendment attempts for amendment-ready active findings. The
  path is resolved against the caller's cwd (like `-plan`, not `-root`) and is
  preflighted, so a missing manifest fails before any step runs.
- `-task-brief <brief.json>` — optional and valid only with `-plan`. Supplies a
  closed AgentFlow task-brief `0.1.0` document for an externally supplied plan.
  The path is resolved against the caller's cwd. Its `declared_risk` must equal
  the plan's `risk_level`. Golem always unions the plan's exact step files and
  gate labels into `candidate_files` and `validation_needs`, respectively.
  Explicit values may add context but cannot remove exact plan facts, including
  by supplying an empty array.
- `-workflow-handoff <recommendation.json>` — optional, task-mode-only input
  emitted by planning mode. It binds task execution to the recommendation that
  was visible at approval. Golem verifies its canonical plan/brief digests and
  compares its candidate with `.agent/workflow.contract.json` before
  constructing the AgentFlow client. It fails closed on a missing, changed, or
  mismatched file. Do not combine it with
  `-workflow-profile` / `-workflow-reason`.
- `-workflow-profile <profile> -workflow-reason <reason>` — optional, paired
  flags for either `-goal` or `-plan`. Both are required together and the reason
  must be non-empty. Golem forwards them to AgentFlow and previews the returned
  override; it never changes a recommendation silently or accepts a changed
  selection whose AgentFlow response lacks override provenance.

`-plan` is mutually exclusive with `-p` (one-shot mode), `-allow-write` /
`-allow-exec`, `-rag-db`, `-delegate`, and `-mcp-stdio` / `-mcp-http`. Task
mode builds its toolset from the locked plan alone, so it refuses to start
if any of those are also passed — it is a constrained proof surface, not a
general-purpose agent session with a plan bolted on.

## Inspecting and resuming an existing run

Inspect a workspace without changing AgentFlow state:

```text
golem -root <workspace> -agentflow-status
golem -root <workspace> -agentflow-status -json
```

Human status shows the authoritative `next-action` state and reason, current
step/gate, attempt owner and lease, typed gate statuses, diagnostics, and the
serial resume disposition. Any suggested command is labeled display-only and
is never executed. JSON mode relays AgentFlow's exact `next-action --json`
bytes. Both forms use actor `golem` and make only that read-only AgentFlow call.
Before returning exit `0` or `2`, status validates the typed projection's
contract fields, actor, attempt owner, lease, and recovery permissions. A
foreign, expired, malformed, or otherwise unsafe projection is displayed as
blocked and exits `3`; JSON mode still relays AgentFlow's exact bytes.
Before exit `2`, status also applies the same P0 and traceability preflight to
the locked plan that resume applies to the supplied matching plan.
When the state is `complete`, human status reads the proof pack only after
`next-action` has verified it, then shows the absolute artifact path and counts
for `passed`, `warning`, `failed`, `not_run`, `skipped`, and `not_applicable`
checks. A missing or malformed summary is an unsafe exit `3`. A merely present
proof file is never reported as verified. JSON output remains byte-exact but
uses the same proof-consistency exit decision. If `next-action` cannot be read,
human mode reports the sanitized failure, JSON mode emits no bytes, and status
exits `3`.

Status exit codes are stable for scripts:

| Exit | Meaning |
|---:|---|
| `0` | Run is complete and proof is verified. |
| `2` | A safe serial resume disposition exists. |
| `3` | Setup is required, or recovery is blocked, invalid, or unsafe. |

Resume an existing run with the original plan and the ordinary headless task
approvals:

```text
golem -root <workspace> -agentflow-resume -plan <plan.json> \
  -plan-workers=1 -approve-plan-edits -approve-plan-gates
```

Resume is serial-only. It rejects evidence ingestion, review manifests,
workflow overrides, planning, one-shot, RAG, delegation, MCP, general
write/exec, and every worker count other than one. `-task-brief` and
`-workflow-handoff` remain optional: self-materialized task runs are resumable
without a handoff, while a supplied planning handoff adds its existing exact
plan/brief digest and workflow-candidate binding.

Before the first mutation, Golem requires the projection to have no diagnostics
and checks all of these bindings:

- the locked plan digest equals the canonical supplied-plan digest after
  excluding only `locked` and `locked_at`, using AgentFlow's Python JSON
  escaping and number encoding exactly;
- the execution-contract digest equals SHA-256 of the exact materialized file
  bytes;
- the existing workflow contract passes the closed-schema validator, plus the
  handoff candidate check when a handoff is supplied;
- an open attempt has a non-empty id, is owned exactly by `golem`, has a live
  or no-deadline lease, and exposes one allowed automatic non-break-glass
  `continue` action. The independent owner check also applies under advisory
  lease policy.

The plan digest now uses AgentFlow's exact Python JSON encoding. Workflow
handoffs saved by earlier Golem versions recorded a Go-canonicalized digest, so
a pre-existing handoff for a plan containing `<`, `>`, `&`, non-ASCII text, or
float literals fails the handoff digest check after upgrading even though the
plan is unchanged; re-run planning to refresh the handoff.

For `validation_missing`, projected command gates are paired by their filtered
plan order, not by the human validation label. Each projected label must equal
the joined plan argv; Golem runs only `missing` gates with plan-owned argv and
never repeats a `satisfied` receipt. Known inspection/legacy projections do not
participate in command pairing, and unknown kinds or statuses fail closed. A
step whose plan declares two command gates with identical argv also fails
closed during recovery, because positional pairing could not be proven
unambiguous under reordering.
The `enforce` lease policy fails closed for `step_unclaimed` even before any
lease exists, because claiming would enter a new finite enforced lease
mid-recovery; enforce-policy runs are therefore only ever settled up to their
interrupted attempt, never advanced to new steps. A live finite enforced lease
also fails closed for `validation_missing`, `step_unverified`, and
`step_uncompleted`, regardless of the displayed remaining time. AgentFlow's gate adapter may append `lease_renewed`, `finish-step` records
verification before its final expiry check, and `complete-step` checks expiry
before taking its separate close lock. A time estimate therefore cannot prove
no-renew, duplicate-free recovery. An operator must resolve that lease
explicitly outside Golem. Advisory and no-deadline attempts retain the normal
serial recovery path.
After any attempt settlement, Golem performs one read-only progress check and
refuses to repeat a mutation when the state did not change. No-settle states go
directly into the existing serial step/tail driver.

The complete disposition table is:

| AgentFlow state | Exit | Resume behavior |
|---|---:|---|
| `uninitialized` | 3 | Setup required; never call `init`. |
| `plan_unlocked` | 3 | Setup required; never relock. |
| `execution_uninitialized` | 3 | Setup required; never call `init-execution`. |
| `state_invalid` | 3 | Fail closed with diagnostics. |
| `step_unclaimed` | 2 | Enter the existing serial step loop; claim only immediately before model work. |
| `file_receipts_missing` | 3 | Fail closed; edits cannot be reconstructed safely. |
| `validation_missing` | 2 | Run only typed missing command gates, then finish the owned attempt. |
| `step_unverified` | 2 | Call the fixed `FinishStep` adapter for the owned attempt. |
| `step_uncompleted` | 2 | Call the fixed `CompleteStep` adapter without re-verifying. |
| `drift_failing` | 3 | Fail closed for operator inspection. |
| `run_unverified` | 2 | Enter the serial tail and call `FinishRun` once. |
| `proof_missing` | 2 | Call `FinishRun` once to generate and verify the first proof. |
| `proof_stale` | 3 | Fail closed; do not overwrite proof after changed inputs. |
| `proof_failing` | 3 | Fail closed; preserve the failing proof. |
| `complete` | 0 | Report verified proof and perform no mutation. |

The lease-safety rule above overrides this table: `step_unclaimed` under an
`enforce` lease policy, and `validation_missing`/`step_unverified`/
`step_uncompleted` under a live finite enforced lease, exit `3` instead of `2`.

Unknown future states fail closed. Resume never initializes, locks, records
evidence or reviews, renews/reclaims leases, re-runs a model for an open
attempt, executes advisory command strings, rebuilds stale/failing proof, or
starts parallel recovery. Normal interactive startup only detects a
case-insensitive `.agent` directory and prints the status/resume commands; it
does not inspect or mutate the ledgers.

## Workflow routing in task mode

An external plan without `-task-brief` remains backward-compatible through a
conservative exact-fact projection: `task_type` is fixed to `feature`,
`declared_risk` comes from the plan, and candidate files and validation needs
come from its steps and gates. Security sensitivity, blast radius, and size stay
absent. This deliberately floors an underspecified low-risk legacy plan at
AgentFlow's medium-feature route; missing signals cannot select docs-only or
small-bugfix. Use an explicit brief when a lighter or stricter task type is
known. Exact plan files and gate labels remain authoritative and are unioned
into that brief, so a claimed small scope cannot hide a larger locked-plan
scope. Golem still does no local classification.

For an external plan, the task driver order is:

1. Probe the base and workflow-routing CLI surfaces.
2. Ask AgentFlow for the route and print it at task startup, before `init` or any
   other `.agent` mutation.
3. When a review manifest is present, probe review ingestion; then initialize,
   record evidence, lock the plan, and materialize that exact
   recommendation once through AgentFlow.
4. Initialize execution, run plan steps and declared gates, and print the same
   route again immediately before review-manifest ingestion.
5. Let `finish-run` build and verify proof under the materialized policy.

For a planning-mode handoff, Golem first validates the saved recommendation,
matches the loaded plan and task brief to their approved digests, and compares
the candidate with AgentFlow's existing workflow contract before any AgentFlow
call. The driver then probes the CLI, prints the saved route, and follows the
same execution sequence while skipping both `recommend-workflow` and
`workflow-contract`; the approved candidate is not materialized twice.

The workflow contract may require gates or capabilities beyond what the loaded
plan/run supplies. Golem does not invent a security scanner, review agent,
capability receipt, waiver, manifest, or plan gate, and it does not claim that
direct contract materialization hydrates those declarations into the plan.
AgentFlow remains responsible for interpreting the contract under its own
validation and proof semantics. In particular, `spec_quality` and `deep`
policies can require a recorded review run at adequate depth; without one,
Golem fails closed and surfaces AgentFlow's failed
`required_review_satisfied` proof check as the actionable error.

## Locked step instructions

For a plan with `requirements`, each model run is instructed from a
deterministic projection of the locked plan: the objective, invariants,
non-goals, current step preconditions/action/files/expected diff, validation
labels and structured command argv, plus only the acceptance criteria named by
that step and their parent requirement text. Requirement text appears once even
when several selected criteria share it. A traced step with no `criterion_ids`
still receives the enriched plan/step header and an explicit empty criteria
section. Unrelated requirements and criteria are excluded.

Requirement-free plans retain the previous minimal instruction bytes exactly.
Before AgentFlow is probed or initialized, evidence is recorded, or a step is
claimed, task mode rejects malformed or dangling traceability, gate criteria
outside their parent step, and criteria without an implementing step or a
proving gate/review floor. The proving-gate/review-floor requirement is an
intentional Golem strictness: AgentFlow v0.4 currently locks a criterion that is
step-mapped but has neither verification mapping.

AgentFlow v0.4 has no canonical design-decision or design-reference fields.
Task mode therefore does not invent a sidecar or proof field for them; projecting
applicable design references remains blocked on an upstream contract extension.

## Review amendments

With `-review-manifest`, task mode completes any ordinary eligible steps, prints
the selected workflow route again, and then calls `record-review --json`.
AgentFlow validates and records the review
evidence before Golem opens an amendment. Golem then validates the returned
projection against the loaded plan and fails before model edits when it is
malformed. The `review_run_id` must match `RR-<UTC timestamp>-<8 hex>`, every
finding needs a non-empty `finding_id`, and (under `amendment_ready: true`)
finding ids must be unique. A finding Golem is about to turn into work is
validated further: its `severity` must be a known value and, if present, its
`location` well-formed; it must carry `owning_step`, `claim`, and
`suggested_fix`; and its `owning_step` must name a locked step.

Only `amendment_ready: true` findings with status `open` or `accepted` become
work. Golem groups them by locked plan order, preserves manifest order within
each step, and opens one `amend-step` with every canonical
`RR-...#finding-id` reference in that group. The model receives the locked step
slice plus only those references, claims, optional locations, and suggested
fixes. The amendment uses the same scope guard, mutation journal, file
receipts, structured gates, and `finish-step` path as an ordinary attempt.

Legacy runs with `amendment_ready: false` and inactive `fixed`, `rejected`, or
`superseded` findings are printed with their reference and status but never
open attempts. Because such findings are display-only, an unrecognized
`severity` or `status`, or a location Golem never consumes, degrades them to
display-only rather than aborting a completed run — only findings queued for
amendment are strictly validated. Golem does not infer ownership, mark findings
fixed, or rewrite review state. After an amendment, the recorded review may still block
`finish-run`; that failure and AgentFlow's recovery state are reported until a
new authoritative review manifest records the updated finding status.

## Proof-mode guards

- **`.agent/` is opaque to the model.** Every path under `.agent/` is denied
  for both read and write, matched case-insensitively on the path's first
  segment, so the model can neither read nor forge AgentFlow's proof state.
- **Edits are confined to the step's effective scope.** For the step
  currently claimed, the allowed set is `step.files` intersected with the
  plan's `allowed_files`, minus `blocked_files`. This mirrors AgentFlow's own
  `record-file-change` scope check exactly, so Golem's pre-write guard and
  AgentFlow's receipt-time check cannot drift apart.
- **No free-form command execution.** Proof mode does not attach the
  `run_command` tool. The only commands that run are the plan's structured
  command gates, dispatched through AgentFlow's `run` subcommand as an argv
  slice — never built or interpreted as a shell string.
- **Every applied edit is receipted.** Each write goes through a journal that
  calls AgentFlow's `record-file-change` for that step and attempt. If a
  receipt call fails, that failure is fatal to the run: the run context is
  cancelled and the model cannot continue past an edit AgentFlow doesn't know
  about.

## Authoring the plan

AgentFlow's `finish-run` drift audit compares the workspace against its git
baseline and flags any changed file not covered by the plan's `allowed_files`
as out-of-scope drift. AgentFlow writes its own proof state under `.agent/`
during the run, so that path must not read as drift: either add `.agent/` to
the workspace's `.gitignore`, or include `.agent/` in the plan's
`allowed_files` (as the fixture at `testdata/agentflow/plan.json` does).
AgentFlow does not manage `.gitignore` itself, so one of the two is required
for `finish-run` to pass. This is independent of Golem's model-facing guard,
which denies the model any access under `.agent/` unconditionally, whatever
`allowed_files` says.

For traced plans, every acceptance criterion must appear in at least one
step's `criterion_ids`. Each command gate that proves a criterion repeats that
ID in its own `criterion_ids`, which must be a subset of the parent step's
mapping. Criteria intended for semantic review instead declare a
`review.minimum_depth`; Golem does not synthesize or cache review results.

## Scope and deferrals (P0)

With `-plan-workers` above `1`, task mode may run one initial cohort only from a
fresh, clean Git root and only when the plan has a dependency edge. Qualifying
steps have no dependencies, and their exact literal files must equal effective
scope, exclude `.agent`, `.git`, and blocked paths, and be pairwise disjoint
under case-insensitive equality and ancestor checks. Existing paths must be
tracked regular files at the recorded base; new paths must be unignored with
safe parents. Symlinks, directories, or ambiguous ownership fall back to
serial. Tracked candidates marked `assume-unchanged` or `skip-worktree` are not
eligible; either flag anywhere in the tracked canonical tree disables the
optional cohort so gates cannot verify different bytes in a fresh worktree.
Sparse checkouts carry those flags pervasively, so parallel mode always runs
serially there. A dirty or non-toplevel canonical root is not a fallback case:
task mode refuses to start and names the offending path, because promotion
must never race uncommitted work.
Each worker runs fresh in its own detached worktree after an opaque
copy of canonical `.agent/` state. Golem validates worker diffs, promotes only
those source bytes, and lets AgentFlow perform dry-run then real ledger
aggregation; AgentFlow does not merge source files. Dependent work then
continues serially in the canonical root, which calls `finish-run` exactly
once.

Deterministic promotion, dry-run, or collision failures roll back canonical
promotion; collision errors name each reported collision kind. During
integration Golem holds an exclusive OS-level file lock under the Git dir —
the kernel releases it if the process dies, so a crash cannot leave the
workspace permanently locked, and the lock file itself persists between runs
by design. Golem anchors
file operations to the opened canonical root, and compares path bytes, mode,
and identity before both promotion and rollback. Canonical drift is therefore
reported instead of overwritten. An ambiguous real aggregation failure
preserves promoted bytes; later serial or proof failures retain aggregated
canonical state. Every failure keeps the worker roots. After successful proof
Golem attempts cleanup; cleanup failures warn without invalidating that proof.

Still deferred:

- Semantic verification — P0 gates are mechanical, command-based checks only
  (`kind: command`); there is no model-judged review of a step's output.
- Parallel resume, dynamic waves, retries or reclaim, shared-tree/process/host writers,
  glob/directory ownership, and merge or conflict automation. This is not a
  scheduler or general parallel execution mode.

AgentFlow's `next-action` output is surfaced to the operator on a failed run
for recovery context, but it is advisory only: Golem prints the suggested
state, reason, and command, and never executes it. Proof state stays
entirely adapter-driven, through the same typed calls the driver made on the
way in.
