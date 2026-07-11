# AgentFlow planning and task modes

Golem planning mode (`-goal`) authors a traceable AgentFlow plan, previews it,
and locks it only after explicit human approval, then stops.
Task mode (`-plan`) separately runs that locked plan step-by-step in a
proof-carrying, single-writer P0 loop. AgentFlow owns durable proof state on
disk; Golem owns the model loop and the in-process guards that keep the model
inside what the plan allows. The two are separate, cooperating processes:
AgentFlow never runs a model, and Golem never writes proof state directly.

## Ownership

| AgentFlow owns | Golem owns |
|---|---|
| Plan lock and validation (`lock-plan`) | Reading the plan document |
| Per-step claim and attempt tracking (`claim-step`) | Driving the state machine (the `driver`) |
| File-change receipts (`record-file-change`) | Running the model per step |
| Command-gate execution and receipts (`run`) | The pre-write scope guard mirroring the step's effective scope |
| Drift detection (`finish-run` audit) | The fatal-on-unreceipted-edit journal |
| The proof pack — durable, model-opaque `.agent/` state | Recovery reporting |

## Planning mode

Run `golem -goal "<text>" -root <workspace>` to have the local model inspect the
workspace with read-only file tools and submit one typed aggregate containing
the objective, non-goals, invariants, requirements, acceptance criteria, steps,
and criterion mappings. Golem compiles that aggregate directly into AgentFlow's
optional `requirements[].acceptance_criteria`, `steps[].criterion_ids`, and
`gates[].criterion_ids` fields; there is no second spec file or proof ledger.

Before any AgentFlow state is initialized or locked, Golem rejects duplicate or
malformed stable IDs, dangling mappings, criteria without an implementing step,
proving gates mapped outside their parent step, criteria without a proving gate
or review floor, and the existing dependency, path, scope, and argv errors.
Criterion IDs share one plan-wide namespace: reusing an id across requirements
is a duplicate. Golem then renders a deterministic, control-safe preview with
scope, risk, file boundaries, rollback, the compiled schema version and drift
budget, the requirement-to-step-to-gate mapping, and exact validation argv, and
asks `Lock this plan? [y/N]`. When a rejected lock leads to a repaired
resubmission, the second preview appends a line-level delta against the previous
one. A denial, EOF, or interruption before any approved submission leaves
AgentFlow proof state unchanged; a denial also saves the compiled plan to a temp
file named in the error so the authoring work can be inspected or adapted.
Approval initializes AgentFlow and attempts to lock `.agent/plan.lock.json`;
Golem then prints the separate `-plan` command. For scripted or CI use,
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

`-plan` is mutually exclusive with `-p` (one-shot mode), `-allow-write` /
`-allow-exec`, `-rag-db`, `-delegate`, and `-mcp-stdio` / `-mcp-http`. Task
mode builds its toolset from the locked plan alone, so it refuses to start
if any of those are also passed — it is a constrained proof surface, not a
general-purpose agent session with a plan bolted on.

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

P0 executes a locked plan with a single writer driving one step at a time.
Deferred to later phases:

- Semantic verification — P0 gates are mechanical, command-based checks only
  (`kind: command`); there is no model-judged review of a step's output.
- Resuming an interrupted run.
- The review phase.
- Parallel or multi-writer steps.

AgentFlow's `next-action` output is surfaced to the operator on a failed run
for recovery context, but it is advisory only: Golem prints the suggested
state, reason, and command, and never executes it. Proof state stays
entirely adapter-driven, through the same typed calls the driver made on the
way in.
