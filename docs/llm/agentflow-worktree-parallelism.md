# Worktree-parallel Agentflow execution

## Decision

**Defer runtime implementation.** Agentflow has the proof primitives needed for
one writer per worktree, but Golem cannot yet run one assigned step without also
initializing, driving, and finishing the whole run. Agentflow also lacks an
authoritative machine-readable resume projection with the active attempt,
owner, lease, and safe next operation. Building now would make Golem infer
durable state from ledgers or command text.

This conclusion is based on:

- `go-llm` `origin/develop` at
  [`649ed37478a374d2ccfd45c2deb5b1952b3afe51`](https://github.com/kstruzzieri/go-llm/commit/649ed37478a374d2ccfd45c2deb5b1952b3afe51).
- Agentflow `origin/main` at
  [`5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4`](https://github.com/kstruzzieri/agentflow/commit/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4).
- [PR #310](https://github.com/kstruzzieri/go-llm/pull/310), which was open
  and unmerged at `2026-07-16T02:55:12Z`, at head
  `9dd9561269b733e4598e3eea2f2a50d08f458e5c`.
  None of its eight commits was in the inspected `origin/develop`, so this note
  does not depend on them.

| Approach | Result |
| --- | --- |
| Build a Golem coordinator now | Rejected: it would need to guess recovery state and split a monolithic lifecycle while shipping. |
| Wrap current Golem and Agentflow CLIs in a script | Rejected: independent Golem drivers would race to run the whole plan and finalize incomplete worktrees. |
| Defer behind explicit worker and resume contracts | Chosen: it reuses Agentflow's shipped lease and aggregation semantics without creating a second proof system. |

The current Golem driver unconditionally initializes and locks state, loops over
all eligible steps, and calls `finish-run`; its one-step method is private to
that loop ([driver](https://github.com/kstruzzieri/go-llm/blob/649ed37478a374d2ccfd45c2deb5b1952b3afe51/cmd/golem/agentflow_driver.go#L61-L138)).
Every claim also uses the fixed owner `golem`, while recovery consumes only
advisory `next-action` fields and plain-text `status`
([client](https://github.com/kstruzzieri/go-llm/blob/649ed37478a374d2ccfd45c2deb5b1952b3afe51/agentflow/client.go#L12-L14),
[recovery projection](https://github.com/kstruzzieri/go-llm/blob/649ed37478a374d2ccfd45c2deb5b1952b3afe51/agentflow/client.go#L325-L350)).
The task-mode contract therefore still explicitly defers resume and parallel
writers ([task-mode documentation](agentflow-task-mode.md#scope-and-deferrals-p0)).

## Safe future contract

### Eligibility and isolation

A coordinator may dispatch a step only when every locked `depends_on` step is
complete in Agentflow's authoritative projection. Agentflow already validates
dependency references and cycles and selects dependency-ready work
([plan schema](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/schemas/plan-lock.schema.json#L90-L113),
[dependency validation](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/validation.py#L224-L269),
[selection](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/execution.py#L400-L421));
Golem should consume that state, not recreate it from logs. `claim-step` itself
does not recheck dependencies, so dispatch must remain coordinator-owned and
fail closed
([claim](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/execution.py#L456-L507)).

The first implementation should parallelize only steps whose effective
`step.files` ownership is provably pairwise disjoint. Exact paths qualify.
Globs, directory scopes, or any ambiguous overlap serialize. A generated file
qualifies only when its exact output is owned by one step and it does not update
a shared manifest, index, lockfile, snapshot, or other generated artifact.
Shared/generated artifacts otherwise form an implicit dependency and serialize.
`.agent/` is Agentflow-owned state and never counts as model-owned step scope.

Each worker gets:

- one worktree and branch created from the same recorded base commit;
- byte-identical locked plan, execution, workflow, assumptions, and runtime
  contracts
  ([required matches](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/aggregate.py#L29-L35));
- one stable, unique Agentflow owner ID and aggregation source ID; and
- `concurrency.lease_policy: enforce`, not Agentflow's advisory default.

The worker may claim its assigned step, run the model and declared gates, and
finish that step. It must not initialize or lock contracts, select another
step, record review for the run, aggregate ledgers, or finish the run.

### Ownership

| Coordinator owns | Agentflow already owns |
| --- | --- |
| Freeze the common contracts and base commit | Validate the locked plan and dependency graph |
| Select a disjoint dependency-ready cohort | Enforce one live owner/lease per attempt |
| Create worktrees, branches, worker IDs, cancellation, and retries | Record attempts, file/command receipts, gates, and abandoned attempts |
| Quiesce workers and merge branches into one canonical worktree | Detect plan, base, step, file, receipt, and output-tree collisions |
| Invoke dry-run, aggregation, and final proof commands once | Namespace source-local IDs and bind aggregation provenance into the proof |

Independent Golem drivers must never initialize, finish, or otherwise mutate a
shared run implicitly. Agentflow's lease is a local worktree lock, not a remote
scheduler; cross-worktree coordination remains Golem's responsibility
([lease contract](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/docs/single-writer-leases.md)).

### Integration and proof

After every worker is quiescent and its assigned step is complete:

1. Merge worker branches into a disposable canonical integration worktree.
   A Git conflict stops the cohort; do not hide conflict-resolution edits
   outside an Agentflow attempt.
2. Run `aggregate-ledgers --dry-run` with the stable source IDs and the recorded
   base commit. The canonical output must already contain the merged source
   tree. Dry-run writes nothing and fails closed on contract, base, step, file,
   receipt, or output-hash collisions
   ([CLI precondition](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/cli.py#L936-L982),
   [collision checks](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/aggregate.py#L544-L579)).
3. Only after a clean preview, run real aggregation. Agentflow namespaces
   worktree-local attempt and receipt IDs and writes `aggregation.json` with
   each source's base, head, and prefix.
4. Run `finish-run --root <canonical> --json` once. It owns the fail-fast
   `audit-drift` -> `verify-run` -> `build-proof` -> `verify-proof` sequence;
   worker worktrees never produce the final proof. Agentflow's
   [terminal sequence](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/porcelain.py#L301-L337)
   and
   [two-worktree example](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/examples/aggregation/run.py#L74-L88)
   establish this canonical-only finalization order.

An unexpected Git, step, or file overlap falls back to serial execution from a
clean canonical base. Do not synthesize a merged receipt: rerun the affected
work under an explicit planned step or amendment so its final bytes have one
authoritative attempt. Contract/base mismatches, malformed ledgers or IDs, and
output-precondition failures instead require correcting or recreating the
invalid inputs; they are not merge conflicts. A partially merged integration
branch is unproven and must not be aggregated or published.

### Failure semantics

| Failure | Required behavior |
| --- | --- |
| Cancellation before claim | Stop dispatch and discard the untouched worker. |
| Cancellation after claim | Preserve the worktree and open attempt; never assign it concurrently. |
| Same owner resumes | Resume only when Agentflow's authoritative projection identifies the attempt and says renewal/resume is safe. |
| Owner is abandoned | After lease expiry, use `reclaim-step` to abandon and supersede it; use `fail-step` only as an explicit break-glass decision. |
| Retry after terminal failure | Open a new attributed attempt; never rewrite or reuse prior receipts. |
| Worker or merge fails after other branches merged | Stop before aggregation. The disposable integration branch is not canonical proof and may be recreated. |
| Dry-run reports a collision | Do not aggregate. Serialize overlap work; correct or recreate invalid contract, base, ledger, or output inputs; then preview again. |

These rules preserve receipt attribution: Agentflow retains shared baseline
rows once and namespaces tree-local attempts and receipts as `WT<source>-...`
([aggregation rewrite](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/aggregate.py#L584-L746)).

## Prerequisites and smallest future slice

Track these prerequisites separately from #213:

1. **Agentflow resumability projection.** Add a stable read-only JSON projection
   containing locked contract identity, step, attempt, owner, lease policy and
   expiry, attempt state, completed receipts/gates, and allowed recovery actions.
   Current `next-action` omits attempt, owner, and lease fields, and `status` has
   no JSON surface
   ([declared JSON shape](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/cli_contract.py#L72-L80),
   [status](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/cli.py#L1290-L1350)).
2. **Agentflow aggregation JSON contract.** Align the declared
   `aggregate-ledgers --json` shape with dry-run and write results before Golem
   adds a typed client. The manifest declares `source_count`, `output`,
   `dry_run`, and `rewrites`, while the implementation returns
   `planned` for preview and `written` for success
   ([manifest](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/cli_contract.py#L72),
   [implementation](https://github.com/kstruzzieri/agentflow/blob/5eda54bd11bbec5000fe24fdebe30dc3fc7bf6f4/src/agentflow/aggregate.py#L954-L1015)).
3. **Golem coordinator/worker seam.** Expose an assigned-step worker with a
   unique owner ID that attaches to coordinator-created contracts and stops
   after `finish-step`; add typed aggregation and coordinator-only finalization
   calls plus feature probes. Reuse the existing driver and Agentflow client;
   do not add a generic scheduler or another ledger.

Once those land, the smallest useful implementation is one bounded wave of
dependency-ready steps with exact, disjoint file paths, followed by fail-closed
merge, dry-run aggregation, real aggregation, and canonical proof. Globs,
dynamic scheduling, automatic conflict resolution, and multi-host workers stay
deferred until that slice proves insufficient.

This spike adds no runtime flags, schemas, adapters, or execution behavior.
