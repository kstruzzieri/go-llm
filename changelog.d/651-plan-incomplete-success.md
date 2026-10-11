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
(the block itself failed, the process died, or an older Golem wrote it) is one
resume cannot settle yet (#652); since #642 Golem otherwise blocks such an
attempt. Runs whose steps all complete are unaffected.
