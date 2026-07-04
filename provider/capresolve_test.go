// provider/capresolve_test.go
//
// Tests for ResolveToolCall (tri-state on-demand capability resolution),
// the read-only cap-probe merge layer, and EnsureToolCallResolved.
package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeCapProbeStore is an in-memory fingerprint.CapProbeStore.
type fakeCapProbeStore struct {
	mu      sync.Mutex
	rows    map[string]fingerprint.CapProbe
	saveErr error // when non-nil, SaveCapProbe fails with it
}

func newFakeCapProbeStore() *fakeCapProbeStore {
	return &fakeCapProbeStore{rows: make(map[string]fingerprint.CapProbe)}
}

func capRowKey(backendID, modelName, capability string) string {
	return backendID + "\x00" + modelName + "\x00" + capability
}

func (s *fakeCapProbeStore) GetCapProbe(_ context.Context, backendID, modelName, capability string) (*fingerprint.CapProbe, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[capRowKey(backendID, modelName, capability)]
	if !ok {
		return nil, fingerprint.ErrNotFound
	}
	cp := row
	return &cp, nil
}

func (s *fakeCapProbeStore) SaveCapProbe(_ context.Context, probe fingerprint.CapProbe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.rows[capRowKey(probe.BackendID, probe.ModelName, probe.Capability)] = probe
	return nil
}

func (s *fakeCapProbeStore) DeleteCapProbes(_ context.Context, backendID, modelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := backendID + "\x00" + modelName + "\x00"
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *fakeCapProbeStore) row(backendID, modelName, capability string) (fingerprint.CapProbe, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[capRowKey(backendID, modelName, capability)]
	return row, ok
}

func (s *fakeCapProbeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// fakeToolCallProber implements fingerprint.ModelProber (stubs) plus
// fingerprint.ToolCallProber with scripted per-model outcomes and a call
// counter. If block is non-nil, ProbeToolCall waits until it is closed.
type fakeToolCallProber struct {
	mu       sync.Mutex
	outcomes map[string]fingerprint.CapProbeOutcome
	errs     map[string]error
	calls    map[string]int
	block    chan struct{}
}

func newFakeToolCallProber() *fakeToolCallProber {
	return &fakeToolCallProber{
		outcomes: make(map[string]fingerprint.CapProbeOutcome),
		errs:     make(map[string]error),
		calls:    make(map[string]int),
	}
}

func (p *fakeToolCallProber) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeToolCallProber) ProbeChat(context.Context, string, interface{}) (*fingerprint.ChatMetrics, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeToolCallProber) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("not implemented")
}

func (p *fakeToolCallProber) ProbeToolCall(_ context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.mu.Lock()
	p.calls[model]++
	block := p.block
	p.mu.Unlock()
	if block != nil {
		<-block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[model]; err != nil {
		return fingerprint.CapProbeOutcome{}, err
	}
	return p.outcomes[model], nil
}

func (p *fakeToolCallProber) callCount(model string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[model]
}

func (p *fakeToolCallProber) totalCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	total := 0
	for _, n := range p.calls {
		total += n
	}
	return total
}

func capProberFactory(prober fingerprint.ModelProber) FingerprintProberFactory {
	return func(_ context.Context, _ ModelKey, _ *ModelInfo, _ Provider) (*FingerprintProberSpec, error) {
		return &FingerprintProberSpec{Prober: prober}, nil
	}
}

// newCapResolveRegistry builds a registry over a single mock provider that
// advertises the given models, with the cap-probe store and prober wired.
func newCapResolveRegistry(t *testing.T, providerName string, models []string, store fingerprint.CapProbeStore, prober fingerprint.ModelProber) *ModelRegistry {
	t.Helper()
	infos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		infos = append(infos, ModelInfo{Name: m})
	}
	reg := &mrMockProviderRegistry{
		providers: map[string]Provider{
			providerName: &mrMockProvider{name: providerName, models: infos},
		},
	}
	opts := []ModelRegistryOption{}
	if store != nil {
		opts = append(opts, WithCapabilityProbeStore(store))
	}
	if prober != nil {
		opts = append(opts, WithCapabilityProber(capProberFactory(prober)))
	}
	mr, err := NewModelRegistry(reg, nil, opts...)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	return mr
}

