# Local CI

This repository uses a Docker-backed local CI runner so lint and race tests run before pushes without relying on GitHub Actions minutes.

## Quickstart

Enable the pre-push hook once per clone:

```bash
git config core.hooksPath .githooks
```

Run the same suite the hook runs:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

Run the full local suite before opening or updating a PR:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode full
```

The Docker runner builds from `Dockerfile.ci`, mounts the repository at `/workspace`, and keeps named cache volumes for Go modules, Go build output, and golangci-lint data.

## Typical Workflow

1. Make code changes normally.
2. Run `scripts/ci-local --mode pre-push` for a host-side check, or use the Docker command above when you want the pinned CI toolchain.
3. Push the branch. The `.githooks/pre-push` hook automatically runs the Docker-backed `pre-push` suite and blocks the push on failure.
4. Run `scripts/ci-local --mode full` or its Docker equivalent before PR handoff when broad compile behavior may have changed.

## Command Contract

Run the pre-push suite directly on the host:

```bash
scripts/ci-local --mode pre-push
```

Run the full suite directly on the host:

```bash
scripts/ci-local --mode full
```

Run the pre-push suite inside Docker:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

Run the full suite inside Docker:

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

The pre-push hook lives at `.githooks/pre-push` and runs:

```bash
docker compose -f docker-compose.ci.yml run --rm ci ./scripts/ci-local --mode pre-push
```

That means pushes fail fast if lint or race tests fail locally.

## Notes

The CI image is based on `golang:1.25-alpine` to match `go.mod`. It installs `build-base` and sets `CGO_ENABLED=1` because Go's race detector requires cgo support even though the module itself avoids cgo-only dependencies.
