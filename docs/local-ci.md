# Local CI

This repository uses a Docker-backed local CI runner so lint, race tests, and compile-smoke checks run before pushes without relying on GitHub Actions minutes.

## Quickstart

Enable the pre-push hook once per clone:

```bash
git config core.hooksPath .githooks
```

Run the same suite the hook runs:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

Run the faster pre-push subset manually when you do not need the compile-smoke pass:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

The Docker runner builds from `Dockerfile.ci`, mounts the repository at `/workspace`, and keeps named cache volumes for Go modules, Go build output, and golangci-lint data.

## Typical Workflow

1. Make code changes normally.
2. Run `scripts/ci-local --mode pre-push` for a faster host-side check while iterating, or use the Docker command above when you want the pinned CI toolchain.
3. Push the branch. The `.githooks/pre-push` hook automatically runs the Docker-backed `full` suite and blocks the push on failure.
4. GitHub Actions are manual-only; use the Actions tab workflow dispatch button only when you intentionally want a remote confirmation run.

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

## GitHub Actions

The GitHub Actions workflows are kept as manual fallback checks through `workflow_dispatch`, but they do not run automatically on `push` or `pull_request`. Local Docker CI is the blocking path for normal development.

## Notes

The CI image is based on `golang:1.25-alpine` to match `go.mod`. It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
