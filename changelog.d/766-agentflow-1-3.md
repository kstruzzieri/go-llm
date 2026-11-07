### Changed — Golem requires AgentFlow 1.3 (#766)

Golem now requires AgentFlow 1.3.0 or a later 1.x. AgentFlow 1.2 and older are
refused before any other AgentFlow call
(`agentflow 1.2 is too old; need >= 1.3`), so upgrade AgentFlow before or with
Golem. This includes fresh `-goal` and `-plan` runs, which still ran on 1.2.1.
CI's AgentFlow compatibility job already pins the AgentFlow v1.3.0 release
commit.

On AgentFlow 1.2.x an edit left by a stopped amendment does not fail the run:
`-agentflow-status` exits 2 with `run_unverified`, and `-agentflow-resume` reaches
`state: complete` with `proof: verified` while the edit stays unverified.
AgentFlow 1.3.0 fails `verify-run` and `finish-run` on it
(kstruzzieri/agentflow#103).

The separate refusal of AgentFlow 1.2.1 for recovery (#756) is gone, because
every 1.2 build is now below the floor. A script that matches
`agentflow 1.2.1 cannot drive recovery` must match the too-old text instead.
`-agentflow-status` now runs `agentflow --version` once.

`agentflow.Client.ProbeRecovery` and the `-review-manifest` run's call to it are
removed. Library callers use `CheckVersion` or `Probe`, which refuse the same
versions.
