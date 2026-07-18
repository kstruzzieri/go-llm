# Agentflow Worktree-Parallel Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add one opt-in, fresh-run, worktree-parallel initial wave to Golem's
Agentflow task mode, followed by the existing serial loop and one canonical
proof.

**Architecture:** Extend the typed Agentflow client with per-worker identity,
resumability, aggregation, and feature probes. Insert one coordinator hook after
`doctor`. It selects only zero-dependency exact-path steps, runs the existing
one-step driver in detached worktrees, promotes verified disjoint files, and
aggregates ledgers. All later work and finalization reuse the current driver.

**Tech stack:** Go standard library, existing `agentflow.Runner`,
`golang.org/x/sync/errgroup`, Git worktrees, Agentflow CLI contracts, Go tests.

## Global constraints

- Work only in `/private/tmp/go-llm-213-parallel-runtime` on
  `codex/213-worktree-parallel-runtime`.
- Use test-first red/green/refactor for runtime changes.
- Default task behavior remains serial.
- Do not add a scheduler, worker subprocess, branch commits/merges, second
  ledger, synthesized or edited `.agent` artifacts, retries, automatic resume,
  or lease-policy mutation. Worktree setup may copy the canonical `.agent/`
  baseline byte-for-byte as opaque state.
- Preserve worktrees on every failure and remove them only after canonical
  proof success.

---

### Task 1: Typed per-worker and resumability contract

**Files:**

- Modify: `agentflow/client.go`
- Modify: `agentflow/client_test.go`
- Modify: `agentflow/probe.go`
- Modify: `agentflow/probe_test.go`
- Modify: `cmd/golem/agentflow_driver.go`
- Modify: `cmd/golem/agentflow_driver_test.go`

- [ ] Write failing client tests for a unique agent on claim, amendment,
  receipt, gate, finish, and `next-action` argv.
- [ ] Write failing projection tests for the contract, agent, attempt, and
  diagnostic fields consumed by fresh-worker validation, while accepting
  additive fields.
- [ ] Write failing probe coverage for `next-action --agent` and the diagnostic
  when a pre-#22 Agentflow build still reports version `0.4.0`.
- [ ] Run `rtk go test ./agentflow -run 'Agent|NextAction|Probe'` and confirm the
  expected red failures.
- [ ] Add the smallest owned-client constructor and replace the fixed actor in
  actor-bearing calls while preserving `NewClient`'s default.
- [ ] Parse only the projection fields this wave consumes and pass `--agent` for
  owned clients without raising ordinary serial task requirements.
- [ ] Add fresh-worker projection validation used only by parallel mode.
- [ ] Re-run the focused tests green.

### Task 2: Typed ledger aggregation

**Files:**

- Modify: `agentflow/client.go`
- Modify: `agentflow/client_test.go`
- Modify: `agentflow/probe.go`
- Modify: `agentflow/probe_test.go`

- [ ] Write a failing table test for clean dry-run, dry-run collision, real
  collision, and successful write, including exact argv and exit codes.
- [ ] Add failing cases for invalid pairings, malformed JSON, exit 2, and
  mismatched status/variant fields.
- [ ] Write failing optional-probe tests for the command and all used flags.
- [ ] Run `rtk go test ./agentflow -run 'Aggregate|ParallelProbe'` and confirm
  failures are caused by missing behavior.
- [ ] Implement `AggregationInput`, stable top-level result fields, a typed
  collision error, and `AggregateLedgers` that parses stdout before handling
  exit 1.
- [ ] Implement `ProbeParallel` without raising non-parallel requirements.
- [ ] Re-run focused tests green.

### Task 3: Flag and driver cohort seam

**Files:**

- Modify: `cmd/golem/main.go`
- Modify: `cmd/golem/main_test.go`
- Modify: `cmd/golem/agentflow_driver.go`
- Modify: `cmd/golem/agentflow_driver_test.go`

- [ ] Write failing flag tests for default `1`, positive values, and
  task-mode-only use of `-plan-workers`.
- [ ] Write failing driver tests proving the parallel feature probe precedes
  mutation, the cohort runs after `doctor`, failure stops the serial loop, and
  success resumes serial work and finalizes exactly once.
- [ ] Run `rtk go test ./cmd/golem -run 'PlanWorkers|ParallelCohort'` and confirm
  red failures.
- [ ] Add the flag and validation.
- [ ] Add one optional cohort hook and optional parallel probe to `driver`; keep
  `runOneStep`, review sequencing, and `FinishRun` unchanged.
