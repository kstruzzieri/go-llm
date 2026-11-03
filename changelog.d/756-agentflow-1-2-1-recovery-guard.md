### Fixed — Golem refuses AgentFlow 1.2.1 for recovery (#756)

AgentFlow 1.2.1 counts a changed file's latest matching receipt from any
attempt, so a stopped attempt's edit satisfies the attempt that replaces it.
On 1.2.1, `-agentflow-resume` would run the gates and complete the step on an
edit nobody accepted instead of failing in `file_receipts_missing`.

- `-agentflow-resume` refuses AgentFlow 1.2.1 (every `1.2.1` build, such as
  `1.2.1rc1` or `1.2.1+local`) after its read-only checks and before its first
  change to the run, and asks AgentFlow for no recovery advice.
- `-agentflow-status` reports every state that would otherwise exit `2` as
  blocked and exits `3`. JSON mode keeps AgentFlow's bytes on stdout and prints
  the refusal as one `golem:` line on stderr.
- A `-review-manifest` run refuses AgentFlow 1.2.1 before `init`: a review
  round over the locked plan retries retained attempts.
- A complete run still reports its verified proof, and fresh `-plan` and
  `-goal` runs without a review manifest still run on 1.2.1. AgentFlow 1.2.0
  is unaffected. An AgentFlow 1.2 whose patch version cannot be read is
  refused for recovery the same way.
