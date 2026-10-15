# Local CI

This repository uses a Docker-backed local CI runner so lint, race tests, and compile-smoke checks run before pushes without relying on GitHub Actions minutes.

## Quickstart

Enable the pre-push hook once per clone:

```bash
scripts/setup-local-ci
```

Run the same suite the hook runs. `--build` builds the image from this checkout
before the gate, reusing cached layers when the Dockerfile has not changed:

```bash
docker compose -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

Run the faster pre-push subset manually when you do not need the compile-smoke pass:

```bash
docker compose -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode pre-push
```

The Docker runner builds from `Dockerfile.ci`, mounts the repository at `/workspace`, and keeps named cache volumes for Go modules, Go build output, and golangci-lint data. The compose file pins the project name to `go-llm`, so linked worktrees share the same image and cache volumes as the main checkout.
Rebuilding from another branch replaces that shared image. The hook builds the
image from the installed hook's trusted `Dockerfile.ci` before testing, but this does not
isolate concurrent builds:
coordinate builds across worktrees when their Dockerfiles differ. For a manual
run with a separate image and cache volumes, use an explicit project name:

```bash
docker compose -p go-llm-my-branch -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

That project starts with cold caches. Remove its image and volumes when done:

```bash
docker compose -p go-llm-my-branch -f docker-compose.ci.yml down --volumes --rmi local
```

On a Linux host the bind-mounted checkout must be readable by uid 1000; the gate never writes into it. The volumes carry a `-ci` suffix (`go-llm_go-build-cache-ci`, `go-llm_go-mod-cache-ci`, `go-llm_golangci-lint-cache-ci`) because the image runs unprivileged and the volumes must be created with that ownership; the root-owned volumes of the earlier image (`go-llm_go-build-cache`, `go-llm_go-mod-cache`, `go-llm_golangci-lint-cache`) are no longer used and can be removed with `docker volume rm`.

## Linked Worktrees

`scripts/ci-local` resolves the module root from its own location and validates that the root contains `go.mod` (declaring this module) and `docker-compose.ci.yml`, instead of asking Git. Linked worktrees store `.git` as a file pointing at host-only metadata that is not visible inside the container, so Git-based root discovery would fail there. The Docker gate and the pre-push hook therefore work from both normal checkouts and `git worktree add` checkouts. The pre-push hook mounts only an export of each pushed tree, which has no `.git`; its isolated `git archive` ignores local and committed attributes.

`scripts/test-ci-local` is a hermetic regression harness for the local gate. It
stubs the external commands, needs no Go toolchain or Docker (only git 2.32 or
newer, for the hook's fixture repository), and covers root
discovery, exact phase ordering, prerequisites, failure status propagation, the
pre-push hook's ref parsing, complete tree export and trusted Docker settings (against a fixture repository and
a linked worktree of it), the native Darwin selectors and requirement environment, and the
cross-platform builds and vets shared by `--mode full` and the `CI` workflow. The
`CI` workflow runs it on every pull request.

## Typical Workflow

1. Make code changes normally.
2. Run `scripts/ci-local --mode pre-push` for a faster host-side check while iterating, or use the Docker command above when you want the pinned CI toolchain.
3. Push the branch. The `.githooks/pre-push` hook exports each pushed tree, builds the trusted CI image and runs the Docker-backed `full` suite on that export, and blocks the push on failure.
4. GitHub runs the required `Lint & Test`, `Test (oldest supported Go)`, `Linux Sandbox (bwrap)`, and `macOS Compile Smoke` checks on PRs to satisfy branch protection. Ordinary push-triggered Actions remain disabled.

## Command Contract

Run the faster pre-push subset directly on the host:

```bash
scripts/ci-local --mode pre-push
```

Run the full suite directly on the host. This includes all `pre-push` checks plus compile smoke, which also runs `go vet ./...` with `GOOS=windows`, `GOOS=freebsd` and `GOOS=darwin` (amd64 and arm64) so code or tests that only type-check on one platform fail locally, and links `cmd/golem` and `cmd/go-llm-mcp` for every release target (Windows, darwin and Linux, each amd64/arm64) with `CGO_ENABLED=0`, as the release does. Linux/amd64 needs this explicit build too: the race-test lane uses cgo.

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
docker compose -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode pre-push
```

Run the full suite inside Docker. This is what the pre-push hook runs automatically:

```bash
docker compose -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

## Check Sets

`pre-push` runs:

- compiled discovery of `TestHardeningContracts`, followed by a fresh,
  verbose, non-race run of that aggregate with its 500 ms budget enforced
