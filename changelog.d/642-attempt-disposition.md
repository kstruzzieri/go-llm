### Fixed — a failed step run or gate now blocks its AgentFlow attempt (#642)

Task mode closed an attempt only when its agent run stopped at a loop cap
(#611). Any other failure returned with the attempt open. After a provider or
observer error, an exhausted context, an interrupt, or a gate whose `run` call
left no receipt, `-agentflow-resume` then settled the attempt by running its
missing gates and `finish-step`, accepting partial work whose gates happened to
pass. After a gate that ran and failed (or that the risk policy refused, which
also leaves a receipt), or an unreceipted edit, `-agentflow-status` exited 3
and `-agentflow-resume` refused (exit 1), so recovery was manual.

Golem now attempts to record each such attempt as `blocked` before exiting 1,
with a fixed reason (`golem: agent run failed`, `golem: agent run cancelled`,
`golem: agent run exhausted its context`,
`golem: unreceipted edit aborted the run`, `golem: agent run stopped: <reason>`
or `golem: gate execution failed`). The error detail stays in the printed
error, not the ledger. Resume's own gate settlement blocks the same way. The
block is attempted on its own 30-second deadline, independent of caller
cancellation, with up to 5 seconds of runner drain time; the deadline bounds
only the block call, not the recovery report that follows. A failed block
still reports both causes. When the step-run ledger cannot be read, Golem does
not block blind: the attempt stays open and the error ends
`attempt not blocked; run -agentflow-status`. A command-receipt ledger read
error after a failed gate ends with `run -agentflow-status` too.

A gate that timed out leaves a receipt that AgentFlow 1.0's run verification
rejects for the rest of that workspace's ledger (kstruzzieri/agentflow#60).
Golem now says so: the failed gate's error names the receipt,
`-agentflow-status` exits 3 instead of offering a resume, and
`-agentflow-resume` refuses before changing anything. A same-plan
`-review-manifest` round also refuses a retained timeout before startup
mutations. Archive the complete `.agent/` outside the working tree and start
and validate a new run; do not delete individual receipts, and do not use
`agentflow init-execution --force`. Restore or commit the earlier run's source
changes first, because run verification needs a receipt in the new ledger for
every changed file. If the run came from a planning handoff, re-plan with
`-goal` or run `-plan` without `-workflow-handoff`: the handoff check reads
`.agent/workflow.contract.json`, which went with the archive. The error names
the receipt by its id when that is an AgentFlow-style id (ASCII letters,
digits, `.`, `_`, `-`; 1 to 128 characters), else by its ledger line.

**Upgrade note:** once the block is recorded for a failed step attempt,
`-agentflow-status` reports `step_unclaimed` (exit 2 unless the root retains a
timed-out command receipt; a gate that ran and failed or an unreceipted edit
was exit 3 before), and `-agentflow-resume` runs the model again in a new
attempt instead of settling the old one on its gates. A failed review
amendment is blocked too, but its step stays completed: resume does not retry
it and its findings stay unresolved. A script that resumes on exit 2 now pays
for a model run after a provider or observer error, an exhausted context, an
interrupt, or a failed gate; before, resume settled each of these on its gates
without the model, except a gate whose `run` call left a receipt. Under the
`enforce` lease policy recovery stays manual (exit 3). Edits from the failed
run stay in the workspace: restore or delete them before resuming, or the new
attempt must write each still-changed in-scope file again. Attempts an older
Golem left open are not closed after the fact, except that since #652 resume
blocks one whose command gate failed and re-runs the step; resume may still
settle any other eligible one on its gates. Block one with
`agentflow block-step` to abandon it.
An attempt can still remain open after a failed block, a step-run ledger Golem
could not read, process death, a resume safety refusal before its gates, or a
`finish-step` error. Golem does not block an attempt the ledger already shows
closed (for example by an operator's `agentflow fail-step`).
`-agentflow-status` shows the authoritative state.