// ---------------------------------------------------------------------------
// ResolveToolCall
// ---------------------------------------------------------------------------

func TestResolveToolCall_ExplicitDeclarationSkipsProbe(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	mr.SetCapabilityOverride(func(k ModelKey) []string {
		if k == key {
			return []string{"chat", "tool_call"}
		}
		return nil
	})
	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	if got := prober.callCount("mystery-model"); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}

	mr.SetCapabilityOverride(func(k ModelKey) []string {
		if k == key {
			return []string{"chat"}
		}
		return nil
	})
	state, err = mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeNo {
		t.Fatalf("state = %q, want no", state)
	}
	if got := prober.callCount("mystery-model"); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}
}

func TestResolveToolCall_MergedCapsShortCircuit(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "ollama-local", []string{"qwen3:8b"}, store, prober)
	key := ModelKey{Provider: "ollama-local", Model: "qwen3:8b"}

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes (catalog declares tools)", state)
	}
	if got := prober.callCount("qwen3:8b"); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}
}

func TestResolveToolCall_CacheHitSkipsProbe(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	if err := store.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID:    "probe-prov",
		ModelName:    "mystery-model",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeYes,
		ModelDigest:  key.String(),
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("seed SaveCapProbe() error: %v", err)
	}

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	if got := prober.callCount("mystery-model"); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}
}

func TestResolveToolCall_StaleCacheReprobes(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	if err := store.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID:    "probe-prov",
		ModelName:    "mystery-model",
		Capability:   "tool_call",
		State:        fingerprint.CapProbeYes,
		ModelDigest:  key.String(),
		ProbeVersion: fingerprint.CurrentToolProbeVersion - 1,
		TestedAt:     time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed SaveCapProbe() error: %v", err)
	}

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	if got := prober.callCount("mystery-model"); got != 1 {
		t.Fatalf("prober calls = %d, want 1", got)
	}
	row, ok := store.row("probe-prov", "mystery-model", "tool_call")
	if !ok {
		t.Fatal("expected persisted row")
	}
	if row.ProbeVersion != fingerprint.CurrentToolProbeVersion {
		t.Fatalf("persisted ProbeVersion = %d, want %d", row.ProbeVersion, fingerprint.CurrentToolProbeVersion)
	}
}

func TestResolveToolCall_ProbePersistsAndInvalidatesProfile(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	// 1. Initial lookup: profile lacks CapToolCall.
	profile, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if profile.Caps.Has(CapToolCall) {
		t.Fatalf("initial profile Caps = %v, want no tool_call", profile.Caps)
	}

	// 2. Resolve: probe runs, verdict persisted.
	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes", state)
	}
	row, ok := store.row("probe-prov", "mystery-model", "tool_call")
	if !ok {
		t.Fatal("expected persisted row")
	}
	if row.Capability != "tool_call" {
		t.Fatalf("row Capability = %q, want tool_call", row.Capability)
	}
	if row.State != fingerprint.CapProbeYes {
		t.Fatalf("row State = %q, want yes", row.State)
	}
	if !row.ExpiresAt.IsZero() {
		t.Fatalf("row ExpiresAt = %v, want zero (yes verdicts do not expire)", row.ExpiresAt)
	}

	// 3. Re-lookup: profile cache was invalidated and the merge layer read
	// the yes row.
	profile, err = mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() after resolve error: %v", err)
	}
	if !profile.Caps.Has(CapToolCall) {
		t.Fatalf("refreshed profile Caps = %v, want tool_call", profile.Caps)
	}
}

