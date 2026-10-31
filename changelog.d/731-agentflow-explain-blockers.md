### Changed — Golem explains a stalled step loop and shows recorded blockers (#731)

Golem's serial step loop now asks AgentFlow `next-step --explain` instead of
plain `next-step`.

- When AgentFlow reports remaining step work but no eligible step
  (`open_attempts` or `dependencies_unmet`), the run stops before `finish-run`
  with `agentflow reports no eligible step while step work remains:
  reason_code "<code>" (<AgentFlow's diagnostics>); no proof was built; run
  -agentflow-status to see whether -agentflow-resume can recover it`.
  Previously such a run went on to the `next-action` check and stopped there
  with less detail.
- A same-plan `-review-manifest` round over a run that died with an attempt
  open now stops at that point, before it records the review, opens an
  amendment or calls the model. Previously it recorded the review first.
- A reason code this Golem does not know, which a newer AgentFlow may add,
  stops the run until Golem is updated.

Human `-agentflow-status` now lists each current recorded blocker from
`next-action`, a blocked or failed attempt that no later attempt of the step
has superseded, after the diagnostics:

```text
recorded blocker: P1/A1 blocked at 2026-10-31T15:30:06+00:00 (owner golem): golem: gate execution failed
```

The lines are escaped like the rest of status, the reason is cut at 200
characters, and they are display-only: exit codes, resume dispositions and
`-agentflow-status -json` output are unchanged. Entries Golem cannot read are
counted on one line that points to `-agentflow-status -json`.
