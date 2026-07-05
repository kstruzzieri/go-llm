# Document API-Key-Backed OpenAI-Compatible Providers (#208) Design

**Date:** 2026-07-04
**Issue:** [#208](https://github.com/kstruzzieri/go-llm/issues/208)
**Status:** Draft; reviewed with findings incorporated; awaiting approval before
implementation plan execution.

## Problem

Most of #208's original mechanics already shipped: `${ENV}` api_key expansion
(config/config.go:343-376, 407-426; PR #223), the README "Use a hosted API"
section (README.md:137-205), and the Bearer-header integration test
(internal/providerbootstrap). The current README already has a hosted-provider
example and capability guidance; the remaining gap is the **Golem-focused**
path in `docs/GETTING_STARTED.md`, whose opening still frames setup around
local models. A user still cannot tell there whether Golem needs a local LLM or
can run against a hosted key, which flags matter for a remote endpoint, and
which capability declarations the agent loop requires.

## Review Findings

1. Do not duplicate the README hosted-provider walkthrough. README already
   covers server-root `base_url`, `${ENV}` secrets, `defaults.agent`,
   `tool_call`, hosted embeddings, compatibility examples, and fallbacks. The
   GETTING_STARTED section should be the Golem-specific path and README should
   get only a cross-link.
2. Example JSON must use the real config schema: `models` is a map keyed by role
   (`config.Config.Models map[string]ModelConfig`), not an array. Any plan or
   doc example using `"models": [...]` is invalid.
3. `-reprobe` is not a top-level `golem` flag. It belongs to `golem models
   -reprobe`; the top-level remote-relevant flags are `-base-url`, `-no-probe`,
   and `-no-cap-probe`.
4. Capability-probe source references must point at the active probe path, not
   only `cmd/golem/capprobe.go`, which now owns the SQLite store path. Verify
   against `cmd/golem/main.go`, `provider/model_registry.go`,
   `fingerprint/probers/openaicompat.go`, `cmd/golem/models.go`, and
   `fingerprint/store.go`.

## Goals

1. A Golem-focused walkthrough: copy-pasteable `models.json` for a hosted
   openai-compat provider, wired to `defaults.agent`, with secret handling.
2. Answer "does Golem require a local LLM?" explicitly in the docs.
3. Document the remote-relevant golem flags and behaviors: `-base-url`,
   `-no-probe` (port scan is localhost-only — irrelevant/noise for remote),
   `-no-cap-probe`, and the cost implication of active tool-probing a paid API.
   Mention `golem models -probe-all` / `golem models -reprobe` only in the
   verification/provenance context.
4. Document required capability declarations for the agent loop
   (`chat`, `stream`, `tool_call` on the agent model; `embed` for RAG), and
   that `tool_call`/`thinking` are never derived.
5. Verify every documented claim against code while writing (base_url is
   server root and `/v1` is appended — provider/openaicompat/client.go:51-75;
   Bearer header only when key non-empty — client.go:42-48, 143-149; `${ENV}` fails
   fast on unset/empty var; expansion only on file-backed `Load`).

## Non-Goals

- No new code or config fields. Docs-only PR unless verification exposes an
  actual gap (a gap becomes its own issue, not scope creep here).
- No provider-by-provider compatibility matrix beyond the existing README
  table.
- No secrets-manager integration guidance beyond "env var, never a literal in
  a committed file".

## Design

### Placement

Extend `docs/GETTING_STARTED.md` with a new section "Running Golem against a
hosted API" (after the existing model-config section, lines 68–115 territory).
README gets one cross-link line from the existing hosted-API section; no
duplicated content. No new doc file — GETTING_STARTED is the established home
for golem setup, and #208's audience lands there.

### Walkthrough content

1. Local vs remote statement: Golem defaults to local (llama.cpp/Ollama) but
   runs fully against any OpenAI-compatible hosted endpoint via
   `api_format: "openai-compat"` + `api_key`.
2. Complete minimal `models.json`: one hosted provider
   (`base_url` = server root, explicit note that the client appends `/v1`),
   `api_key: "${PROVIDER_API_KEY}"`, timeout note (default 5m), one agent
   model with explicit `capabilities: ["chat", "stream", "tool_call"]`, one
   embedding model marked optional (RAG), `defaults.agent` /
   `defaults.embedding` wiring.
3. Secret handling: `${ENV}` expansion semantics — fails fast when unset or
   empty, never falls back to the literal, errors never echo secrets; keep
   keys out of committed models.json.
4. Golem flags for remote: `-no-probe` (skip localhost port scan),
   `-no-cap-probe` (active tool-capability probe hits the paid endpoint; cached
   in fingerprints.db), `golem models -probe-all` / `golem models -reprobe`
   for explicit provenance refresh, and `-base-url` interplay with the primary
   openai-compat agent provider.
5. Capability declarations: what the agent loop requires and why
   `tool_call` must be explicit (never derived from `type`); carve-down note
   for servers missing `/v1/completions`.
6. Verification runbook: `golem models` to confirm resolution/provenance,
   then a one-shot `golem -p "..."` smoke.

### Verification step (in-PR, not doc prose)

Before writing each claim, confirm against source; where a claim is testable
and untested, prefer citing the existing test (apikey integration test) over
adding new ones. Manual smoke against a real hosted endpoint is Keith's call
at review time (needs a key).

## Testing

- Docs-only: `env -u GOROOT go test ./...` still green (no code change).
- Example JSON in the walkthrough validated by feeding it to `config.Load` in
  a throwaway test or `go run` snippet during development (not committed) to
  guarantee copy-paste correctness — field names, enum values, defaults
  section shape.
