### Fixed — -agentflow-resume checks for step work before building a proof (#688)

`-agentflow-resume` ran AgentFlow's `finish-run` without the step-work check a
fresh `-plan` run makes since #651. When another writer opened an attempt while
resume ran, `finish-run` still passed (kstruzzieri/agentflow#58) and wrote
`.agent/proof-pack.json` over the unfinished work before resume failed with
`agentflow resume finished in state "<state>", want complete`.

Resume now asks `next-action` before `finish-run`, as a fresh run does. If
AgentFlow still reports step or attempt work at that check, resume exits 1
without running `finish-run` or building a proof, names the AgentFlow state and
step, and sends you to `-agentflow-status`, which reports whether
`-agentflow-resume` can recover. A writer that opens an attempt after the check
can still reach `finish-run`; resume's final check then fails as before. Without
another writer, resume reaches `finish-run` as before, after one more read-only
`next-action` call; if that call fails, resume stops there instead.
