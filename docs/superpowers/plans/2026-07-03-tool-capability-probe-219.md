# Tool-Capability Probe + Discoverability (#219) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Auto-detect `tool_call` for undeclared models via a cached active probe, fix the derived-override ceiling that erases catalog capabilities, and make capability state discoverable (`golem models`, remediation hints).

**Architecture:** Registry gains a capability *floor* seam (derived caps OR-merge instead of replacing) and a tri-state `ResolveToolCall` resolver backed by a new `capability_probes` table (fingerprint store schema v2). Probing is bounded-eager at golem preflight (stop at first capable chain entry) and lazy at route time (unknown candidates probed before the capability gate rejects). Explicit `models.json` capabilities stay authoritative in both directions.

**Tech Stack:** Go stdlib, modernc.org/sqlite (existing), mock `httptest` servers for probe tests.

**Spec:** `docs/superpowers/specs/2026-07-03-tool-capability-probe-219-design.md` — read it first; it is the contract.

**Worktree/build notes (read before Task 1):**
- Execute in a linked worktree: `git worktree add ../go-llm-219 -b feat/tool-cap-probe-219 develop` (base develop@9c8b77c).
- First commit on the branch adds the spec + this plan (both currently untracked in the main checkout — copy them in).
- Every `go` command MUST be `env -u GOROOT go ...` (split-GOROOT workaround on this machine).
- Docker pre-push hook cannot run from a linked worktree: run the gate natively (`env -u GOROOT go test ./...` then `env -u GOROOT go vet ./...`) and push with `git push --no-verify`.
- NO emojis anywhere in commits or the eventual PR.

**Review updates folded in before implementation:**
- Capability-probe persistence is a narrow `fingerprint.CapProbeStore`, not new methods on the full `fingerprint.Store`; this keeps Golem capability-only mode and existing test fakes small.
- openai-compat probe snippets match current provider types: `ChatResponse.ToolCalls` is direct, and `ChatRequest.ToolChoice` needs explicit wire plumbing.
- Ollama passive probing uses `p.client.ShowModel(ctx, model)`, matching `DetectKind`.
- `-no-cap-probe` disables active probing only; catalog/floor capability merges still apply and are covered by tests.

---

### Task 1: Fingerprint store schema v2 — `capability_probes` + CapProbe API

**Files:**
- Modify: `fingerprint/fingerprint.go` (CapProbe types + constants)
- Modify: `fingerprint/migration.go` (migration v2)
- Modify: `fingerprint/store.go` (CapProbeStore interface + SQLiteStore methods)
- Test: `fingerprint/capprobe_test.go` (create)

- [ ] **Step 1: Write failing tests**

```go
// fingerprint/capprobe_test.go
package fingerprint

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/fp.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestCapProbe_SaveGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	in := CapProbe{
		BackendID: "http://localhost:8080", ModelName: "byo-model",
		Capability: "tool_call", State: CapProbeYes,
		ModelDigest: "sha256:abc", ProbeVersion: CurrentToolProbeVersion,
		TestedAt: now,
	}
	if err := s.SaveCapProbe(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.GetCapProbe(ctx, in.BackendID, in.ModelName, "tool_call")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CapProbeYes || got.ModelDigest != "sha256:abc" ||
		got.ProbeVersion != CurrentToolProbeVersion || !got.TestedAt.Equal(now) {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.ExpiresAt.IsZero() {
		t.Fatalf("yes row must not expire, got %v", got.ExpiresAt)
	}
}

func TestCapProbe_GetMissingReturnsErrNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetCapProbe(context.Background(), "b", "m", "tool_call")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCapProbe_SaveUpsertsOnSameKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := CapProbe{
		BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeInconclusive, ModelDigest: "d1",
		ProbeVersion: CurrentToolProbeVersion, TestedAt: time.Now(),
		ExpiresAt: time.Now().Add(24 * time.Hour),
	}
	if err := s.SaveCapProbe(ctx, base); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	base.State = CapProbeYes
	base.ExpiresAt = time.Time{}
	if err := s.SaveCapProbe(ctx, base); err != nil {
		t.Fatalf("save 2 (upsert): %v", err)
	}
	got, err := s.GetCapProbe(ctx, "b", "m", "tool_call")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != CapProbeYes || !got.ExpiresAt.IsZero() {
		t.Fatalf("upsert did not replace: %+v", got)
	}
}

func TestCapProbe_DeleteCapProbes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	in := CapProbe{BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeNo, ModelDigest: "d", ProbeVersion: 1, TestedAt: time.Now()}
	if err := s.SaveCapProbe(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := s.DeleteCapProbes(ctx, "b", "m"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetCapProbe(ctx, "b", "m", "tool_call"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

// Valid centralizes the freshness contract: digest match, version match, expiry.
func TestCapProbe_Valid(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		probe  CapProbe
		digest string
		want   bool
	}{
		{"fresh yes", CapProbe{State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, TestedAt: now}, "d", true},
		{"digest mismatch", CapProbe{State: CapProbeYes, ModelDigest: "old", ProbeVersion: CurrentToolProbeVersion}, "new", false},
		{"version mismatch", CapProbe{State: CapProbeYes, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion + 1}, "d", false},
		{"expired inconclusive", CapProbe{State: CapProbeInconclusive, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, ExpiresAt: now.Add(-time.Minute)}, "d", false},
		{"unexpired inconclusive", CapProbe{State: CapProbeInconclusive, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, ExpiresAt: now.Add(time.Hour)}, "d", true},
		{"no-expiry no", CapProbe{State: CapProbeNo, ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion}, "d", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.probe.Valid(tt.digest, now); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.digest, got, tt.want)
			}
		})
	}
}

func TestMigrationV2_UpgradesExistingV1DB(t *testing.T) {
	// Simulate a v1 database, then re-run migrations and use the new table.
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/fp.db")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := NewStore(context.Background(), db); err != nil { // runs all migrations
		t.Fatalf("initial store: %v", err)
	}
	// Second open must be idempotent.
	s, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	in := CapProbe{BackendID: "b", ModelName: "m", Capability: "tool_call",
		State: CapProbeYes, ModelDigest: "d", ProbeVersion: 1, TestedAt: time.Now()}
	if err := s.SaveCapProbe(context.Background(), in); err != nil {
		t.Fatalf("save after re-migrate: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./fingerprint/ -run 'CapProbe|MigrationV2' -v`
Expected: FAIL — `undefined: CapProbe`, `undefined: CapProbeYes`, etc.

- [ ] **Step 3: Implement types + migration + store methods**

Append to `fingerprint/fingerprint.go`:

```go
// CapProbeState is the tri-state outcome of a capability probe.
// The empty string means "unknown" (never probed / cache miss) and is
// never persisted.
type CapProbeState string

const (
	CapProbeYes          CapProbeState = "yes"
	CapProbeNo           CapProbeState = "no"
	CapProbeInconclusive CapProbeState = "inconclusive"
)

// CurrentToolProbeVersion identifies the tool-call probe request shape.
// Bump when the probe protocol changes (tool definition, prompt,
// tool_choice escalation); cached rows from other versions are ignored.
const CurrentToolProbeVersion = 1

// CapProbeInconclusiveTTL bounds how long an inconclusive verdict is
// trusted before the next demand re-probes.
const CapProbeInconclusiveTTL = 24 * time.Hour

// CapProbeDigestlessNoTTL bounds a negative verdict for models with no
// content digest (openai-compat fallback keying): a wedged "no" silently
// blocks usage, so digestless negatives expire rather than sticking until
// a manual --reprobe.
const CapProbeDigestlessNoTTL = 7 * 24 * time.Hour

// CapProbe is one persisted capability-probe verdict, keyed by
// (backend_id, model_name, capability). Distinct from Profile so
// capability-only resolution can never masquerade as a complete
// fingerprint profile.
type CapProbe struct {
	BackendID    string
	ModelName    string
	Capability   string // canonical token, e.g. "tool_call"
	State        CapProbeState
	ModelDigest  string    // runtime digest, or key fallback when digestless
	ProbeVersion int       // CurrentToolProbeVersion at probe time
	TestedAt     time.Time
	ExpiresAt    time.Time // zero = does not expire
}

// Valid reports whether the cached probe still applies for the given
// current digest at time now: digest and probe version must match and the
// row must not be expired.
func (p CapProbe) Valid(currentDigest string, now time.Time) bool {
	if p.ModelDigest != currentDigest {
		return false
	}
	if p.ProbeVersion != CurrentToolProbeVersion {
		return false
	}
	if !p.ExpiresAt.IsZero() && now.After(p.ExpiresAt) {
		return false
	}
	return true
}
```

Append migration in `fingerprint/migration.go` (add to the `migrations` slice and define the function):

```go
	{
		version:     2,
		description: "capability_probes table for tri-state capability verdicts",
		fn:          migrateV2,
	},
```

```go
func migrateV2(tx *sql.Tx) error {
	const stmt = `CREATE TABLE IF NOT EXISTS capability_probes (
		backend_id    TEXT NOT NULL,
		model_name    TEXT NOT NULL,
		capability    TEXT NOT NULL,
		state         TEXT NOT NULL,
		model_digest  TEXT NOT NULL,
		probe_version INTEGER NOT NULL,
		tested_at     INTEGER NOT NULL,
		expires_at    INTEGER,
		PRIMARY KEY (backend_id, model_name, capability)
	)`
	if _, err := tx.Exec(stmt); err != nil {
		return fmt.Errorf("fingerprint: migrate v2: %w", err)
	}
	return nil
}
```

Add a narrow `CapProbeStore` interface and `SQLiteStore` methods in
`fingerprint/store.go`. Do NOT add these methods to `Store`: capability-only
Golem wiring should not force every full fingerprint-profile fake to implement
capability-probe persistence.

