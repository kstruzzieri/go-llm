# Pre-push security regression gate (#453) — Spec and Implementation Plan

> **Status: PR #540 published; Gemini follow-up fixes implemented, verified and reviewed on 2026-09-11.**
> **Revision: 2026-09-11 — Gemini PR #540 review addressed.**
> For agentic workers: after approval, use `superpowers:subagent-driven-development`
> or `superpowers:executing-plans`, with an independent review after each task.

**Goal:** Make the shipped security policy contracts an explicit pre-push gate,
preserve discovery and execution of platform-eligible security unit tests, and
prove that failures block the hook. Keep capability-dependent exclusions explicit.

**Architecture:** Add one fresh, targeted contract-test phase to the existing
`scripts/ci-local`, with a compiled-test presence check before execution. Keep
the existing repository-wide race test pass as the owner of complete unit-test
discovery for the active platform. Reuse the shell self-test and native sandbox jobs.

**Tech stack:** Existing Bash/POSIX shell, Go 1.25.0, Docker Compose, Alpine CI
image, and GitHub Actions. Add Alpine `python3` for the existing audit test; no Go
dependency, test framework, service, or runtime security change.

**Spec:** Part I of this document. Part II implements it.

**Planning baseline:** `origin/develop` at
`14af019b8fdeda251ad1a25a84393a1c90fe25b8`, independently verified against GitHub's
`refs/heads/develop` on 2026-09-09 local time. The main working checkout is older,
at `654f1cc`, with unrelated untracked `copilot.json` and `output/`. At planning
time, inspection used `git show origin/develop:...`; no source, branch, worktree,
or Git ref was changed, and no tests had been run.

## Approval decisions

Keith approved these decisions together before implementation:

| ID | Recommendation | Consequence |
|---|---|---|
| D1 — gate shape | Add a `security contracts` phase to both existing modes, before formatting/lint; verify Go lists the compiled aggregate before running it. Retain the existing race pass and the hook's `full` mode. | A missing aggregate fails locally, rather than relying on a later CI source scan. One fresh aggregate run adds named evidence; the broad pass selects the remaining platform-eligible tests. No third mode or separate suite registry. |
| D2 — enforceable coverage | Keep real sandbox confinement and root-dependent permission checks on native CI; correct the three omitted Darwin confinement selectors. Install and require Python 3 for the existing audit regression, with an actionable rebuild hint. | Small scope extension to `.github/workflows/darwin-smoke.yml`. Host-side `ci-local` also requires Python 3. Existing Docker users must rebuild the CI image once. Local execution remains subject to documented platform/permission skips; no container-user or privilege changes. |
| D3 — fuzzing | Leave bounded fuzz generation to #512. | Existing signing fuzz seeds still execute in the full race suite. This PR closes only #453, and makes no generated-fuzz coverage claim. |

The approval gate comes from Keith's explicit request and the supplied handoff.
The Darwin workflow correction is outside the handoff's four listed ownership
files and is deliberately presented here for approval, alongside necessary
self-test and documentation edits.

## Gemini review disposition

The review supports D1–D3, but its recommendation is not Keith's approval and
does not establish that unimplemented acceptance tests pass. Only the corrected
#453 attachment applies here; the earlier #473 and GeoIQ feedback does not.

