# Golem Think-Mode Setting (#220) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `golem -think off|on|low|medium|high` drives real model reasoning behavior on both backends, `models.json` gains a per-model think override, and captured thinking renders distinctly in the REPL.

**Architecture:** Wire the existing-but-unsent think intent (`ModelOptions.Think`) plus a new `ThinkEffort` hint through both provider request builders; bind the selected `RoutePlan.Profile` into per-request parse controls; add a `SetThinkOverride` registry hook mirroring `SetCapabilityOverride`; thread `ModelOptions` through `agent.Request`; forward `ChatResponse.Thinking` via a new optional `ThinkingObserver`; golem maps the flag, uses a chain-aware ThinkMode gate, and renders thinking dim.

**Tech Stack:** Go stdlib only. Spec: `docs/superpowers/specs/2026-07-04-golem-think-mode-220-design.md`.

**Session rules (every task):**
- Worktree: `git worktree add ../go-llm-220 -b feat/golem-think-220 develop` (linked worktree — docker pre-push hook cannot run there; run the gate natively and push with `git push --no-verify`).
- Per AGENTS.md, execute shell commands through `rtk` (for example,
  `rtk env -u GOROOT go test ./provider/`). Command snippets below show the
  raw command payload; prefix them with `rtk` when running.
- Every go command: `env -u GOROOT go ...`.
- No emojis anywhere in commits or the PR.
- First commit in the worktree: `git add -f docs/superpowers/specs/2026-07-04-golem-think-mode-220-design.md docs/superpowers/plans/2026-07-04-golem-think-mode-220.md` (directory is gitignored; force-add is the established pattern), message `docs(spec): think-mode setting design and plan (#220)`.

---

### Task 1: Ollama provider sends the wire `think` field

**Files:**
- Modify: `provider/types.go` (ModelOptions, ~line 279; ChatRequest parse controls, ~line 191)
- Modify: `provider/ollama.go:587-609` (`toOllamaChatRequest`)
- Test: `provider/ollama_think_wire_test.go` (new)

**Context:** `ollama/types.go:55` already declares `Think *bool \`json:"think,omitempty"\`` on `ollama.ChatRequest`, but `toOllamaChatRequest` never sets it — `Options.Think` today only gates client-side tag extraction (`shouldExtractThinking`, provider/ollama.go:104). This task makes the intent reach the server. Existing provider tests use `httptest.Server` mocks; follow that pattern (see `provider/ollama_test.go` for the harness idiom).

- [ ] **Step 1: Add `ThinkEffort` to `ModelOptions`**

In `provider/types.go`, after the existing `Think *bool` field:

```go
	// ThinkEffort is an optional reasoning-effort hint: "low", "medium", or
	// "high". Empty means no hint. A non-empty effort with Think == nil
	// implies thinking on. Providers ignore it when Think is explicitly
	// false. Values are not validated here; the CLI boundary validates and
	// openai-compat servers reject unknown efforts themselves.
	ThinkEffort string `json:"think_effort,omitempty"`
```

- [ ] **Step 2: Add per-request parse controls to `ChatRequest`**

In `provider/types.go`, on `ChatRequest`, add non-wire parser controls:

```go
	// ParseThinkMode optionally overrides the provider instance's parser mode
	// for this request. Router-filled from the selected ModelProfile; direct
	// provider callers can leave it nil to use the provider default.
	ParseThinkMode *ThinkMode `json:"-"`
	// ParseThinkTags optionally overrides the provider instance's parser tags
	// for this request. nil uses the provider default tags.
	ParseThinkTags *ThinkTags `json:"-"`
```

These are deliberately `json:"-"`: they are local parsing policy, not provider
wire fields.

- [ ] **Step 3: Write the failing wire and parser tests**

Create `provider/ollama_think_wire_test.go`:

```go
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureThink decodes the raw /api/chat body and returns the "think" field:
// present true, present false, or absent (nil).
func captureThink(t *testing.T, body []byte) *bool {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	tv, ok := raw["think"]
	if !ok {
		return nil
	}
	var b bool
	if err := json.Unmarshal(tv, &b); err != nil {
		t.Fatalf("think field not a bool: %s", tv)
	}
	return &b
}

func TestOllamaChatWiresThink(t *testing.T) {
	tests := []struct {
		name string
		opts ModelOptions
		want *bool // nil => field must be absent
	}{
		{"unset options omit think", ModelOptions{}, nil},
		{"think true sent", ModelOptions{Think: boolPtr(true)}, boolPtr(true)},
		{"think false sent", ModelOptions{Think: boolPtr(false)}, boolPtr(false)},
		{"effort alone implies true", ModelOptions{ThinkEffort: "high"}, boolPtr(true)},
		{"explicit false wins over effort", ModelOptions{Think: boolPtr(false), ThinkEffort: "high"}, boolPtr(false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body := readAll(t, r.Body)
				got = captureThink(t, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
			}))
			defer srv.Close()

			p := NewOllamaProvider(newTestOllamaClient(t, srv.URL))
			_, err := p.Chat(context.Background(), ChatRequest{
				Model:    "m",
				Messages: []ChatMessage{{Role: "user", Content: "hi"}},
				Options:  tt.opts,
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			assertThinkField(t, got, tt.want)
		})
	}
}

func TestOllamaChatStreamWiresThink(t *testing.T) {
	var got *bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r.Body)
		got = captureThink(t, body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}` + "\n"))
	}))
	defer srv.Close()

	p := NewOllamaProvider(newTestOllamaClient(t, srv.URL))
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
		Options:  ModelOptions{Think: boolPtr(true)},
	}, func(ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	assertThinkField(t, got, boolPtr(true))
}

