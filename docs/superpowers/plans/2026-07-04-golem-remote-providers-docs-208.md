# Document API-Key-Backed OpenAI-Compatible Providers (#208) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Golem-focused documentation for running against a hosted OpenAI-compatible endpoint with an API key: copy-pasteable config, secret handling, remote-relevant flags, required capability declarations.

**Architecture:** Docs-only PR. Extend `docs/GETTING_STARTED.md` with a "Running Golem against a hosted API" section; one cross-link line in README. README already has the general hosted-provider walkthrough, so do not duplicate it. Every claim verified against source before it is written; the example JSON is proven loadable by a throwaway `config.Load` check during development.

**Tech Stack:** Markdown. Spec: `docs/superpowers/specs/2026-07-04-golem-remote-providers-docs-208-design.md`.

**Session rules:**
- Branch AFTER #220 ships (sequential): `git worktree add ../go-llm-208 -b docs/golem-remote-providers-208 develop` (or reuse an in-place feature branch off fresh develop — docs-only, no hook problem either way; if a linked worktree is used, native gate + `git push --no-verify`).
- `env -u GOROOT go ...` for every go command.
- No emojis in commits or the PR.
- First commit: `git add -f docs/superpowers/specs/2026-07-04-golem-remote-providers-docs-208-design.md docs/superpowers/plans/2026-07-04-golem-remote-providers-docs-208.md`, message `docs(spec): remote-provider docs design and plan (#208)`.

---

### Task 1: Claim verification checklist

**Files:** none modified — this task produces verified facts for Task 2.

- [ ] **Step 1: Verify each claim against source, recording file:line**

| Claim to document | Verify at |
| --- | --- |
| `docs/GETTING_STARTED.md` still frames setup as local-only and needs the Golem-specific remote path | `docs/GETTING_STARTED.md:3-15, 68-117` |
| README already has the general hosted-provider section; only add a cross-link, no duplicate walkthrough | `README.md:137-205` |
| `base_url` is server root; client appends `/v1/...` | `provider/openaicompat/client.go:51-75`, endpoint callers in `provider/openaicompat/openaicompat.go` |
| Bearer header sent only when api_key non-empty | `provider/openaicompat/client.go:42-48, 143-149` |
| `${ENV}` expansion: fails fast on unset/empty, never falls back to literal, errors never echo the secret | `config/config.go:311-335, 343-376, 407-426` |
| Expansion happens only on file-backed `Load`, not programmatic Config | `config/config.go:311-335` call site |
| Timeout default 5m when omitted | `config/config.go:429-449` |
| `api_format` defaults to `"ollama"` when omitted (so remote configs MUST set `openai-compat`) | `config/config.go:429-449` |
| Config schema uses `models` as a role-keyed map, not an array | `config/config.go:79-84`; compare existing `models.json` |
| Agent model needs explicit `capabilities: ["chat","stream","tool_call"]`; `tool_call` never derived | `config/config.go:139-147`, `provider/catalog.go:187-203`, `cmd/golem/modelcaller.go:305-363` |
| `-no-probe` skips loopback port discovery only; remote/non-loopback URLs are never scanned | `cmd/golem/main.go:62-67`, `cmd/golem/backend_resolve.go:102-140` |
| `-base-url` overrides the primary openai-compat agent provider and must be server root without `/v1` | `cmd/golem/main.go:62-67`, `cmd/golem/backend_resolve.go:36-43, 102-132` |
| Cap probe can hit the endpoint for undeclared models; cached in fingerprints.db; `-no-cap-probe` disables top-level probing | `cmd/golem/main.go:288-321`, `provider/model_registry.go:290-425`, `fingerprint/probers/openaicompat.go:172-254`, `cmd/golem/capprobe.go:48-57`, `fingerprint/store.go:340-407` |
| `golem models -probe-all` and `golem models -reprobe` are subcommand flags for explicit probe/provenance refresh; `-reprobe` is not top-level `golem` | `cmd/golem/models.go:50-58, 189-198` |
| Bearer integration already tested | `internal/providerbootstrap/apikey_integration_test.go` |

Expected: every row confirmed or corrected. A WRONG claim here becomes a doc correction, not silent drift; a MISSING behavior becomes a new issue, not scope creep.

- [ ] **Step 2: Prove the example config loads**

Write the Task 2 example JSON to a scratch file:

```json
{
  "providers": {
    "hosted": {
      "base_url": "https://api.openai.com",
      "api_format": "openai-compat",
      "api_key": "${HOSTED_API_KEY}"
    }
  },
  "models": {
    "agent": {
      "name": "gpt-4o",
      "provider": "hosted",
      "type": "dense",
      "context_window": 128000,
      "capabilities": ["chat", "stream", "tool_call"]
    }
  },
  "defaults": {
    "agent": "agent"
  }
}
```

