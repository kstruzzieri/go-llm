# Local CI

This repository uses a Docker-backed local CI runner so lint, race tests, and compile-smoke checks run before pushes without relying on GitHub Actions minutes.

## Quickstart

Enable the pre-push hook once per clone:

```bash
scripts/setup-local-ci
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

`scripts/test-ci-local` is a hermetic regression harness for this root-discovery behavior. It stubs `go` and `golangci-lint`, needs no toolchain or Docker, and covers outside-CWD invocation, unusable worktree `.git` metadata, hostile `CDPATH`, the exact gate commands per mode, and invalid or wrong-module layouts. The `CI` workflow runs it on every pull request.

## Typical Workflow

1. Make code changes normally.
2. Run `scripts/ci-local --mode pre-push` for a faster host-side check while iterating, or use the Docker command above when you want the pinned CI toolchain.
3. Push the branch. The `.githooks/pre-push` hook automatically runs the Docker-backed `full` suite and blocks the push on failure.
4. GitHub runs the required `Lint & Test` workflow on PRs to satisfy branch protection. Push-triggered Actions and macOS smoke are disabled unless manually dispatched.

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

- `golangci-lint run`
- `go test -race ./...`

`full` runs the pre-push suite plus:

- `go test -run '^$' ./...`

## Git Hook

The pre-push hook lives at `.githooks/pre-push` and runs the full Docker-backed suite:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

That means pushes fail locally if lint, race tests, or compile-smoke checks fail.

The setup script only updates this clone's local Git config:

```bash
git config core.hooksPath .githooks
```

## GitHub Actions

The `CI` workflow still runs on `pull_request` so the protected `develop` branch receives the required `Lint & Test` status. It does not run on ordinary pushes.

The macOS compile-smoke workflow is kept as a manual fallback check through `workflow_dispatch`. Local Docker CI is the blocking path before pushes during normal development.

## Notes

The CI image is based on `golang:1.25-alpine` to match `go.mod`. It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