func assertThinkField(t *testing.T, got, want *bool) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("think field present (%v), want absent", *got)
	case want != nil && got == nil:
		t.Fatalf("think field absent, want %v", *want)
	case want != nil && got != nil && *got != *want:
		t.Fatalf("think = %v, want %v", *got, *want)
	}
}
```

Helper notes: `boolPtr`, `readAll`, and `newTestOllamaClient` — reuse the package's existing test helpers if present (grep `func boolPtr`, `func readAll`, and how existing ollama provider tests construct the client from a test server URL); define locally only what is missing, matching existing naming.

Also add a small parser regression in the existing Ollama provider think tests:

```go
func TestOllamaThinkEffortActivatesToggleParser(t *testing.T) {
	// Provider has ParseThinkMode/ThinkToggle for this request and the server
	// returns "<think>why</think>answer" in content. With Options{ThinkEffort:
	// "high"} and Think nil, Chat/ChatStream must return Thinking=="why" and
	// Content=="answer". With Think=false plus effort, tags must pass through
	// as content (explicit false wins).
}
```

Use `ChatRequest.ParseThinkMode` so the test covers the new per-request parser
override instead of provider-wide `WithThinkMode`.

- [ ] **Step 4: Run tests, verify failure**

Run: `env -u GOROOT go test ./provider/ -run 'TestOllama(Chat.*WiresThink|ThinkEffortActivatesToggleParser)' -v`
Expected: FAIL — `think field absent, want true` (and effort case), because `toOllamaChatRequest` never sets it. The "unset options" case passes (guards the zero-change default).

- [ ] **Step 5: Implement the mapping and parser controls**

In `toOllamaChatRequest` (provider/ollama.go:599-607), after the `oReq` literal:

```go
	// Wire the caller's think intent. Explicit Think wins; a bare effort
	// hint implies thinking on. nil + no effort leaves the field absent so
	// the request is byte-identical to pre-#220 behavior.
	if req.Options.Think != nil {
		oReq.Think = req.Options.Think
	} else if req.Options.ThinkEffort != "" {
		on := true
		oReq.Think = &on
	}
```

Note: the ollama wire field stays `*bool`; effort levels collapse to `true` here (Ollama string efforts for gpt-oss are a non-goal; the field type upgrade is the documented path if ever needed).

Update parser selection in `provider/ollama.go`:

```go
func (p *OllamaProvider) effectiveThinkMode(req ChatRequest) ThinkMode {
	if req.ParseThinkMode != nil {
		return *req.ParseThinkMode
	}
	return p.thinkMode
}

func (p *OllamaProvider) effectiveThinkTags(req ChatRequest) ThinkTags {
	if req.ParseThinkTags != nil {
		return *req.ParseThinkTags
	}
	return p.thinkTags
}

func thinkToggleActive(opts ModelOptions) bool {
	if opts.Think != nil {
		return *opts.Think
	}
	return opts.ThinkEffort != ""
}
```

Use those helpers in non-streaming `ExtractThinking` and streaming
`NewThinkParser`. For `ThinkToggle`, `Think=false` still disables parsing;
effort-only activates parsing.

- [ ] **Step 6: Run tests, verify pass**

Run: `env -u GOROOT go test ./provider/ -run 'TestOllama(Chat.*WiresThink|ThinkEffortActivatesToggleParser)' -v`
Expected: PASS (all cases).

- [ ] **Step 7: Package regression + commit**

Run: `env -u GOROOT go test ./provider/ ./ollama/`
Expected: PASS.

```bash
git add provider/types.go provider/ollama.go provider/ollama_think_wire_test.go
git commit -m "feat(provider): wire ModelOptions think intent to the ollama think field (#220)"
```

---

### Task 2: OpenAI-compat provider sends `reasoning_effort` and `chat_template_kwargs`

**Files:**
- Modify: `provider/openaicompat/types.go:11-22` (`chatRequest`)
- Modify: `provider/openaicompat/openaicompat.go:725+` (`applyOptionsChat`)
- Test: `provider/openaicompat/think_wire_test.go` (new)

**Context:** llama.cpp (primary backend) honors `chat_template_kwargs.enable_thinking` for Qwen3-family templates and forwards `reasoning_effort` where supported; both are ignored by servers that don't know them. Everything is omitempty — unset options must keep the request byte-identical to today. Mapping table (from the spec):

| Options state | reasoning_effort | chat_template_kwargs |
| --- | --- | --- |
| `Think == nil`, `ThinkEffort == ""` | omitted | omitted |
| `ThinkEffort = "low"/"medium"/"high"` | that value | `{"enable_thinking": true}` |
| `Think = &true`, no effort | omitted | `{"enable_thinking": true}` |
| `Think = &false` | omitted (effort ignored) | `{"enable_thinking": false}` |

- [ ] **Step 1: Write the failing wire and parser tests**

Create `provider/openaicompat/think_wire_test.go`:

```go
package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type thinkWire struct {
	effort string         // "" => absent
	kwargs map[string]any // nil => absent
}