```go
// Store defines fingerprint persistence operations.
type Store interface {
	Get(ctx context.Context, backendID, modelName string) (*Profile, error)
	GetFailure(ctx context.Context, backendID, modelName string) (*FailureInfo, error)
	Save(ctx context.Context, profile Profile) error
	NeedsFingerprint(ctx context.Context, backendID, modelName, currentDigest string) (bool, error)
	SaveFailure(ctx context.Context, backendID, modelName, modelDigest, errMsg string) error
}

// CapProbeStore defines capability-probe persistence operations.
// It is intentionally separate from Store so capability-only resolution
// never affects NeedsFingerprint / IncompleteCapabilities semantics and
// test fakes for full fingerprint profiles do not need cap-probe methods.
type CapProbeStore interface {
	GetCapProbe(ctx context.Context, backendID, modelName, capability string) (*CapProbe, error)
	SaveCapProbe(ctx context.Context, probe CapProbe) error
	DeleteCapProbes(ctx context.Context, backendID, modelName string) error
}

var _ CapProbeStore = (*SQLiteStore)(nil)
```

```go
// GetCapProbe retrieves a capability-probe row. Returns ErrNotFound when
// no row exists for the key.
func (s *SQLiteStore) GetCapProbe(ctx context.Context, backendID, modelName, capability string) (*CapProbe, error) {
	var p CapProbe
	var testedAtMs int64
	var expiresAtMs sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT backend_id, model_name, capability, state, model_digest,
			probe_version, tested_at, expires_at
		FROM capability_probes
		WHERE backend_id = ? AND model_name = ? AND capability = ?`,
		backendID, modelName, capability,
	).Scan(&p.BackendID, &p.ModelName, &p.Capability, &p.State, &p.ModelDigest,
		&p.ProbeVersion, &testedAtMs, &expiresAtMs)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("fingerprint: get cap probe %q/%q/%q: %w", backendID, modelName, capability, err)
	}
	p.TestedAt = time.UnixMilli(testedAtMs)
	if expiresAtMs.Valid {
		p.ExpiresAt = time.UnixMilli(expiresAtMs.Int64)
	}
	return &p, nil
}

// SaveCapProbe inserts or replaces the probe row for its key.
func (s *SQLiteStore) SaveCapProbe(ctx context.Context, probe CapProbe) error {
	var expiresAt any // nil => NULL (no expiry)
	if !probe.ExpiresAt.IsZero() {
		expiresAt = probe.ExpiresAt.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO capability_probes
			(backend_id, model_name, capability, state, model_digest,
			 probe_version, tested_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		probe.BackendID, probe.ModelName, probe.Capability, string(probe.State),
		probe.ModelDigest, probe.ProbeVersion, probe.TestedAt.UnixMilli(), expiresAt)
	if err != nil {
		return fmt.Errorf("fingerprint: save cap probe %q/%q/%q: %w",
			probe.BackendID, probe.ModelName, probe.Capability, err)
	}
	return nil
}

// DeleteCapProbes removes all capability rows for a model (used by
// `golem models --reprobe`).
func (s *SQLiteStore) DeleteCapProbes(ctx context.Context, backendID, modelName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM capability_probes WHERE backend_id = ? AND model_name = ?`,
		backendID, modelName)
	if err != nil {
		return fmt.Errorf("fingerprint: delete cap probes %q/%q: %w", backendID, modelName, err)
	}
	return nil
}
```

Run a full build. No existing `fingerprint.Store` fake should need cap-probe
methods unless that test opts into capability resolution; tests that do opt in
should fake only `fingerprint.CapProbeStore`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./fingerprint/... -v` then `env -u GOROOT go build ./...`
Expected: PASS; no unrelated full-profile Store fake churn.

- [ ] **Step 5: Commit**

```bash
git add fingerprint/
git commit -m "feat(fingerprint): capability_probes table + tri-state CapProbe API (schema v2)"
```

---

### Task 2: Registry capability floor (`SetCapabilityFloor`)

**Files:**
- Modify: `provider/model_registry.go`
- Test: `provider/model_registry_test.go` (append)

- [ ] **Step 1: Write failing tests**

Follow the existing test-harness patterns in `provider/model_registry_test.go`
(fake ProviderResolver returning canned ModelInfo). Cases:

```go
func TestSetCapabilityFloor_ORsBelowCatalogAndOverride(t *testing.T) {
	// Model with a catalog family hit that carries "tools" (e.g. "qwen3:8b").
	// Floor supplies chat|generate|stream. Expect final caps to contain
	// CapToolCall (catalog survives) AND the floor bits.
	reg := newTestRegistryWithModel(t, "llamacpp", "qwen3:8b") // helper mirrors existing tests
	reg.SetCapabilityFloor(func(key ModelKey) []string {
		return []string{"chat", "generate", "stream"}
	})
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	want := CapChat | CapGenerate | CapStream | CapToolCall
	if !p.Caps.Has(want) {
		t.Fatalf("caps = %v, want superset of %v", p.Caps, want)
	}
}

func TestSetCapabilityFloor_ExplicitOverrideStillReplaces(t *testing.T) {
	// Floor + catalog give tool_call; explicit override without tool_call
	// must win (declared set is authoritative both directions).
	reg := newTestRegistryWithModel(t, "llamacpp", "qwen3:8b")
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "generate", "stream"} })
	reg.SetCapabilityOverride(func(ModelKey) []string { return []string{"chat", "stream"} })
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "qwen3:8b"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if p.Caps != (CapChat | CapStream) {
		t.Fatalf("caps = %v, want exactly chat|stream", p.Caps)
	}
}

func TestSetCapabilityFloor_InvalidTokensDroppedWithHook(t *testing.T) {
	// Non-canonical floor tokens: rejection hook fires, caps unchanged
	// (mirror override corner-case handling; never zero or partially apply).
	reg := newTestRegistryWithModel(t, "llamacpp", "unknown-model")
	var hookKey ModelKey
	reg.SetOverrideRejectionHook(func(key ModelKey, tokens []string, err error) { hookKey = key })
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"completion"} }) // alias => strict-reject
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	_ = p
	if hookKey.Model != "unknown-model" {
		t.Fatalf("rejection hook not fired for floor tokens")
	}
}

func TestSetCapabilityFloor_InvalidatesCacheAndGuardsVersion(t *testing.T) {
	// Same shape as the existing SetCapabilityOverride TOCTOU/invalidations
	// tests: Lookup once (cache warm), SetCapabilityFloor, Lookup again and
	// expect the floor applied (cache flushed).
	reg := newTestRegistryWithModel(t, "llamacpp", "unknown-model")
	if _, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"}); err != nil {
		t.Fatalf("warm lookup: %v", err)
	}
	reg.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
	p, err := reg.Lookup(context.Background(), ModelKey{Provider: "llamacpp", Model: "unknown-model"})
	if err != nil {
		t.Fatalf("lookup after floor: %v", err)
	}
	if !p.Caps.Has(CapChat | CapStream) {
		t.Fatalf("floor not applied after cache flush: %v", p.Caps)
	}
}
```

If `newTestRegistryWithModel` doesn't exist, build it on the existing fake
resolver used by other registry tests in that file (do not invent a new fake).

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./provider/ -run SetCapabilityFloor -v`
Expected: FAIL — `undefined: reg.SetCapabilityFloor`.

- [ ] **Step 3: Implement the floor**

In `provider/model_registry.go`:

1. Add field + type + setter (mirror `SetCapabilityOverride` exactly, sharing
   `overrideVersion` as the single policy version counter so a floor change
   also invalidates in-flight buildProfile writes):

```go
// CapabilityFloor returns baseline canonical capability tokens for a model
// key, or nil when none. Floor caps are OR-merged into the profile at the
// LOWEST precedence (with the static catalog layer): they guarantee a
// minimum capability set for models whose provider exposes no capability
// metadata (openai-compat), WITHOUT erasing catalog/fingerprint/runtime
// additions the way the REPLACE override does. Tokens must be canonical
// (ParseCapsStrict); invalid slices are dropped with the rejection hook
// fired and never zero or shrink the profile.
type CapabilityFloor func(key ModelKey) []string
```

```go
	capFloor        CapabilityFloor // field beside capOverride
```

```go
// SetCapabilityFloor installs (or clears) the capability floor hook.
// Shares SetCapabilityOverride's invalidation + version-guard semantics:
// the profile cache is flushed and the policy version bumped so in-flight
// merges under the old floor cannot repopulate the cache.
func (r *ModelRegistry) SetCapabilityFloor(fn CapabilityFloor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capFloor = fn
	r.overrideVersion++
	clear(r.profiles)
}
```

2. `buildProfile`: snapshot the floor alongside the override:

```go
	r.mu.RLock()
	override := r.capOverride
	floor := r.capFloor
	overrideVer := r.overrideVersion
	rejectionHook := r.rejectionHook
	r.mu.RUnlock()
```

Pass `floor` through to `merge` (add a parameter).

3. In `merge`, apply the floor right after the static-catalog block (lowest
   precedence, pure OR):

```go
	// Capability floor (lowest precedence, OR-merge). Guarantees baseline
	// caps for providers that expose no capability metadata without the
	// REPLACE override's erasure semantics. Invalid tokens are dropped with
	// the rejection hook fired — a floor must never zero or shrink caps.
	if floor != nil {
		if floorTokens := floor(key); len(floorTokens) > 0 {
			floorCaps, err := ParseCapsStrict(floorTokens)
			switch {
			case err != nil:
				if rejectionHook != nil {
					rejectionHook(key, floorTokens, err)
				}
			case floorCaps != 0:
				profile.Caps |= floorCaps
			}
		}
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./provider/ -v`
Expected: PASS (all existing registry tests too — floor is additive).

