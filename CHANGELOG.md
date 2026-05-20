# Changelog

All notable changes to `go-llm` are documented here. Downstream consumers
(Firn IDE, Flux ML, Quantum Trader) should consult this before any
`go get -u github.com/kstruzzieri/go-llm`.

## Unreleased

### Breaking for consumers of `models.json` defaults

The root `models.json` lineup has been retargeted for 2026 model releases.
**Consumers that read `models.json` via `config.Load` and expect specific
model names will observe different models at runtime.**

| Role          | Was                    | Now                       |
|---------------|------------------------|---------------------------|
| `general`     | `qwen3.5:27b`          | `gemma4:31b`              |
| `analysis`    | `qwen3.5:27b`          | `gemma4:31b`              |
| `fast`        | `qwen3.5:35b-a3b`      | `qwen3.6:35b-a3b`         |
| `agent` (new) | —                      | `gemma4:31b`              |
| `coding`      | `qwen3-coder-next:latest` | unchanged              |
| `lightweight` | `qwen3:8b`             | unchanged                 |
| `embedding`   | `qwen3-embedding:8b`   | unchanged                 |

**Before upgrading, consumers MUST:**

1. `ollama pull gemma4:31b` and `ollama pull qwen3.6:35b-a3b` on every
   deployment host. Absent pulls will produce 404s from Ollama at first
   use (circuit breaker will trip and fall back per the `fallbacks`
   chain, so behavior degrades rather than crashes — but quality will
   drop).
2. If you pinned to `qwen3.5:27b` or `qwen3.5:35b-a3b` in app-level
   code, that pinning is unaffected by this change (the library only
   reads from `models.json`). You can keep pinning explicitly.
3. Review `docs/llm/recommendation.md` for the rationale and the
   GLM-5.1 parallel experiment plan.

### Added

- **`agent` role** in `models.json` defaults, pointing at `gemma4:31b`.
- **`agent` / `tool-use` weight profiles** in
  `provider/router_score.go:defaultWeightProfiles` so the new role
  gets meaningful scoring instead of falling back to `chat`.
- **`ollama.ChatRequest.KeepAlive`** field exposing Ollama's
  `keep_alive` directive. Useful for benchmark runs that want a model
  to stay warm longer than the 5-minute default.
- **`cmd/llm-bench`** — scaffold for model A/B comparison on captured
  traces. See `docs/llm/benchmark-plan.md`.
- **`qwen3.6`, `qwen3-coder-next`, `gemma4`** families in
  `provider/catalog.json`.
- **`docs/llm/`** — 2026 local-LLM analysis, three candidate setups,
  recommendation, and benchmark plan.

### Changed

- Catalog: `qwen3-coder-next` gains a `latest` variant alias kept in
  sync with `80b` (guarded by test).

### Router: provider-instance pinning (#81)

- **`provider.RoutingRequest.Provider`** — new optional `string` field that
  hard-scopes routing to a specific provider *instance* (the config-time
  name, e.g. `ollama-local-a`, `vllm-prod-1`). Acts as a pre-score filter:
  empty `Model` + `Provider` scopes `Recommend`; unqualified `Model` +
  `Provider` pins `ModelKey{Provider, Model}` via `Lookup`; qualified
  `Model` (`provider/model`) + non-empty `Provider` must agree on identity
  or `Router.Route` returns the new `ErrProviderMismatch` sentinel before
  candidate resolution. `PreferredChain` is authoritative when set —
  chain selectors carry their own provider identity and the per-request
  `Provider` hint is ignored under chain routing.
- **`provider.ChatRequest.Provider`, `GenerateRequest.Provider`,
  `EmbedRequest.Provider`** — optional `string` fields (`json:"provider,omitempty"`)
  forwarded by `Router.Chat / ChatStream / Generate / GenerateStream / Embed`
  into `RoutingRequest.Provider`. Router selection metadata only; not
  forwarded to the concrete provider's execution call (the provider already
  knows its own identity).
- **`provider.RecommendOpts.RestrictToProvider`** — single-string hard
  filter on the recommendation path. Distinct from the still-unused soft
  `PreferredProviders`. An unknown provider name surfaces as a provider
  resolution error rather than degrading to a silent empty result.
- **Sticky-key derivation** — `RoutingRequest.Provider` participates in
  `StickyKey` so two scoped requests with identical affinity/model/use-case
  keep independent sticky entries. Empty `Provider` produces byte-identical
  keys to pre-change behavior, preserving existing affinity warmth.
- **JSON wire** — unset `provider` fields are omitted on the wire
  (`omitempty`). Keyed Go struct literals are additive-compatible.
  Unkeyed composite literals of the exported request structs would need
  positional adjustment — none observed in the in-tree consumers.