| Feedback | Disposition |
|---|---|
| Keep the aggregate phase, broad race pass, Python prerequisite, three Seatbelt selector fixes, and #512 deferral | Accept. Preserve the existing design and task boundaries. |
| Add a stale-container rebuild hint | Accept the actionable hint; simplify the implementation. Print conditional wording for host/Docker users on missing-command failure, without a `/.dockerenv` detector, new environment flag, or automatic build. Document the one-time image rebuild. |
| Use a short scope notice and expose the aggregate's elapsed log | Accept. Pin one literal notice and keep `-v`; no new timing instrumentation is needed. |
| A source-presence assertion protects against zero-test success | Strengthen. That assertion originally ran only in GitHub's self-test. Replace it with Go's `-list` check inside `ci-local`, plus hermetic missing-name/failure tests, so a renamed or build-excluded aggregate fails the local push gate too. |
| A contract violation fails pre-push within one second | Reject the unmeasured timing guarantee. The 500 ms assertion covers the aggregate body; compilation, discovery, Go startup, Docker startup, cache state, and scheduling add time. Other regressions are caught later by the broad pass. |
| All unit tests execute in Docker | Correct to complete discovery for the active build. Root-dependent permission tests and runtime capability checks can still skip or omit branches; see Spec §3. A successful package result alone is not evidence that each case executed. |
| Add `TestProcessExitObserver` to the background runtime selector | Verified adjacent gap; defer. The two Darwin-only tests identify themselves as #481 regressions, within the workflow's #346 background-exec area. Record the names and suggested selector in the PR's remaining-work note; do not widen #453 or claim coverage of them. |