- [ ] **Step 5: Commit**

```bash
git add provider/model_registry.go provider/model_registry_test.go
git commit -m "feat(provider): capability floor seam - OR-merge baseline caps below catalog/override"
```

---

### Task 3: providerbootstrap floor/override split

**Files:**
- Modify: `internal/providerbootstrap/capabilities.go`
- Modify: `internal/providerbootstrap/bootstrap.go` (install call)
- Test: `internal/providerbootstrap/capabilities_test.go` (append)

- [ ] **Step 1: Write failing tests**

```go
func TestBuildCapabilityFloors_DerivedOpenAICompatOnly(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			"ollama":   {BaseURL: "http://localhost:11434"},
		},
		Models: map[string]config.ModelConfig{
			"agent":   {Provider: "llamacpp", Name: "byo-model", Type: "dense"}, // undeclared => floor
			"chat":    {Provider: "llamacpp", Name: "declared", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}}, // explicit => override, NOT floor
			"embed":   {Provider: "ollama", Name: "qwen3-embedding:8b", Type: "embedding"}, // ollama undeclared => neither
	},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	if got := floors[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; len(got) == 0 {
		t.Fatalf("undeclared openai-compat model missing floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; ok {
		t.Fatalf("explicitly declared model must not get a floor")
	}
	if _, ok := floors[provider.ModelKey{Provider: "ollama", Model: "qwen3-embedding:8b"}]; ok {
		t.Fatalf("ollama model must not get a floor")
	}

	overrides, err := buildCapabilityOverrides(cfg)
	if err != nil {
		t.Fatalf("overrides: %v", err)
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "byo-model"}]; ok {
		t.Fatalf("undeclared openai-compat model must no longer get a REPLACE override")
	}
	if _, ok := overrides[provider.ModelKey{Provider: "llamacpp", Model: "declared"}]; !ok {
		t.Fatalf("explicit declaration must remain an override")
	}
}

func TestBuildCapabilityFloors_SharedKeyUnionsDerived(t *testing.T) {
	// Two roles, same key, different types => floors union (OR semantics
	// make conflicts impossible; no error path for floors).
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"llamacpp": {APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "llamacpp", Name: "m", Type: "dense"},
			"embed": {Provider: "llamacpp", Name: "m", Type: "embedding"},
		},
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		t.Fatalf("floors: %v", err)
	}
	got := floors[provider.ModelKey{Provider: "llamacpp", Model: "m"}]
	// dense derives chat,generate,stream; embedding derives embed => union of all four.
	want := map[string]bool{"chat": true, "generate": true, "stream": true, "embed": true}
	if len(got) != len(want) {
		t.Fatalf("floor union = %v, want keys %v", got, want)
	}
	for _, tok := range got {
		if !want[tok] {
			t.Fatalf("unexpected floor token %q in %v", tok, got)
		}
	}
}
```

Also UPDATE existing tests that assert derived openai-compat caps arrive as
overrides (they now arrive as floors) — run the package tests and adjust
assertions that break, preserving their intent.

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ -run CapabilityFloors -v`
Expected: FAIL — `undefined: buildCapabilityFloors`.

- [ ] **Step 3: Implement the split**

In `internal/providerbootstrap/capabilities.go`:

1. `buildCapabilityOverrides`: DELETE the derived-caps branch (the
   `if len(caps) == 0 { ... apiFormat != "openai-compat" ... caps = m.ResolvedCapabilities() }`
   block, lines 68-81). Overrides are now explicit-only:

```go
	for _, role := range roles {
		m := cfg.Models[role]
		if m.Provider == "" || m.Name == "" {
			continue
		}
		caps := m.Capabilities
		if len(caps) == 0 {
			continue // derived caps are floors now, not overrides
		}
		// ... (ParseCapsStrict + conflict detection unchanged)
```

2. New `buildCapabilityFloors` (derived, openai-compat, undeclared only;
   union across roles for a shared key):

```go
// buildCapabilityFloors computes baseline capability floors for undeclared
// openai-compat models: their provider exposes no capability metadata, so
// the type-derived set guarantees routability. Floors OR-merge in the
// registry (lowest precedence), so unlike the old REPLACE override they
// cannot erase catalog/fingerprint/runtime capabilities (issue #219 root
// cause). Shared keys across roles union their derived sets — OR semantics
// make conflicts impossible.
func buildCapabilityFloors(cfg *config.Config) (map[provider.ModelKey][]string, error) {
	out := make(map[provider.ModelKey][]string)
	if cfg == nil {
		return out, nil
	}
	union := make(map[provider.ModelKey]map[string]bool)
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		m := cfg.Models[role]
		if m.Provider == "" || m.Name == "" || len(m.Capabilities) > 0 {
			continue // explicit declarations are overrides, not floors
		}
		pCfg, ok := cfg.Providers[m.Provider]
		if !ok {
			continue
		}
		apiFormat := pCfg.APIFormat
		if apiFormat == "" {
			apiFormat = "ollama"
		}
		if apiFormat != "openai-compat" {
			continue
		}
		derived := m.ResolvedCapabilities()
		if len(derived) == 0 {
			continue
		}
		if _, err := provider.ParseCapsStrict(derived); err != nil {
			return nil, fmt.Errorf("providerbootstrap: model %q derived floor for %s/%s: %w", role, m.Provider, m.Name, err)
		}
		key := provider.ModelKey{Provider: m.Provider, Model: m.Name}
		if union[key] == nil {
			union[key] = make(map[string]bool)
		}
		for _, tok := range derived {
			union[key][tok] = true
		}
	}
	for key, set := range union {
		toks := make([]string, 0, len(set))
		for tok := range set {
			toks = append(toks, tok)
		}
		sort.Strings(toks)
		out[key] = toks
	}
	return out, nil
}

// installCapabilityFloors installs the computed floors onto the registry.
func installCapabilityFloors(mr *provider.ModelRegistry, cfg *config.Config) error {
	if mr == nil || cfg == nil {
		return nil
	}
	floors, err := buildCapabilityFloors(cfg)
	if err != nil {
		return err
	}
	if len(floors) == 0 {
		return nil
	}
	mr.SetCapabilityFloor(func(key provider.ModelKey) []string {
		caps, ok := floors[key]
		if !ok {
			return nil
		}
		out := make([]string, len(caps))
		copy(out, caps)
		return out
	})
	return nil
}
```

3. `bootstrap.go` `New()`: install floors right after overrides:

```go
	if err := installCapabilityOverrides(mr, effCfg); err != nil {
		return nil, err
	}
	if err := installCapabilityFloors(mr, effCfg); err != nil {
		return nil, err
	}
```

IMPORTANT side effect to preserve: `capabilitiesForKey` (used by the prober
factory as a capability HINT) intentionally still returns explicit-or-derived
caps — do not change it.

- [ ] **Step 4: Run tests + full build**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ ./provider/ -v`
Then run: `env -u GOROOT go build ./...`
Expected: PASS after updating any existing derived-override assertions.

- [ ] **Step 5: Commit**

```bash
git add internal/providerbootstrap/
git commit -m "fix(providerbootstrap): derived openai-compat caps become floors, not REPLACE overrides (#219 root cause)"
```

---

### Task 4: openai-compat tool-call probe (two-attempt protocol)

**Files:**
- Modify: `fingerprint/fingerprint.go` (CapProbeOutcome + ToolCallProber)
- Modify: `fingerprint/probers/openaicompat.go` (ProbeToolCall)
- Modify: `provider/types.go` (`ChatRequest.ToolChoice`)
- Modify: `provider/openaicompat/types.go` (`chatRequest.ToolChoice`)
- Modify: `provider/openaicompat/openaicompat.go` (copy ToolChoice to wire request)
- Test: `fingerprint/probers/openaicompat_toolprobe_test.go` (create)

- [ ] **Step 1: Define the shared probe-outcome contract (small, no test yet)**

Append to `fingerprint/fingerprint.go`:

```go
// CapProbeOutcome is a classified probe verdict ready to persist.
// TTL zero means the row does not expire.
type CapProbeOutcome struct {
	State  CapProbeState
	TTL    time.Duration
	Detail string // short human-readable classification, for diagnostics
}

// ToolCallProber is an optional interface a ModelProber may implement to
// actively determine tool-call support. A non-nil error means the probe
// was transient/diagnostic (network failure, auth, rate limit) and MUST
// NOT be persisted; the outcome is only meaningful when err is nil.
type ToolCallProber interface {
	ProbeToolCall(ctx context.Context, model string) (CapProbeOutcome, error)
}
```

(`fingerprint.go` needs `"context"` added to its imports.)

- [ ] **Step 2: Write failing tests for the probe matrix**

Use `httptest.NewServer` with a scripted handler; construct the prober via
`openaicompat.New(...)` pointed at the test server (mirror the construction
already used in `fingerprint/probers/openaicompat_test.go`).

```go
// fingerprint/probers/openaicompat_toolprobe_test.go
package probers

// Test matrix (table-driven, one scripted handler per case).
// Each case scripts attempt-1 (and attempt-2 when reached) responses and
// asserts: outcome.State, outcome.TTL, err != nil (transient), and the
// number of HTTP requests received.
//
//  name                          attempt1                attempt2      wantState        wantTTL                        wantErr  wantReqs
//  yes_on_required               200 tool_calls          -             CapProbeYes      0                              false    1
//  inconclusive_on_required      200 no tool_calls       -             CapProbeInconclusive CapProbeInconclusiveTTL    false    1
//  escalate_then_yes             400                     200 calls     CapProbeYes      0                              false    2
//  escalate_then_inconclusive    422                     200 no calls  CapProbeInconclusive CapProbeInconclusiveTTL    false    2
//  no_when_tools_rejected        400                     400           CapProbeNo       0                              false    2
//  auth_diagnostic_not_persisted 401                     -             -                -                              true     1
//  not_found_diagnostic          404                     -             -                -                              true     1
//  rate_limited_diagnostic       429                     -             -                -                              true     1
//  server_error_transient        500                     -             -                -                              true     1
//  escalated_then_500            400                     500           -                -                              true     2
//  network_error_transient       (server closed)         -             -                -                              true     0
//
// Also assert on the recorded attempt-1 request body:
//   - tools[0].function.name == "get_time"
//   - tool_choice == "required"
//   - stream absent/false, max_tokens == 128, temperature == 0
// and that attempt-2's body has NO tool_choice field but still has tools.
```

