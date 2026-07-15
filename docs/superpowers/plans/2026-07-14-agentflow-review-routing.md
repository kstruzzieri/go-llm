# Agentflow Review Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route every Agentflow-backed Golem task through Agentflow's authoritative workflow recommendation and proof policy.

**Architecture:** Add one typed recommendation/materialization seam to the existing Agentflow client. Planning mode derives a brief from PlanIR, previews the returned contract before approval, materializes it after approval, and saves a digest-bound handoff. Task mode either validates and reuses that approved handoff or recommends and materializes once for an external plan. Agentflow remains the review/proof-policy authority.

**Tech Stack:** Go standard library, existing `agentflow.Runner`, Agentflow CLI contracts, Go tests.

## Global Constraints

- Work only in `/private/tmp/go-llm-track-a-271` on `codex/271-agentflow-review-routing`.
- Do not implement a local classifier, repository scan, prose heuristic, review agent, scanner, manifest fabrication, or policy framework.
- Missing optional task signals remain unknown; only exact authored plan facts may fill them.
- Recommendation must be read-only and precede every plan/contract mutation.
- Only Agentflow may write `.agent/workflow.contract.json`.
- Preserve #212 review ingestion and all non-Agentflow defaults.
- Do not touch Track B's `mcp/**` or RAG/chat files.

---

### Task 1: Typed recommendation and materialization client

**Files:**
- Create: `agentflow/workflow.go`
- Create: `agentflow/workflow_test.go`
- Modify: `agentflow/client.go`
- Modify: `agentflow/probe.go`
- Modify: `agentflow/probe_test.go`

**Interfaces:**
- Produces: `TaskBrief`, `WorkflowRecommendation`, `(*Client).ProbeWorkflow`, `(*Client).RecommendWorkflow`, `(*Client).MaterializeWorkflowContract`.
- `WorkflowRecommendation.CandidateJSON()` returns a defensive copy of the exact validated candidate.

- [ ] Write failing tests for brief JSON preservation, exact argv/stdin, all required projection fields, schema/profile/enum validation, selection consistency, override consistency, and secure exact-candidate staging.
- [ ] Run `rtk go test ./agentflow -run 'Workflow|Recommend'` and confirm failures are caused by the missing interfaces.
- [ ] Implement the minimum typed structs and validation; keep Agentflow classification out of Go.
- [ ] Add the workflow feature probe for both commands and their stable flags with an upgrade diagnostic.
- [ ] Implement materialization with `os.CreateTemp`, close-before-call, deferred removal, and `workflow-contract --root <root> --from-json <temp>`.
- [ ] Re-run focused tests and keep the package green.

### Task 2: Real Agentflow contract coverage

**Files:**
- Create: `agentflow/workflow_contract_test.go`

**Interfaces:**
- Consumes: Task 1's client methods and `agentflowRunnerForTest`.
- Produces: integration-tag-compatible tests against `AGENTFLOW_SRC`.

- [ ] Write tests for docs-only, bounded low-risk bugfix, medium feature, broad feature, and high/security briefs using the real CLI.
- [ ] Prove recommendation creates no `.agent` directory.
- [ ] Prove the materialized JSON equals the candidate and a deep route without adequate review evidence fails proof with an actionable diagnostic.
- [ ] Run `rtk env AGENTFLOW_SRC=/Users/keith.struzzieri/projects/agentflow/github/agentflow go test -tags=agentflow_integration ./agentflow -run 'Workflow|Recommend|Proof'` and confirm the new tests fail before implementation, then pass after Task 1.

### Task 3: PlanIR task signals and authoring preview

**Files:**
- Modify: `agentflow/compiler.go`
- Modify: `agentflow/compiler_test.go`
- Modify: `cmd/golem/agentflow_author.go`
- Modify: `cmd/golem/agentflow_author_test.go`

**Interfaces:**
- Produces: `agentflow.TaskBriefFromPlan(plan, taskType)` for exact step-file and validation-gate facts.
- Consumes: Task 1's recommendation and materialization methods.

- [ ] Write failing schema/brief tests for required task type and optional security, blast-radius, and size signals.
- [ ] Write failing author tests proving recommendation occurs during tool planning, preview contains every required route field, and no init/lock/materialization occurs before approval.
- [ ] Write failing tests for mismatched or stale plan/recommendation arguments and exact-once post-lock materialization.
- [ ] Implement PlanIR fields and prompt/schema text without adding a second planning document.
- [ ] Append a control-safe workflow section to the existing deterministic plan preview and include it in resubmission deltas.
- [ ] Store the recommendation bound to the previewed tool arguments; after approval lock the plan, materialize only that candidate, and save a handoff bound to canonical plan/task-brief digests.
- [ ] Run `rtk go test ./agentflow ./cmd/golem -run 'Agentflow|Workflow|Recommend|Plan'`.