func TestResolveToolCall_TransientErrorNotPersisted(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.errs["mystery-model"] = errors.New("connection refused")
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	state, err := mr.ResolveToolCall(ctx, key)
	if err == nil {
		t.Fatal("expected error from transient probe failure")
	}
	if state != "" {
		t.Fatalf("state = %q, want empty (unknown)", state)
	}
	if store.count() != 0 {
		t.Fatalf("store rows = %d, want 0 (transient errors never persisted)", store.count())
	}
	if got := prober.callCount("mystery-model"); got != 1 {
		t.Fatalf("prober calls = %d, want 1", got)
	}

	// Second call probes again -- nothing cached the failure.
	if _, err := mr.ResolveToolCall(ctx, key); err == nil {
		t.Fatal("expected error on second resolve")
	}
	if got := prober.callCount("mystery-model"); got != 2 {
		t.Fatalf("prober calls = %d, want 2", got)
	}
}

func TestResolveToolCall_SingleflightDedup(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober.block = make(chan struct{})
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	const callers = 8
	var wg sync.WaitGroup
	var launched sync.WaitGroup
	states := make([]fingerprint.CapProbeState, callers)
	errs := make([]error, callers)
	launched.Add(callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			launched.Done()
			states[i], errs[i] = mr.ResolveToolCall(ctx, key)
		}(i)
	}
	launched.Wait()
	close(prober.block)
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if states[i] != fingerprint.CapProbeYes {
			t.Fatalf("caller %d state = %q, want yes", i, states[i])
		}
	}
	// Stragglers that miss the shared flight hit the persisted row instead
	// of re-probing, so exactly one probe runs either way.
	if got := prober.callCount("mystery-model"); got != 1 {
		t.Fatalf("prober calls = %d, want 1", got)
	}
}

func TestResolveToolCall_DisabledReturnsUnknown(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	// Store installed but NO prober: resolution is disabled.
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, nil)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != "" {
		t.Fatalf("state = %q, want empty (unknown, never a claim)", state)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}
}

// TestResolveToolCall_SyntheticFactoryDigestIgnored pins the real-world
// factory shape: providerbootstrap's fingerprintDigestForModel returns a
// synthetic "config-caps:<caps>" digest for openai-compat models (no
// runtime digest). Cap-probe rows must be keyed by the read-side digest
// (runtimeInfo.Digest -> key.String()), NOT the synthetic spec digest --
// otherwise probe verdicts never merge into profiles and negatives dodge
// the digestless TTL cap.
func TestResolveToolCall_SyntheticFactoryDigestIgnored(t *testing.T) {
	ctx := context.Background()

	syntheticFactory := func(prober fingerprint.ModelProber) FingerprintProberFactory {
		return func(_ context.Context, _ ModelKey, _ *ModelInfo, _ Provider) (*FingerprintProberSpec, error) {
			return &FingerprintProberSpec{Prober: prober, ModelDigest: "config-caps:chat,stream"}, nil
		}
	}
	newRegistry := func(t *testing.T, store *fakeCapProbeStore, prober *fakeToolCallProber) *ModelRegistry {
		t.Helper()
		reg := &mrMockProviderRegistry{
			providers: map[string]Provider{
				"openai-local": &mrMockProvider{name: "openai-local", models: []ModelInfo{{Name: "mystery-model"}}},
			},
		}
		mr, err := NewModelRegistry(reg, nil,
			WithCapabilityProbeStore(store), WithCapabilityProber(syntheticFactory(prober)))
		if err != nil {
			t.Fatalf("NewModelRegistry() error: %v", err)
		}
		return mr
	}
	key := ModelKey{Provider: "openai-local", Model: "mystery-model"}

	t.Run("yes row keyed by fallback and merges", func(t *testing.T) {
		store := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		mr := newRegistry(t, store, prober)

		state, err := mr.ResolveToolCall(ctx, key)
		if err != nil {
			t.Fatalf("ResolveToolCall() error: %v", err)
		}
		if state != fingerprint.CapProbeYes {
			t.Fatalf("state = %q, want yes", state)
		}
		row, ok := store.row("openai-local", "mystery-model", "tool_call")
		if !ok {
			t.Fatal("expected persisted row")
		}
		if row.ModelDigest != key.String() {
			t.Fatalf("row ModelDigest = %q, want key fallback %q (synthetic factory digest must be ignored)", row.ModelDigest, key.String())
		}
		profile, err := mr.Lookup(ctx, key)
		if err != nil {
			t.Fatalf("Lookup() error: %v", err)
		}
		if !profile.Caps.Has(CapToolCall) {
			t.Fatalf("profile Caps = %v, want tool_call merged from probe row", profile.Caps)
		}
	})

	t.Run("no row gets digestless TTL cap", func(t *testing.T) {
		store := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
		mr := newRegistry(t, store, prober)

		before := time.Now()
		state, err := mr.ResolveToolCall(ctx, key)
		if err != nil {
			t.Fatalf("ResolveToolCall() error: %v", err)
		}
		if state != fingerprint.CapProbeNo {
			t.Fatalf("state = %q, want no", state)
		}
		row, ok := store.row("openai-local", "mystery-model", "tool_call")
		if !ok {
			t.Fatal("expected persisted row")
		}
		if row.ExpiresAt.IsZero() {
			t.Fatal("digestless no must expire, got zero ExpiresAt")
		}
		if max := before.Add(fingerprint.CapProbeDigestlessNoTTL + time.Minute); row.ExpiresAt.After(max) {
			t.Fatalf("ExpiresAt = %v, want within ~%v", row.ExpiresAt, fingerprint.CapProbeDigestlessNoTTL)
		}
		if min := before.Add(fingerprint.CapProbeDigestlessNoTTL - time.Minute); row.ExpiresAt.Before(min) {
			t.Fatalf("ExpiresAt = %v, want at least ~%v out", row.ExpiresAt, fingerprint.CapProbeDigestlessNoTTL)
		}
	})
}