Write the full table + handler; keep each scripted response a plain
`chat.completions` JSON body (copy the response shape from
`fingerprint/probers/openaicompat_test.go`'s existing chat fixtures, adding
`"tool_calls": [{"id":"1","type":"function","function":{"name":"get_time","arguments":"{}"}}]`
for the tool-call cases).

- [ ] **Step 3: Run tests to verify they fail**

Run: `env -u GOROOT go test ./fingerprint/probers/ -run ToolProbe -v`
Expected: FAIL — `ProbeToolCall` undefined.

- [ ] **Step 4: Implement ProbeToolCall**

Append to `fingerprint/probers/openaicompat.go`:

```go
// toolProbeTimeout bounds one tool-call probe request. It must absorb a
// llama-swap model load (seconds to tens of seconds for large models),
// unlike the #218 port-scan probe's 800ms budget.
const toolProbeTimeout = 30 * time.Second

// probeToolSchema is the minimal no-arg function used to elicit a call.
var probeToolSchema = provider.Tool{
	Type: "function",
	Function: provider.ToolFunction{
		Name:        "get_time",
		Description: "Get the current time.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	},
}

// ProbeToolCall actively determines tool-call support with at most two
// chat-completions requests (spec section 3):
//
//	attempt 1: tools + tool_choice "required" (deterministic elicitation)
//	attempt 2 (only after attempt-1 400/422): tools without tool_choice,
//	           prompt-engineered — some servers reject tool_choice but
//	           accept tools.
//
// Verdicts: 200 with tool_calls => yes; 200 without => inconclusive
// (short TTL: the model ignored the request, which varies by template);
// 400/422 on attempt 2 => no (the tools array itself is rejected).
// Auth/endpoint/rate-limit statuses and transport failures return a
// non-nil error — diagnostic only, never persisted.
func (p *OpenAICompatProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	if p == nil || p.prov == nil {
		return fingerprint.CapProbeOutcome{}, fmt.Errorf("fingerprint: openaicompat tool probe %q: provider is required", model)
	}
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()

	outcome, retry, err := p.toolProbeAttempt(ctx, model, true)
	if err != nil || !retry {
		return outcome, err
	}
	// Attempt 1 was rejected with 400/422: the server may object to
	// tool_choice rather than tools. Re-try without tool_choice; a second
	// 400/422 means the tools array itself is unsupported => hard no.
	outcome, retry, err = p.toolProbeAttempt(ctx, model, false)
	if err != nil {
		return outcome, err
	}
	if retry {
		return fingerprint.CapProbeOutcome{
			State:  fingerprint.CapProbeNo,
			Detail: "server rejected tools request (400/422 on both attempts)",
		}, nil
	}
	return outcome, nil
}

// toolProbeAttempt runs one probe request. retry=true signals a 400/422
// rejection that the caller may escalate past (attempt 1) or convert to a
// hard no (attempt 2).
func (p *OpenAICompatProber) toolProbeAttempt(ctx context.Context, model string, forceToolChoice bool) (fingerprint.CapProbeOutcome, bool, error) {
	temp := 0.0
	req := provider.ChatRequest{
		Model: model,
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "Call the get_time tool to get the current time."},
		},
		Tools: []provider.Tool{probeToolSchema},
		Options: provider.ModelOptions{
			NumPredict:  128, // tool-call JSON needs room; truncation reads as a false negative
			Temperature: &temp,
		},
	}
	if forceToolChoice {
		req.ToolChoice = "required"
	}

	resp, err := p.prov.Chat(ctx, req)
	if err != nil {
		var hs interface{ HTTPStatusCode() int }
		if errors.As(err, &hs) {
			switch code := hs.HTTPStatusCode(); {
			case code == 400 || code == 422:
				return fingerprint.CapProbeOutcome{}, true, nil
			default:
				// 401/403/404/405/429, 5xx, anything else with a status:
				// says nothing about the model. Diagnostic, not persisted.
				return fingerprint.CapProbeOutcome{}, false, fmt.Errorf("fingerprint: openaicompat tool probe %q: %w", model, err)
			}
		}
		// Transport-level failure (network, timeout, cancel): transient.
		return fingerprint.CapProbeOutcome{}, false, fmt.Errorf("fingerprint: openaicompat tool probe %q: %w", model, err)
	}
	if len(resp.ToolCalls) > 0 {
		return fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes, Detail: "model produced a tool call"}, false, nil
	}
	return fingerprint.CapProbeOutcome{
		State:  fingerprint.CapProbeInconclusive,
		TTL:    fingerprint.CapProbeInconclusiveTTL,
		Detail: "200 response without tool_calls",
	}, false, nil
}
```

Add the required request plumbing before running the probe tests:

```go
// provider/types.go, ChatRequest
ToolChoice string `json:"tool_choice,omitempty"` // currently honored by openai-compat
```

```go
// provider/openaicompat/types.go, chatRequest
ToolChoice string `json:"tool_choice,omitempty"`
```

```go
// provider/openaicompat/openaicompat.go, toChatRequest
r := chatRequest{
	Model:      req.Model,
	Messages:   msgs,
	Stream:     stream,
	Tools:      toWireTools(req.Tools),
	ToolChoice: req.ToolChoice,
}
```

Add a focused openai-compat request-conversion test (or extend an existing one)
that `provider.ChatRequest{ToolChoice: "required"}` serializes
`"tool_choice":"required"` and that the zero value omits the field.

- [ ] **Step 5: Run tests to verify they pass**

Run: `env -u GOROOT go test ./fingerprint/... ./provider/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add fingerprint/ provider/
git commit -m "feat(fingerprint): active openai-compat tool-call probe with two-attempt classification"
```

---

### Task 5: Ollama passive tool-call probe

**Files:**
- Modify: `fingerprint/probe.go` (OllamaProber.ProbeToolCall)
- Test: `fingerprint/probe_test.go` (append)

- [ ] **Step 1: Write failing tests**

`OllamaProber` already talks to a client interface with `ShowModel`
(inspect `fingerprint/probe.go` — `DetectKind` reads `/api/show` capabilities
through `p.client.ShowModel(ctx, model)`; reuse exactly that call path and its
existing test fake). Cases:

```go
func TestOllamaProbeToolCall_CapabilitiesWithTools(t *testing.T) {
	// fake /api/show returns Capabilities: ["completion","tools"]
	// => CapProbeYes, TTL 0, err nil
}

func TestOllamaProbeToolCall_CapabilitiesWithoutTools(t *testing.T) {
	// fake /api/show returns Capabilities: ["completion"]
	// => CapProbeNo, TTL 0, err nil (array present and lacks tools: curated no)
}

func TestOllamaProbeToolCall_MissingCapabilitiesArray(t *testing.T) {
	// fake /api/show returns Capabilities: nil (older Ollama)
	// => CapProbeInconclusive, TTL CapProbeInconclusiveTTL, err nil
	// (never persist a hard no on missing data)
}

func TestOllamaProbeToolCall_ShowError(t *testing.T) {
	// fake /api/show errors => err non-nil (transient, not persisted)
}
```

Write them against the same fake client type the existing OllamaProber tests
use, asserting `fingerprint.CapProbeOutcome` fields.

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./fingerprint/ -run OllamaProbeToolCall -v`
Expected: FAIL — method undefined.

- [ ] **Step 3: Implement**

```go
// ProbeToolCall determines tool support passively from /api/show model
// metadata — no generation request, no model load. A present capabilities
// array is authoritative in both directions; a missing array (older
// Ollama) is inconclusive, never a hard no.
func (p *OllamaProber) ProbeToolCall(ctx context.Context, model string) (CapProbeOutcome, error) {
	info, err := p.client.ShowModel(ctx, model) // match the exact call DetectKind uses
	if err != nil {
		return CapProbeOutcome{}, fmt.Errorf("fingerprint: ollama tool probe %q: %w", model, err)
	}
	if info.Capabilities == nil {
		return CapProbeOutcome{
			State:  CapProbeInconclusive,
			TTL:    CapProbeInconclusiveTTL,
			Detail: "ollama /api/show has no capabilities array",
		}, nil
	}
	for _, c := range info.Capabilities {
		if c == "tools" {
			return CapProbeOutcome{State: CapProbeYes, Detail: "ollama capabilities lists tools"}, nil
		}
	}
	return CapProbeOutcome{State: CapProbeNo, Detail: "ollama capabilities array lacks tools"}, nil
}
```

Match receiver/client names to the real `OllamaProber` (read `fingerprint/probe.go` first).

- [ ] **Step 4: Run tests, then commit**

Run: `env -u GOROOT go test ./fingerprint/... -v` — Expected: PASS.

```bash
git add fingerprint/
git commit -m "feat(fingerprint): passive ollama tool-call probe via /api/show capabilities"
```

---

### Task 6: Registry resolver — `ResolveToolCall` + merge integration + `EnsureToolCallResolved`

**Files:**
- Modify: `provider/model_registry.go`
- Test: `provider/capresolve_test.go` (create)

- [ ] **Step 1: Write failing tests**

Fake `fingerprint.CapProbeStore` (in-memory map implementing only the three
cap-probe methods) and a fake prober factory whose prober implements
`fingerprint.ToolCallProber` with scripted outcomes + a call counter. Cases:

```go
// All tests construct: NewModelRegistry(fakeResolver, nil,
//   WithCapabilityProbeStore(fakeStore),
//   WithCapabilityProber(fakeFactory)) — the new options.