- the containment inventory check, profile `linux-host` (below)
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
Only the two declared boundary groups (`ZT-605` and `ZT-606`) may skip; any
other `SKIP` line inside the aggregate fails, including the retired `ZT-602`
slot (#431 project-trust coverage lives in `cmd/golem`, in
`TestProjectTrustLiveConsentAndFreshness`, and in the `projectcontext` tests).
Removing a contract call outright emits no skip, so that remains a review
responsibility; the gate proves the named groups executed, not that their
contents are complete.

The aggregate phase uses these exact commands (with `GOROOT` unset and
`GOFLAGS=' '` exported for the rest of the script):

```bash
go test -list '^TestHardeningContracts$' ./agent
go test -count=1 -timeout 60s -v -run '^TestHardeningContracts$' ./agent -args -hardening-budget
```

The 60-second timeout bounds a hung contract well under Go's ten-minute
default. `-hardening-budget` makes the aggregate assert its 500 ms wall-clock
budget, cleanup included. Only this isolated run passes it; the
repository-wide race pass runs the same contracts without it, because there the
aggregate competes with every other package's tests for disk and CPU.

Isolation reduces that contention but does not remove it. Isolated outliers so
far concentrate in one contract, ZT-603 (MCP catalog trust), which makes
durable pin writes and opens loopback MCP sessions; a local experiment slowed
it more under write-and-fsync load than under CPU load (measurements in PR
#604). When a budget failure's `-v` output shows ZT-603 dominating, suspect
machine load, especially disk, before a regression.

The script also requires the run to log `budget enforced: true`. Renaming the
flag in the test fails this run on an undefined flag; dropping it from the
script fails `scripts/test-ci-local`, which pins the exact command, and fails
the log check; deleting the budget block fails the log check. Deleting or
inverting only the assertion remains a review responsibility.

### Containment inventory check (#452)

After the aggregate passes, every mode (`security` included) runs the
execution checker for the `linux-host` profile of
`docs/security/containment-inventory.json`:

```bash
go run ./internal/containmentinv/check -inventory docs/security/containment-inventory.json -profile linux-host -evidence-out "${TMPDIR:-/tmp}/containment-evidence.json"
```

The checker is the single runner of `TestContainment` in this phase, and of
every other test leaf a row of the inventory makes mandatory on the profile. It
groups those refs by package and runs each package once with
`go test -count=1 -json -timeout <profile timeout> [-race] [-tags ...] -run <pattern>`,
where the pattern anchors and quotes each `/`-separated component of the
leaf names. It decides from the complete event stream: every cited leaf needs a
`run` and a terminal `pass` event, the package needs its own `pass`, and
`go test` must exit 0. A missing leaf, a skip, a malformed or truncated stream,
text build output, a build failure or a timeout fails the profile. `linux-host`
allows no skip; other profiles allow only their enumerated
`<leaf>=capability:<cap>` entries, matched against the reason the test logs
when it skips. A failure prints the failed tests' output and
`containment inventory check failed (profile linux-host)`, and ends `ci-local`
before formatting. The checker's own tests (`internal/containmentinv`) feed it
recorded event streams; no ordinary package test launches `go test`.

Each profile's `-timeout` lives in the inventory: 15 s, about four times the
slowest package run measured (`./cmd/golem`, 3.6 s wall on an M3 Max).

Each run writes evidence JSON. It is CI output, never committed:

| Field | Meaning |
| --- | --- |
| `source_digest` | SHA-256 over the sorted `path\|sha256(content)` lines of every tracked file, hidden directories such as `.github` included: `git ls-files` in a checkout, every file but `.git` in the hook's export, so both digest the same tree alike. A symlink digests its target. |
| `tree`, `commit` | `GO_LLM_CI_TREE` and `GO_LLM_CI_COMMIT`, set by the pre-push hook and the CI checker steps |
| `dirty` | `true` when either variable is absent; `commit` is then `unknown` |
| `inventory_digest` | SHA-256 of the inventory file |
| `profile`, `flags` | the profile and the `go test` flags every package run shared |
| `executed` | `<package> <leaf>` for every leaf that passed, sorted; an allowed skip is not listed |

A manual run (`scripts/ci-local` on a working tree, or Docker without the
hook) has no forwarded identity, so it records `"commit": "unknown"` and
`"dirty": true` and is never treated as CI evidence.

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

The pre-push hook lives at `.githooks/pre-push`. Git passes it one
`<local ref> <local oid> <remote ref> <remote oid>` line per updated ref. The
hook peels each updated id to its tree and exports each distinct tree under a
fresh `mktemp -d` directory. It uses isolated Git metadata with export exclusions
and checkout conversions disabled: local, global and committed attributes cannot exclude tracked files,
transform their bytes or run host-side smudge filters. Executable bits and
symlinks are preserved. The source objects must be available locally; a partial
clone with missing objects fails the gate until those objects are fetched.

The hook snapshots `docker-compose.ci.yml` and `Dockerfile.ci` from its own
installation's repository into a separate, minimal build directory. From there
it runs the equivalent of:

```bash
GO_LLM_CI_SOURCE=/path/to/export GO_LLM_CI_TREE=<tree> GO_LLM_CI_COMMIT=<commit> docker compose --env-file /dev/null -f docker-compose.ci.yml run --build --rm ci ./scripts/ci-local --mode full
```

`GO_LLM_CI_TREE` and `GO_LLM_CI_COMMIT` name the gated tree and the pushed
commit; the Compose service passes both through, and the containment evidence
records them. An annotated tag forwards the commit it peels to, and when
several refs name one tree the first listed supplies the commit. A tag of a
bare tree has no commit, so its evidence records `commit: unknown`.

The container therefore sees exactly the pushed source tree: gitignored files (such as
the Go snapshots under `docs/superpowers/` in a main checkout), untracked files
and uncommitted source edits cannot change its input. `scripts/ci-local` and the
tests come from the pushed tree, but host-side Docker settings come only from
the trusted hook installation. Pushed Compose, Dockerfile, `.env` and
`.dockerignore` files are never loaded as host configuration. This matters when
forwarding another person's fetched branch: its Compose file must not be able
to grant itself host mounts or privileged execution. Review CI tooling changes
in the hook installation's checkout before using them; an absolute
`core.hooksPath` can point there from other worktrees. The minimal build directory
contains only the trusted Compose and Dockerfile, so changes that add Dockerfile
`COPY` inputs must update the hook as well. Manual Docker commands still use the
current checkout's configuration and source. A tree without the local-CI script
and module markers, such as an old tag, cannot pass the hook. Only ref tips are gated,
not the commits between them; a branch and its tag at one commit are gated
once; several trees are gated in the order git lists them, and the first
failure blocks the push. A push that only deletes remote refs, or updates
nothing, runs no gate. A line that is not git's four-field shape (two 40- or
64-hex ids, a `refs/` remote ref, a non-empty source) fails the push; a source
expression containing a newline cannot be parsed, so push such a source by its
id. Local `git replace` mappings are ignored: the gate checks the objects git
sends.

