### Fixed — -agentflow-resume recovers an attempt left open after its gate failed (#652)

An owned AgentFlow attempt whose validation gate had failed but stayed open
(its block failed, the process died, or an older Golem wrote it) could not be
recovered: `-agentflow-status` exited 3 with
`agentflow gate "..." has unknown status "failed"`, and `-agentflow-resume`
refused with the same error. AgentFlow credits an attempt's first matching
gate receipt, so re-running the gate on that attempt can never pass.

`-agentflow-status` now exits 2 with
`resume: block the attempt whose gate failed, then re-run the step serially`.
`-agentflow-resume` blocks the attempt with the fixed reason
`golem: resume found failed command gate`, runs none of its gates, then claims
a new attempt and runs the model for that step again. The failed receipt is
kept. Both still refuse before any change (status exit 3, resume exit 1) under
an `enforce` lease policy, when the root retains a timed-out receipt, when the
attempt has another owner, or when AgentFlow does not allow an automatic,
non-break-glass `continue`. An open review-amendment attempt is still refused.
Exit 2 offers a retry, not a guaranteed proof: a gate refused by
`risk_policy: block` is refused again on the new attempt until the policy
changes.

The `enforce` refusal for `step_unclaimed` now says "under an enforced lease"
instead of "under a finite enforced lease"; it always applied to any
`enforce` policy.

**Upgrade note:** resuming such a workspace now costs a model run for that
step. Edits from the failed attempt stay in the workspace; the new attempt
must write each still-changed in-scope file again.