func TestResolveToolCall_ExplicitDeclarationSkipsProbe(t *testing.T) {
	// SetCapabilityOverride includes tool_call for the key =>
	// ResolveToolCall returns CapProbeYes, prober call count == 0.
	// Override WITHOUT tool_call => CapProbeNo, count == 0.
}

func TestResolveToolCall_MergedCapsShortCircuit(t *testing.T) {
	// Catalog-hit model (qwen3:8b => catalog tools) => CapProbeYes, count == 0.
}

func TestResolveToolCall_CacheHitSkipsProbe(t *testing.T) {
	// Pre-seed store with a valid yes row (digest = key fallback,
	// ProbeVersion current) => CapProbeYes, count == 0.
}

func TestResolveToolCall_StaleCacheReprobes(t *testing.T) {
	// Pre-seed with ProbeVersion-1 row => probe runs (count == 1),
	// new row persisted with current version.
}

func TestResolveToolCall_ProbePersistsAndInvalidatesProfile(t *testing.T) {
	// Unknown model, prober scripted yes:
	//  1. Lookup => profile lacks CapToolCall.
	//  2. ResolveToolCall => CapProbeYes, store row saved with
	//     Capability "tool_call", TTL zero.
	//  3. Lookup again => profile NOW has CapToolCall (cache invalidated,
	//     merge read the yes row).
}

func TestResolveToolCall_TransientErrorNotPersisted(t *testing.T) {
	// Prober scripted to return error => state "", err non-nil,
	// store has no row, count == 1; second call probes again (count == 2).
}

func TestResolveToolCall_SingleflightDedup(t *testing.T) {
	// Prober blocks on a channel; launch 8 concurrent ResolveToolCall for
	// the same key; release; assert count == 1 and all callers got yes.
}

func TestResolveToolCall_DisabledReturnsUnknown(t *testing.T) {
	// Registry WITHOUT WithCapabilityProber => state "", nil error, no probe.
}

func TestEnsureToolCallResolved_FiltersAndRefreshes(t *testing.T) {
	// Three profiles: one with CapToolCall, one probe-yes, one probe-no.
	// required = CapChat|CapStream|CapToolCall.
	// EnsureToolCallResolved returns refreshed profiles where the probe-yes
	// model now carries CapToolCall; the probe-no model is returned
	// unchanged (caller's gate drops it); the capable one untouched
	// (count for it == 0).
}

func TestLookupAndRecommendStayPure(t *testing.T) {
	// With WithCapabilityProber installed: plain Lookup and Recommend on an
	// unknown model never invoke the prober (count == 0).
}
```

Write them fully (the fake cap-probe store is ~40 lines; put it in this test
file). Do not fake the full `fingerprint.Store` unless a test specifically
needs full profiling behavior.

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./provider/ -run 'ResolveToolCall|EnsureToolCall|StayPure' -v`
Expected: FAIL — `WithCapabilityProbeStore` / `WithCapabilityProber` undefined.

- [ ] **Step 3: Implement**

In `provider/model_registry.go`:

```go
// WithCapabilityProbeStore installs the narrow cache used by ResolveToolCall
// and the read-only cap-probe merge layer. It is intentionally separate from
// the full fingerprint Store so Golem can persist capability verdicts without
// enabling full latency/embedding/chat profiling.
func WithCapabilityProbeStore(store fingerprint.CapProbeStore) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.capProbeStore = store
	}
}

// WithCapabilityProber installs provider-aware prober selection used ONLY
// for on-demand capability resolution (ResolveToolCall). Unlike
// WithFingerprintProberFactory it never triggers full profiling from
// Lookup: Lookup/Recommend stay pure, and active probes run only at the
// two named call sites (golem preflight, router candidate resolution).
func WithCapabilityProber(fn FingerprintProberFactory) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.capProber = fn
	}
}
```

Fields: `capProbeStore fingerprint.CapProbeStore`,
`capProber FingerprintProberFactory`, and `capResolveGroup singleflight.Group`.
Import `golang.org/x/sync/singleflight`; the module already uses it in
`fingerprint/profiler.go` and provider packages, so this is not a new
dependency.

```go
// ResolveToolCall resolves the tri-state tool_call capability for a model:
// explicit declaration > merged caps > valid cache row > active probe.
// Returns "" (unknown) without probing when no capability prober is
// installed or the provider has none. A non-nil error is transient
// (probe/transport failure) and nothing was persisted.
func (r *ModelRegistry) ResolveToolCall(ctx context.Context, key ModelKey) (fingerprint.CapProbeState, error) {
	// 1. Explicit declaration is authoritative in both directions.
	r.mu.RLock()
	override := r.capOverride
	r.mu.RUnlock()
	if override != nil {
		if toks := override(key); len(toks) > 0 {
			if caps, err := ParseCapsStrict(toks); err == nil && caps != 0 {
				if caps.Has(CapToolCall) {
					return fingerprint.CapProbeYes, nil
				}
				return fingerprint.CapProbeNo, nil
			}
		}
	}

	// 2. Merged profile already carries the bit (catalog/floor/runtime/probe row).
	profile, err := r.Lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if profile.Caps.Has(CapToolCall) {
		return fingerprint.CapProbeYes, nil
	}

	if r.capProber == nil || r.capProbeStore == nil {
		return "", nil // resolution disabled: unknown, never a claim
	}

	v, err, _ := r.capResolveGroup.Do(key.String(), func() (any, error) {
		return r.resolveToolCallSlow(ctx, key)
	})
	if err != nil {
		return "", err
	}
	state, _ := v.(fingerprint.CapProbeState)
	return state, nil
}
```

`resolveToolCallSlow` (called inside the singleflight body):

```go
func (r *ModelRegistry) resolveToolCallSlow(ctx context.Context, key ModelKey) (fingerprint.CapProbeState, error) {
	p, err := r.providers.Resolve(key)
	if err != nil {
		return "", err
	}
	runtimeInfo, err := r.queryRuntime(ctx, key)
	if err != nil {
		return "", err
	}
	spec, err := r.capProber(ctx, key, runtimeInfo, p)
	if err != nil || spec == nil || spec.Prober == nil {
		return "", err
	}
	tcp, ok := spec.Prober.(fingerprint.ToolCallProber)
	if !ok {
		return "", nil // provider has no tool-call probe: unknown
	}

	digest := spec.ModelDigest
	if digest == "" {
		digest = key.String()
	}
	backendID := key.Provider // match EnsureProfile's existing backend_id convention

	// 3. Valid cache row.
	now := time.Now()
	if row, gerr := r.capProbeStore.GetCapProbe(ctx, backendID, key.Model, "tool_call"); gerr == nil {
		if row.Valid(digest, now) {
			return row.State, nil
		}
	}

	// 4. Active probe.
	outcome, perr := tcp.ProbeToolCall(ctx, key.Model)
	if perr != nil {
		return "", perr // transient/diagnostic: never persisted
	}
	row := fingerprint.CapProbe{
		BackendID: backendID, ModelName: key.Model, Capability: "tool_call",
		State: outcome.State, ModelDigest: digest,
		ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: now,
	}
	if outcome.TTL > 0 {
		row.ExpiresAt = now.Add(outcome.TTL)
	}
	// Digestless negatives expire (spec: a wedged no silently blocks usage).
	if outcome.State == fingerprint.CapProbeNo && digest == key.String() {
		row.ExpiresAt = now.Add(fingerprint.CapProbeDigestlessNoTTL)
	}
	if serr := r.capProbeStore.SaveCapProbe(ctx, row); serr != nil {
		// Persistence failure degrades to per-run behavior; the verdict
		// still stands for this caller.
		_ = serr
	}
	if outcome.State == fingerprint.CapProbeYes {
		r.invalidateProfile(key) // next Lookup re-merges with the yes row
	}
	return outcome.State, nil
}
```

Storage key convention: use `key.Provider` as `backend_id`, matching the
existing `EnsureProfile(ctx, key.Provider, key.Model, ...)` call in
`model_registry.go`. Do not derive a backend ID from base URL here.

Merge integration — in `buildProfile` after the fingerprint layer (only a
READ, no probe):

```go
	// Layer 3.5: capability-probe verdicts (yes rows only add bits).
	if r.capProbeStore != nil {
		digest := runtimeInfo.Digest
		if digest == "" {
			digest = key.String()
		}
		if row, err := r.capProbeStore.GetCapProbe(ctx, key.Provider, key.Model, "tool_call"); err == nil {
			if row.State == fingerprint.CapProbeYes && row.Valid(digest, time.Now()) {
				capProbeCaps = CapToolCall
			}
		}
	}
```

Thread `capProbeCaps` into `merge` as a new parameter and OR it in right after
the fingerprint block (`profile.Caps |= capProbeCaps`).

`invalidateProfile`:

```go
// invalidateProfile drops one cached profile so the next Lookup re-merges.
func (r *ModelRegistry) invalidateProfile(key ModelKey) {
	r.mu.Lock()
	delete(r.profiles, key)
	r.mu.Unlock()
}
```

`EnsureToolCallResolved` (router-facing helper):