func captureThinkWire(t *testing.T, body []byte) thinkWire {
	t.Helper()
	var raw struct {
		ReasoningEffort    *string        `json:"reasoning_effort"`
		ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	w := thinkWire{kwargs: raw.ChatTemplateKwargs}
	if raw.ReasoningEffort != nil {
		w.effort = *raw.ReasoningEffort
	}
	return w
}

func TestChatWiresThinkControls(t *testing.T) {
	fp := func(b bool) *bool { return &b }
	tests := []struct {
		name       string
		opts       provider.ModelOptions
		wantEffort string
		wantKwargs map[string]any // nil => field absent
	}{
		{"unset omits both", provider.ModelOptions{}, "", nil},
		{"effort sends both", provider.ModelOptions{ThinkEffort: "low"},
			"low", map[string]any{"enable_thinking": true}},
		{"think true sends kwargs only", provider.ModelOptions{Think: fp(true)},
			"", map[string]any{"enable_thinking": true}},
		{"think false disables", provider.ModelOptions{Think: fp(false)},
			"", map[string]any{"enable_thinking": false}},
		{"think false suppresses effort", provider.ModelOptions{Think: fp(false), ThinkEffort: "high"},
			"", map[string]any{"enable_thinking": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got thinkWire
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got = captureThinkWire(t, body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
			}))
			defer srv.Close()

			p := newTestProvider(t, srv.URL)
			_, err := p.Chat(context.Background(), provider.ChatRequest{
				Model:    "m",
				Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
				Options:  tt.opts,
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got.effort != tt.wantEffort {
				t.Errorf("reasoning_effort = %q, want %q", got.effort, tt.wantEffort)
			}
			assertKwargs(t, got.kwargs, tt.wantKwargs)
		})
	}
}

func TestChatStreamWiresThinkControls(t *testing.T) {
	var got thinkWire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = captureThinkWire(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := newTestProvider(t, srv.URL)
	err := p.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "m",
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
		Options:  provider.ModelOptions{ThinkEffort: "medium"},
	}, func(provider.ChatResponse) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if got.effort != "medium" {
		t.Errorf("reasoning_effort = %q, want medium", got.effort)
	}
	assertKwargs(t, got.kwargs, map[string]any{"enable_thinking": true})
}

func assertKwargs(t *testing.T, got, want map[string]any) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("chat_template_kwargs present (%v), want absent", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("chat_template_kwargs absent, want %v", want)
	}
	wantOn, gotOn := want["enable_thinking"].(bool), got["enable_thinking"]
	if gotOn != wantOn {
		t.Fatalf("enable_thinking = %v, want %v", gotOn, wantOn)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected extra kwargs: %v", got)
	}
}
```

`newTestProvider` — reuse the package's existing test constructor for a Provider against an httptest URL (grep the existing `_test.go` files for the established helper; do not invent a second one).

Also add an openai-compat parser regression mirroring the Ollama one:

```go
func TestThinkEffortActivatesToggleParser(t *testing.T) {
	// Provider request has ParseThinkMode=ThinkToggle and the server returns
	// "<think>why</think>answer" in content. Options{ThinkEffort:"high"} with
	// Think nil extracts Thinking=="why"; Think=false plus effort leaves tags
	// in Content (explicit false wins).
}
```

Use `provider.ChatRequest.ParseThinkMode` so the test proves per-selected-route
parse controls work for this provider too.

- [ ] **Step 2: Run tests, verify failure**

Run: `env -u GOROOT go test ./provider/openaicompat/ -run 'TestChat.*WiresThinkControls|TestThinkEffortActivatesToggleParser' -v`
Expected: FAIL — struct has no such fields yet (compile error is the failing state here; that is acceptable for a wire-shape addition).

- [ ] **Step 3: Add wire fields + mapping**

`provider/openaicompat/types.go`, extend `chatRequest`:

```go
	// ReasoningEffort and ChatTemplateKwargs carry the caller's think
	// controls (#220). llama.cpp forwards reasoning_effort to templates
	// that support it and honors chat_template_kwargs.enable_thinking for
	// Qwen3-family templates; servers ignore unknown fields/kwargs. Both
	// are omitempty so unset options leave the request unchanged.
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
```

`applyOptionsChat` (provider/openaicompat/openaicompat.go:725+), append:

```go
	// Think controls (#220). Explicit Think=false wins: no effort is sent
	// and the template is asked not to think. A bare effort implies on.
	switch {
	case opts.Think != nil && !*opts.Think:
		r.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	case opts.ThinkEffort != "":
		r.ReasoningEffort = opts.ThinkEffort
		r.ChatTemplateKwargs = map[string]any{"enable_thinking": true}
	case opts.Think != nil: // Think == &true, no effort
		r.ChatTemplateKwargs = map[string]any{"enable_thinking": true}
	}
```

Update `shouldExtractThinking` / streaming parser setup to use
`provider.ChatRequest.ParseThinkMode` and `ParseThinkTags` when present, falling
back to the provider instance defaults. `ThinkToggle` is active when
`Think=true`, or when `Think=nil` and `ThinkEffort` is non-empty; explicit
`Think=false` wins and disables parsing.

- [ ] **Step 4: Run tests, verify pass**

Run: `env -u GOROOT go test ./provider/openaicompat/ -run 'TestChat.*WiresThinkControls|TestThinkEffortActivatesToggleParser' -v`
Expected: PASS.

- [ ] **Step 5: Package regression + commit**

Run: `env -u GOROOT go test ./provider/...`
Expected: PASS.

```bash
git add provider/openaicompat/types.go provider/openaicompat/openaicompat.go provider/openaicompat/think_wire_test.go
git commit -m "feat(openaicompat): send reasoning_effort and enable_thinking template kwarg (#220)"
```

---

### Task 3: Registry `SetThinkOverride` hook (REPLACE semantics)

**Files:**
- Modify: `provider/model_registry.go` (hook + merge apply, adjacent to the capability override at :1088-1134)
- Modify: `provider/route_plan.go` (`buildChatRequest`, ~line 773)
- Test: `provider/model_registry_think_override_test.go` (new)
- Test: `provider/route_plan_think_profile_test.go` (new)

**Context:** Mirror `SetCapabilityOverride` (model_registry.go:206) exactly: same mutex, same `overrideVersion` bump, same cache invalidation, same pass-the-hook-into-`buildProfile` discipline (the closure is captured by the caller and passed in so the merge applies the SAME override the caller version-checks at cache-write time — do NOT read `r.thinkOverride` inside the merge; that reintroduces the TOCTOU the version counter closes, see the comment block at :1112-1116). **First step of this task: read `SetCapabilityOverride`'s body and `buildProfile`'s signature/call sites, then replicate the pattern.** The #219 lesson applies: REPLACE semantics must be explicit and tested in both directions.

- [ ] **Step 1: Read the existing pattern**

Read `provider/model_registry.go:188-230` (SetCapabilityOverride/SetCapabilityFloor), the `buildProfile` signature and its callers, and the merge block at :1088-1134. Note exactly how `override` is threaded and where `overrideVersion` guards cache writes.

- [ ] **Step 2: Write the failing tests**

Create `provider/model_registry_think_override_test.go`. Follow the package's existing registry-test setup idiom (grep how capability-override tests construct a registry with a fake provider). Cases:

```go
func TestThinkOverrideReplacesModePerField(t *testing.T) {
	// Arrange a model whose merged profile has ThinkMode=ThinkToggle and
	// non-default tags (pick a catalog family that provides them, or use
	// the inference fallback: a "qwen3*" name yields ThinkToggle).
	// mode-only override:
	//   SetThinkOverride returning (mode=&ThinkAlways, tags=nil)
	//   => profile.ThinkMode == ThinkAlways, profile.ThinkTags unchanged.
	// tags-only override:
	//   (nil, &ThinkTags{Open: "<r>", Close: "</r>"})
	//   => tags replaced, mode unchanged.
	// both => both replaced.
	// hook returns (nil, nil) => profile identical to no-hook baseline.
}

func TestThinkOverrideDoesNotImplyCapThinking(t *testing.T) {
	// Model without CapThinking; override mode=&ThinkAlways.
	// profile.Caps must NOT gain CapThinking.
}

func TestSetThinkOverrideInvalidatesCache(t *testing.T) {
	// Lookup once (caches profile), install override, Lookup again =>
	// new value visible (proves invalidation + version bump). Clearing the
	// hook (SetThinkOverride(nil)) restores the merged value on next Lookup.
}

