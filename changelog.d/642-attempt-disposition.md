### Fixed — a failed step run or gate no longer leaves its AgentFlow attempt open for resume to settle (#642)

Task mode closed an attempt only when its agent run stopped at a loop cap
(#611). Any other failure (a provider or observer error, an exhausted context,
an interrupt, an unreceipted edit, or a validation gate's command error)
returned with the attempt open, and `-agentflow-resume` then settled it by
running its missing gates and `finish-step`, accepting partial work whose gates
happened to pass.

Golem now records each such attempt as `blocked` before exiting 1, with a fixed
reason (`golem: agent run failed`, `golem: agent run cancelled`,
`golem: agent run exhausted its context`,
`golem: unreceipted edit aborted the run`, `golem: agent run stopped: <reason>`
or `golem: gate execution failed`). The error detail stays in the printed
error, not the ledger. Resume's own gate settlement blocks the same way. The
block is attempted on its own 30-second deadline, independent of caller
cancellation, with up to 5 seconds of runner drain time. A failed block still
reports both causes; this does not bound the subsequent recovery report.

A gate that timed out leaves a receipt that AgentFlow 1.0's run verification
rejects for the rest of that workspace's ledger (kstruzzieri/agentflow#60).
Golem now says so: the failed run's error names the receipt,
`-agentflow-status` exits 3 instead of offering a resume, and
`-agentflow-resume` refuses before changing anything. A same-plan
`-review-manifest` round also refuses a retained timeout before startup
mutations. Archive the complete `.agent/` outside the working tree and start
and validate a new run; do not delete individual receipts.

**Upgrade note:** Ctrl-C during a step now closes its attempt, so a later
`-agentflow-resume` runs the model for that step again instead of settling the
interrupted attempt on its gates. Edits from the failed run stay in the
workspace; the new attempt must write each still-changed in-scope file again.
An attempt can still remain open after a failed block, process death, a
`beforeGates` safety refusal, or a `FinishStep` error; `-agentflow-status`
shows the authoritative state.