```go
// EnsureToolCallResolved resolves tool_call for each profile that lacks it
// when required demands it, refreshing resolved-yes profiles in place.
// It never removes candidates — the caller's capability gate stays the
// single point of rejection. No-op when resolution is disabled or the
// required mask does not include CapToolCall. Errors are per-candidate and
// non-fatal (transient probe failures leave the profile unchanged).
func (r *ModelRegistry) EnsureToolCallResolved(ctx context.Context, profiles []*ModelProfile, required Capability) []*ModelProfile {
	if !r.canResolveToolCall() || !required.Has(CapToolCall) {
		return profiles
	}
	out := make([]*ModelProfile, len(profiles))
	copy(out, profiles)
	for i, p := range out {
		if p == nil || p.Caps.Has(CapToolCall) {
			continue
		}
		state, err := r.ResolveToolCall(ctx, p.Key)
		if err != nil || state != fingerprint.CapProbeYes {
			continue
		}
		if fresh, lerr := r.Lookup(ctx, p.Key); lerr == nil {
			out[i] = fresh
		}
	}
	return out
}

func (r *ModelRegistry) canResolveToolCall() bool {
	return r != nil && r.capProber != nil && r.capProbeStore != nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./provider/... -v`
Expected: PASS (including existing registry tests).

- [ ] **Step 5: Commit**

```bash
git add provider/
git commit -m "feat(provider): ResolveToolCall tri-state resolver + capability-probe merge layer"
```

---

### Task 7: Router integration (chain, recommend, tail)

**Files:**
- Modify: `provider/router.go` (`resolveCandidates`)
- Modify: `provider/router_chain.go` (`routeChain`, `recommendTailProfiles`)
- Test: `provider/router_capresolve_test.go` (create)

- [ ] **Step 1: Write failing tests**

Build on the existing router test harness (fake registry/providers used by
`router_test.go`). Cases:

```go
func TestRouteChain_UnknownCandidateProbedBeforeGate(t *testing.T) {
	// Chain ["llamacpp/byo"], RequiredCaps chat|stream|tool_call, model
	// lacks tool_call but prober scripted yes => Route succeeds and the
	// prober ran exactly once, BEFORE any feedback-snapshot read (assert
	// via ordering hooks if the harness records reads; otherwise assert
	// success + probe count == 1).
}

func TestRouteChain_ConfirmedNoSkippedWithoutProbe(t *testing.T) {
	// Store pre-seeded no-row => Route fails ErrNoViableCandidate (strict
	// chain), probe count == 0.
}

func TestResolveCandidates_RecommendStripsThenFilters(t *testing.T) {
	// Empty Model, RequiredCaps includes tool_call. Two models: A merged
	// without tool_call + prober yes; B without + prober no.
	// resolveCandidates returns A only; Recommend was called with
	// RequiredCaps &^ CapToolCall (assert A appeared as a candidate at all —
	// under the old code Recommend would have filtered it out first).
}

func TestRecommendTail_AppliesSameResolution(t *testing.T) {
	// Non-strict chain with a recommend tail: tail candidates get the same
	// strip-resolve-filter treatment.
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./provider/ -run 'RouteChain_Unknown|ConfirmedNo|RecommendStrips|RecommendTail_Applies' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`provider/router.go resolveCandidates` — empty-Model branch becomes:

```go
	if req.Model == "" {
		reqCaps := req.RequiredCaps
		if r.registry.canResolveToolCall() && reqCaps.Has(CapToolCall) {
			// Recommend filters by caps BEFORE the router sees candidates,
			// which would silently drop probe-resolvable models. Recommend
			// without the tool_call bit, resolve it lazily, then re-filter.
			candidates, err := r.registry.Recommend(ctx, RecommendOpts{
				RequiredCaps:       reqCaps &^ CapToolCall,
				AvailableRAM:       r.availableRAM,
				PreferWarm:         req.PreferWarm,
				RestrictToProvider: req.Provider,
			})
			if err != nil {
				return nil, err
			}
			candidates = r.registry.EnsureToolCallResolved(ctx, candidates, reqCaps)
			filtered := candidates[:0]
			for _, p := range candidates {
				if p.Caps.Has(CapToolCall) {
					filtered = append(filtered, p)
				}
			}
			return filtered, nil
		}
		return r.registry.Recommend(ctx, RecommendOpts{ /* unchanged */ })
	}
```

`provider/router_chain.go routeChain` — inside the chain loop, after
`resolveChainEntry` and BEFORE `snap.readCandidates` (spec: probe I/O before
feedback reads and scoring; scorers stay pure):

```go
		profiles = r.registry.EnsureToolCallResolved(ctx, profiles, req.RequiredCaps)
```

`recommendTailProfiles` — same strip-resolve-filter dance as
`resolveCandidates` (it calls `Recommend` at router_chain.go:268 with
`req.RequiredCaps`; restructure identically, sharing a small unexported helper
`r.recommendWithToolCallResolution(ctx, opts, reqCaps)` so the logic exists
once — put the helper in router.go and use it from both call sites).

Note: `resolveCandidates`'s qualified/LookupAny branches need no change —
chain and single-key routes hit the gate with profiles the chain loop already
resolved, and single-model routes fail the gate visibly (the caller named one
model; per spec the gate is the single rejection point — `EnsureToolCallResolved`
on those branches too keeps unknown-never-skipped for direct routes; ADD it):

```go
	if key, ok := parseModelSelector(req.Model); ok {
		profile, err := r.registry.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		return r.registry.EnsureToolCallResolved(ctx, []*ModelProfile{profile}, req.RequiredCaps), nil
	}
```

(and the `req.Provider != ""` + `LookupAny` branches likewise).

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./provider/... -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add provider/
git commit -m "feat(provider): route-time lazy tool_call resolution in chain, recommend, and direct routes"
```

---

### Task 8: Golem wiring — capability-probe store, bootstrap option, `-no-cap-probe`

**Files:**
- Modify: `internal/providerbootstrap/bootstrap.go` (Options + wiring)
- Create: `cmd/golem/capprobe.go` (store open helper)
- Modify: `cmd/golem/main.go` (flag + wiring)
- Test: `internal/providerbootstrap/bootstrap_test.go` (append), `cmd/golem/capprobe_test.go` (create)

- [ ] **Step 1: Write failing tests**

```go
// internal/providerbootstrap/bootstrap_test.go (append)
func TestNew_CapabilityProbeStoreInstallsCapProberNotFullFactory(t *testing.T) {
	// Options{CapabilityProbeStore: store}
	// => registry has capProber installed, fpProberFactory NOT installed.
	// Assert behaviorally: Lookup on an openai-compat model performs no
	// probe HTTP calls (scripted test server records requests), while
	// ResolveToolCall does.
}
```

```go
// cmd/golem/capprobe_test.go
func TestCapProbeStorePath_UsesDataDir(t *testing.T) {
	// with XDG_DATA_HOME=/tmp/x => /tmp/x/golem/fingerprints.db
	// (reuse the dataDirBase seam/pattern from session.go tests)
}

func TestOpenCapProbeStore_FallsBackToMemoryOnFailure(t *testing.T) {
	// Point the path at an unwritable location; expect a non-nil store
	// (in-memory) plus a warning string, not an error.
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ ./cmd/golem/ -run 'CapabilityProbeStore|CapProbeStore' -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

`internal/providerbootstrap/bootstrap.go` Options:

```go
	// CapabilityProbeStore wires capability-probe rows and on-demand
	// ResolveToolCall without enabling full fingerprint profiling. Golem
	// sets this and leaves FingerprintStore nil; MCP/full-profiler callers
	// can rely on FingerprintStore's SQLiteStore also satisfying
	// fingerprint.CapProbeStore.
	CapabilityProbeStore fingerprint.CapProbeStore
```

Wiring in `New()`:

```go
	mrOpts := []provider.ModelRegistryOption{}
	factory := proberFactory(effCfg, ollamaClients)
	if opts.FingerprintStore != nil {
		mrOpts = append(mrOpts, provider.WithFingerprintProberFactory(factory))
	}
	capProbeStore := opts.CapabilityProbeStore
	if capProbeStore == nil {
		if s, ok := opts.FingerprintStore.(fingerprint.CapProbeStore); ok {
			capProbeStore = s
		}
	}
	if capProbeStore != nil {
		mrOpts = append(mrOpts,
			provider.WithCapabilityProbeStore(capProbeStore),
			provider.WithCapabilityProber(factory),
		)
	}
```

Full-profiling consumers get the capability resolver too when their
`FingerprintStore` is the SQLite store — same factory, no extra eager cost;
route-time resolution simply also works under MCP.

`cmd/golem/capprobe.go`:

```go
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/fingerprint"
	_ "modernc.org/sqlite"
)

// capProbeStorePath resolves the shared capability-probe DB path:
// $XDG_DATA_HOME/golem/fingerprints.db (else ~/.local/share/golem/...).
// Mirrors memoryDBPath's use of dataDirBase.
func capProbeStorePath(getenv func(string) string) (string, error) {
	base, err := dataDirBase(getenv) // existing helper in session.go
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "golem", "fingerprints.db"), nil
}

// openCapProbeStore opens (creating if needed) the capability-probe store.
// On any failure it degrades to an in-memory store for this run and
// returns a warning — probe results still apply, persistence is lost.
func openCapProbeStore(ctx context.Context, getenv func(string) string) (fingerprint.CapProbeStore, string) {
	path, err := capProbeStorePath(getenv)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
			var db *sql.DB
			if db, err = sql.Open("sqlite", "file:"+path); err == nil {
				var s fingerprint.CapProbeStore
				if s, err = fingerprint.NewStore(ctx, db); err == nil {
					return s, ""
				}
				_ = db.Close()
			}
		}
	}
	db, merr := sql.Open("sqlite", "file::memory:")
	if merr != nil {
		return nil, fmt.Sprintf("capability probe cache disabled: %v (memory fallback failed: %v)", err, merr)
	}
	s, serr := fingerprint.NewStore(ctx, db)
	if serr != nil {
		return nil, fmt.Sprintf("capability probe cache disabled: %v (memory fallback failed: %v)", err, serr)
	}
	return s, fmt.Sprintf("capability probe cache degraded to in-memory: %v", err)
}
```

(Check how `memory.go`/`session.go` create parent dirs and open modernc sqlite;
if this helper needs WAL/busy-timeout pragmas for durability, copy the existing
session pattern instead of inventing a new one.)

`cmd/golem/main.go`:

- flags struct: `noCapProbe bool`; parseFlags:

```go
	fs.BoolVar(&f.noCapProbe, "no-cap-probe", false, "disable the active tool-capability probe for undeclared models (catalog and explicit capabilities still apply)")