- [ ] Re-run focused tests green.

### Task 4: Selection and Git worktree coordinator

**Files:**

- Create: `cmd/golem/agentflow_parallel.go`
- Create: `cmd/golem/agentflow_parallel_test.go`

- [ ] Write failing pure selection tests for worker bound, plan order,
  all-empty dependency-graph fallback, dependency exclusion, literal-path
  validation, case-folded equality/ancestor overlap, `.git`, ignored paths,
  directories/symlinks, and serial fallback.
- [ ] Write failing temp-repository tests for one common base, detached
  worktrees, byte-identical `.agent` copies, concurrent assigned workers, and
  exact-diff validation.
- [ ] Write failing tests proving worker failure and unexpected drift leave the
  canonical source untouched and preserve worktrees.
- [ ] Run `rtk go test ./cmd/golem -run 'Parallel|Worktree|Cohort'` and confirm
  red failures.
- [ ] Implement the plan-order selector, clean-tree checks, safe literal-path
  joining, tracked-or-new-unignored checks, recorded-HEAD/toplevel recheck,
  detached worktree lifecycle, `.agent` copier, and `errgroup` worker dispatch.
- [ ] Keep worker/source IDs deterministic and within Agentflow's identifier
  contracts.
- [ ] Re-run focused tests green and under `-race`.

### Task 5: Canonical promotion, aggregation, and production worker

**Files:**

- Modify: `cmd/golem/agentflow_parallel.go`
- Modify: `cmd/golem/agentflow_parallel_test.go`
- Modify: `cmd/golem/agentflow_driver.go`
- Modify: `cmd/golem/agentflow_driver_test.go`
- Modify: `cmd/golem/repl.go`
- Modify: `cmd/golem/main.go`

- [ ] Write failing tests for file create/modify/delete promotion and rollback,
  dry-run-before-write ordering, collision rollback, ambiguous write failure,
  and no cleanup before proof.
- [ ] Write failing assigned-worker tests proving one fresh owned client and
  orchestrator run exactly one step without init, selection, review,
  aggregation, or finalization.
- [ ] Run focused tests and confirm red failures.
- [ ] Implement snapshot-backed promotion of only validated changed paths.
- [ ] Wire dry-run and real aggregation with the exact base SHA; distinguish
  guaranteed no-write collisions from ambiguous real-write failures.
- [ ] Factor root-specific step-runner construction so each worker owns its
  workspace, journal, and orchestrator; use one interrupt watcher for the wave.
- [ ] Retain the coordinator in `runAgentflowTask`, clean it only after
  `driver.run` returns a proof, and report preserved paths on failure.
- [ ] Re-run focused and race tests green.

### Task 6: Real Agentflow proof and documentation

**Files:**

- Modify: `cmd/golem/agentflow_smoke_test.go`
- Modify: `docs/llm/agentflow-task-mode.md`

- [ ] Add a failing integration test with two disjoint initial steps and one
  dependent serial step. Assert namespaced aggregation provenance, all final
  source bytes, and one verified canonical proof pack.
- [ ] Run with
  `rtk env AGENTFLOW_SRC=/Users/keith.struzzieri/projects/agentflow/github/agentflow go test -tags=agentflow_integration ./agentflow ./cmd/golem -run 'Parallel|Aggregate|Smoke'`
  and confirm the new path fails before the final wiring.
- [ ] Document the opt-in flag, exact-path rules, serial fallback, fresh-run
  ceiling, worktree preservation, canonical-only proof, and deferred features.
- [ ] Re-run the integration test green.

### Task 7: Verification, review, and publish

- [ ] Run focused Agentflow and Golem tests.
- [ ] Run `rtk go test -race ./agentflow ./cmd/golem`.
- [ ] Run the full Agentflow integration-tag suite against current
  `AGENTFLOW_SRC`.
- [ ] Run `rtk go test ./...` and `rtk go vet ./...`.
- [ ] Run `rtk git diff --check`, inspect every changed path, and search for
  placeholders, direct `.agent` writes, worker `FinishRun`, and accidental
  branch/commit commands.
- [ ] Request an independent review against `origin/develop`; fix every
  critical or important finding with a regression test and repeat affected
  verification.
- [ ] Commit intentionally, push `codex/213-worktree-parallel-runtime`, and
  open a ready PR to `develop` with `Closes #213` and exact verification
  evidence.
