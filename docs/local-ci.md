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
On a Linux host the bind-mounted checkout must be readable by uid 1000; the gate never writes into it. The volumes carry a `-ci` suffix (`go-llm_go-build-cache-ci`, `go-llm_go-mod-cache-ci`, `go-llm_golangci-lint-cache-ci`) because the image runs unprivileged and the volumes must be created with that ownership; the root-owned volumes of the earlier image (`go-llm_go-build-cache`, `go-llm_go-mod-cache`, `go-llm_golangci-lint-cache`) are no longer used and can be removed with `docker volume rm`.

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

Run only the security-contract phase (about two seconds warm). The GitHub
`Lint & Test` job runs this same mode, so the gate is enforced server-side even
when the local hook is bypassed:

```bash
scripts/ci-local --mode security
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
out tests. It requires both the exact top-level aggregate and its `Active` group
to report `PASS`: every real contract lives under `Active`, so a zero-test
success, a top-level `SKIP`, or a skipped `Active` group fails before formatting.
Only the four declared boundary groups (`ZT-602`, `ZT-603`, `ZT-605`, and `ZT-606`) may
skip; any other `SKIP` line inside the aggregate fails. Removing a contract call
outright emits no skip, so that remains a review responsibility; the gate proves
the named groups executed, not that their contents are complete.

The aggregate phase uses these exact commands (with `GOROOT` unset and
`GOFLAGS=' '` exported for the rest of the script). The 60-second timeout bounds a
hung contract well under Go's ten-minute default; the aggregate itself asserts a
500 ms budget.

```bash
go test -list '^TestHardeningContracts$' ./agent
go test -count=1 -timeout 60s -v -run '^TestHardeningContracts$' ./agent
```

The local Docker service runs as an unprivileged user (uid 1000), so the
permission-denial tests that skip under root execute in the gate exactly as they
do on the GitHub runner. Native CI workflows still separately enforce real Linux
bwrap confinement and real Darwin Seatbelt confinement; the local image provides
neither. Generated fuzzing remains tracked by #512.

The compose service sets `init: true`, so tini runs as PID 1 and reaps the
orphans a killed process group leaves behind. The process-reaping tests also
treat a zombie as gone, because an invocation without an init (`sh -c '...; go
test ...'` execs its final command, leaving `go` as PID 1) never reaps them and
`kill(pid, 0)` alone would then report every dead orphan as alive.

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

The `CI` workflow runs on `pull_request` so protected branches receive the required `Lint & Test` status. It does not run on ordinary pushes. Its `Security contracts` step runs `scripts/ci-local --mode security`, the same discovery, exact-`PASS`, and skip-allowlist gate the pre-push hook runs, so a renamed, skipped, or hollowed-out aggregate cannot pass the required check by bypassing the local hook.

The macOS compile-smoke workflow also runs on pull requests to `develop` and `main`, providing the required native-Darwin status and real Seatbelt confinement coverage. It remains available as a manual fallback through `workflow_dispatch`. Local Docker CI is the blocking path before pushes during normal development.

The Darwin background-exec selector also runs the #481 exit-observer
regressions (`TestProcessExitObserverAlreadyExitedChildRecovers` and
`TestProcessExitObserverRegistrationFailsClosed`); `scripts/test-ci-local` pins
that selector alongside the Seatbelt ones.

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

Because the Docker service runs unprivileged, tests that skip under root
execute in the gate (`TestWriteFilePreparingJournalAbortsOnWriteFailure`, for
example), and `scripts/ci-local` refuses to run the suite as root so a stale
root image cannot report a hollow green. Tests that skip on a case-sensitive
filesystem still skip in the container; the native Darwin job covers those.

## Notes

The CI image is based on `golang:1.27-alpine`, matching the `toolchain` line in `go.mod` (the module's minimum language version stays `go 1.25.0`). It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
