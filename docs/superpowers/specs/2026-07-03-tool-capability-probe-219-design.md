# Tool-Capability Probe + Capability Discoverability (#219)

**Date:** 2026-07-03
**Issue:** [#219](https://github.com/kstruzzieri/go-llm/issues/219)
**Base:** develop@9c8b77c (post PR #265 / #218 merge)
**Status:** Reviewed design (findings folded; ready for implementation plan)

## Problem

`tool_call` is deliberately never derived from model `type`, so undeclared
openai-compat models cannot route as agent models even when they genuinely
support function calling. The issue text predates two fixes (#217 preflight
connectivity split, #184 openai-compat prober), so the remaining work was
re-scoped against the current tree.

### Root cause (differs from the issue's framing)

`internal/providerbootstrap/capabilities.go:67-81`: for openai-compat models
with **no explicit `capabilities`** in models.json, bootstrap installs the
type-DERIVED caps (`chat,generate,stream`) as the **highest-precedence
REPLACE capability override**. The registry merge (`provider/model_registry.go
merge()`) assembles caps as `catalog | fingerprint | runtime`, then the
override replaces the result wholesale. Consequences:

- The static catalog already stamps `tools` on qwen2.5/3/3.5/3.6,
  qwen3-coder-next, gemma3/4, llama3.x, phi3/4, mistral, mixtral,
  deepseek-coder-v2 — but the derived override **erases it** for every
  undeclared openai-compat model.
- Any probe result written into the fingerprint merge layer would be erased
  by the same override. The probe is dead on arrival unless this changes.

The derived override exists because openai-compat `/v1/models` carries no
capability data: without it, a catalog-miss model merges to zero caps and is
unroutable. It is a floor implemented as a ceiling.

## Design overview

Five parts, one PR:

1. **Floor/ceiling split** — registry gains a capability *floor* seam;
   derived caps stop erasing catalog/probe data.
2. **Tri-state capability probe cache** — fingerprint store schema v2,
   `capability_probes` table, separate from `fingerprint_profiles`.
3. **Active tool-call probe** — openaicompat prober extension (two-attempt
   protocol), passive ollama branch.
4. **Bounded-eager preflight + route-time lazy resolution** — probe only
   until one chain entry is proven capable; unknown fallbacks probed at
   route time, never silently skipped.
5. **Discoverability** — remediation hints in preflight warnings,
   `golem models` subcommand, catalog audit, docs.

Explicit `models.json` capabilities remain authoritative in BOTH directions:
a declared set without `tool_call` is a hard no; the probe never runs for
explicitly declared models.

## 1. Capability floor (registry seam)

- `ModelRegistry.SetCapabilityFloor(fn func(key ModelKey) []string)` —
  parallel to `SetCapabilityOverride`. Floor caps are parsed with
  `ParseCapsStrict` and OR-merged into the profile at the **lowest**
  precedence (alongside the static catalog layer), before fingerprint,
  runtime, and the explicit override.
- `providerbootstrap.buildCapabilityOverrides` splits:
  - explicit `capabilities` → REPLACE override (unchanged, cross-role
    conflict check unchanged, explicit-only);
  - derived caps for undeclared openai-compat models → **floor** (new
    `buildCapabilityFloors`), installed via `SetCapabilityFloor`.
- Same cache-invalidation/versioning treatment as `SetCapabilityOverride`
  (mirror ALL invariants: cache invalidation, override-version TOCTOU guard,
  zero/invalid-token rejection semantics — floors that fail strict parsing
  are dropped with the rejection hook fired, never zero the profile).
- Effect: undeclared `llamacpp/gemma4:31b` gets catalog `tools` → tool-capable
  with zero config and zero probe. The probe only matters for BYO aliases the
  catalog misses.

## 2. Tri-state probe cache (fingerprint store schema v2)

New table:

```sql
CREATE TABLE capability_probes (
  backend_id    TEXT NOT NULL,   -- fingerprint.NormalizeBackendID
  model_name    TEXT NOT NULL,
  capability    TEXT NOT NULL,   -- "tool_call" (schema supports future caps)
  state         TEXT NOT NULL,   -- "yes" | "no" | "inconclusive"
  model_digest  TEXT NOT NULL,   -- runtime digest, or key fallback (digestless)
  probe_version INTEGER NOT NULL,-- request-shape identity; bump => re-probe
  tested_at     INTEGER NOT NULL,
  expires_at    INTEGER,         -- NULL = does not expire
  PRIMARY KEY (backend_id, model_name, capability)
);
```

- **Separate from `fingerprint_profiles`**: capability-only results never
  masquerade as complete profiles; MCP's full profiler and its
  `NeedsFingerprint`/`IncompleteCapabilities` logic are untouched.
- `fingerprint.Store` interface gains `GetCapProbe`/`SaveCapProbe`/
  `DeleteCapProbes` (in-repo implementors only; noted as an interface
  extension). Migration v2 via the existing `fingerprint_schema_version`
  mechanism.
- Expiry policy:
  - `yes`: no expiry; invalidated by digest change, `probe_version` bump, or
    `golem models --reprobe`. A stale yes fails loudly at call time (agent
    tool call errors surface), so it is the safe sticky direction.
  - `no`: no expiry when digest-keyed; **7-day expiry when digestless**
    (openai-compat fallback keying) so a fixed server/template does not stay
    wedged until manual `--reprobe`. A wedged no silently blocks usage —
    the damaging direction — hence the TTL.
  - `inconclusive`: 24-hour expiry, then re-probe on next demand.
  - Rows whose `model_digest` mismatches the current runtime digest, or whose
    `probe_version` differs from the binary's, are treated as absent.
- Transient probe failures (network, timeout, 5xx, context cancel) are
  **never persisted** — no claim is recorded; the next demand re-probes.
- Merge integration: `buildProfile` gains a capability-probe input beside the
  fingerprint layer — a valid `yes` row ORs `CapToolCall` into the profile
  (below runtime, below explicit override). `no`/`inconclusive` rows add
  nothing to caps but are exposed to the resolver/preflight for state
  classification.

## 3. Probe mechanics

### openai-compat (active; the primary case)

At most two requests, non-streaming, `temperature 0`, `max_tokens 128`
(tool-call JSON needs room; a small cap truncates mid-call and produces a
false negative). Tool: no-arg `get_time` function; user prompt:
"Call the get_time tool to get the current time."

Attempt 1 — `tools` + `tool_choice: "required"`:

| Response | Verdict |
| --- | --- |
| 200, non-empty `tool_calls` | `yes` (persist) |
| 200, no `tool_calls` | `inconclusive` (persist, 24h TTL) — model ignored a forced tool choice |
| 400 / 422 | escalate to attempt 2 (server may reject `tool_choice`, not `tools`) |
| 401 / 403 / 404 / 405 / 429 | diagnostic, **not persisted** (auth/endpoint/rate problems say nothing about the model) |
| other 4xx/5xx / network / timeout | transient, not persisted |

Attempt 2 — `tools` only, no `tool_choice` (prompt-engineered elicitation):

| Response | Verdict |
| --- | --- |
| 200, non-empty `tool_calls` | `yes` (persist) |
| 200, no `tool_calls` | `inconclusive` (persist, 24h TTL) |
| 400 / 422 | `no` (persist) — the `tools` array itself is rejected (e.g. llama.cpp template without tool support) |
| other 4xx/5xx / network / timeout | transient, not persisted |

Probe context timeout ~30s (must absorb a llama-swap model load; distinct
from #218's 800ms port-scan probe). Implemented as a new method on
`fingerprint/probers.OpenAICompatProber` (e.g. `ProbeToolCall`), exposed via
an optional interface so `fingerprint.ModelProber` itself is unchanged.

### ollama (passive; cheap parity)

Query `/api/show` (no model load):

- response has a `capabilities` array containing `tools` → `yes`;
- response has a `capabilities` array WITHOUT `tools` → `no`;
- response has **no capabilities array** (older Ollama) → `inconclusive`
  (never persist `no` on missing data).

## 4. Resolution policy

New registry method (name indicative):

```go
func (r *ModelRegistry) ResolveToolCall(ctx context.Context, key ModelKey) (CapProbeState, error)
```

Singleflight-deduplicated. Order:

1. Explicit declaration exists for key → authoritative (`yes`/`no` per the
   declared set), no probe.
2. Merged profile already has `CapToolCall` (catalog/floor/runtime) → `yes`,
   no probe.
3. Valid cache row (digest + probe_version match, not expired) → its state.
4. Active probe (per §3) → persist per policy → invalidate the profile cache
   entry so the next `Lookup` re-merges with the new row.
5. Probing disabled (`-no-cap-probe`, or no prober available for the
   provider) → `unknown` without probing.

`Lookup` itself **never probes**. Active resolution happens only at the named
call sites below. Golem never wires the full fingerprint profiler.

### Preflight (bounded eager)

Restructure `preflightToolCapable` / `evalChainEntry` (cmd/golem):

- Walk the chain in order. Per entry: registry lookup (connectivity
  classification from #217 unchanged); if tool-capable → capable, **stop
  probing** further entries.
- Entry resolved but lacking `tool_call`: call `ResolveToolCall` — but only
  while no capable entry has been found yet. `yes` → capable, stop. `no` /
  `inconclusive` → warning with remediation hint, continue.
- After the first capable entry: remaining entries are classified passively
  (declared/cached state only, no live probes); unknowns are reported as
  "capability unknown; probed on first use" info lines, confirmed-no entries
  as warnings.
- Zero capable entries after exhaustion → fatal, inlining per-entry
  diagnostics (connectivity, probe outcomes, remediation lines).
- Empty chain (recommend mode): see Recommend integration below.

### Route-time (lazy; unknown never silently skipped)

- Chain candidates: in the Router's candidate-resolution path, before
  feedback-snapshot reads and before scoring, a candidate that would fail the
  `RequiredCaps` gate only because `CapToolCall` is missing and whose probe
  state is unknown → `ResolveToolCall(ctx, key)` first, then re-lookup or
  refresh that candidate and re-evaluate the gate. Only when a resolver is
  enabled. Do not put active probe I/O in `scoreCandidate`/`scoreAll`; those
  stay pure scoring functions.
- **Recommend paths** (`provider/model_registry.go` Recommend;
  consumed by `router.go` empty-model routing and `router_chain.go` chain
  tail; also the preflight empty-chain branch): `Recommend` filters by
  `RequiredCaps` before the router sees candidates, so lazy probing must be
  applied around it. When the resolver is enabled and `RequiredCaps` includes
  `CapToolCall`: recommend with `RequiredCaps &^ CapToolCall`, resolve
  `tool_call` for candidates missing it, then filter to the full mask.
  `Lookup`/`Recommend` themselves stay pure.

## 5. Golem wiring

- Fingerprint store: `$XDG_DATA_HOME/golem/fingerprints.db` (via the
  existing `dataDirBase`, beside memories.db). Open failure → warning +
  per-run in-memory cap-probe cache (probing still works; persistence
  degrades). Contents are non-sensitive (model names, normalized backend
  hosts — userinfo already stripped by `NormalizeBackendID`); no
  hardened-open treatment.
- `providerbootstrap.Options` gains capability-probe wiring (store handle +
  tool-prober factory reusing the existing `proberFactory` provider
  plumbing), distinct from the full-profiling `FingerprintStore` path used
  by MCP. Bootstrap installs the resolver on the registry when configured.
- New flag `-no-cap-probe`: disables active probing at preflight AND route
  time (resolver returns unknown). The floor fix still applies, so catalog
  `tool_call` for undeclared openai-compat models survives even with this flag;
  `-no-cap-probe` disables only active live probes, not the floor/ceiling bug
  fix. Unknown catalog-miss models behave as not-capable with a remediation
  hint. The explicit-config path is unchanged. Named distinctly from #218's
  `-no-probe` (port scan).
- `-p` one-shot mode: probing stays enabled (an undeclared model must work in
  one-shot too); result is cached like any other run.

## 6. Discoverability (secondary items, all in-slice)

- **Remediation hint**: capability warnings gain the exact fix line —
  `add "capabilities": ["chat","generate","stream","tool_call"] to the
  <role> model entry in models.json` — with a tail that varies by state:
  declared-without-tool_call ("declared capabilities lack tool_call"),
  probed-no ("model did not produce a tool call when probed"), inconclusive
  ("probe was inconclusive; declare capabilities to override").
- **`golem models` subcommand** (pattern: `golem index`): lists each
  agent-chain entry with resolved capabilities, per-cap provenance
  (explicit / catalog / floor / probed(+age) / runtime), probe state for
  `tool_call`, and flags entries missing it. Flags:
  - `--probe-all`: actively probe every chain entry now (full-chain
    certification; ignores bounded-eager stopping; still respects explicit
    declarations).
  - `--reprobe`: delete cached probe rows for chain models, then probe
    non-explicit entries.
  - Shared-key visibility: when one role declares explicit caps and another
    role references the same provider/model undeclared, the key-wide explicit
    override wins over floor/catalog/probe — the listing must surface this
    ("explicit, declared by role <X>").
  - Provenance comes from a **narrow exported explain helper** on
    `ModelRegistry` scoped to what `golem models` needs (tool_call-focused;
    no general public CapProvenance API).
- **Catalog**: audit pass only — `tools` is already stamped on all major
  families; gap-fill any misses found (e.g. verify qwen3-coder-next
  variants).
- **Docs**: BYO/models doc — `capabilities` semantics (explicit = replace,
  derived = floor, cross-role consistency rule), tool-capable bundled-model
  table, probe behavior, cache location, `-no-cap-probe`, `golem models`.

## 7. Error handling / degradation summary

- Probe failure/timeout: never fatal, never persisted; preflight warns,
  routing proceeds with remaining candidates.
- Store open/migration failure: warn, degrade to in-memory cache for the run.
- Floor parse failure (non-canonical tokens): dropped + rejection hook, never
  zeroes caps (mirrors override corner-case handling).
- `-no-cap-probe`: disables active probes only; floor/catalog capability
  merging still fixes undeclared openai-compat catalog hits.
- Preflight remains non-fatal when ≥1 chain entry is capable (warnings only),
  fatal with full inlined diagnostics when none is.

## 8. Testing

Table-driven throughout; mock HTTP servers; no live backend in unit tests.

- Merge precedence: floor OR-merges below catalog/fingerprint/runtime;
  explicit override still replaces (probe-yes + explicit-no → no wins;
  floor + catalog tools survive for undeclared openai-compat).
- Floor invariants mirror override: cache invalidation on SetCapabilityFloor,
  version guard, invalid-token rejection (hook fired, caps preserved).
- Probe classification matrix: both attempts × (200+calls, 200-empty,
  400/422, 401/403/404/405/429, 5xx, timeout) — verdict + persistence
  asserted for each cell.
- Ollama passive branch: capabilities-with-tools / capabilities-without /
  missing-array → yes / no / inconclusive.
- Store v2: migration from v1, TTL expiry (inconclusive 24h, digestless-no
  7d), digest mismatch → treated absent, probe_version bump → treated
  absent, delete-for-reprobe.
- Resolver: order-of-precedence (explicit beats cache beats probe),
  singleflight dedup, profile-cache invalidation after persist.
- Preflight bounded-eager: stop-after-first-capable (probe count asserted via
  fake resolver), passive classification after stop, fatal path inlines
  probe outcomes + remediation lines, `-no-cap-probe` performs no active
  probes while still preserving floor/catalog `tool_call` (#217 tests keep
  passing).
- Recommend integration: RequiredCaps-with-tool_call recommend path resolves
  then filters; empty-chain preflight benefits; Lookup/Recommend purity
  (no probe side effects) asserted.
- Route-time: unknown candidate probed before gate rejection and before
  scoring/feedback reads; confirmed-no candidate skipped without probe.
- No-probe flag regression: undeclared catalog-known openai-compat model keeps
  `tool_call` through floor/catalog even with `-no-cap-probe`; undeclared
  catalog-miss model remains not-capable without active probing.
- Shared model key: role A explicit + role B undeclared same key → explicit
  wins; `golem models` output surfaces the declaring role.
- `golem models`: provenance rendering, --probe-all probes all entries,
  --reprobe busts rows first.
- Integration (build tag, live llama.cpp): probe yes on a tool-capable
  model; probe no on a non-tool template.

## 9. Out of scope

- Full profiling in golem (future `golem models --profile` when routing
  consumes latency metrics).
- `thinking` / `insert` probes (same seam; separate issues).
- Launcher port-handoff (#218 slice B), GO_LLM_SCAN_PORTS.
- Probe-result sharing between MCP and golem beyond the common store schema.
- Public general-purpose CapProvenance API.

## Design decisions log

1. **Floor seam over alternatives** (derived caps via prober hint →
   fingerprint layer: fails storeless, MCP regression; dynamic override
   closure consulting probe cache: duplicates merge logic). Floor fixes
   catalog-for-openai-compat as a side effect; invariant lives at the
   provider layer. (User-approved.)
2. **Two-attempt probe protocol** — user finding on 4xx→no being too broad:
   `tool_choice: required` first, escalate on 400/422, persist `no` only when
   the `tools` array itself is rejected; auth/endpoint/rate 4xx never
   persisted.
3. **Bounded-eager + route-lazy** (user policy): startup proves one capable
   entry then stops; unknown must never mean silently skip.
4. **Capability resolver, not profiler-with-benchmarks-off** (user): separate
   cache table + completeness isolation from MCP's full profiles.
5. **Sticky-yes / expiring-no asymmetry**: stale yes fails loudly, wedged no
   blocks silently → digestless no gets 7d TTL.
6. **`golem models` name** (over `doctor`): matches scope selection; `doctor`
   reserved for future broader diagnostics.