Prove loadability with a throwaway test (never run golem against a real endpoint for this):

```go
// temporary, NOT committed: place in config/ as loadcheck_x_test.go,
// run once, delete.
package config_test

import (
	"os"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
)

func TestRemoteExampleLoads(t *testing.T) {
	os.Setenv("HOSTED_API_KEY", "dummy")
	defer os.Unsetenv("HOSTED_API_KEY")
	cfg, err := config.Load("/tmp/golem-remote-example.json")
	if err != nil {
		t.Fatalf("example from docs does not load: %v", err)
	}
	if cfg.Providers["hosted"].APIKey != "dummy" {
		t.Fatalf("api_key not expanded")
	}
	if cfg.Defaults["agent"] != "agent" || cfg.Models["agent"].Name != "gpt-4o" {
		t.Fatalf("agent default/model wiring mismatch")
	}
}
```

Run: `HOSTED_API_KEY=dummy env -u GOROOT go test ./config/ -run TestRemoteExampleLoads -v` — confirm PASS, then delete the temp test file. IMPORTANT: first diff the example's field names against `config/config.go` structs and the real `models.json` — in particular whether the top level uses `"providers"` as a map and what the defaults section is actually called (`defaults.agent` vs another key). Fix the example to match reality, not the other way around.

---

### Task 2: Write the GETTING_STARTED section

**Files:**
- Modify: `docs/GETTING_STARTED.md` (new section after the model-config material around lines 68-115)
- Modify: `README.md` (one cross-link line in the existing "Use a hosted API" section, :137-150)

- [ ] **Step 1: Write "Running Golem against a hosted API"**

Content order (prose per house style — plain, terse, no emojis):

1. Direct answer up front: Golem does not require a local LLM; any OpenAI-compatible endpoint works via `api_format: "openai-compat"` + `api_key`.
2. The verified example config from Task 1, verbatim, with the three inline caveats: `base_url` is the server root (client appends `/v1`); `api_format` must be set explicitly (default is `ollama`); `capabilities` must declare `tool_call` explicitly (never derived) plus `chat` and `stream` for the agent loop.
3. Secret handling: `${ENV_VAR}` expansion semantics (fails fast when unset/empty, no literal fallback, errors never contain the secret); never commit a literal key; expansion applies only to file-loaded configs.
4. Remote-relevant flags: `-no-probe` (loopback port scan is pointless against remote and remote/non-loopback URLs are never scanned), `-no-cap-probe` (active tool-capability probe sends real requests to the paid endpoint; verdicts cached in fingerprints.db), `golem models -probe-all` / `golem models -reprobe` (explicit probe/provenance refresh), `-base-url` (overrides the primary openai-compat agent provider's base URL; server root, no `/v1`).
5. Optional RAG note: `defaults.embedding` + a hosted embedding model with `"embed"`; otherwise golem runs with file tools only.
6. Verification runbook: `golem models` to confirm resolution and capability provenance, then `HOSTED_API_KEY=... golem -p "say hi"` smoke.
7. Timeout note: provider timeout defaults to 5m; set `"timeout"` for slower hosted models if needed.
8. Update the top prerequisites/opening language so GETTING_STARTED no longer implies a local model backend is mandatory.

- [ ] **Step 2: README cross-link**

One line at the end of the existing hosted-API section: see docs/GETTING_STARTED.md "Running Golem against a hosted API" for the Golem-specific walkthrough. No content duplication.

- [ ] **Step 3: Full gate + commit**

```bash
env -u GOROOT go test ./...
git add docs/GETTING_STARTED.md README.md
git commit -m "docs(golem): hosted openai-compat provider walkthrough (#208)"
git push --no-verify -u origin docs/golem-remote-providers-208
```

Expected: tests PASS (no code changed).

- [ ] **Step 4: PR**

`gh pr create` targeting develop, plain-text body: what was documented, the claim-verification table from Task 1 (file:line evidence), note that the example JSON was load-verified. Mention manual live-endpoint smoke is available to Keith at review (needs a real key). No emojis.

---

## Plan Self-Review Notes

- Spec coverage: goals 1-5 map to Task 2 items 1-7 (walkthrough), Task 1 (verification), Task 2 Step 2 (cross-link). Non-goals respected: zero code changes; gaps found in Task 1 become filed issues.
- Review finding incorporated: the original scratch example used `"models": [...]`, but the real schema is a role-keyed map. The plan now uses `"models": {"agent": ...}` with `defaults.agent: "agent"`.
- Review finding incorporated: cap-probe verification targets now point at the current active-probe path; `cmd/golem/capprobe.go` alone is only the store/cache path.
- The example JSON's exact top-level schema (providers map key name, defaults section shape) is deliberately verified in Task 1 before being committed to prose.