### Task 4: Flags and conservative external-plan brief

**Files:**
- Modify: `cmd/golem/main.go`
- Modify: `cmd/golem/main_test.go`
- Modify: `cmd/golem/agentflow_driver.go`
- Modify: `cmd/golem/agentflow_driver_test.go`

**Interfaces:**
- Produces flags `-task-brief`, `-workflow-handoff`, `-workflow-profile`, and `-workflow-reason` limited to Agentflow modes.
- Produces a strict external brief reader and a conservative `task_type=feature` fallback using plan risk/files/gates only.

- [ ] Write failing flag tests: brief is task-mode-only; profile and reason must be paired and limited to Agentflow modes.
- [ ] Write failing brief tests for closed JSON, plan-risk mismatch, exact plan-file/gate fill-in, and conservative fallback that cannot select a lightweight profile from absent signals.
- [ ] Implement only the four narrow flags and their mode/pairing validations.
- [ ] Resolve the brief path against caller cwd; decode before any Agentflow call.
- [ ] Preserve explicit signals, always union exact plan files/gates so an explicit brief cannot hide scope, and reject risk mismatch.
- [ ] Run `rtk go test ./cmd/golem -run 'Agentflow|Workflow|Recommend|Plan'`.

### Task 5: Task startup, review, and proof sequencing

**Files:**
- Modify: `cmd/golem/agentflow_driver.go`
- Modify: `cmd/golem/agentflow_driver_test.go`
- Modify: `cmd/golem/agentflow_smoke_test.go`

**Interfaces:**
- External-plan driver order: probe -> workflow probe -> recommend -> preview -> init -> evidence -> lock -> materialize -> init execution -> steps -> route reminder -> review ingestion -> finish run.
- Planning handoff order: validate plan/brief digests and existing contract before client construction -> probe -> workflow probe -> preview saved route -> init -> evidence -> lock -> init execution -> steps -> route reminder -> review ingestion -> finish run. It does not recommend or materialize again.

- [ ] Write failing sequence tests proving recommendation is read-only/pre-mutation, materialization is exact-once and post-lock, and the same candidate reaches proof.
- [ ] Write failing output tests for startup and pre-review route fields.
- [ ] Write failing tests that a silent selection change is rejected, #212 findings still become amendments, and missing deep-review evidence surfaces Agentflow's proof requirement.
- [ ] Implement the minimum driver fields/order and reuse the authoring route renderer.
- [ ] Re-run focused driver, review, and smoke tests.

### Task 6: Documentation and regression fence

**Files:**
- Modify: `docs/llm/agentflow-task-mode.md`

**Interfaces:**
- Documents authored signals, brief/handoff/override flags, conservative external-plan behavior, preview/approval timing, digest binding, materialization ownership, and Agentflow proof semantics.

- [ ] Update only Agentflow task-mode documentation.
- [ ] Search the diff for local-classifier language, Track B paths, placeholders, and direct `.agent/workflow.contract.json` writes.
- [ ] Run `rtk gofmt -w` on changed Go files and re-run focused tests.

### Task 7: Verification, review, and publish

**Files:**
- Review every changed file; no new production files beyond those above unless a failing test proves the need.

- [ ] Run `rtk go test ./agentflow ./cmd/golem -run 'Agentflow|Workflow|Recommend|Review|Plan|Proof'`.
- [ ] Run `rtk go test -race ./agentflow ./cmd/golem`.
- [ ] Run `rtk env AGENTFLOW_SRC=/Users/keith.struzzieri/projects/agentflow/github/agentflow go test -tags=agentflow_integration ./agentflow ./cmd/golem`.
- [ ] Run `rtk go test ./...`.
- [ ] Run `rtk go vet ./...`.
- [ ] Request an independent code review against `origin/develop`, fix every critical/important finding with a red-green regression test, and repeat affected verification.
- [ ] Inspect `git diff --check`, ownership-fence paths, and final status.
- [ ] Commit intentionally, push `codex/271-agentflow-review-routing`, and open a draft PR to `develop` with `Closes #271` and verification evidence.
