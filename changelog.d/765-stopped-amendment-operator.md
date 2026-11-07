### Fixed — -agentflow-status and -agentflow-resume hand a stopped amendment's edit to an operator (#765)

AgentFlow 1.3.0 will not finish a run while an edit left by a stopped attempt is unverified, and it leaves the next step to a person: `next-action` reports `step_amendment_needed` with `amend` marked `automatic: false`. Golem reported this as a malformed projection, then refused the amendment the operator opened, so the operator had to finish the work by hand.

- `-agentflow-status` prefixed every projection check failure with `resume: blocked by unsafe or malformed projection:`, including refusals of a well-formed projection. The line now reads `resume: blocked:`, and each reason speaks for itself. Exit codes and JSON output are unchanged. A script that matches the old text must change.
- When AgentFlow allows `amend` but marks it `automatic: false`, status and resume say an operator must decide, and what to do: run the advised `amend-step`, record the edit in the new attempt, then resume. The steps are the same whether the operator keeps the edit or restores the file first.
- `-agentflow-resume` now settles an amendment an operator opened and recorded an edit in. It still refuses, before any change, an amendment with no file receipt of its own, one Golem opened itself, one with an unfinished review amendment on the step or linked to a review itself, one with a failed command gate, and one owned by another agent.

Golem's compatibility job now verifies AgentFlow 1.3.0. #766, in this same release, raises the floor to 1.3, so v0.8.0 itself requires AgentFlow 1.3 or a later 1.x.
