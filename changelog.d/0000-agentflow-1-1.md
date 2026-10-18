### Changed — Golem requires AgentFlow 1.1 and resumes timed-out gates (#0000)

Golem now requires AgentFlow 1.1.0 or a later 1.x. AgentFlow 1.0 is refused
before any other AgentFlow call (`agentflow 1.0 is too old; need >= 1.1`), so
upgrade AgentFlow before or with Golem. CI's AgentFlow compatibility job pins
the AgentFlow v1.1.0 release commit.

AgentFlow 1.1 can recover a gate that timed out (kstruzzieri/agentflow#60,
#70), and Golem now follows it:

- A gate that times out still blocks its attempt, but the error no longer
  appends `retained timeout receipt ... prevents this root's ledger from
  passing AgentFlow 1.0 run verification ...`. `-agentflow-status` offers the
  step for resume (exit 2), and `-agentflow-resume` runs it again in a new
  attempt. Once that attempt completes, AgentFlow verifies the run and reports
  the old receipt as a `superseded_timeout` warning.
- `-agentflow-status` and `-agentflow-resume` handle AgentFlow 1.1's new
  `next-action` states. `attempt_timed_out` (an open attempt holding a
  timed-out receipt, including a `verified` one or one whose advisory lease
  expired) exits 2 with
  `resume: block the timed-out attempt, then re-run the step serially`; resume
  blocks it with the fixed reason `golem: resume found timed-out gate`, then
  continues from the state AgentFlow reports. `step_amendment_needed` (a
  completed step that needs a replacement attempt) exits 2 with
  `resume: amend the completed step, then continue serially`; resume runs
  `amend-step` with the reason `golem: resume replaces the step's attempt` and
  runs the model on the step's own goal. It amends one step per run: when
  another step also needs a replacement, resume stops before `finish-run`
  without building a proof, and the next resume amends that step. Both states
  exit 3, and resume refuses them before any change, under an `enforce` lease
  policy, as it already does for `step_unclaimed`, and when the step has a
  review amendment that never completed, since a replacement would drop its
  findings
  (`step <id> has an unfinished review amendment; resume would replace it
  with the step's own goal and drop the review findings, ...`). Golem never
  runs `reclaim-step`.
- The refusal for a retained timed-out receipt (status exit 3, resume, and a
  same-plan `-review-manifest` round) now applies only to a receipt no later
  attempt can supersede: one inside an attempt that completed, which only
  AgentFlow 1.0 recorded, or one whose attempt the step-run ledger does not
  record under the receipt's step or records inconsistently (under two steps,
  or closed twice). Its text is now
  `retained timeout receipt <id> in <root> cannot be superseded by a later
  attempt, so this root's ledger cannot pass AgentFlow run verification; ...`,
  with the same archive remedy.
- Golem no longer reads `.agent/step-runs.jsonl` before it blocks an attempt:
  AgentFlow 1.1 refuses to block an attempt that is already closed and writes
  nothing (kstruzzieri/agentflow#61). The endings
  `attempt already closed (<event>); not blocked` and
  `check attempt state before blocking: ...` are gone; such a refusal ends
  `record blocked attempt: <agentflow error>`, and `-agentflow-status` no
  longer reads that ledger to decide the failed-gate disposition.

**Upgrade note:** a workspace an earlier Golem left with a blocked timed-out
attempt, which `-agentflow-status` reported as exit 3, is now resumable and
costs a model run for that step. Fix the gate or its timeout first: the retry
runs the gate again with the same timeout.