```

- in `run()`, before `providerbootstrap.New`:

```go
	var capStore fingerprint.CapProbeStore
	var capStoreWarn string
	if !f.noCapProbe {
		capStore, capStoreWarn = openCapProbeStore(ctx, os.Getenv)
	}
```

- pass into the bundle:

```go
	bundle, err := providerbootstrap.New(ctx, providerbootstrap.Options{
		Config:                          cfg,
		OllamaURLOverride:               f.ollamaURL,
		OpenAICompatURLOverrideProvider: backendRes.providerKey,
		OpenAICompatURLOverride:         backendRes.baseURL,
		CapabilityProbeStore:            capStore, // nil when -no-cap-probe
	})
```

- append `capStoreWarn` (when non-empty) to `warns`.

- [ ] **Step 4: Run tests + full build**

Run: `env -u GOROOT go test ./internal/providerbootstrap/ ./cmd/golem/ -v`
Then run: `env -u GOROOT go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/providerbootstrap/ cmd/golem/
git commit -m "feat(golem): wire capability-probe store + -no-cap-probe flag (capability-only mode)"
```

---

### Task 9: Preflight restructure — bounded eager + remediation hints

**Files:**
- Modify: `cmd/golem/modelcaller.go`
- Modify: `cmd/golem/main.go` (call site)
- Test: `cmd/golem/preflight_test.go` (append)

- [ ] **Step 1: Write failing tests**

Existing preflight tests use a fake `capChecker`; extend the harness with a
fake resolver:

```go
type fakeToolResolver struct {
	states map[provider.ModelKey]fingerprint.CapProbeState
	errs   map[provider.ModelKey]error
	calls  []provider.ModelKey
}

func (f *fakeToolResolver) ResolveToolCall(ctx context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error) {
	f.calls = append(f.calls, key)
	if err := f.errs[key]; err != nil {
		return "", err
	}
	return f.states[key], nil
}
```

Cases:

```go
func TestPreflight_BoundedEager_StopsAfterFirstCapable(t *testing.T) {
	// chain: [A (unknown, probe=>yes), B (unknown), C (declared no)]
	// => nil error; resolver called ONLY for A; B reported as
	// "capability unknown; probed on first use" non-fatal line; C keeps
	// its capability warning with remediation hint.
}

func TestPreflight_ProbeNoContinuesToNextEntry(t *testing.T) {
	// chain: [A (probe=>no), B (registry-capable)]
	// => nil error; resolver called for A only; warning for A mentions
	// "did not produce a tool call" + the remediation line with
	// `"capabilities": ["chat","generate","stream","tool_call"]`.
}

func TestPreflight_AllExhaustedFatalIncludesProbeOutcomes(t *testing.T) {
	// chain: [A (probe=>no), B (probe=>inconclusive), C (lookup error)]
	// => error; message contains A's probed-no line, B's inconclusive line
	// ("probe was inconclusive; declare capabilities to override"),
	// C's connectivity diagnostic (existing #217 text preserved).
}

func TestPreflight_NoCapProbe_NeverResolves(t *testing.T) {
	// nil resolver (or resolver disabled): active probing never runs.
	// Run the EXISTING #217 lookup/connectivity table through the new
	// signature to prove those diagnostics are unchanged; resolver call
	// count == 0. Catalog/floor-capable profiles still pass because that
	// merge happens upstream and is not disabled by -no-cap-probe.
}

func TestPreflight_NoCapProbe_CatalogKnownStillCapable(t *testing.T) {
	// Spec section 8 "no-probe flag regression": nil resolver, chain entry
	// whose fake profile carries CapToolCall (floor+catalog merged upstream)
	// => capable with zero warnings; an undeclared catalog-miss entry in the
	// same chain stays not-capable with the remediation hint.
}

