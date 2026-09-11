# Local CI

This repository uses a Docker-backed local CI runner so lint, race tests, and compile-smoke checks run before pushes without relying on GitHub Actions minutes.

## Quickstart

Enable the pre-push hook once per clone:

```bash
scripts/setup-local-ci
```

After updating to this version, rebuild the CI image once so it includes the
Python prerequisite used by the audit regression test:

```bash
docker compose -f docker-compose.ci.yml build ci
```

Run the same suite the hook runs:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

Run the faster pre-push subset manually when you do not need the compile-smoke pass:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

The Docker runner builds from `Dockerfile.ci`, mounts the repository at `/workspace`, and keeps named cache volumes for Go modules, Go build output, and golangci-lint data. The compose file pins the project name to `go-llm`, so linked worktrees share the same image and cache volumes as the main checkout.

## Linked Worktrees

`scripts/ci-local` resolves the module root from its own location and validates that the root contains `go.mod` (declaring this module) and `docker-compose.ci.yml`, instead of asking Git. Linked worktrees store `.git` as a file pointing at host-only metadata that is not visible inside the container, so Git-based root discovery would fail there. The Docker gate and the pre-push hook therefore work from both normal checkouts and `git worktree add` checkouts.

`scripts/test-ci-local` is a hermetic regression harness for the local gate. It
stubs the external commands, needs no toolchain or Docker, and covers root
discovery, exact phase ordering, prerequisites, failure status propagation, the
pre-push hook, and the native Darwin selectors and requirement environment. The
`CI` workflow runs it on every pull request.

## Typical Workflow

1. Make code changes normally.
2. Run `scripts/ci-local --mode pre-push` for a faster host-side check while iterating, or use the Docker command above when you want the pinned CI toolchain.
3. Push the branch. The `.githooks/pre-push` hook automatically runs the Docker-backed `full` suite and blocks the push on failure.
4. GitHub runs the required `Lint & Test` and `macOS Compile Smoke` workflows on PRs to satisfy branch protection. Ordinary push-triggered Actions remain disabled.

## Command Contract

Run the faster pre-push subset directly on the host:

```bash
scripts/ci-local --mode pre-push
```

Run the full suite directly on the host. This includes all `pre-push` checks plus compile smoke:

```bash
scripts/ci-local --mode full
```

Run the faster pre-push subset inside Docker:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

Run the full suite inside Docker. This is what the pre-push hook runs automatically:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

## Check Sets

`pre-push` runs:

- compiled discovery of `TestHardeningContracts`, followed by a fresh,
  verbose, non-race run of that aggregate
- `golangci-lint fmt --diff` (all Go files, including inactive build tags)
- `golangci-lint run`
- `go test -race ./...`

`full` runs the pre-push suite plus:

- `go test -run '^$' ./...`

The repository-wide race pass owns discovery of platform-eligible unit tests.
The additional security-contract phase discovers and runs only
`TestHardeningContracts`; it does not replace or duplicate that broad pass.
Both host and Docker runs require `python3` so the existing audit regression
cannot silently skip for a missing interpreter.

The gate clears inherited `GOROOT` for both Go and lint and overrides `GOFLAGS`,
including values saved with `go env -w`, so local defaults cannot silently filter
out tests. It also requires the exact top-level aggregate to report `PASS`;
a zero-test success or top-level `SKIP` fails before formatting. Declared deferred
subtests remain allowed. This confirms execution of the named aggregate, not the
completeness of its implementation; changes inside the test still need review.

The aggregate phase uses these exact commands (in that sanitized environment):

```bash
env -u GOROOT go test -list '^TestHardeningContracts$' ./agent
env -u GOROOT go test -count=1 -v -run '^TestHardeningContracts$' ./agent
```

Local tests retain platform and permission skips. Native CI workflows separately
enforce real Linux bwrap confinement, real Darwin Seatbelt confinement, and the
permission-denial cases that need a non-root Linux runner. The local Docker
service remains unprivileged and runs as root, so it does not claim those checks.
Generated fuzzing remains tracked by #512.

## Git Hook

The pre-push hook lives at `.githooks/pre-push` and runs the full Docker-backed suite:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

That means pushes fail locally if security contracts, lint, race tests, or
compile-smoke checks fail.

The setup script only updates this clone's local Git config:

```bash
git config core.hooksPath .githooks
```

## GitHub Actions

The `CI` workflow runs on `pull_request` so protected branches receive the required `Lint & Test` status. It does not run on ordinary pushes.

The macOS compile-smoke workflow also runs on pull requests to `develop` and `main`, providing the required native-Darwin status and real Seatbelt confinement coverage. It remains available as a manual fallback through `workflow_dispatch`. Local Docker CI is the blocking path before pushes during normal development.

Remaining work for #481: the Darwin background-exec selector still does not
match `TestProcessExitObserverAlreadyExitedChildRecovers` or
`TestProcessExitObserverRegistrationFailsClosed`. A follow-up should add
`|TestProcessExitObserver` to that existing selector.

### Linux sandbox confinement (#441)

The `Linux Sandbox (bwrap)` job in the `CI` workflow runs the bwrap
behavioral suite on a pinned `ubuntu-24.04` VM with
`GO_LLM_REQUIRE_BWRAP=1`, which turns a capability skip into a hard
failure. This job is the required behavioral gate for the bwrap backend:
the Docker-backed local CI container runs the cross-platform policy and unit
tests, but its image does not provide bwrap or promise user-namespace capability.
Do not add a privileged or security-opt-weakened compose service to emulate that
runner; the VM job is the gate.

When Ubuntu's unprivileged-userns AppArmor restriction is active, the job
loads the distro's narrow `bwrap-userns-restrict` profile for
`/usr/bin/bwrap` instead of disabling the global sysctl.

The root Docker service also cannot prove every permission-denial path:
`TestWriteFilePreparingJournalAbortsOnWriteFailure` skips as root, while
`TestSafeEtcPolicyPaths` silently omits its permission-denial branch. An empty
skip log therefore does not establish complete permission coverage; the native
non-root Linux job owns it.

## Notes

The CI image is based on `golang:1.25-alpine` to match `go.mod`. It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
