### Fixed — Golem verifies existing proofs before recovery (#689)

Status and resume now independently run AgentFlow's read-only `verify-proof`
on an existing proof before offering or starting recovery. Invalid, malformed,
or unverifiable proofs stop status with exit 3 and resume with exit 1 before
any recovery mutation, even when step work or `run_unverified` hides the proof.
An initially valid proof may be replaced after settlement or serial work
makes it stale; settlement now reaches the same terminal path as serial work.
Resume compares the proof's existence and digest after the final `next-action`
check and before `finish-run`, refusing a new, replaced, or removed proof.
An absent proof remains recoverable. Inspect and move a blocking proof aside,
then run status and resume; the remaining state may still require step work.

Fresh `-plan` runs refuse an exposed `proof_failing` state before `finish-run`,
but retain the existing ability to rebuild a proof hidden by `run_unverified`.
Proof checks are not atomic with concurrent changes to other proof inputs or
with `finish-run` (kstruzzieri/agentflow#58). Resume still refuses stale or
failing proofs and drift on entry, and drift exposed by settlement.