func TestRoutePlanAppliesProfileThinkParserControls(t *testing.T) {
	// RoutePlan with Profile{ThinkMode: ThinkToggle, ThinkTags: &ThinkTags{
	// Open:"<r>", Close:"</r>"}} and Request.Options{Think:true}.
	// buildChatRequest must set ParseThinkMode/ParseThinkTags on the outgoing
	// ChatRequest without changing the caller's Request.Options.
}

func TestRoutePlanClearsWireThinkForThinkNoneProfile(t *testing.T) {
	// RoutePlan with Profile{ThinkMode: ThinkNone} and Request.Options
	// containing Think=true + ThinkEffort="high". buildChatRequest must clear
	// Options.Think and ThinkEffort so a routed fallback known not to support
	// thinking does not receive wire think controls.
}
```

Write the registry tests as real table-driven tests against the registry public
API (`Lookup`), not by calling the merge internals — the cache/version behavior
is part of the contract. The route-plan tests can call `buildChatRequest`
directly from package `provider`.

- [ ] **Step 3: Run tests, verify failure**

Run: `env -u GOROOT go test ./provider/ -run 'ThinkOverride|RoutePlan.*Think' -v`
Expected: FAIL — `SetThinkOverride` undefined.

- [ ] **Step 4: Implement**

Type + setter (adjacent to `SetCapabilityOverride`):

```go
// ThinkOverride returns per-model think overrides from user config. A nil
// mode/tags pointer means "no override for that field" — the merged value
// is kept. Non-nil REPLACES the merged value wholesale (same contract as
// the capability override: config is the final word, per field).
type ThinkOverride func(key ModelKey) (mode *ThinkMode, tags *ThinkTags)

// SetThinkOverride installs (or clears) the think override hook.
// Shares SetCapabilityOverride's invalidation + version-guard semantics.
func (r *ModelRegistry) SetThinkOverride(fn ThinkOverride) {
	// Mirror SetCapabilityOverride's body exactly: lock, assign
	// r.thinkOverride, bump overrideVersion, invalidate cached profiles.
}
```

Registry struct gains `thinkOverride ThinkOverride` next to the capability hook field. Thread it into `buildProfile` alongside `override` (same capture-then-pass pattern), and apply in the merge immediately after the capability override block (:1134):

```go
	// Config think override (final precedence, #220). Per-field REPLACE:
	// a nil pointer keeps the merged value; non-nil replaces it. Unlike
	// capabilities there is no parse step to reject — config.validate
	// already rejected bad enum strings at load time.
	if thinkOverride != nil {
		if mode, tags := thinkOverride(key); mode != nil || tags != nil {
			if mode != nil {
				profile.ThinkMode = *mode
			}
			if tags != nil {
				profile.ThinkTags = *tags
			}
		}
	}
```

(Adjust `profile.ThinkTags` assignment to the field's actual type — check whether `ModelProfile.ThinkTags` is `ThinkTags` or `*ThinkTags` at provider/model_profile.go:110-137 and dereference accordingly.)

- [ ] **Step 5: Bind selected route profile into ChatRequest parse controls**

In `provider/route_plan.go`, update `buildChatRequest` to copy request options,
fill the non-wire parser controls from `rp.Profile`, and clear wire controls for
known non-thinking selected profiles:

```go
	opts := rp.Request.Options
	var parseMode *ThinkMode
	var parseTags *ThinkTags
	if rp.Profile != nil {
		mode := rp.Profile.ThinkMode
		parseMode = &mode
		if rp.Profile.ThinkTags != nil {
			tags := *rp.Profile.ThinkTags
			parseTags = &tags
		}
		if rp.Profile.ThinkMode == ThinkNone {
			opts.Think = nil
			opts.ThinkEffort = ""
		}
	}
	return ChatRequest{
		Model:          rp.Model,
		Messages:       rp.Request.Messages,
		Options:        opts,
		Tools:          rp.Request.Tools,
		Stream:         stream,
		ParseThinkMode: parseMode,
		ParseThinkTags: parseTags,
	}
```

This is the seam that makes catalog/config `ThinkMode` and `ThinkTags`
effective for real routed calls; provider instances are backend-wide and cannot
carry per-model parser policy.

- [ ] **Step 6: Run tests, verify pass**

Run: `env -u GOROOT go test ./provider/ -run 'ThinkOverride|RoutePlan.*Think' -v`
Expected: PASS.

- [ ] **Step 7: Race + regression + commit**

Run: `env -u GOROOT go test -race ./provider/`
Expected: PASS.

```bash
git add provider/model_registry.go provider/route_plan.go provider/model_registry_think_override_test.go provider/route_plan_think_profile_test.go
git commit -m "feat(provider): per-model think override hook with per-field replace semantics (#220)"
```

---

### Task 4: config schema `think_mode` / `think_tags` + validation

**Files:**
- Modify: `config/config.go` (ModelConfig :59-77, validate() near :511-528)
- Modify: `provider/catalog.go` or `provider/types.go` (export a strict think-mode parser)
- Test: `config/think_config_test.go` (new)

- [ ] **Step 1: Write the failing tests**

Create `config/think_config_test.go` with a table over inline JSON configs (follow the package's existing pattern of writing a temp models.json and calling `Load`):

```go
func TestThinkModeValidation(t *testing.T) {
	tests := []struct {
		name    string
		model   string // the model JSON fragment
		wantErr string // "" => must load
	}{
		{"valid toggle", `{"name":"m","type":"dense","think_mode":"toggle"}`, ""},
		{"valid uppercase normalized", `{"name":"m","type":"dense","think_mode":"NONE"}`, ""},
		{"invalid enum", `{"name":"m","type":"dense","think_mode":"maybe"}`, `think_mode`},
		{"tags both set", `{"name":"m","type":"dense","think_tags":{"open":"<r>","close":"</r>"}}`, ""},
		{"tags missing close", `{"name":"m","type":"dense","think_tags":{"open":"<r>"}}`, `think_tags`},
		{"tags equal", `{"name":"m","type":"dense","think_tags":{"open":"<r>","close":"<r>"}}`, `think_tags`},
		{"tags without mode allowed", `{"name":"m","type":"dense","think_tags":{"open":"<a>","close":"</a>"}}`, ""},
	}
	// Each case: build a minimal valid config around the fragment, Load,
	// assert error contains wantErr (and names the model), or nil error.
	// For the valid uppercase case also assert the loaded ModelConfig
	// stores the lowercased value ("none").
}
```

- [ ] **Step 2: Run tests, verify failure**

Run: `env -u GOROOT go test ./config/ -run ThinkModeValidation -v`
Expected: FAIL — unknown fields are silently ignored today, so the invalid-enum case loads without error.

- [ ] **Step 3: Implement schema + validation**

`ModelConfig` additions:

```go
	// ThinkMode optionally overrides the catalog/inferred think mode for
	// this model: "none", "always", "toggle", or "auto" (lowercased at
	// load). Empty means no override. Invalid values fail Load — user
	// config fails loud, unlike the lenient embedded-catalog parser.
	ThinkMode string `json:"think_mode,omitempty"`
	// ThinkTags optionally overrides the reasoning tag delimiters.
	ThinkTags *ThinkTagsConfig `json:"think_tags,omitempty"`
