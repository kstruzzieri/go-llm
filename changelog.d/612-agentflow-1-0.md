### Changed — Golem requires AgentFlow 1.x (#612)

Golem now drives AgentFlow 1.x only. Planning mode compiles plan schema
`1.0.0`, and the real-CLI suite and the CI compatibility job run against the
AgentFlow 1.0.0 release.

#### Upgrade note

- **Version:** `-goal`, `-plan`, `-agentflow-resume` and `-agentflow-status`
  run `agentflow --version` first and refuse anything outside 1.x, before any
  mutation. Messages: `agentflow 0.4 is too old; need >= 1.0`, and
  `agentflow 2.0 is newer than this Golem supports; need 1.x`. A refused
  AgentFlow is not asked for recovery advice. `-agentflow-status -json` prints
  the version message as one `golem:` line on stderr, with empty stdout and
  exit 3.
- **Plans:** a plan whose `schema_version` major is not 1 (including Golem's
  previous `0.3.0` and external `0.4.0` plans) is refused before any AgentFlow
  call. Migrate it to `1.0.0` and review it again, or re-plan with `-goal`.
- **Existing 0.x state:** `-plan`, `-agentflow-resume` and `-goal` refuse a
  workspace whose `.agent/` plan lock, execution contract or any execution
  ledger row is not AgentFlow 1.x, naming the file. Finish or `build-proof` the
  run with AgentFlow 0.x, move `.agent/` aside, then re-plan.
  `-agentflow-status` stays read-only and exits 3; with a 0.x plan lock,
  AgentFlow reports `state_invalid`. See "Upgrading from AgentFlow 0.x" in
  `docs/llm/agentflow-task-mode.md`.
- `-agentflow-status` now makes two read-only AgentFlow calls:
  `--version`, then `next-action`.
- Developers running go-llm's own tests with an AgentFlow 0.x binary on PATH
  will see the untagged real-CLI tests in `./agentflow` (lock-plan and review)
  fail: install AgentFlow 1.x, or set `AGENTFLOW_SRC` to a 1.x checkout.
