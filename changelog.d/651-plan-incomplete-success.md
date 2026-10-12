### Fixed — a fresh -plan run no longer reports success while step work remains (#651)

A fresh `golem -plan` over a workspace that already held an open AgentFlow
attempt (for example one an older Golem left open after its gate failed)
skipped the open step, ran `finish-run`, exited 0 and printed `proof pack:`,
although the proof recorded `steps_completed` below `steps_total`. AgentFlow's
`next-step` returns nothing both when every step is done and when the
remaining steps hold open attempts, and a non-strict `finish-run` does not
require completed steps (kstruzzieri/agentflow#58).

Before `finish-run`, task mode now asks AgentFlow's `next-action` and continues
only when every step has completed and no attempt is open. Otherwise it exits 1
without running `finish-run` or building a proof, names the AgentFlow state and
step, and sends you to `-agentflow-status`, which reports whether
`-agentflow-resume` can recover. An attempt left open after its gate failed
(the block itself failed, the step-run ledger could not be read, the process
died, or an older Golem wrote it) is blocked by `-agentflow-resume`, which then
re-runs the step (#652); since #642 Golem otherwise blocks such an attempt.
Since #643 a fresh `-plan` refuses such a workspace before any AgentFlow call;
this check still covers a same-plan `-review-manifest` round over an unfinished
run and an attempt another writer opens during the run. Runs whose steps all
complete are unaffected.