```

New type next to ModelConfig:

```go
// ThinkTagsConfig is the models.json shape for custom reasoning delimiters.
// Both fields are required when the object is present and must differ.
type ThinkTagsConfig struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}
```

Add an exported strict parser in provider:

```go
func ParseThinkModeStrict(s string) (ThinkMode, error) {
	switch strings.ToLower(s) {
	case "none":
		return ThinkNone, nil
	case "always":
		return ThinkAlways, nil
	case "toggle":
		return ThinkToggle, nil
	case "auto":
		return ThinkAuto, nil
	default:
		return ThinkNone, fmt.Errorf("unknown think_mode %q (want none, always, toggle, or auto)", s)
	}
}
```

Leave catalog `parseThinkMode` lenient and unexported for trusted embedded
catalog data; user config and bootstrap use the strict parser.

In `validate()`, in the per-model loop (near the ParseCapsStrict call at :528):

```go
		if m.ThinkMode != "" {
			mode, err := provider.ParseThinkModeStrict(m.ThinkMode)
			if err != nil {
				return fmt.Errorf("config: model %q: invalid think_mode %q (want none, always, toggle, or auto)", role, m.ThinkMode)
			}
			m.ThinkMode = mode.String()
			cfg.Models[role] = m // map value write-back; validate iterates by value
		}
		if tt := m.ThinkTags; tt != nil {
			if tt.Open == "" || tt.Close == "" {
				return fmt.Errorf("config: model %q: think_tags requires both open and close", role)
			}
			if tt.Open == tt.Close {
				return fmt.Errorf("config: model %q: think_tags open and close must differ", role)
			}
		}
```

(Match the file's actual error-string style — read two adjacent validation errors first and keep the prefix format consistent.)

- [ ] **Step 4: Run tests, verify pass**

Run: `env -u GOROOT go test ./config/ -run ThinkModeValidation -v`
Expected: PASS.

- [ ] **Step 5: Package regression + commit**

Run: `env -u GOROOT go test ./config/`
Expected: PASS.

```bash
git add config/config.go provider/catalog.go provider/types.go config/think_config_test.go
git commit -m "feat(config): per-model think_mode and think_tags overrides (#220)"
```

---

### Task 5: providerbootstrap installs the think override from config

**Files:**
- Modify: `internal/providerbootstrap/capabilities.go` (adjacent to the SetCapabilityOverride install at :205)
- Test: extend the package's existing capabilities/bootstrap test file with a think case

- [ ] **Step 1: Read the install site**

Read `internal/providerbootstrap/capabilities.go:180-230` — how the capability override closure captures config, how keys are matched (`provider.ModelKey` construction), and which test exercises it.

- [ ] **Step 2: Write the failing test**

In the same test file that covers the capability override install, add:

```go
func TestBootstrapInstallsThinkOverride(t *testing.T) {
	// Config: one model with think_mode: "always" and think_tags
	// {open: "<r>", close: "</r>"}; another model with neither.
	// Bootstrap, then Lookup both through the registry:
	//  - overridden model: profile.ThinkMode == provider.ThinkAlways,
	//    tags == {<r>, </r>}
	//  - untouched model: profile matches the no-config baseline.
	// Mirror the arrange/act style of the capability override test above it.
}

func TestBuildThinkOverridesSameKeyCompatibility(t *testing.T) {
	// Two roles point at the same provider/model.
	//  - identical think_mode values are OK
	//  - one role with mode-only plus one with tags-only combines per field
	//  - conflicting modes fail loudly
	//  - conflicting tags fail loudly
}
```

- [ ] **Step 3: Run, verify failure**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ -run ThinkOverride -v`
Expected: FAIL — no hook installed.

- [ ] **Step 4: Implement the install**

Add a pure builder next to `buildCapabilityOverrides` rather than doing lookup
inside the closure:

```go
type thinkOverrideEntry struct {
	role string
	mode *provider.ThinkMode
	tags *provider.ThinkTags
}

func buildThinkOverrides(cfg *config.Config) (map[provider.ModelKey]thinkOverrideEntry, error) {
	// Sorted-role walk, like buildCapabilityOverrides.
	// Ignore entries with neither ThinkMode nor ThinkTags.
	// For duplicate provider/model keys:
	//   - matching per-field declarations are OK
	//   - one mode-only plus one tags-only combines
	//   - conflicting mode or tags returns a providerbootstrap error naming both roles
	// Use provider.ParseThinkModeStrict even though config.Load validates; this
	// keeps programmatic Config callers fail-loud too.
}
```

Then install from the precomputed map:

```go
func installThinkOverrides(mr *provider.ModelRegistry, cfg *config.Config) error {
	if mr == nil || cfg == nil {
		return nil
	}
	overrides, err := buildThinkOverrides(cfg)
	if err != nil {
		return err
	}
	if len(overrides) == 0 {
		return nil
	}
	mr.SetThinkOverride(func(key provider.ModelKey) (*provider.ThinkMode, *provider.ThinkTags) {
		entry, ok := overrides[key]
		if !ok {
			return nil, nil
		}
		var mode *provider.ThinkMode
		if entry.mode != nil {
			m := *entry.mode
			mode = &m
		}
		var tags *provider.ThinkTags
		if entry.tags != nil {
			t := *entry.tags
			tags = &t
		}
		return mode, tags
	})
	return nil
}
```

Call `installThinkOverrides(mr, effCfg)` in `providerbootstrap.New` next to the
capability override/floor installs.