The Go tool documents `-list` as listing top-level tests without running them;
Docker documents `compose run --build` as the explicit build-before-run option.
These support the discovery check and rebuild guidance, not measured runtime or
completed test claims. See [Go testing flags](https://pkg.go.dev/cmd/go@go1.25.0#hdr-Testing_flags)
and [Compose run options](https://docs.docker.com/reference/cli/docker/compose/run/).

## Global constraints

- Spec + TDD plan approved by Keith BEFORE any implementation.
- Create an isolated linked worktree from fresh `origin/develop`; never commit
  to `develop`. Branch: `feat/453-security-gate`; directory:
  `.worktrees/453-security-gate`.
- Start every command operating on the worktree with its absolute `cd ... &&`.
  Prefix interactive executables with `rtk`; use `rtk proxy` for raw execution.
  Every Go invocation uses `env -u GOROOT go ...`, including inside `ci-local`.
- Preserve the existing format, lint, repository-wide race, and full-mode
  compile-smoke gates. Do not weaken Docker isolation or add privileged services.
- No production Go changes, new security policy, new test framework, generated
  test registry, or changes to another lane's runtime/REPL files.
- Extend existing tests. Every new assertion must have a corresponding mutation
  that makes it fail; pin literal command arguments, status codes, and diagnostics
  independently of the implementation being checked.
- Review each task until clean, then critically review the findings. If the
  handoff's literal `/code-review` and `/criticize-review` commands are unavailable,
  use equivalent independent review and critique; do not claim those commands ran.
- Add `changelog.d/453-security-gate.md`; never edit `CHANGELOG.md`.
- Full lint: `golangci-lint run --max-same-issues 0 --max-issues-per-linter 0 ./...`.
- Full Docker gate: `docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full`.
  Capture actual exit status directly; never use `go test | tail`.
- PR targets `develop`, includes `Closes #453`, and contains no emojis.
  Implementation approval does not itself authorize a push or PR publication.

## Part I — Spec

### 1. What exists and what changes

The handoff's mode description is stale. At the verified baseline:

- Both `pre-push` and `full` already run formatting, lint, and
  `go test -race ./...`.
- `full` adds `go test -run '^$' ./...` compile smoke.
- `.githooks/pre-push` already selects `full`; Dockerfile and Compose defaults
  select `pre-push`. `scripts/setup-local-ci` and the documentation agree.
- `scripts/test-ci-local` already stubs commands and checks exact mode ordering,
  outside-CWD execution, linked-worktree layout, hostile `CDPATH`, and bad roots.
  `.github/workflows/ci.yml` runs that self-test on pull requests.

Consequently, #453 does not require another exhaustive package list. The existing
race pass selects security unit tests available for its platform/build, including
new tests added later; individual tests can still skip for runtime prerequisites.
The missing deliverable is an explicit fresh contract phase, tested failure
propagation, and an accurate statement of capability-dependent coverage.

Rejected alternatives: a third `security` mode would add routing and opt-in
behavior; a second list of every security test would duplicate the race pass and
drift as tests are added. Neither is needed for the acceptance criterion.

### 2. Local command contract

After argument and module-root validation, require `go`, `golangci-lint`, and
`python3`. A missing executable exits 127 with the existing
`missing required command: <name>` diagnostic before any test phase.
Follow it with this literal hint, applicable without detecting the environment:

`hint: install the missing command for host runs; for Docker CI, rebuild with: docker compose -f docker-compose.ci.yml build ci`

Do not add `/.dockerenv` probing or automatic rebuilding. The unchanged hook does
not request `--build`; an existing image without Python must fail visibly until
the developer rebuilds it. Put this one-time upgrade command in the local CI docs.

Before running phases, unset inherited `GOROOT` for both Go and lint and set
`GOFLAGS` to a single space. Unlike an unset or empty value, nonempty whitespace
overrides flags saved with `go env -w` while supplying no flags to Go. All phases
inherit this environment; other Go environment settings remain unchanged.

Both modes run these phases, sequentially:

1. **security contracts:**
   first run `env -u GOROOT go test -list '^TestHardeningContracts$' ./agent`.
   Capture its stdout while preserving its nonzero exit status and stderr. Require
   an exact output line `TestHardeningContracts`, using a fixed whole-line match
   (`grep -Fxq`), not a substring. If absent, emit
   `missing required security test: TestHardeningContracts` to stderr and exit 1.
   Only then run
   `env -u GOROOT go test -count=1 -v -run '^TestHardeningContracts$' ./agent`.
2. **format:** existing `golangci-lint fmt --diff`.
3. **lint:** existing `golangci-lint run`.
4. **race tests:** existing `env -u GOROOT go test -race ./...`.
5. **compile smoke**, `full` only:
   `env -u GOROOT go test -run '^$' ./...`.

Capture and replay the targeted execution's stdout even when it fails, preserve
its nonzero exit status, and require its exact top-level verbose `PASS` result
before formatting. A generic package `PASS`, zero-test success, top-level `SKIP`,
or child/renamed test pass is insufficient. Declared deferred subtests remain
allowed. This guards execution of the aggregate, not the contents of its body.

The discovery invocation runs no tests and checks the compiled test list rather
than source spelling or filename. It replaces the proposed source-declaration
assertion; do not keep both checks or introduce a suite registry/output framework.
The targeted execution uses `-count=1` to bypass the test-result cache and `-v` to
expose active groups, deferred skips, and the aggregate's elapsed-time report.
Run it alone, without other test packages or race instrumentation competing
inside the phase. Preserve its shipped <500 ms assertion; this measures aggregate
test execution, not discovery, compilation, tool startup, total CI time, or cold downloads.
Measure added warm wall time during verification; do not invent a sub-500 ms
whole-gate guarantee or relax the existing assertion to make a runner pass.

Any nonzero phase status terminates `ci-local` immediately. Compose and the hook
must propagate failure. There is no retry, ignored status, pipeline that masks
failure, new bypass flag, or claim that a skipped test passed.

Keep the hook, Docker CMD, and Compose command selections unchanged. Their
existing entry points inherit the phase through `ci-local`. No extra Compose
service or change to Git hook configuration is necessary.

### 3. Coverage and its limits

| Security area | Execution that enforces it |
|---|---|
| #451 contract aggregate: literal tool framing, dispatch/ownership, detectors, secrets, argument invariants, egress and workspace boundaries | New uncached non-race `TestHardeningContracts` phase; also selected by the existing broad race pass. |
| Remaining fence and interceptor tests; signing and key management; fuzz seed corpus | Existing `go test -race ./...`, subject to platform/build and runtime prerequisites. No generated fuzzing under this proposal. |
| Memory record signing/provenance; mutation receipts; checkpoint authentication; audit; project-context consent; Golem canary; signed delegate proposals | Existing `go test -race ./...`, including shipped #431, #438 and #450 host integration tests omitted from the handoff's short suite list. |
| Sandbox policy, argument builders, scratch isolation and other Linux-compatible tests | Existing broad race pass in Docker. Capability-dependent subprocess confinement is not credited as locally enforced. |
| Audit must not write Python bytecode under the audited tree | Existing `TestAuditProofRunnerDoesNotWritePythonBytecode`; install Alpine `python3` and require the executable so missing Python cannot silently skip it. |
| Actual Linux bwrap confinement | Existing native Ubuntu VM job, non-race and race, `-count=1`, `GO_LLM_REQUIRE_BWRAP=1`. |
| Actual Darwin Seatbelt confinement | Existing native macOS job, non-race and race, `GO_LLM_REQUIRE_SEATBELT=1`, with the selector correction below and explicit `-count=1`. |
| Permission-denial tests that cannot exercise their contract as root | Existing native non-root Linux `Lint & Test` job. The root Docker service does not enforce these cases; verify execution from native job evidence before claiming it. |

The Docker image currently lacks both bwrap and Python. Python is an ordinary
test prerequisite that the image can provide. Real bwrap also needs kernel
capabilities that this normal Docker runner does not promise. Do not install or
require bwrap in local CI, enable privileged execution, weaken seccomp, or export
the sandbox requirement variables into Docker to manufacture a coverage claim.
Seatbelt is unavailable in a Linux image.

Print this single line immediately after the security section heading, and
document the same split in the script header and `docs/local-ci.md`:

`note: local tests retain platform/permission skips; native bwrap/Seatbelt confinement runs in CI workflows`

Direct host runs
may opportunistically execute supported sandbox tests, but local CI does not
claim the enforced native-job coverage.

The current Docker service runs as root. For example,
`TestWriteFilePreparingJournalAbortsOnWriteFailure` skips as root, and
`TestSafeEtcPolicyPaths` omits its permission-denial branch as root without a
`t.Skip` event. `rag/indexer_unix_test.go` also skips permission-error cases as
root. Therefore neither broad discovery nor an empty skip log establishes
complete execution. Preserve these existing conditions and the Docker user;
the native non-root job owns permission-dependent execution. D2 explicitly
accepts this local/native split rather than promising every security case runs
before every push.

`TestHardeningContracts` deliberately reports five deferred boundary subtests
(#431–#435). #431 has since shipped separate tests that the full pass selects;
its stale aggregate placeholder is outside this scripts/CI task. Do not edit
the harness or reject every skip. Native Seatbelt helper-process legs also
intentionally skip under race instrumentation, which is why both native passes
must remain.

### 4. Darwin selector correction

The current native selector excludes these existing tests:

- `TestScratchSeatbeltComposition` and
  `TestScratchSeatbeltClonedTreePassesLinkValidation` in
  `agent/tools/scratch_sandbox_darwin_test.go`.
- `TestNewExecBackendSeatbeltMatchesRealCapability` in
  `agent/tools/exec_darwin_test.go`.

Use this selector in both existing Seatbelt steps:

`^(TestSeatbelt|TestSBPL|TestProbeSeatbelt|TestScratchSeatbelt|TestNewExecBackendSeatbelt)`

Add `-count=1` to both commands; preserve each step's requirement environment,
the race/non-race distinction, runner, triggers, and action pins. Linux already
selects its scratch twins and uses `-count=1`; its workflow needs no edit.

This establishes workflow execution. Remote branch-protection requirements have
not been inspected, and this change does not configure them or claim they were
verified.

Adjacent gap, outside this change: `TestProcessExitObserverAlreadyExitedChildRecovers`
and `TestProcessExitObserverRegistrationFailsClosed` in
`agent/tools/background_exit_darwin_test.go` match none of the existing Darwin
execution selectors. They pin #481, and compile smoke only compiles them.
Record a remaining-work note proposing `|TestProcessExitObserver` on the existing
background-exec selector; leave that step unchanged in #453. No external ticket
or comment is created by this planning task.

### 5. Files

| File | Planned change |
|---|---|
| `scripts/ci-local` | Header/coverage notice, compiled aggregate discovery and targeted execution, Python prerequisite/rebuild hint, GOROOT-safe Go invocations. |
| `scripts/test-ci-local` | Extend existing hermetic harness for ordering, prerequisites, failure propagation, entry points, and Darwin selector regression. |
| `Dockerfile.ci` | Add Alpine `python3` to the existing package installation. Preserve image/tool versions. |
| `.github/workflows/darwin-smoke.yml` | Correct both native selectors and disable their test-result cache. |
| `docs/local-ci.md`, `README.md` local CI paragraph | Describe the new phase, one-time image rebuild, Python prerequisite, coverage split and unchanged hook behavior. |
| `changelog.d/453-security-gate.md` | Required changelog fragment. |
| `docs/plans/2026-09-09-security-gate-453-spec-plan.md` | Copy the approved version of this document into the feature worktree. |

`.githooks/pre-push`, `docker-compose.ci.yml`, `scripts/setup-local-ci`, and the
Linux workflow are read/verified integration surfaces; no change is currently
needed. No product Go source or test changes are planned.

## Part II — TDD implementation plan

### Task 1 — Local phase and failure propagation

**Files:** `scripts/ci-local`, `scripts/test-ci-local`, `Dockerfile.ci`.
**Consumes:** existing argument/root validation, command stub/log fixture, and
`TestHardeningContracts`.
**Produces:** the exact mode contract in Spec §2, inherited by existing hook and
Compose entry points.

- [x] After approval, refresh the remote baseline, check lane ownership and
  worktree ignore rules, and create `.worktrees/453-security-gate` on
  `feat/453-security-gate` from `origin/develop`. Copy this approved document into
  its planned repository path. Leave the main checkout's untracked files alone.
- [x] Extend the current command-log fixture with configurable discovery output
  and failures for discovery and security execution. Keep expected argv literal:
  `test -list ^TestHardeningContracts$ ./agent`, then
  `test -count=1 -v -run ^TestHardeningContracts$ ./agent`, before format/lint.
  Discovery succeeds only when its stdout includes the exact aggregate name.
  Provide a Python stub for ordinary command-only cases. Do not add a source
  declaration scan as a second mechanism.
- [x] Add the following cases to the existing self-test, preserving its current
  root/layout cases:

  | Case | Independent expected result |
  |---|---|
  | Default mode, `--mode pre-push`, and `--mode full` | One discovery invocation followed by exactly one security-test execution; format/lint/race unchanged; compile smoke only in full. |
  | Discovery exits zero with empty output or only `TestHardeningContractsRenamed` | Exit 1 and literal missing-security-test diagnostic; no contract execution or later gate command. Cover both modes. |
  | Discovery lists the exact name among package-summary output | Continue to security execution; no reliance on ordering of non-name lines. |
  | Discovery command exits 38, even if stdout contains the expected name | Preserve exit 38 and its stderr; no contract execution or later gate command. |
  | Security command stub exits 37, for each mode | `ci-local` exits 37; no subsequent format, lint, race, or compile invocation appears in the log. |
  | Existing race command stub fails | Both modes fail; full mode never reaches compile smoke. |
  | Missing `python3` in a deliberately controlled PATH | Exit 127, literal missing-command diagnostic followed by the Spec §2 hint, no discovery/test invocation. Keep the test independent of host Python installation or container-marker files. |
  | Caller supplies a poisoned `GOROOT` | Every Go stub sees GOROOT unset; all expected arguments and working directories are preserved. |
  | Execute the real hook against a Docker stub, with no Git push | Exact existing Compose/full argv; nonzero Docker status propagates, zero status succeeds. |
  | Docker CMD and Compose service command | Existing pre-push entry points remain present; Docker package installation includes Python. |
  | Scope diagnostic | Literal Spec §3 notice, once, before discovery/execution. No blanket skip rejection. |

- [x] Run `rtk proxy bash scripts/test-ci-local`. Confirm the pre-implementation
  failure is specifically the missing security command/contract, not a broken
  stub or an unavailable test dependency.
- [x] Implement the minimal edits from Spec §2; add only `python3` to the image's
  existing `apk add` command. Re-run the shell self-test to green.
- [x] Mutate one behavior at a time in disposable copies: remove/reorder the
  security command, ignore its exit, accept missing/substring-only discovery,
  swallow discovery failure, change a selector, remove Python prerequisite/image
  package or rebuild hint, leak GOROOT, suppress hook failure, or remove the
  scope notice. Each corresponding new assertion must go red. Restore each
  mutation before the next check; retain no mutation in the worktree.
- [x] Prove actual test selection with a disposable fixture mutation: change
  the literal inner `hello` to `hallo` in
  `agent/testdata/hardening/fencing/ordinary.want`, preserving both framing
  markers. Run the exact new Go command and require a byte-contract
  failure/nonzero status. Restore the
  original fixture bytes and confirm green. This temporary validation is after
  approval; the final diff contains no agent test/fixture edit.
- [x] In a disposable validation copy, rename the aggregate declaration while
  keeping its body intact. Run `ci-local` and require the missing-security-test
  diagnostic/exit 1 before the remaining gates. Restore it before final checks.
- [x] Complete independent code review, resolve findings, critique the review,
  and review the final diff before committing the task on the feature branch.

### Task 2 — Native coverage, documentation, and release evidence

**Files:** `.github/workflows/darwin-smoke.yml`, `scripts/test-ci-local`,
`docs/local-ci.md`, `README.md`, the changelog fragment, and approved spec/plan.
**Consumes:** Task 1's local gate and the existing native confinement jobs.
**Produces:** accurate platform coverage and a verified final change.

- [x] First extend the existing shell self-test to pin both literal Darwin
  confinement commands: the selector in Spec §4, `-count=1`, and the separate
  non-race/race invocations. Require the existing Seatbelt environment gate.
  Run it and confirm it fails on the currently omitted selectors.
- [x] Amend just those two workflow commands. Re-run the self-test to green.
  Remove each newly added prefix, `-count=1`, or requirement assertion in a
  disposable workflow copy and verify the relevant check fails. Preserve the
  native Linux workflow unchanged.
- [x] Update docs and add the changelog fragment. State that the full race pass
  owns platform-eligible unit discovery, the extra phase discovers/runs just the
  aggregate, and native confinement/non-root permission coverage is separate.
  Include the one-time image rebuild and adjacent #481 remaining-work note.
  #512 remains open. Do not describe deferred aggregate groups or absent
  platform/permission execution as passing coverage.
- [x] Build the changed image with `rtk proxy docker compose -f docker-compose.ci.yml build ci`.
  A stale existing image is insufficient evidence for the Python prerequisite.
- [x] In that image, run the exact discovery and fresh security commands and the focused audit
  command `env -u GOROOT go test -count=1 -v -run '^TestAuditProofRunnerDoesNotWritePythonBytecode$' ./cmd/golem`.
  Confirm the audit test actually runs rather than skips. Record warm aggregate
  elapsed time and total security-phase wall time separately, including discovery.
- [x] Run `rtk proxy bash scripts/test-ci-local`, shell syntax checks, Compose
  config validation, changelog validation against the feature base, and
  `git diff --check`. No new testing framework is needed.
- [x] Run the handoff's full lint and full Docker gate with their actual exit
  statuses captured. Do not rerun the full gate repeatedly without a change or
  failure that warrants it.
- [x] On the native macOS host, execute both corrected Seatbelt commands with
  `GO_LLM_REQUIRE_SEATBELT=1`, `env -u GOROOT go`, and `-count=1`. Capability
  failure is a failure to report, not permission to disable the requirement.
  The unchanged Linux job remains responsible for native bwrap execution.
- [x] Complete independent final review and critique. Check that no temporary
  mutation, unrelated file, another lane's code, or direct CHANGELOG edit remains.
  Prepare a PR description naming the real coverage split, measured validation,
  adjacent #481 selector gap, and `Closes #453`. Push/publication requires the
  applicable session authorization.

### Completion evidence and approval boundary

Implementation is complete only when the shell tests demonstrate discovery,
phase and hook failure propagation, the missing-aggregate and real fixture
mutations fail, restored tests pass, Python
audit coverage is observed, corrected Darwin tests execute on a capable host,
and the required lint/full Docker gates and reviews are clean. If a runner is
unavailable, report the exact unverified check rather than substituting a skip.

Historical approval boundary: this plan originally made no source changes or
validation runs and requested approval for revised D1–D3 and Tasks 1–2 together.
Keith granted that approval before implementation began, then separately
authorized publication of PR #540 and these review fixes. Merge is not authorized.

Sources: [#453](https://github.com/kstruzzieri/go-llm/issues/453),
[#451](https://github.com/kstruzzieri/go-llm/issues/451),
[#512](https://github.com/kstruzzieri/go-llm/issues/512),
[#429](https://github.com/kstruzzieri/go-llm/issues/429), the supplied handoff,
and repository files inspected at the pinned planning baseline.


## Execution evidence — 2026-09-10

Implemented on `feat/453-security-gate` from the verified `14af019b8f` base.
Task 1 commit: `c8af6fb`. No production Go or fixture changes remain. The main
checkout and its unrelated untracked files were preserved.

| Verification | Observed result |
|---|---|
| Existing shell harness before changes; both tasks' TDD | Baseline green; each new contract first failed against the old implementation; restored harness green on macOS and Alpine. |
| Local gate mutations | 18 disposable shell mutations failed, covering ordering, selectors, missing aggregate, statuses, Python/hint, GOROOT, notices, modes and entry points. |
| Actual fixture mutation | Inner `hello` → `hallo` failed with a byte-contract mismatch at byte 66; restored bytes passed. |
| Actual declaration mutation | Renamed aggregate produced exit 1 and the exact missing-test diagnostic before formatting; restored Go discovery listed the aggregate. |
| Rebuilt image and Python audit | Build exit 0; exact discovery/aggregate commands passed; both bytecode-audit subprocess cases executed and passed without skipping. |
| Warm timing inside Docker | Aggregate body 48.225167 ms; discovery plus execution 1.691305 s, excluding Docker startup. |
| Required full lint | Exit 0, zero issues, using both unlimited-issue flags. |
| Required full Docker gate | Exit 0 through security contracts, formatting, lint, all race packages and compile smoke. |
| Required native Seatbelt passes | Non-race and race exit 0; all three newly selected tests passed. Non-race exercised behavioral helper legs that intentionally skip under race. |
| Shell syntax, Compose config, changelog and diff checks | Exit 0. |
| Independent reviews and root critique | Task 1 and whole-branch/Task 2 reviews approved. Root verified the corrected CDPATH, executable mode, full-mode GOROOT and step-bound workflow assertions; no actionable findings remain. |

The first Docker build stalled resolving the public frontend with the default
client configuration and was cancelled. A temporary empty Docker client config,
using the existing Desktop plugins/socket, completed the build; no user Docker
settings changed. No stale-image result was substituted.

Native Linux bwrap/non-root permission execution and remote branch protection
were not verified on this Mac. Their existing CI responsibility is unchanged.
The five deferred aggregate groups and expected race-incompatible Seatbelt
helper skips remain documented exclusions; #481 and #512 remain separate work.

Detailed reports and command logs are retained in the ignored worktree directory
`.superpowers/sdd/2026-09-09-security-gate-453-spec-plan/`, including the local PR
description with `Closes #453`. PR #540 was subsequently published at the user's
request; no merge was performed.


## Gemini PR review disposition — 2026-09-11

The user requested review and correction of the supplied findings against PR
#540 at `03b953f`. This follow-up retains the previously approved scope decisions.

| Finding or recommendation | Disposition |
|---|---|
| `GOFLAGS=-skip=TestHardeningContracts` silently suppresses the aggregate | Confirmed. Actual Go discovery listed the aggregate, but execution exited 0 with no tests. The gate now overrides environment and persisted flags for all phases. Merely unsetting/emptying `GOFLAGS` leaves `go env -w` defaults active. |
| High-severity attacker push bypass | Narrowed. The reproduced path is a direct host run or a deliberately configured container; the shipped Compose service does not forward the host variable. A user controlling their own hook can already bypass it. The silent configuration failure is still fixed. |
| Top-level `t.Skip()` yields success | Confirmed and fixed with a positive, exact top-level pass requirement. A real temporary skip was rejected with exit 1 before formatting; restored execution passed. Gemini's negative-only SKIP/FAIL grep would still accept a generic zero-test PASS, so it was not used. |
| Deleted or skipped internal subcontracts | A named aggregate pass does not prove its implementation is intact. Preserve the five declared deferred groups and code review; do not duplicate the aggregate's internal test registry in Bash. The limitation is explicit in local CI docs. |
| Stale `GOROOT` still reaches lint | Confirmed: the real linter exited 3 for `/poisoned`. Clear it once for the shared Go/lint phase environment. |
| Literal YAML/Docker assertions are formatting-sensitive | Retain the approved literal configuration contract. Valid formatting changes can update the corresponding expectations; no new YAML dependency or generalized parser is warranted. Assertions remain fail-closed and step-bound. |
| Aggregate runs in both targeted and broad race passes | Intentional: one fresh, non-race aggregate plus the existing broad race selection. The first phase runs only the aggregate, not every test in `agent`. |
| Root Docker hides permission assertions; local hooks are bypassable | Existing, documented limits. Native non-root Linux and native confinement jobs retain responsibility. This change does not promise an unbypassable local security boundary or claim branch-protection settings were verified. |
| Remove the host Python prerequisite | Retain D2: running the full local gate requires the interpreter so the existing audit regression cannot silently skip. A new environment policy switch and Go test change would add scope and weaken the agreed host gate. |
| Add #481 background observer selectors | Retain the explicit #481 deferral; this review establishes no new dependency on that unrelated runtime work. |
| Relax the 500 ms aggregate assertion | Retain the existing contract: the review reports a possible timing risk, not a reproduced regression. No product/test-harness timing change in this scripts/CI fix. |
| Dedicated phase implies exhaustive security coverage | Docs continue to state that the dedicated phase covers one aggregate; remaining platform-eligible security tests belong to the broad race pass, with explicit native/root/deferred limits. |

Follow-up TDD evidence: the expanded shell harness first rejected inherited
flags (exit 39), then rejected a generic zero-test PASS (expected exit 1, got 0).
After the fixes it passes in both modes, including exact output replay, original
failure statuses, renamed/child/skipped outcomes, and allowed deferred subtests.
Seven disposable mutations each fail: leaked lint GOROOT, missing GOFLAGS
override, empty GOFLAGS fallback, removed or overly broad pass check, swallowed
execution status, and lost verbose output. No Go fixture/source mutation remains.

Final follow-up validation: macOS and Alpine shell harnesses, shell syntax,
changelog validation and diff checks passed. Full host lint reported zero issues.
The full Docker gate exited 0 with poisoned GOROOT plus caller and persisted
GOFLAGS configured to skip tests, exercising security, formatting, lint, race
tests and compile smoke under the corrected environment. Independent review of
all five changed files approved with no actionable findings. Root critique
confirmed that declared subtest skips remain allowed, saved flags cannot defeat
the override, and a failed process takes precedence over printed PASS output.

Detailed follow-up logs are retained under the ignored
`.superpowers/sdd/2026-09-09-security-gate-453-spec-plan/gemini-followup/` directory.
The earlier Daybreak approval applies to `03b953f`; it is not represented as a
review of these later changes.
