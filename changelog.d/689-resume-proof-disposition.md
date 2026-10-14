### Fixed — Golem replaces only a proof its own work made stale (#689)

AgentFlow's proof hashes the `.agent` ledgers, so an older proof (for example
one a non-strict `finish-run` built over unfinished steps, the #651 bug) goes
`proof_stale` as soon as `-agentflow-resume` records anything. When resume's
serial loop completed the last step, `finish-run` replaced that proof; when
resume's settlement of an open attempt completed it instead, resume exited 1
with `agentflow resume blocked after settlement in state "proof_stale"`, and
the run had no Golem recovery path. Settlement now continues to the same
`finish-run`. A proof AgentFlow reports as `proof_failing` (malformed, or
built by a newer AgentFlow) is no longer overwritten: resume exits 1 after a
settlement exposes it, and resume's tail and fresh `-plan` runs exit 1 before
`finish-run`. AgentFlow checks run verification first, so a run with no
recorded run verification reports `run_unverified` instead, and `finish-run`
still rebuilds any proof there. Resume still refuses `proof_stale`,
`proof_failing` and `drift_failing` on entry, and drift a settlement exposes.
