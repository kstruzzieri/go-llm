### Fixed — a failed step run or gate now closes its AgentFlow attempt (#642)

Task mode closed an attempt only when its agent run stopped at a loop cap
(#611). Any other failure returned with the attempt open. After a provider or
observer error, an exhausted context, an interrupt, or a gate command that
never ran, `-agentflow-resume` then settled the attempt by running its missing
gates and `finish-step`, accepting partial work whose gates happened to pass.
After a gate that ran and failed, or an unreceipted edit, status and resume
refused the open attempt (exit 3), so recovery was manual.

Golem now attempts to record each such attempt as `blocked` before exiting 1,
with a fixed reason (`golem: agent run failed`, `golem: agent run cancelled`,
`golem: agent run exhausted its context`,
`golem: unreceipted edit aborted the run`, `golem: agent run stopped: <reason>`
or `golem: gate execution failed`). The error detail stays in the printed
error, not the ledger. Resume's own gate settlement blocks the same way. The
block is attempted on its own 30-second deadline, independent of caller
cancellation, with up to 5 seconds of runner drain time; the deadline bounds
only the block call, not the recovery report that follows. A failed block
still reports both causes.

A gate that timed out leaves a receipt that AgentFlow 1.0's run verification
rejects for the rest of that workspace's ledger (kstruzzieri/agentflow#60).
Golem now says so: the failed gate's error names the receipt,
`-agentflow-status` exits 3 instead of offering a resume, and
`-agentflow-resume` refuses before changing anything. A same-plan
`-review-manifest` round also refuses a retained timeout before startup
mutations. Archive the complete `.agent/` outside the working tree and start
and validate a new run; do not delete individual receipts, and do not use
`agentflow init-execution --force`.

**Upgrade note:** once the block is recorded, `-agentflow-status` reports
`step_unclaimed` after any failed run or gate other than a timeout (exit 2,
where a gate that ran and failed or an unreceipted edit was exit 3 before), and
`-agentflow-resume` runs the model again in a new attempt instead of settling
the old one on its gates. A script that resumes on exit 2 now pays for a model
run after an interrupt or a failed gate. Under the `enforce` lease policy
recovery stays manual (exit 3). Edits
from the failed run stay in the workspace: restore or delete them before
resuming, or the new attempt must write each still-changed in-scope file
again. Attempts an older Golem left open are not closed after the fact; block
one with `agentflow block-step` to abandon it. An attempt can still remain open
after a failed block, process death, a resume safety refusal before its gates,
or a `finish-step` error. Golem does not block an attempt the ledger already
shows closed (for example by an operator's `agentflow fail-step`).
`-agentflow-status` shows the authoritative state.