An interrupted push (Ctrl-C, or a closed terminal) removes the gate's container, named
`go-llm-prepush-<pid>`, and the export; without that, compose would exit and
leave the container running. If the removal fails (the Docker daemon is
unreachable, say), the hook prints the `docker rm -f` command to run.

The export lives under `$TMPDIR`, which the Docker host must be able to
bind-mount. Docker Desktop shares it by default; on another runtime, point
`TMPDIR` at a shared directory for the push.

The manual commands above gate the working tree, uncommitted edits included.
To gate a commit exactly as a push does:

```bash
printf 'manual %s refs/heads/manual 0000000000000000000000000000000000000000\n' "$(git rev-parse HEAD)" | .githooks/pre-push
```

Pushes fail locally if the image build, security contracts, lint, race tests,
or compile-smoke checks fail for a pushed tree.

The setup script only updates this clone's local Git config:

```bash
git config core.hooksPath .githooks
```

## GitHub Actions

The `CI` workflow runs on `pull_request` so protected branches receive the required `Lint & Test` status. It does not run on ordinary pushes. Its `Security contracts` step runs `scripts/ci-local --mode security`, the same discovery, exact-`PASS`, and skip-allowlist gate the pre-push hook runs, so a renamed, skipped, or hollowed-out aggregate cannot pass the required check by bypassing the local hook. That includes the containment inventory check for `linux-host`.

The workflow runs the full race-test suite on two toolchains, logging `go version`
immediately before each run:

