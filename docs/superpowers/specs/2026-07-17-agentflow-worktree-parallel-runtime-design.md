# Agentflow Worktree-Parallel Runtime Design

## Goal

Complete go-llm #213 with the smallest useful runtime slice: Golem may execute
one initial cohort of independent Agentflow plan steps in isolated Git
worktrees, combine their proof ledgers into the invocation root, and then
return to the existing serial driver and canonical `finish-run`.

The mode is opt-in. Existing task mode remains serial by default.

## Verified contract baseline

This design targets:

- go-llm `origin/develop` at `2110132c0188b32cce1e8c7ba40ba1076e953a93`
  (merged PR #310);
- Agentflow `origin/main` at
  `49c9a53aa90c9b31c7ca3b8ddde999e084aa502e`;
- Agentflow PR #22 / issue #20, which added the authoritative `next-action
  --agent --json` resumability projection; and
- Agentflow PR #23 / issue #21, which aligned the public
  `aggregate-ledgers --json` variants with runtime.

Agentflow still reports version `0.4.0`, including builds from before those
changes. Golem therefore feature-probes the exact commands and flags it uses;
the version string is not sufficient evidence.

The resumability projection is authoritative for one worktree. It intentionally
reports one selected step and treats multiple open attempts in the same tree as
invalid. It is not a parallel ready-set API. This slice does not ask it to be
one: every worker begins from the same fresh, locked state in a separate tree,
and the coordinator dispatches only initial steps with no dependency edges.

## Approaches considered

### Independent Golem subprocesses

Rejected. The current task command owns initialization, step selection,
reviews, and finalization. Starting it in each worktree would create several
whole-run drivers rather than assigned-step workers.

### Worker branches followed by Git merges

Rejected for the first slice. Committing worker changes needs an author
identity and changes the user's history; merging or cherry-picking also changes
the invocation root's index or merge state. Existing task mode leaves source
changes uncommitted and unstaged. Parallel mode should preserve that contract.

### One in-process bounded wave

Chosen. The coordinator creates detached worktrees from one recorded commit,
runs the existing one-step driver with a unique Agentflow owner in each, copies
only verified exact-path results into the clean invocation root, and asks
Agentflow to aggregate the ledgers. No scheduler, subprocess protocol, branch,
commit, merge engine, or second proof system is added.

The coordinator copies the already-created canonical `.agent/` baseline as an
opaque byte tree during worktree setup. It never interprets, edits, or
synthesizes those artifacts; all later proof mutations still go through
Agentflow. Exact copying is required so aggregation sees byte-identical locked
contracts in every input.

## User surface

Add `-plan-workers N`, default `1`. It is valid only with `-plan` and must be
positive. Values greater than one enable a single worktree cohort bounded by
`N`. If fewer than two safe steps exist, Golem reports a serial fallback and
runs the unchanged step loop without creating worktrees.

This is fresh-run-only. There is no automatic resume, reclaim, retry, dynamic
second wave, or multi-host dispatch.

## Eligibility

The coordinator considers steps in locked-plan order and selects at most `N`.
A step qualifies only when:

- the plan contains at least one real dependency edge;
- `depends_on` is empty;
- every `files` entry is a clean workspace-relative literal path;
- no entry contains glob metacharacters or denotes `.agent/` or `.git`;
- no existing entry or parent resolves through a symlink;
- no existing entry is a directory; and
- its paths are disjoint from every selected step, conservatively using
  case-insensitive equality and path-ancestor comparison.

Only an initial zero-dependency cohort is dispatched. This avoids inventing a
ready-set contract: the locked plan is already cycle/reference validated, and
all dependent or ambiguous work remains for Agentflow's existing serial
`next-step` loop.

The dependency-edge requirement preserves Agentflow's legacy semantics.
Agentflow deliberately treats a plan with no `depends_on` edges anywhere as
serial plan order, even though every decoded `depends_on` slice is empty. Such
a plan always falls back to Golem's serial loop.

The source root must be a clean Git worktree outside `.agent/` before dispatch
and still clean before promotion. This prevents parallel mode from overwriting
caller changes. Golem records the exact `HEAD` and Git toplevel and revalidates
both immediately before promotion, so a clean checkout or commit during the
wave cannot be mistaken for the original base. An existing selected path must
be tracked at that base and have neither `assume-unchanged` nor `skip-worktree`
set; a new selected path must not be ignored. Each worker's final Git status,
including ignored entries outside `.agent/`, must be a subset of its assigned
literal paths. That catches model drift and gate-generated shared artifacts
before canonical source or proof state changes.

## Lifecycle and ownership

The existing driver keeps ownership of workflow routing, initialization,
evidence, plan locking, execution initialization, review amendments, and final
proof. Parallel mode inserts one optional cohort after `doctor` and before the
serial `next-step` loop.

For each selected step, the coordinator:

1. creates a detached worktree at the same recorded `HEAD`;
2. copies canonical `.agent/` byte-for-byte, rejecting symlinks;
3. assigns stable owner `golem-wN` and aggregation source `wN`;
4. verifies the worker's authoritative resumability projection is fresh,
   locked, agent-bound, diagnostic-free, and has no open attempt; and
5. invokes the existing `runOneStep`, which only claims the assigned step,
   runs the model and gates, and finishes that step.

Every copied worker projects the same first plan-order eligible step. Golem
does not misread that single-step projection as authorization for the whole
cohort. The coordinator proves the assigned step is an initial DAG root; the
projection proves only that the copied tree and agent identity are fresh and
valid, and the assigned `claim-step` remains Agentflow's mutation boundary.

Every worker gets its own workspace, receipt journal, and agent orchestrator.
One goroutine owns interrupt cancellation for the wave; workers do not race to
consume the interrupt channel. A worker never initializes contracts, selects
more work, records run-level review, aggregates ledgers, or calls `finish-run`.

Agentflow's default lease policy remains advisory. That is sufficient for this
ceiling because there is one in-process coordinator and exactly one writer per
isolated worktree. Enforced leases are required before adding independent
processes, retries, automatic resume, or multi-host workers; Golem must not edit
Agentflow's execution contract directly to simulate that future mode.

## Canonical integration and proof

Before workers start, the coordinator snapshots every assigned canonical path.
Workers are quiescent before integration. The coordinator promotes only the
validated worker diff and rolls the files back if promotion or dry-run
aggregation fails. It holds one exclusive Git-dir lock across snapshot,
promotion, and aggregation; opens a root-anchored filesystem handle so a
swapped parent cannot escape the invocation root; and compares bytes, mode, and
file identity immediately before both replacement and rollback. Any canonical
drift is a hard stop that preserves the newer caller bytes.

It then calls:

1. `aggregate-ledgers --dry-run --json` with every worker input, stable source
   ID, the invocation root as output, and the exact common base SHA;
2. the same command without `--dry-run` only after a clean preview.

The output root must already contain the workers' final source bytes because
Agentflow combines ledgers, not files. Agentflow detects contract, base, step,
file, receipt, and output-hash collisions, namespaces tree-local IDs, and
atomically replaces canonical `.agent/` on a successful write.

A structured collision writes no proof state and rolls source promotion back.
A transport failure during the real write is ambiguous: Golem stops, preserves
the promoted source and worktrees for inspection, and never retries or
finalizes automatically.

After successful aggregation, the unchanged serial loop handles skipped and
dependent steps. Reviews remain serial. The existing canonical `FinishRun`
runs exactly once. Worker worktrees are removed only after that proof succeeds;
all failures preserve them and print their paths.

## Typed Agentflow seams

`agentflow.Client` gains:

- a stable per-client agent identity while preserving `NewClient`'s `golem`
  default;
- `--agent` consumption plus typed contract, agent, attempt, and diagnostic
  resumability fields used by the fresh-worker check;
- a parallel feature probe for `aggregate-ledgers` and every used flag; and
- a typed aggregation result plus a collision error that preserves valid JSON
  emitted with exit status 1.

Only Agentflow's stable top-level aggregation variants are typed. Nested
`planned`, `written`, and collision records remain raw JSON because Agentflow
does not freeze their individual shapes. Unused resumability details such as
receipts and gates remain additive JSON rather than speculative Go APIs.

## Failure boundaries

| Failure | Result |
| --- | --- |
| No eligible pair | Serial fallback; no worktree created |
| Setup or worker failure | Cancel peers; canonical source untouched; preserve worktrees |
| Unexpected worker diff | Stop before promotion; preserve worktrees |
| Canonical source changes during the wave | Stop before promotion |
| Promotion or dry-run failure/collision | Restore canonical source; preserve worktrees |
| Real aggregation collision | Restore canonical source; preserve worktrees |
| Real aggregation transport failure | Preserve promoted source and worktrees; do not retry |
| Later serial/review/proof failure | Preserve aggregated canonical state and worktrees |
| Canonical proof success | Remove worker worktrees; report proof pack normally |

## Deferred ceiling

Dynamic waves, glob/directory ownership, automatic conflict resolution,
committing or merging worker branches, retries, resume/reclaim automation,
independent worker processes, enforced leases, and multi-host execution remain
out of scope. Add them only after this bounded path demonstrates a concrete
need.

## Verification

Unit tests pin feature probes, per-owner argv, every aggregation JSON/exit
variant, fresh resumability validation, cohort selection, driver sequencing,
worktree isolation, unexpected-diff rejection, promotion rollback, preservation
on failure, and cleanup after proof. An Agentflow integration test runs two
independent root steps in worktrees, aggregates their ledgers, runs a dependent
step serially, and verifies one canonical proof pack.