func TestResolveToolCall_RejectedOverrideFallsThrough(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	// "completion" is a multi-bit catalog alias: ParseCapsStrict rejects it,
	// so the override must be a no-op here (no fabricated No claim) and
	// resolution falls through to the probe path.
	mr.SetCapabilityOverride(func(k ModelKey) []string {
		if k == key {
			return []string{"completion"}
		}
		return nil
	})

	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("ResolveToolCall() error: %v", err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("state = %q, want yes from probe fallthrough", state)
	}
	if got := prober.callCount("mystery-model"); got != 1 {
		t.Fatalf("prober calls = %d, want 1 (rejected override must not short-circuit)", got)
	}
}

// ---------------------------------------------------------------------------
// EnsureToolCallResolved
// ---------------------------------------------------------------------------

func TestEnsureToolCallResolved_FiltersAndRefreshes(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-yes"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	prober.outcomes["mystery-no"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeNo}
	mr := newCapResolveRegistry(t, "multi", []string{"qwen3:8b", "mystery-yes", "mystery-no"}, store, prober)

	var profiles []*ModelProfile
	for _, m := range []string{"qwen3:8b", "mystery-yes", "mystery-no"} {
		p, err := mr.Lookup(ctx, ModelKey{Provider: "multi", Model: m})
		if err != nil {
			t.Fatalf("Lookup(%s) error: %v", m, err)
		}
		profiles = append(profiles, p)
	}
	if !profiles[0].Caps.Has(CapToolCall) {
		t.Fatalf("qwen3:8b should carry catalog tool_call, got %v", profiles[0].Caps)
	}

	required := CapChat | CapStream | CapToolCall
	out := mr.EnsureToolCallResolved(ctx, profiles, required)
	if len(out) != len(profiles) {
		t.Fatalf("len(out) = %d, want %d (never removes candidates)", len(out), len(profiles))
	}

	// Capable profile untouched, never probed.
	if out[0] != profiles[0] {
		t.Fatal("capable profile should be returned unchanged")
	}
	if got := prober.callCount("qwen3:8b"); got != 0 {
		t.Fatalf("qwen3:8b prober calls = %d, want 0", got)
	}

	// Probe-yes model refreshed with CapToolCall.
	if !out[1].Caps.Has(CapToolCall) {
		t.Fatalf("mystery-yes Caps = %v, want tool_call after refresh", out[1].Caps)
	}

	// Probe-no model returned unchanged; the caller's gate is the single
	// rejection point.
	if out[2] != profiles[2] {
		t.Fatal("probe-no profile should be returned unchanged")
	}
	if out[2].Caps.Has(CapToolCall) {
		t.Fatalf("mystery-no Caps = %v, want no tool_call", out[2].Caps)
	}

	// Input slice must not be mutated.
	if profiles[1].Caps.Has(CapToolCall) && out[1] == profiles[1] {
		t.Fatal("input slice entry was mutated in place")
	}
}