| Check | Toolchain | Checks |
| --- | --- | --- |
| `Lint & Test` | `go-version-file: go.mod` (the `toolchain` directive) | CI/changelog self-tests and guards, lint, formatting, security contracts, race tests, cross-platform compile smoke (`cmd/golem` and `cmd/go-llm-mcp` builds for every release target, plus `go vet ./...` for Windows, FreeBSD and darwin amd64/arm64, all cgo-off), and GoReleaser config validation |
| `Test (oldest supported Go)` | `actions/setup-go` resolves the `go-version: '1.N.x'` query in `ci.yml` | `go test -race ./...` only |
| `Windows SQLite URIs` | `go-version-file: go.mod` on `windows-latest` | With `CGO_ENABLED=0`: `go test -count=1 -p 1 -v` (one package at a time, so the lock-budget timing tests are not starved) on `./internal/sqlitedsn ./internal/sqlitetest ./transcript ./memory ./provider ./rag`, covering native Windows path resolution, SQLite URI parsing, the Windows VFS and each store opener's own tests (concurrent first opens, fail-closed WAL, sidecars), which the Linux compile smoke cannot exercise; then the workspace identity and undo tests in `./agent/tools` and `./cmd/golem` (#658). `-v` keeps the lock-holder tests' logged timings visible |

The compatibility job sets `GOTOOLCHAIN=local` for both its version log and tests,
so the newer `toolchain` directive cannot silently replace the compiler selected
by setup-go. `1.N.x` is a setup-go version query, not a concrete toolchain name.
The current job retains the exact required status name `Lint & Test`; sandbox
jobs and release tooling continue to use the toolchain declared in `go.mod`.

The compatibility lane tests the oldest Go the module accepts: the minor of the
`go` directive in `go.mod`. While that matches the `toolchain` line it only adds
the latest patch release; once `toolchain` moves to a newer minor it is the only
lane that builds with the floor. `scripts/test-ci-local` fails when the lane and
the `go` directive drift apart or when `GOTOOLCHAIN: local` is removed, so move
the lane in the same change that moves the floor. Keep the floor on a release
inside the [Go support window](https://go.dev/doc/devel/release#policy) (each
major release is supported until two newer major releases exist). The check name
carries no version, so branch protection keeps requiring it across bumps.
A cross-version contract or golden-vector failure is a toolchain dependency to
investigate before release; do not hide it by upgrading the compatibility run.

The macOS compile-smoke workflow also runs on pull requests to `develop` and `main`, providing the required native-Darwin status and real Seatbelt confinement coverage. It remains available as a manual fallback through `workflow_dispatch`. Local Docker CI is the blocking path before pushes during normal development.

The Darwin background-exec selector also runs the #481 exit-observer
regressions (`TestProcessExitObserverAlreadyExitedChildRecovers` and
`TestProcessExitObserverRegistrationFailsClosed`); `scripts/test-ci-local` pins
that selector alongside the Seatbelt ones.

### Containment inventory profiles (#452)

Each inventory profile runs the containment checker in the job that has its
platform and capabilities:

| Profile | Runs in | Notes |
| --- | --- | --- |
| `linux-host` | `Lint & Test`, `Security contracts` step (through `ci-local`); the pre-push hook; manual `ci-local` | no skips |
| `bwrap` | `Linux Sandbox (bwrap)`, `Containment inventory (bwrap)` | `GO_LLM_REQUIRE_BWRAP=1` |
| `agentflow-integration` | `Agentflow compatibility`, inside both the installed and source steps | `-tags agentflow_integration`; a separate step would lack the venv or `AGENTFLOW_SRC`, and its required tests would skip |
| `windows` | `Windows SQLite URIs`, `Containment inventory (windows)` | exec scenarios skip with their enumerated capability reasons |
| `darwin-host` | `macOS Compile Smoke`, `Containment inventory (darwin-host)` | no skips |
| `seatbelt` | `macOS Compile Smoke`, `Containment inventory (seatbelt)` | `GO_LLM_REQUIRE_SEATBELT=1`, no race |

Every checker step sets `GO_LLM_CI_COMMIT` to `github.sha` and resolves
`GO_LLM_CI_TREE` in the step with `git rev-parse "${GITHUB_SHA}^{tree}"`. It
writes `$RUNNER_TEMP/containment-<profile>.json` (the agentflow steps add
`-installed` or `-source`), and each job uploads its files as a
`containment-*` artifact. `scripts/test-ci-local` pins every checker step and
upload, so removing one fails the self-test.

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

The CI image is based on `golang:1.27.1-alpine`, pinned to the patch release on the `toolchain` line in `go.mod`, which the GitHub lint job also uses. A floating minor tag can move to a newer patch whose export data the pinned golangci-lint cannot read, which fails the gate's lint stage on every branch. Bump the image tag, the `toolchain` line and `GOLANGCI_LINT_VERSION` in `Dockerfile.ci` together. It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