func TestPreflight_EmptyChainRecommendResolves(t *testing.T) {
	// Empty chain: registry Recommend(without tool_call bit) returns one
	// model lacking tool_call; resolver says yes => preflight passes.
	// (Fake capChecker.Recommend must record the RequiredCaps it was
	// called with: assert it did NOT include CapToolCall.)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run Preflight -v`
Expected: FAIL (signature/behavior missing).

- [ ] **Step 3: Implement**

`cmd/golem/modelcaller.go`:

```go
// toolCallResolver is the narrow slice of *provider.ModelRegistry the
// preflight needs for active capability resolution. nil disables probing
// (-no-cap-probe or no store).
type toolCallResolver interface {
	ResolveToolCall(ctx context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error)
}
```

Remediation constants:

```go
const remediationCaps = `"capabilities": ["chat","generate","stream","tool_call"]`

// remediationHint renders the exact models.json fix line for a chain entry.
func remediationHint(sel string) string {
	return fmt.Sprintf("if it supports function calling, add %s to the model entry for %q in models.json", remediationCaps, sel)
}
```

`preflightToolCapable` gains the resolver and applies bounded-eager:

```go
func preflightToolCapable(ctx context.Context, reg capChecker, chain []string, resolveEndpoint endpointResolver, resolver toolCallResolver) (warnings []string, err error) {
	if len(chain) == 0 {
		return preflightRecommendToolCapable(ctx, reg, resolver)
	}

	capable := 0
	for _, sel := range chain {
		// Active probing is allowed only until the first capable entry:
		// startup proves ONE usable model then stops loading fallbacks
		// (llama-swap cost); later unknowns resolve at route time.
		allowProbe := capable == 0
		ok, warn := evalChainEntry(ctx, reg, sel, resolveEndpoint, resolver, allowProbe)
		if ok {
			capable++
			continue
		}
		warnings = append(warnings, warn)
	}
	if capable == 0 {
		return warnings, fmt.Errorf("golem: tool-capability preflight failed:\n%s", strings.Join(warnings, "\n"))
	}
	return warnings, nil
}
```

`evalChainEntry` extension (keep the #217 connectivity path byte-identical):

```go
func evalChainEntry(ctx context.Context, reg capChecker, sel string, resolveEndpoint endpointResolver, resolver toolCallResolver, allowProbe bool) (capable bool, warning string) {
	// ... existing Lookup/LookupAny + connectivity classification UNCHANGED ...
	// On the resolved-but-not-capable path (both branches), replace the
	// bare notCapable return with classifyToolCapability:
	return classifyToolCapability(ctx, sel, key, resolver, allowProbe)
}

// classifyToolCapability turns an entry that resolved without tool_call
// into (capable, warning) using the resolver when probing is allowed.
func classifyToolCapability(ctx context.Context, sel string, key provider.ModelKey, resolver toolCallResolver, allowProbe bool) (bool, string) {
	if resolver == nil {
		return false, fmt.Sprintf("agent fallback %q is not tool-capable (chat|stream|tool_call); %s", sel, remediationHint(sel))
	}
	if !allowProbe {
		return false, fmt.Sprintf("agent fallback %q: tool capability unknown; probed on first use", sel)
	}
	state, err := resolver.ResolveToolCall(ctx, key)
	switch {
	case err != nil:
		return false, fmt.Sprintf("agent fallback %q: tool-capability probe failed: %v; %s", sel, err, remediationHint(sel))
	case state == fingerprint.CapProbeYes:
		return true, ""
	case state == fingerprint.CapProbeNo:
		return false, fmt.Sprintf("agent fallback %q: model did not produce a tool call when probed; %s", sel, remediationHint(sel))
	case state == fingerprint.CapProbeInconclusive:
		return false, fmt.Sprintf("agent fallback %q: tool-capability probe was inconclusive; declare capabilities to override: %s", sel, remediationHint(sel))
	default: // unknown (resolution disabled for this provider)
		return false, fmt.Sprintf("agent fallback %q is not tool-capable (chat|stream|tool_call); %s", sel, remediationHint(sel))
	}
}
```

Bare-selector branch: `LookupAny` succeeded but none capable — call
`classifyToolCapability` with the FIRST profile's key (a bare selector may
match several providers; probing the first match is the pragmatic choice —
document it in a comment).

Empty-chain branch:

```go
func preflightRecommendToolCapable(ctx context.Context, reg capChecker, resolver toolCallResolver) ([]string, error) {
	if resolver == nil {
		// No resolution available: original behavior.
		profs, rerr := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps})
		if rerr != nil {
			return nil, fmt.Errorf("golem: tool-capability preflight (recommend): %w", rerr)
		}
		if len(profs) == 0 {
			return nil, fmt.Errorf("golem: no tool-capable model available (require chat|stream|tool_call)")
		}
		return nil, nil
	}
	// Recommend WITHOUT the tool_call bit, then resolve lazily until one
	// candidate confirms (bounded eager, same policy as the chain path).
	profs, rerr := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps &^ provider.CapToolCall})
	if rerr != nil {
		return nil, fmt.Errorf("golem: tool-capability preflight (recommend): %w", rerr)
	}
	for _, p := range profs {
		if p.Caps.Has(provider.CapToolCall) {
			return nil, nil
		}
	}
	for _, p := range profs {
		if state, err := resolver.ResolveToolCall(ctx, p.Key); err == nil && state == fingerprint.CapProbeYes {
			return nil, nil
		}
	}
	return nil, fmt.Errorf("golem: no tool-capable model available (require chat|stream|tool_call)")
}
```

`cmd/golem/main.go` call site:

```go
	var resolver toolCallResolver
	if !f.noCapProbe && capStore != nil {
		resolver = bundle.Models
	}
	warns, err := preflightToolCapable(ctx, bundle.Models, plan.chain, resolveEndpoint, resolver)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ -v`
Expected: PASS including ALL pre-existing preflight tests (adapted to the new
signature with a nil resolver — their behavior must not change).

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/
git commit -m "feat(golem): bounded-eager preflight probing with remediation hints"
```

---

### Task 10: `golem models` subcommand

**Files:**
- Modify: `provider/model_registry.go` (narrow explain export)
- Create: `cmd/golem/models.go`
- Modify: `cmd/golem/main.go` (dispatch)
- Test: `cmd/golem/models_test.go`, `provider/capresolve_test.go` (append explain tests)

- [ ] **Step 1: Write failing tests**

Registry explain (append to `provider/capresolve_test.go`):

```go
func TestExplainToolCall_Provenance(t *testing.T) {
	// Cases: explicit-override key => Source "explicit";
	// catalog-tools key => Source "catalog"; probe-yes row => Source
	// "probe" with TestedAt set; nothing => Source "unknown".
}
```

`cmd/golem/models_test.go` (render tests against fakes; follow the
`runIndex` test style):

```go
func TestRunModels_ListsChainWithProvenance(t *testing.T) {
	// Two chain entries; fake registry: A explicit incl tool_call, B probe-no.
	// Output contains one line per entry with caps + provenance + a
	// MISSING tool_call flag for B, plus the remediation hint.
}

func TestRunModels_SharedKeyNamesDeclaringRole(t *testing.T) {
	// cfg has role "chat" declaring caps for the same key the agent chain
	// uses undeclared => output marks the entry "explicit (declared by
	// model entry \"chat\")".
}

func TestRunModels_ProbeAllProbesEveryEntry(t *testing.T) {
	// --probe-all with 3 non-explicit entries => resolver called 3 times
	// (no bounded-eager stop).
}

func TestRunModels_ReprobeDeletesRowsFirst(t *testing.T) {
	// --reprobe => DeleteCapProbes called per non-explicit entry before
	// resolver runs.
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `env -u GOROOT go test ./cmd/golem/ -run RunModels -v`
Then run: `env -u GOROOT go test ./provider/ -run ExplainToolCall -v`
Expected: FAIL.

- [ ] **Step 3: Implement**

Registry export (narrow, tool_call-scoped — deliberately NOT a general
provenance API):

```go
// ToolCallExplanation is diagnostic provenance for one model's tool_call
// capability, consumed by `golem models`. Deliberately narrow.
type ToolCallExplanation struct {
	Caps     Capability
	Has      bool
	Source   string    // "explicit" | "catalog" | "runtime" | "probe" | "unknown"
	State    fingerprint.CapProbeState // cached probe state, "" if none
	TestedAt time.Time // zero when no probe row
}

// ExplainToolCall reports where a model's tool_call bit comes from (or why
// it is absent).
func (r *ModelRegistry) ExplainToolCall(ctx context.Context, key ModelKey) (ToolCallExplanation, error) {
	// Order mirrors ResolveToolCall's precedence:
	// 1. explicit override (parse; contains/lacks tool_call => "explicit")
	// 2. merged profile caps: if profile.Caps.Has(CapToolCall), distinguish
	//    catalog vs probe vs runtime. Check catalog first; then valid
	//    probe-yes row; otherwise runtime/floor.
	// 3. when profile lacks tool_call, valid probe no/inconclusive rows
	//    explain the absence as "probe".
	// 4. otherwise "unknown".
	//
	// To distinguish catalog vs runtime/floor/probe, re-run the catalog lookup
	// (r.catalog.lookup with ParseModelName like buildProfile) and check its
	// Caps for CapToolCall => "catalog". If not catalog but a valid probe-yes
	// row exists, source is "probe"; otherwise source is "runtime" (the floor
	// is treated as registry/provider-layer runtime knowledge for operator
	// diagnostics).
}
```

(Write the body following that comment; ~40 lines, all pieces exist.)

`cmd/golem/models.go` — subcommand skeleton mirroring `runIndex`
(flag set: `-config`, `-base-url`, `-no-probe`, `-ollama-url`, `--probe-all`
as `-probe-all`, `--reprobe` as `-reprobe`; Go flags use single dash):

```go
// runModels is the `golem models` entry point: resolve the agent chain,
// print each entry's capabilities + tool_call provenance, and optionally
// probe (-probe-all) or re-probe (-reprobe) non-explicit entries.
func runModels(ctx context.Context, args []string, out, errOut io.Writer) error
```

Flow (write it fully):
1. parse flags; load config; `resolveBackend` (same as run(), reusing
   backendResolveOpts); `providerbootstrap.New` with capability-probe store
   (open via `openCapProbeStore`; `-probe-all`/`-reprobe` require it — if the
   store warning is non-empty print it to errOut).
2. `resolveAgentChain`; for empty chain print the recommend-mode note and the
   recommend set with explanations.
3. Per entry: parse selector; `reg.Lookup`; `reg.ExplainToolCall`; assemble a
   line:

```
llamacpp/gemma4:31b   caps=chat|generate|stream|tool_call   tool_call=yes (catalog)
llamacpp/byo-model    caps=chat|generate|stream              tool_call=MISSING (probe: no, 2h ago)
    fix: if it supports function calling, add "capabilities": [...] to the model entry ...
```

4. Shared-key annotation: walk `cfg.Models` for another role declaring
   explicit caps on the same key; if found append `(declared by model entry "<role>")`.
5. `-reprobe`: for each non-explicit entry `store.DeleteCapProbes` +
   `reg.Refresh(key)` if a Refresh exists (else the resolver's invalidate
   covers it), then resolve.
6. `-probe-all`: `reg.ResolveToolCall` per non-explicit entry (no stop), print
   updated states.
7. Lookup errors render the same connectivity diagnostics preflight uses
   (reuse `preflightConnectivityWarn` via the endpoint resolver).

`cmd/golem/main.go` dispatch:

```go
	switch args[0] {
	case "index":
		return runIndex(context.Background(), args[1:], stdout, stderr)
	case "models":
		return runModels(context.Background(), args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown command %q (did you mean \"index\" or \"models\"?)", args[0])
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `env -u GOROOT go test ./cmd/golem/ ./provider/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/golem/ provider/
git commit -m "feat(golem): models subcommand - capability listing, provenance, -probe-all/-reprobe"
```

---

### Task 11: Catalog audit + docs + integration test

**Files:**
- Modify: `provider/catalog.json` (only if the audit finds gaps)
- Modify: `docs/llm/` BYO/models doc (locate: `ls docs/llm/`)
- Create: `cmd/golem/capprobe_integration_test.go` (build-tagged)

- [ ] **Step 1: Catalog audit**

Compare `provider/catalog.json` family `caps` against upstream tool-support
reality for the bundled lineup (qwen2.5/3/3.5/3.6, qwen3-coder-next, gemma2/3/4,
llama3.x, phi3/4, mistral, mixtral, deepseek-*, codellama, starcoder*,
embeddings). Current state (verified 2026-07-03): `tools` present on all
majors; absent on deepseek-r1, codellama, starcoder, starcoder2, gemma2 —
all correct denials. Expected outcome: NO changes; document the audit result
in the commit message if so. If a gap IS found, add the `tools` alias to that
family's caps.

- [ ] **Step 2: Docs**

Update the BYO models doc (find the exact file with
`grep -rln "capabilities" docs/llm/`):

- `capabilities` field semantics: explicit list REPLACES (both directions —
  omitting `tool_call` denies it); derived caps are a FLOOR for undeclared
  openai-compat models (catalog additions survive); cross-role consistency
  rule for explicit declarations.
- Tool-capable bundled models table (from the catalog audit).
- The probe: when it runs (undeclared openai-compat models at agent startup /
  route time), what it sends (get_time tool, two attempts), cache location
  (`$XDG_DATA_HOME/golem/fingerprints.db`), `-no-cap-probe`, and
  `golem models` / `-probe-all` / `-reprobe`.

- [ ] **Step 3: Integration test (build tag `integration`)**

```go
//go:build integration

// cmd/golem/capprobe_integration_test.go
// Requires a running llama.cpp llama-server (env GO_LLM_TEST_OPENAI_BASE
// + GO_LLM_TEST_TOOL_MODEL [+ optional GO_LLM_TEST_NOTOOL_MODEL]).
// Asserts: ProbeToolCall => yes on the tool-capable model; => no/inconclusive
// on the non-tool model when provided; probe result lands in a temp store
// and a second resolve is a cache hit (no second HTTP request — count via
// a recording proxy httputil.ReverseProxy).
```

- [ ] **Step 4: Full gate + commit**

Run: `env -u GOROOT go test ./...`
Then run: `env -u GOROOT go vet ./...`
Expected: PASS.

```bash
git add provider/catalog.json docs/ cmd/golem/
git commit -m "docs: capabilities semantics, tool-capable model table, probe + golem models reference"
```

---

### Final verification (before PR)

- [ ] `env -u GOROOT go test ./... -race` — PASS
- [ ] `env -u GOROOT go vet ./...` — clean
- [ ] `env -u GOROOT gofmt -l .` — empty
- [ ] Manual smoke: `golem models` against a live llama.cpp with an undeclared
      model; first run probes (visible latency), second run cache-hits.
- [ ] /code-review cycles until clean + /criticize-review (user's standing rule).
- [ ] PR into develop titled `feat(provider/golem): auto-detect tool_call via active probe + capability discoverability`, body `Closes #219`, no emojis.