func TestEnsureToolCallResolved_NoOpWhenDisabledOrNotRequired(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "multi", []string{"mystery-model"}, store, prober)

	p, err := mr.Lookup(ctx, ModelKey{Provider: "multi", Model: "mystery-model"})
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	in := []*ModelProfile{p}

	// required lacks CapToolCall: input returned as-is, no probes.
	out := mr.EnsureToolCallResolved(ctx, in, CapChat|CapStream)
	if len(out) != 1 || out[0] != p {
		t.Fatal("expected input passthrough when tool_call not required")
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0", got)
	}

	// Resolution disabled (no prober): passthrough too.
	mrDisabled := newCapResolveRegistry(t, "multi", []string{"mystery-model"}, store, nil)
	out = mrDisabled.EnsureToolCallResolved(ctx, in, CapChat|CapStream|CapToolCall)
	if len(out) != 1 || out[0] != p {
		t.Fatal("expected input passthrough when resolution disabled")
	}
}

// TestInvalidateProfileBumpsPolicyVersion pins the guard against the
// invalidation/build race: a buildProfile in flight when a probe lands
// snapshots the policy version at entry, so invalidateProfile must bump
// that shared counter -- otherwise the in-flight build's cache write
// passes the version check and permanently re-caches a stale profile
// (the row-hit fast path never invalidates, so it would never self-heal).
func TestInvalidateProfileBumpsPolicyVersion(t *testing.T) {
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	mr.mu.RLock()
	before := mr.overrideVersion
	mr.mu.RUnlock()

	mr.invalidateProfile(key)

	mr.mu.RLock()
	after := mr.overrideVersion
	mr.mu.RUnlock()
	if after != before+1 {
		t.Fatalf("overrideVersion = %d after invalidateProfile, want %d (must bump to fence in-flight buildProfile cache writes)", after, before+1)
	}
}

// TestEnsureToolCallResolved_SaveFailureKeepsVerdict pins the "verdict
// stands for this caller" contract: when SaveCapProbe fails (swallowed as
// non-fatal), the refreshed Lookup cannot see a probe row and still lacks
// CapToolCall -- but the probe DID answer yes, so the returned profile
// must carry the bit rather than letting the caller's gate reject a
// confirmed-capable candidate.
func TestEnsureToolCallResolved_SaveFailureKeepsVerdict(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	store.saveErr = errors.New("disk full")
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)
	key := ModelKey{Provider: "probe-prov", Model: "mystery-model"}

	p, err := mr.Lookup(ctx, key)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	in := []*ModelProfile{p}

	out := mr.EnsureToolCallResolved(ctx, in, CapChat|CapStream|CapToolCall)
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if !out[0].Caps.Has(CapToolCall) {
		t.Fatalf("out[0].Caps = %v, want tool_call despite lost store write", out[0].Caps)
	}
	// Input profile must not be mutated in place.
	if p.Caps.Has(CapToolCall) {
		t.Fatalf("input profile mutated: Caps = %v", p.Caps)
	}
}

// ---------------------------------------------------------------------------
// Purity of Lookup / Recommend
// ---------------------------------------------------------------------------

func TestLookupAndRecommendStayPure(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	prober := newFakeToolCallProber()
	prober.outcomes["mystery-model"] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "probe-prov", []string{"mystery-model"}, store, prober)

	if _, err := mr.Lookup(ctx, ModelKey{Provider: "probe-prov", Model: "mystery-model"}); err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if _, err := mr.Recommend(ctx, RecommendOpts{}); err != nil {
		t.Fatalf("Recommend() error: %v", err)
	}
	if got := prober.totalCalls(); got != 0 {
		t.Fatalf("prober calls = %d, want 0 (Lookup/Recommend never probe)", got)
	}
}
