# Agentflow Review Routing Design

## Goal

Implement go-llm #271 without adding a Golem-owned risk classifier. Golem
authors or forwards explicit task signals, asks Agentflow for the authoritative
workflow recommendation, previews the selected route before approval or task
mutation, and materializes the exact returned workflow contract through
Agentflow before execution proof is built.

## Chosen approach

Use a narrow typed adapter around these stable Agentflow commands:

- `recommend-workflow --stdin --json` for read-only recommendation.
- `workflow-contract --from-json` for the only durable contract write.

This is the handoff-prescribed approach. A local classifier was rejected because
it would duplicate Agentflow policy. Accepting only a caller-supplied workflow
contract was also rejected because it would skip the required recommendation
and its explainable signal trace.

## Task brief ownership

`PlanIR` gains only the signals the planner must author: required `task_type`
plus optional `security_sensitive`, `blast_radius`, and `declared_size`.
`declared_risk` remains the existing `risk_level`. Candidate files come from
the compiled step file scopes, and validation needs come from the compiled
validation-gate labels. Golem does not scan the repository, keyword-match prose,
or infer security sensitivity.

The Agentflow-facing brief is schema `0.1.0` and preserves optional-field
absence. Explicit external briefs are decoded as closed JSON. An external plan
without a brief remains supported through a conservative exact-fact projection:
`task_type=feature`, the plan's declared risk, its step files, and its validation
gates. Blast radius, size, and security sensitivity stay absent. This floors an
underspecified legacy plan at `medium-feature` instead of inventing a lightweight
route. An explicit brief whose risk conflicts with the plan is rejected.

## Recommendation adapter

The adapter sends the brief on stdin through the existing argv-based `Runner`.
It validates a narrow projection containing:

- recommendation and selection pack/profile;
- rationale, signals, alternatives, and optional override;
- the complete workflow-contract candidate.

It fails closed on missing fields, schema versions other than `0.1.0`, unknown
profiles or enums, malformed nested policy data, candidate/selection mismatch,
or an unrecorded selection change. The original candidate JSON is retained for
materialization so Golem does not reconstruct or forge the contract.

An explicit profile selection is accepted only with a non-empty operator reason.
Both are forwarded to Agentflow and shown in the preview. Agentflow remains the
authority on whether that selection is an override.

## Sequencing and mutation boundary

Planning mode performs the base and workflow feature probes before model work.
For a locally valid `submit_plan`, its tool-planning phase calls the read-only
recommendation command and appends the route to the existing plan preview. The
preview includes the selected and recommended profiles, rationale and signals,
review depth, required review-run bit, required capabilities, required gates,
alternatives, and override reason. Approval still gates all mutation.

After approval, Golem initializes and locks the plan, then stages the retained
candidate in a mode-0600 temporary file and invokes
`workflow-contract --from-json`. The temporary file is removed. Golem never
writes `.agent/workflow.contract.json` directly.

Task mode recommends and prints the route before `init`, plan locking, or any
other `.agent` write. After plan lock it materializes the same candidate exactly
once, then initializes execution. The selected route is printed again before
review-manifest ingestion. `finish-run` remains the proof authority for required
review depth, review-run adequacy, capabilities, and gates.

## Compatibility and ownership

Non-Agentflow modes do not parse, probe, recommend, preview, or materialize any
workflow route. Existing Agentflow external plans remain runnable through the
conservative projection described above. Existing #212 review-manifest ingestion
and finding-linked amendments are unchanged; they consume the selected route
only through Agentflow proof policy.

Changes stay inside `agentflow/**`, `cmd/golem/agentflow_*`, narrowly necessary
Agentflow flags/tests in `cmd/golem/main*`, and Agentflow task-mode documentation.
No `mcp/**` or RAG/chat wiring is touched.

## Verification

Unit tests use fake `Runner` responses to pin argv, stdin, typed validation,
read-only ordering, previews, override rules, exact-once materialization, legacy
brief derivation, and #212 sequencing. Contract tests run the real current
Agentflow checkout for all five representative routes, read-only recommendation,
contract materialization, and proof failure when a deep route lacks an adequate
review run. Focused, race, full, integration-tagged, and vet commands from the
handoff are required before publishing.
