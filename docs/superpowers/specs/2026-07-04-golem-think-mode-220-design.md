# Golem User-Facing Reasoning / Think-Mode Setting (#220) Design

**Date:** 2026-07-04
**Issue:** [#220](https://github.com/kstruzzieri/go-llm/issues/220)
**Status:** Draft; awaiting approval before implementation plan execution.

## Problem

go-llm already models reasoning: `CapThinking`, per-family `ThinkMode`
(`none`/`always`/`toggle`/`auto`) + `ThinkTags` from the static catalog, and
captured reasoning (`ChatResponse.Thinking`, `Usage.ReasoningTokens`). But the
control surface has three gaps:

1. **`ModelOptions.Think` is parse-side only.** `shouldExtractThinking`
   (provider/ollama.go:104, provider/openaicompat/openaicompat.go:658) gates
   client-side tag extraction, but the wire request is identical either way.
   The native `ollama` client already has a `Think *bool` wire field
   (ollama/types.go:55) that the provider never sets. The openaicompat
   `chatRequest` (provider/openaicompat/types.go:11) has no reasoning field at
   all. Toggling "think" today changes what the user sees, not what the model
   does.
2. **Think configuration is catalog-static.** `ModelConfig` (config/config.go:59)
   has no `think_mode`/`think_tags`; the merge in
   `provider/model_registry.go` takes ThinkMode only from the static catalog or
   the family-name inference fallback. Capabilities gained a config REPLACE
   override (model_registry.go:1088–1134); think did not.
3. **Golem exposes no control and drops thinking on the floor.** No `-think`
   flag exists, `agent.Request` has no options passthrough
   (agent/orchestrator.go:65 builds `ChatRequest{Messages, Tools, Stream}`),
   and the orchestrator forwards only `c.Content` to `Observer.OnToken`
   (orchestrator.go:126) — `c.Thinking` is never rendered.

## Goals

1. `golem -think off|on|low|medium|high` drives actual model reasoning behavior
   on both backend paths for the selected route profile (Ollama wire `think`;
   llama.cpp/openai-compat `reasoning_effort` +
   `chat_template_kwargs.enable_thinking`).
2. Graceful no-op with a one-line startup notice when every configured agent
   candidate resolves and has effective ThinkMode `none`.
3. `models.json` per-model `think_mode` / `think_tags` override that REPLACES
   the catalog/inferred values, without a catalog change.
4. Captured thinking renders in the golem REPL visually distinct from the
   answer (dim/labeled), streaming, in both REPL and one-shot modes.
5. Unset flag and absent config keys produce byte-identical wire requests to
   today (zero-change default).

## Non-Goals

- No mid-session `/think` REPL command (flag is per-run).
- No think budgets (`ThinkBudget` exists; untouched).
- No `generate`/FIM-path think control (golem uses chat).
- No Ollama string effort levels ("low"/"medium"/"high" as wire strings for
  gpt-oss); the wire field stays `*bool`, effort maps to `true`.
- No change to `CapThinking` advertisement defaults on either provider.
- No thinking persistence in conversation storage beyond current behavior.

## Design

### 1. ModelOptions: one new field

```go
// provider/types.go
type ModelOptions struct {
    // ... existing fields, including Think *bool ...
    // ThinkEffort is an optional reasoning-effort hint: "low", "medium",
    // or "high". Empty means no hint. Only meaningful when Think is nil
    // or true; providers ignore it when Think is explicitly false.
    ThinkEffort string `json:"think_effort,omitempty"`
}
```

Division of labor: `Think *bool` = on/off intent (existing field, now also
wire-applied); `ThinkEffort` = optional intensity hint. Validation of the
effort string happens at the golem flag boundary, not in providers (providers
pass through what they're given; unknown strings are sent as-is on the
openai-compat path where the server validates, and collapse to `true` on the
ollama path).

Effort-only means "thinking on" for both wire behavior and parsing. In
`ThinkToggle` mode, `Think=false` still wins, but `Think=nil` plus non-empty
`ThinkEffort` activates the inline think parser so library callers do not get
raw `<think>` tags after requesting an effort level.

### 1.5. ChatRequest: route-selected parse controls

The registry's `ModelProfile.ThinkMode` / `ThinkTags` must reach the provider
parse path. Provider instances are backend-wide (`ollama`, `llamacpp`, etc.),
so provider-level `WithThinkMode` / `WithThinkTags` cannot represent per-model
catalog or `models.json` overrides.

Add non-wire parser controls to `provider.ChatRequest`:

```go
// ParseThinkMode optionally overrides the provider instance's parser mode for
// this request. Router-filled from the selected ModelProfile; direct provider
// callers can leave it nil to use the provider default.
ParseThinkMode *ThinkMode `json:"-"`
// ParseThinkTags optionally overrides the provider instance's parser tags for
// this request. nil uses the provider default tags.
ParseThinkTags *ThinkTags `json:"-"`
```

`RoutePlan.buildChatRequest` copies the selected `RoutePlan.Profile` into
these fields. It also clears `Options.Think` and `Options.ThinkEffort` when the
selected profile's `ThinkMode` is `ThinkNone`, so routed fallbacks do not send a
wire think control to a known non-thinking model. Direct provider callers still
get the raw provider behavior described below.

### 2. Ollama provider: wire `opts.Think`

In `OllamaProvider.Chat`/`ChatStream` request construction: when
`opts.Think != nil`, set the outgoing `ollama.ChatRequest.Think` field.
`opts.ThinkEffort` non-empty with `Think == nil` implies `Think: true`.

Inline parsing uses `req.ParseThinkMode` / `req.ParseThinkTags` when present,
falling back to the provider instance defaults. `ThinkToggle` is active when
`Think=true`, or when `Think=nil` and `ThinkEffort` is non-empty.

Ollama returns HTTP 400 for `think: true` on non-thinking models; routed golem
calls clear the option for selected `ThinkNone` profiles (RoutePlan gate
above), and direct provider callers get the informative provider error. No
client-side capability guessing.

Parse-side `ThinkToggle` logic changes only for the new effort hint:
toggle-off still disables parsing, while effort-only activates parsing to match
the wire request.

### 3. OpenAI-compat provider: two request fields

```go
// provider/openaicompat/types.go chatRequest additions
ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
```

Mapping in request construction:

| Options state | reasoning_effort | chat_template_kwargs |
| --- | --- | --- |
| `Think == nil`, `ThinkEffort == ""` | omitted | omitted |
| `ThinkEffort = "low"/"medium"/"high"` | that value | `{"enable_thinking": true}` |
| `Think = &true`, no effort | omitted | `{"enable_thinking": true}` |
| `Think = &false` | omitted | `{"enable_thinking": false}` |

Rationale: llama.cpp (primary backend) honors `chat_template_kwargs` for
Qwen3-family templates (`enable_thinking`) and passes `reasoning_effort`
where the template supports it; vLLM accepts both; servers ignore unknown
template kwargs. Everything is `omitempty`, so unset options change nothing
on the wire.

Inline parsing uses `req.ParseThinkMode` / `req.ParseThinkTags` when present,
falling back to the provider instance defaults. `ThinkToggle` activation matches
the Ollama path: explicit false wins, effort-only implies on.

Accepted edge (explicitly, not punted): if the user forces `-think off` and a
server ignores `enable_thinking: false`, the model may still emit
`<think>` tags; with `ThinkToggle` + `Think=false` the parse gate is off and
tags would leak into the answer text. Mitigation: the golem flag path keeps
extraction active by never disabling parse for `auto`/`always` profiles, and
for toggle profiles the primary backend honors the kwarg. If this leaks in
practice, the follow-up is a parse-always-strip mode — out of scope now.

### 4. Registry: config think override hook

Mirror the capability override seam exactly:

```go
// provider/model_registry.go
type ThinkOverride func(key ModelKey) (mode *ThinkMode, tags *ThinkTags)

func (r *ModelRegistry) SetThinkOverride(fn ThinkOverride)
```

- Applied in the merge at final precedence, immediately adjacent to the
  capability override block (model_registry.go:1117), passed into
  `buildProfile` the same way `override` is (no re-read from `r`; same
  `overrideVersion` counter and invalidation path — bump the shared counter in
  `SetThinkOverride` and clear cached profiles, identical to
  `SetCapabilityOverride`).
- REPLACE semantics per field, independently: non-nil `mode` replaces
  `profile.ThinkMode`; non-nil `tags` replaces `profile.ThinkTags`. One set
  without the other keeps the merged value for the other field.
- No interaction with `Caps`: a think override does not add or imply
  `CapThinking` (capability declaration stays explicit, matching the
  "never derived" invariant for `tool_call`/`thinking`/`insert`).

`providerbootstrap` builds a per-key think override map before installing the
hook, not a "first matching role wins" lookup. Multiple roles may point at the
same provider/model; matching per-field declarations are allowed, complementary
mode-only/tags-only declarations combine, and conflicting mode or tag
declarations fail startup loudly. This mirrors capability override conflict
detection and avoids nondeterministic config behavior.

### 5. Config schema: per-model think fields

```go
// config/config.go ModelConfig additions
ThinkMode string            `json:"think_mode,omitempty"`
ThinkTags *ThinkTagsConfig  `json:"think_tags,omitempty"`

type ThinkTagsConfig struct {
    Open  string `json:"open"`
    Close string `json:"close"`
}
```

Validation in `config.validate()`:

- `think_mode` ∈ {`none`, `always`, `toggle`, `auto`} (case-insensitive at
  parse, stored lowercase); anything else is a load error naming the model —
  no silent fallback-to-none (the catalog's lenient `parseThinkMode` default
  is for trusted embedded data; user config fails loud, matching the
  ParseCapsStrict philosophy at the validation site).
- `think_tags` requires both `open` and `close` non-empty and `open != close`.
- `think_tags` without `think_mode` is allowed (tags-only override).

Expose a strict string parser (`provider.ParseThinkModeStrict` or equivalent)
and use it in both `config.validate()` and `providerbootstrap`. The catalog's
lenient `parseThinkMode` remains unexported for trusted embedded data only.

`internal/providerbootstrap` installs the registry hook from config, exactly
where it installs the capability override today, using the conflict-checked
per-key map above.

### 6. Agent: options passthrough + thinking observer

```go
// agent/types.go Request addition
// Options carries per-run model options (temperature, think controls, ...)
// applied to every model call in the run. Zero value preserves current
// behavior.
Options provider.ModelOptions
```

Orchestrator (`agent/orchestrator.go:65`) sets `Options: req.Options` on the
`ChatRequest`. Note `model_caller.go:51` already forwards
`req.Options`/`NumPredict` for its bookkeeping — verify the summarize path
(`model_caller.go:121`) keeps its own fixed options and does NOT inherit
think settings (summaries should not burn reasoning tokens).

New optional observer interface, mirroring `PressureObserver`:

```go
// agent/observer.go
type ThinkingEvent struct {
    Step    int
    Content string // thinking delta
}

type ThinkingObserver interface {
    OnThinking(ctx context.Context, ev ThinkingEvent) error
}
```

Orchestrator stream callback: when `c.Thinking != ""` and the observer
implements `ThinkingObserver`, forward it; otherwise drop (today's behavior).
Error semantics match `OnToken` (an observer error aborts the stream the same
way).

### 7. Golem CLI

New flag:

```text
-think string
    reasoning control for the agent model: off, on, low, medium, high
    (default: model decides)
```

- Flag parse validates the value; bad values are a startup error.
- Mapping: `off` → `Think=&false`; `on` → `Think=&true`;
  `low|medium|high` → `Think=&true, ThinkEffort=<level>`.
- Support gate at startup: evaluate the configured agent chain through the
  existing selector semantics (`provider/model` via `Lookup`, bare model via
  `LookupAny`). If every configured agent candidate resolves and each is
  `ThinkNone`, print `think: model <name> does not support thinking; -think
  ignored` and do not set options. Mixed chains and lookup/recommend unknowns
  fail open: set options, and `RoutePlan.buildChatRequest` clears them for any
  selected `ThinkNone` fallback. The gate keys off ThinkMode, not
  `CapThinking`, because openai-compat never advertises `CapThinking`.
- The resolved options ride `agent.Request.Options` for every turn (REPL and
  one-shot).

### 8. Golem rendering

The golem terminal observer implements `ThinkingObserver`:

- First thinking delta of a step prints a dim `[thinking]` header line; deltas
  stream dim (ANSI SGR 2) when stdout is a TTY, plain otherwise; a separator
  newline is printed before the first answer token that follows thinking.
- Applies in REPL and one-shot modes; no flag to hide it (models that think
  produce it; `-think off` prevents it at the source).
- Non-TTY output keeps the `[thinking]` header so logs remain parseable.

## Acceptance mapping

- Thinking-capable model + flag → wire-level change (mock-server-verified) and
  distinct rendering: sections 2, 3, 7, 8.
- Non-thinking model → graceful no-op notice: section 7.
- models.json override effective without catalog change: sections 1.5, 4, 5.

## Testing

- provider/ollama: mock HTTP asserts `think` present/absent/true/false for
  each `Options` state, Chat + ChatStream; effort implies true. Toggle parser
  tests assert effort-only extracts inline thinking and explicit false wins.
- provider/openaicompat: mock HTTP asserts the full mapping table above,
  including field absence when options unset (guard the zero-change default).
  Toggle parser tests match the Ollama effort-only/false behavior.
- config: table tests for think_mode/think_tags validation (valid values,
  bad enum, half-empty tags, open==close, tags-only).
- registry: think override REPLACE semantics both directions (mode-only,
  tags-only, both, neither), version-counter invalidation on SetThinkOverride,
  no CapThinking implication.
- provider route plan: selected profile's ThinkMode/ThinkTags reach
  ChatRequest parse controls; ThinkNone selected profiles clear wire think
  options before provider execution.
- providerbootstrap: same-key think overrides combine compatible per-field
  declarations and reject conflicting mode/tags declarations.
- agent: Request.Options reaches ChatRequest; summarize path unaffected;
  ThinkingObserver receives deltas, plain Observer unaffected, observer error
  aborts stream.
- golem: flag validation, mapping to options, chain/recommend support gate,
  ThinkNone no-op notice, renderer thinking/answer separation (TTY + non-TTY).
- `env -u GOROOT go test -race ./provider/... ./agent ./config ./cmd/golem`,
  then full `env -u GOROOT go test ./...`.
