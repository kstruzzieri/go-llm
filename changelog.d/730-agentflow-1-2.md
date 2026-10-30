### Changed — Golem requires AgentFlow 1.2 (#730)

Golem now requires AgentFlow 1.2.0 or a later 1.x. AgentFlow 1.1 and older
are refused before any other AgentFlow call
(`agentflow 1.1 is too old; need >= 1.2`), so upgrade AgentFlow before or
with Golem. CI's AgentFlow compatibility job pins the AgentFlow v1.2.0
release commit.

In AgentFlow 1.2, `next-action` answers only from the ledger rows it
validated. When the step, command or file receipt ledgers change while it
runs, it reports `state_invalid` ("execution ledgers changed during
next-action; run it again") instead of an answer steered by an unvalidated
row. Golem already refuses `state_invalid`: `-agentflow-status` exits 3 with
`resume: blocked by invalid Agentflow state`, and `-agentflow-resume` stops
before any change. Both now act only on the validated projection; when you
see that refusal while another process is writing `.agent/`, run the command
again. Golem does not use 1.2's `next-step --explain` or the
`recorded_blockers` key of `next-action --json` yet; it ignores the new key.
