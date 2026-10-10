### Changed — a fresh -plan or -goal refuses a workspace with AgentFlow execution history (#643)

Re-running `golem -plan` in place over a started or finished AgentFlow run used
to relock the plan silently: AgentFlow's `lock-plan` overwrites
`.agent/plan.lock.json`, while `init` and `init-execution` keep the existing
execution contract and ledgers, which then described a plan they were not
recorded against. `-goal` could do the same when the plan lock was absent or
reset.

Both now refuse before any AgentFlow call when `.agent/` holds
`execution.contract.json` or rows in `step-runs.jsonl`,
`command-receipts.jsonl`, `file-receipts.jsonl` or `verification-runs.jsonl`.
They exit 1, name the first artifact found, and point at `-agentflow-status`
and `-agentflow-resume`, or at moving `.agent/` aside.

**Upgrade note:** a script that re-runs `-plan` in the same workspace,
including after a finished run, must now resume the run or start from a
workspace without `.agent/` execution state. The `-goal` handoff (a locked plan
with no execution contract), a missing `.agent/`, the pristine `agentflow init`
scaffold, and `-agentflow-resume` are unaffected.
