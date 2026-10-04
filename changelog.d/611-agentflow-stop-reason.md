### Fixed — AgentFlow task steps that stop early fail before their gates (#611)

An AgentFlow task step whose agent run stopped before finishing used to go on
to its gates and `finish-step`. That happened when the run used up
`-max-steps`, hit three consecutive tool errors (including #575 default guard
denials), repeated the same tool call three times, or exhausted a run token
budget, so a step judged only by weak gates could be recorded as completed.
Golem now fails the attempt before any gate, records it as `blocked` in
AgentFlow's ledger with the reason `golem: agent run stopped: <reason>`, and
exits 1 with
`agentflow task failed: step <id> attempt <id>: agent run stopped: <reason>; attempt recorded as blocked`.
Planning mode (`-goal`) names the reason as well:
`the planner did not submit a plan: agent run stopped: <reason>`.

#### Upgrade note

`-agentflow-status` reports a stopped step as `step_unclaimed` (exit 2), and
`-agentflow-resume` runs it again in a new attempt instead of settling the
stopped attempt on its gates. Edits from the stopped run stay in place: restore
or delete them before resuming, or the new attempt must write each still-changed
in-scope file. If it does not, resume fails closed in `file_receipts_missing`
and leaves that attempt open. Block it with `agentflow block-step` before
restoring files, as `docs/llm/agentflow-task-mode.md` describes. Under the
`enforce` lease policy recovery stays manual (exit 3). Golem's AgentFlow
probe now also requires `block-step` with `--root`, `--attempt`, `--reason`,
`--agent` and `--json`, which every supported AgentFlow provides. Other
step-run failures, such as a provider error, still leave the attempt open as
before.
