# AgentFlow planning and task modes

Golem planning mode (`-goal`) authors and locks an AgentFlow plan, then stops.
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
workspace with read-only file tools, submit a structured plan, and lock it as
`.agent/plan.lock.json`. Golem prints the separate `-plan` command after the
lock succeeds; `-goal` never executes the plan or edits source files. It refuses
to replace a locked plan, a non-empty draft, or an unrecognized plan file.

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