- [ ] **Step 5: Run, verify pass + commit**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ ./provider/ ./config/`
Expected: PASS.

```bash
git add internal/providerbootstrap/bootstrap.go internal/providerbootstrap/capabilities.go provider/catalog.go provider/model_registry.go internal/providerbootstrap/*_test.go
git commit -m "feat(bootstrap): install config think override into the model registry (#220)"
```

---

### Task 6: agent `Request.Options` passthrough

**Files:**
- Modify: `agent/types.go:67-77` (Request), `agent/orchestrator.go:57-70` (buildChatRequest + call site at :119)
- Test: `agent/options_passthrough_test.go` (new)

**Context:** `buildChatRequest` currently sets only `Options.NumPredict` from `outputReserve`. The summarize path (`agent/model_caller.go:121`) builds its own fixed `ModelOptions` and must NOT inherit think settings.

- [ ] **Step 1: Write the failing tests**

Create `agent/options_passthrough_test.go` using the package's existing fake `ModelCaller` idiom (see `agent/model_caller_test.go` and `orchestrator_test.go` for the capture pattern):

```go
func TestRequestOptionsReachChatRequest(t *testing.T) {
	// Fake model captures the ChatRequest. Run with
	// Request{Goal: "g", Options: provider.ModelOptions{
	//     Think: boolPtr(true), ThinkEffort: "high", Temperature: f64Ptr(0.2)}}.
	// Assert captured Options.Think/ThinkEffort/Temperature match.
}

func TestOutputReserveStillWinsNumPredict(t *testing.T) {
	// Request.Options.NumPredict = 111, Budget.OutputReserve = 222.
	// Captured Options.NumPredict == 222 (reserve keeps priority; the
	// reserve is budget-derived and stays authoritative).
}

func TestSummarizePathDoesNotInheritThink(t *testing.T) {
	// Drive the summarize call (see model_caller_test.go:115 for how the
	// existing test triggers it) with think options set on the main request.
	// Captured summarize ChatRequest.Options.Think == nil, ThinkEffort == "".
}
```

- [ ] **Step 2: Run, verify failure**

Run: `env -u GOROOT go test ./agent/ -run 'OptionsReach|OutputReserve|SummarizePathDoesNotInherit' -v`
Expected: first test FAILs (Options dropped); other two may pass (guards).

- [ ] **Step 3: Implement**

`agent/types.go` Request addition:

```go
	// Options carries per-run model options (think controls, temperature,
	// ...) applied to every model call in the run. Zero value preserves
	// prior behavior. Budget.OutputReserve still overrides NumPredict.
	Options provider.ModelOptions
```

`agent/orchestrator.go`:

```go
func buildChatRequest(st State, specs []provider.Tool, outputReserve int, opts provider.ModelOptions) provider.ChatRequest {
	// ... existing msgs assembly unchanged ...
	req := provider.ChatRequest{Messages: msgs, Tools: specs, Stream: true, Options: opts}
	if outputReserve > 0 {
		req.Options.NumPredict = outputReserve
	}
	return req
}
```

Call site (:119): `buildChatRequest(assembled, specs, req.Budget.OutputReserve, req.Options)`.

- [ ] **Step 4: Run, verify pass + regression + commit**

Run: `env -u GOROOT go test ./agent/`
Expected: PASS.

```bash
git add agent/types.go agent/orchestrator.go agent/options_passthrough_test.go
git commit -m "feat(agent): thread per-run model options into every chat request (#220)"
```

---

### Task 7: `ThinkingObserver` + orchestrator forwarding

**Files:**
- Modify: `agent/observer.go`, `agent/orchestrator.go:119-127` (stream callback)
- Test: `agent/thinking_observer_test.go` (new)

**Context:** The stream callback returns early on `c.Content == ""` (orchestrator.go:120) — thinking-only chunks are swallowed there today, so the forward MUST happen before that guard.

- [ ] **Step 1: Write the failing tests**

Create `agent/thinking_observer_test.go`:

```go
func TestThinkingForwardedToThinkingObserver(t *testing.T) {
	// Fake model emits chunks: {Thinking: "step one"}, {Thinking: "step two",
	// Content: "answer"}, then Done. Observer implementing ThinkingObserver
	// records events. Assert two ThinkingEvents in order with correct Step,
	// and OnToken still received "answer".
}

func TestPlainObserverUnaffectedByThinking(t *testing.T) {
	// Same chunks, observer WITHOUT ThinkingObserver: Run succeeds,
	// OnToken sees only "answer" (current behavior preserved).
}

func TestThinkingObserverErrorAbortsRun(t *testing.T) {
	// OnThinking returns an error => Run returns it (same contract as
	// OnToken/OnPressure).
}
```

Follow `tool_result_observer_test.go` for the optional-interface test fixture idiom.

- [ ] **Step 2: Run, verify failure**

Run: `env -u GOROOT go test ./agent/ -run Thinking -v`
Expected: FAIL — `ThinkingObserver` undefined.

- [ ] **Step 3: Implement**

`agent/observer.go` (next to PressureObserver):

```go
// ThinkingEvent reports a streamed reasoning delta from the model, separated
// from answer content by the provider layer.
type ThinkingEvent struct {
	Step    int
	Content string
}

// ThinkingObserver is an OPTIONAL extension of Observer. When an Observer
// also implements it, the Orchestrator calls OnThinking for every reasoning
// delta before any OnToken for the same chunk. Observers that do not
// implement it keep today's behavior (thinking is dropped). A returned error
// aborts Run, like the other observer callbacks.
type ThinkingObserver interface {
	OnThinking(ctx context.Context, e ThinkingEvent) error
}
```

`agent/orchestrator.go` stream callback — insert BEFORE the `if c.Content == ""` guard:

```go
			if c.Thinking != "" {
				if to, ok := obs.(ThinkingObserver); ok {
					if terr := to.OnThinking(ctx, ThinkingEvent{Step: step, Content: c.Thinking}); terr != nil {
						return terr
					}
				}
			}
			if c.Content == "" {
				return nil
			}
```

- [ ] **Step 4: Run, verify pass + race + commit**

Run: `env -u GOROOT go test -race ./agent/`
Expected: PASS.

```bash
git add agent/observer.go agent/orchestrator.go agent/thinking_observer_test.go
git commit -m "feat(agent): optional ThinkingObserver receives reasoning deltas (#220)"
```

---

### Task 8: golem `-think` flag, mapping, and support gate

**Files:**
- Modify: `cmd/golem/main.go` (flags struct + parseFlags :59-110, wiring in run())
- Modify: `cmd/golem/repl.go` (replSession :18-45, Request build :156-165)
- Test: `cmd/golem/think_flag_test.go` (new)

- [ ] **Step 1: Write the failing tests**

Create `cmd/golem/think_flag_test.go`:

```go
func TestParseThinkFlag(t *testing.T) {
	tests := []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{[]string{}, "", false},
		{[]string{"-think", "off"}, "off", false},
		{[]string{"-think", "on"}, "on", false},
		{[]string{"-think", "low"}, "low", false},
		{[]string{"-think", "medium"}, "medium", false},
		{[]string{"-think", "high"}, "high", false},
		{[]string{"-think", "HIGH"}, "high", false}, // case-insensitive
		{[]string{"-think", "max"}, "", true},
	}
	// parseFlags(tt.args); assert f.think and error presence. Error message
	// must name the valid values.
}

func TestThinkOptionsMapping(t *testing.T) {
	tests := []struct {
		think      string
		wantThink  *bool
		wantEffort string
	}{
		{"", nil, ""},
		{"off", boolPtr(false), ""},
		{"on", boolPtr(true), ""},
		{"low", boolPtr(true), "low"},
		{"medium", boolPtr(true), "medium"},
		{"high", boolPtr(true), "high"},
	}
	// thinkModelOptions(tt.think) => provider.ModelOptions; assert fields.
}

func TestThinkSupportGate(t *testing.T) {
	// fake profile source (reuse the capChecker seam from modelcaller.go
	// and the existing golem model/preflight tests):
	//  - flag unset => zero options, no lookup performed, empty notice.
	//  - empty chain / recommend mode => options applied, empty notice
	//    (selected model is not known until routing).
	//  - all resolved configured chain candidates ThinkNone => zero options
	//    and a notice containing "-think ignored".
	//  - mixed chain (some ThinkNone, some ThinkToggle/Auto/Always) => options
	//    applied; RoutePlan clears wire controls if a ThinkNone fallback is
	//    actually selected.
	//  - bare selectors use LookupAny; provider/model selectors use Lookup.
	//  - Lookup / LookupAny errors fail open: options applied, empty notice.
}
```

- [ ] **Step 2: Run, verify failure**

Run: `env -u GOROOT go test ./cmd/golem/ -run Think -v`
Expected: FAIL — flag/helpers undefined.

- [ ] **Step 3: Implement**

flags struct: add `think string`. parseFlags:

```go
	fs.StringVar(&f.think, "think", "", "reasoning control for the agent model: off, on, low, medium, high (default: model decides); no-op with a notice when the model does not support thinking")
```

After `fs.Parse`, validate + normalize:

```go
	if f.think != "" {
		f.think = strings.ToLower(f.think)
		switch f.think {
		case "off", "on", "low", "medium", "high":
		default:
			return flags{}, fmt.Errorf("golem: invalid -think %q (want off, on, low, medium, or high)", f.think)
		}
	}
```

Helpers (new file `cmd/golem/think.go`):

```go
// thinkModelOptions maps the validated -think value to per-run model options.
// Empty input returns zero options (model decides; no wire fields sent).
func thinkModelOptions(v string) provider.ModelOptions {
	switch v {
	case "":
		return provider.ModelOptions{}
	case "off":
		off := false
		return provider.ModelOptions{Think: &off}
	case "on":
		on := true
		return provider.ModelOptions{Think: &on}
	default: // low, medium, high — validated at the flag boundary
		on := true
		return provider.ModelOptions{Think: &on, ThinkEffort: v}
	}
}

// resolveThinkOptions gates the -think flag on the configured agent chain's
// effective ThinkMode. Empty chain means recommend mode, so fail open: the
// selected model is not known until routing. If every resolved configured
// candidate is ThinkNone, return zero options with a one-line notice. Mixed
// chains fail open; RoutePlan clears wire controls for a ThinkNone fallback if
// that fallback is actually selected. The gate keys off ThinkMode (not
// CapThinking) because openai-compat never advertises CapThinking.
func resolveThinkOptions(ctx context.Context, src capChecker, chain []string, flagVal string) (provider.ModelOptions, string) {
	if flagVal == "" {
		return provider.ModelOptions{}, ""
	}
	if len(chain) == 0 {
		return thinkModelOptions(flagVal), ""
	}
	// Resolve provider/model selectors with Lookup and bare selectors with
	// LookupAny, matching router/preflight selector semantics. Unknowns fail
	// open; do not make startup brittle on a diagnostic feature.
	resolved, unknown, allNone, exampleNone := resolvedThinkSupport(ctx, src, chain)
	if resolved > 0 && !unknown && allNone {
		return provider.ModelOptions{}, fmt.Sprintf("think: model %s does not support thinking; -think ignored", exampleNone)
	}
	return thinkModelOptions(flagVal), ""
}
```

(`capChecker`/`resolvedThinkSupport`: reuse the existing profile-lookup seam in
cmd/golem — `modelcaller.go` already has `capChecker` with `Lookup` and
`LookupAny`, plus `parseSelector`; adapt names to what is there rather than
adding a parallel seam. `unknown=true` means at least one selector lookup failed
or recommend-mode deferred selection, so the caller fails open.)

Wiring in `run()`: after `resolveAgentChain` and before startup notices, call
`resolveThinkOptions(ctx, bundle.Models, plan.chain, f.think)`. Add a
`thinkLine string` to `startupInfo`/`startupNotices` so the notice is printed as
`think: ...` without a `warning:` prefix. Store the options on `replSession`:

```go
	// replSession addition (repl.go):
	modelOptions provider.ModelOptions // per-run model options (-think)
```

Request build (repl.go:156): add `Options: sess.modelOptions,`. Confirm the one-shot path funnels through the same Request construction (repl.go:88 `runOneShot` shares it); if it builds its own Request, add the field there too.

- [ ] **Step 4: Run, verify pass + commit**

Run: `env -u GOROOT go test ./cmd/golem/`
Expected: PASS.

```bash
git add cmd/golem/main.go cmd/golem/think.go cmd/golem/repl.go cmd/golem/think_flag_test.go
git commit -m "feat(golem): -think flag with model-support gate (#220)"
```

---

### Task 9: golem renders thinking distinctly

**Files:**
- Modify: `cmd/golem/render.go` (renderer), plus the telemetry compose path (`composeObserver`, grep its definition in cmd/golem)
- Test: `cmd/golem/render_thinking_test.go` (new)

**Context:** `renderer` (render.go:16) implements `agent.Observer`; `writeDim` (:88-100) is the existing dim-line helper and `r.color` the TTY gate. `composeObserver(rend, sink)` wraps the renderer when telemetry is on — optional interfaces do not survive wrapping unless the composite forwards them; check how the composite handles `PressureObserver`/`ToolResultObserver` today and extend the same mechanism, otherwise `-telemetry` would silently disable thinking rendering.

One-shot mode already calls `runOnce` with `stderr` and writes only the final
answer to `stdout`; keep thinking on that progress channel so scripts continue
to get answer-only stdout.

- [ ] **Step 1: Write the failing tests**

Create `cmd/golem/render_thinking_test.go`:

```go
func TestRendererThinkingDistinctFromAnswer(t *testing.T) {
	// renderer with color=true into bytes.Buffer.
	// OnThinking("I should"), OnThinking(" check"), OnToken("answer").
	// Assert output contains a "[thinking]" header line, the deltas wrapped
	// in dim SGR (\x1b[2m ... \x1b[0m), and that a newline separates the
	// thinking block from "answer" (renderer.lastNL bookkeeping intact).
}

func TestRendererThinkingNoColor(t *testing.T) {
	// color=false: "[thinking]" header still present (log parseability),
	// no ANSI escapes anywhere in output.
}

func TestRendererThinkingResetsPerStep(t *testing.T) {
	// OnThinking step 0, OnStep, OnThinking step 1 => second step prints
	// its own [thinking] header.
}

func TestComposedObserverForwardsThinking(t *testing.T) {
	// composeObserver(renderer, sink): assert the composite satisfies
	// agent.ThinkingObserver and forwards to the renderer.
}
```

- [ ] **Step 2: Run, verify failure**

Run: `env -u GOROOT go test ./cmd/golem/ -run RendererThinking -v`
Expected: FAIL — renderer has no OnThinking.

- [ ] **Step 3: Implement**

renderer state: add `thinkOpen bool` (header printed for current step) — reset in `OnStep` and when the first answer token arrives. Methods:

```go
// OnThinking streams reasoning deltas dim, under a one-per-step
// "[thinking]" header, keeping them visually distinct from the answer.
// The header is plain text so non-TTY logs stay parseable.
func (r *renderer) OnThinking(_ context.Context, e agent.ThinkingEvent) error {
	if !r.thinkOpen {
		if !r.lastNL {
			if _, err := io.WriteString(r.out, "\n"); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(r.out, r.dim("[thinking]")+"\n"); err != nil {
			return err
		}
		r.thinkOpen = true
		r.lastNL = true
	}
	_, err := io.WriteString(r.out, r.dim(e.Content))
	if err == nil && e.Content != "" {
		r.lastNL = strings.HasSuffix(e.Content, "\n")
	}
	return err
}
```

Add a small `dim(s string) string` helper (returns `"\x1b[2m"+s+"\x1b[0m"` when `r.color`, else `s`) and refactor `writeDim` to use it (one dim implementation, not two). In `OnToken`, when `r.thinkOpen` and this is the first answer content after thinking, close the block:

```go
	if r.thinkOpen {
		r.thinkOpen = false
		if !r.lastNL {
			if _, err := io.WriteString(r.out, "\n"); err != nil {
				return err
			}
			r.lastNL = true
		}
	}
```

Reset `thinkOpen = false` in `OnStep` as well. Extend the telemetry composite to forward `OnThinking` to whichever wrapped observer implements `agent.ThinkingObserver`, following exactly how it forwards the other optional interfaces.

- [ ] **Step 4: Run, verify pass + commit**

Run: `env -u GOROOT go test ./cmd/golem/`
Expected: PASS.

```bash
git add cmd/golem/render.go cmd/golem/render_thinking_test.go
git commit -m "feat(golem): render model thinking dim and distinct from the answer (#220)"
```

---

### Task 10: docs touch-up + full gate

**Files:**
- Modify: `cmd/golem/doc.go` (one line for `-think`), `README.md` (flag mention in the golem section if one exists — check first)
- No new tests.

- [ ] **Step 1: Docs**

Add `-think` to the golem doc.go feature summary (one sentence, matching the existing terse style). Grep README for a golem flag list; update only if such a list exists — do not invent a new section.

- [ ] **Step 2: Full verification gate (native, from the worktree)**

```bash
fmt_out=$(env -u GOROOT gofmt -l . | grep -v '^vendor/' || true); test -z "$fmt_out" || { printf '%s\n' "$fmt_out"; exit 1; }
env -u GOROOT go vet ./...
env -u GOROOT go test -race ./provider/... ./agent/... ./config/... ./cmd/golem/... ./internal/providerbootstrap/...
env -u GOROOT go test ./...
```

Expected: gofmt clean, vet clean, all tests PASS.

- [ ] **Step 3: Commit + push**

```bash
git add cmd/golem/doc.go README.md
git commit -m "docs(golem): document the -think reasoning flag (#220)"
git push --no-verify -u origin feat/golem-think-220
```

(`--no-verify` because the docker pre-push hook cannot run from a linked worktree; the native gate above replaces it.)

- [ ] **Step 4: PR**

`gh pr create` targeting develop. Plain-text body (NO emojis anywhere): problem, wire-level behavior table, config override semantics, acceptance mapping, test evidence. Then /code-review cycles until clean per house rules.

---

## Plan Self-Review Notes

- Spec coverage: sections 1-8 of the design map to Tasks 1-9; acceptance criteria all land (wire change Tasks 1-2, route-profile parse controls Task 3, no-op notice Task 8, config override Tasks 3-5, distinct rendering Tasks 7+9, zero-change default guarded by the "unset omits" test cases in Tasks 1-2 and the plain-observer test in Task 7).
- Known judgment points for the executor (read the code first, keep the plan's contract): exact registry lock/version body (Task 3 Step 1), `ModelProfile.ThinkTags` pointer-ness (Task 3 Step 4), route-plan request copying without mutating `RoutePlan.Request.Options` (Task 3 Step 5), config normalization site (Task 4 Step 3), same-key think override conflict handling (Task 5), the existing golem profile-lookup seam names (Task 8 Step 3), composite observer forwarding mechanism (Task 9).
