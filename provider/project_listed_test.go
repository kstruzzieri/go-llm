// provider/project_listed_test.go
//
// Tests for ProjectListedModels: the read-only, list-fed capability
// projection (#456 slice 4). The projection must merge facts for
// already-listed models without firing fingerprint probes, tool-call
// probes, provider re-queries, or profile-cache writes.
package provider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

func TestProjectListedModels_ListingFactsAndOrder(t *testing.T) {
	ctx := context.Background()
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	infos := []ModelInfo{
		{Name: "zeta", Family: "fam-z", ContextWindow: 8192, Capabilities: []string{"chat"}},
		{Name: "alpha", Family: "fam-a", ContextWindow: 32768, Capabilities: []string{"chat", "tools"}},
	}
	facts, err := mr.ProjectListedModels(ctx, "prov", infos)
	if err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("len(facts) = %d; want 2", len(facts))
	}
	// Input order preserved (caller sorts; projection must not reorder).
	if facts[0].Key.Model != "zeta" || facts[1].Key.Model != "alpha" {
		t.Fatalf("order = %s, %s; want zeta, alpha", facts[0].Key.Model, facts[1].Key.Model)
	}
	if facts[0].Key != (ModelKey{Provider: "prov", Model: "zeta"}) {
		t.Fatalf("facts[0].Key = %v", facts[0].Key)
	}
	if facts[0].ContextWindow != 8192 || facts[1].ContextWindow != 32768 {
		t.Fatalf("context windows = %d, %d; want 8192, 32768", facts[0].ContextWindow, facts[1].ContextWindow)
	}
	if !facts[0].Caps.Has(CapChat) {
		t.Fatalf("zeta caps = %v; listing-declared chat missing", facts[0].Caps)
	}
	if facts[0].KnownMask&facts[0].Caps != facts[0].Caps {
		t.Fatalf("zeta KnownMask %v does not cover Caps %v", facts[0].KnownMask, facts[0].Caps)
	}
	if facts[0].ProfileSource != "merged" {
		t.Fatalf("ProfileSource = %q; want %q (the projection always runs the full merge)", facts[0].ProfileSource, "merged")
	}
}

func TestProjectListedModels_CapProbeRows(t *testing.T) {
	ctx := context.Background()
	store := newFakeCapProbeStore()
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, nil, WithCapabilityProbeStore(store))
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	now := time.Now()
	saveRow := func(model string, state fingerprint.CapProbeState, digest string) {
		if err := store.SaveCapProbe(ctx, fingerprint.CapProbe{
			BackendID: "prov", ModelName: model, Capability: "tool_call",
			State: state, ModelDigest: digest,
			ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: now,
		}); err != nil {
			t.Fatalf("SaveCapProbe(%s) error: %v", model, err)
		}
	}
	saveRow("said-yes", fingerprint.CapProbeYes, (ModelKey{Provider: "prov", Model: "said-yes"}).String())
	saveRow("said-no", fingerprint.CapProbeNo, (ModelKey{Provider: "prov", Model: "said-no"}).String())
	saveRow("stale-no", fingerprint.CapProbeNo, "sha256:old-content-digest")
	// Row written while the runtime was digestless (digest = key.String()),
	// but THIS listing now reports a real content digest. Correct behavior:
	// identity changed/unverifiable under the listing digest -> NO CLAIM.
	// This is the fixture that makes the digest mutation (below) go red:
	// validating with capProbeDigest(key, nil) instead of (key, &info)
	// falls back to key.String(), matches the row, and wrongly claims.
	saveRow("was-digestless", fingerprint.CapProbeNo, (ModelKey{Provider: "prov", Model: "was-digestless"}).String())

	infos := []ModelInfo{
		{Name: "said-yes"},
		{Name: "said-no"},
		{Name: "stale-no"},
		{Name: "unprobed"},
		{Name: "was-digestless", Digest: "sha256:now-live-digest"},
	}
	facts, err := mr.ProjectListedModels(ctx, "prov", infos)
	if err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	byName := map[string]ListedModelFacts{}
	for _, f := range facts {
		byName[f.Key.Model] = f
	}
	if len(byName) != 5 {
		t.Fatalf("got %d distinct facts; want 5", len(byName))
	}
	if f := byName["said-yes"]; !f.Caps.Has(CapToolCall) || !f.KnownMask.Has(CapToolCall) {
		t.Fatalf("said-yes: Caps=%v KnownMask=%v; want tool_call set in both", f.Caps, f.KnownMask)
	}
	if f := byName["said-no"]; f.Caps.Has(CapToolCall) || !f.KnownMask.Has(CapToolCall) {
		t.Fatalf("said-no: Caps=%v KnownMask=%v; want KNOWN-no (mask set, cap clear)", f.Caps, f.KnownMask)
	}
	if f := byName["stale-no"]; f.Caps.Has(CapToolCall) || f.KnownMask.Has(CapToolCall) {
		t.Fatalf("stale-no: Caps=%v KnownMask=%v; a digest-mismatched row must claim nothing", f.Caps, f.KnownMask)
	}
	if f := byName["unprobed"]; f.KnownMask.Has(CapToolCall) {
		t.Fatalf("unprobed: KnownMask=%v; no row must mean unknown", f.KnownMask)
	}
	if f := byName["was-digestless"]; f.Caps.Has(CapToolCall) || f.KnownMask.Has(CapToolCall) {
		t.Fatalf("was-digestless: Caps=%v KnownMask=%v; a digestless row must not claim against a digested listing", f.Caps, f.KnownMask)
	}
}

// TestProjectListedModels_CatalogAndFingerprintLayers gives the catalog and
// fingerprint layers positive coverage: qwen3:8b is a static-catalog hit
// (caps ["completion","tools"], context_window 40960), and the seeded
// fingerprint profile carries EffectiveContext 24576, which outranks the
// catalog context in merge. The listing deliberately supplies NO
// Capabilities and NO ContextWindow so each asserted fact can only come
// from its layer: CapToolCall only from the catalog, ContextWindow 24576
// only from the fingerprint profile (a listing ContextWindow would
// override it -- runtime layer is higher precedence).
func TestProjectListedModels_CatalogAndFingerprintLayers(t *testing.T) {
	ctx := context.Background()
	key := ModelKey{Provider: "prov", Model: "qwen3:8b"}
	store := newMrMockFingerprintStore()
	store.profiles["prov\x00qwen3:8b"] = &fingerprint.Profile{
		BackendID: key.Provider, ModelName: key.Model, ModelDigest: key.String(),
		ProfileVersion: fingerprint.CurrentProfileVersion, EffectiveContext: 24_576,
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, store,
		WithReadOnlyFingerprintProfiles(func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
			return &FingerprintProberSpec{Prober: &mrRecordingFingerprintProber{}, ModelDigest: key.String()}, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	facts, err := mr.ProjectListedModels(ctx, "prov", []ModelInfo{{Name: "qwen3:8b"}})
	if err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("len(facts) = %d; want 1", len(facts))
	}
	f := facts[0]
	if !f.Caps.Has(CapToolCall) {
		t.Fatalf("Caps = %v; want catalog-supplied tool_call (listing declared no capabilities)", f.Caps)
	}
	if f.Family != "qwen3" {
		t.Fatalf("Family = %q; want catalog family %q", f.Family, "qwen3")
	}
	if f.ContextWindow != 24_576 {
		t.Fatalf("ContextWindow = %d; want fingerprint EffectiveContext 24576 (catalog alone would give 40960)", f.ContextWindow)
	}
}

func TestProjectListedModels_OverrideWins(t *testing.T) {
	ctx := context.Background()
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	mr.SetCapabilityOverride(func(k ModelKey) []string {
		if k.Model == "pinned" {
			return []string{"chat", "tool_call"}
		}
		return nil
	})
	facts, err := mr.ProjectListedModels(ctx, "prov", []ModelInfo{{Name: "pinned"}})
	if err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	if !facts[0].Caps.Has(CapToolCall) {
		t.Fatalf("pinned Caps=%v; explicit override must contribute tool_call", facts[0].Caps)
	}
}

func TestProjectListedModels_NeverProbesNeverRequeries(t *testing.T) {
	// Registry wired for BOTH kinds of active probing -- the configuration
	// providerbootstrap installs in production -- plus a cap-probe store.
	// The projection must fire neither prober and must not re-query the
	// provider. In-test positive controls (the Lookup below) prove the
	// fingerprint counter and the Models counter can detect what they
	// guard against; the tool-call counter's liveness is covered by the
	// capresolve tests. NOTE: the provider MUST list the control model, or
	// the Lookup positive control cannot reach the fingerprint prober.
	ctx := context.Background()
	store := newFakeCapProbeStore()
	tcProber := newFakeToolCallProber()
	fpProber := &mrRecordingFingerprintProber{}
	modelsCalls := 0
	prov := &mrCountingProvider{
		mrMockProvider: &mrMockProvider{
			name:   "prov",
			models: []ModelInfo{{Name: "mystery-a"}, {Name: "mystery-b"}},
		},
		callCount: &modelsCalls,
	}
	reg := &mrMockProviderRegistry{providers: map[string]Provider{"prov": prov}}
	mr, err := NewModelRegistry(reg, newMrMockFingerprintStore(),
		WithFingerprintProberFactory(func(_ context.Context, _ ModelKey, _ *ModelInfo, _ Provider) (*FingerprintProberSpec, error) {
			return &FingerprintProberSpec{Prober: fpProber}, nil
		}),
		WithCapabilityProbeStore(store),
		WithCapabilityProber(capProberFactory(tcProber)),
	)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}

	infos := []ModelInfo{{Name: "mystery-a"}, {Name: "mystery-b"}}
	if _, err := mr.ProjectListedModels(ctx, "prov", infos); err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	if n := fpProber.detectCalls + fpProber.chatCalls; n != 0 {
		t.Fatalf("projection fired %d fingerprint probe calls; want 0", n)
	}
	if n := tcProber.totalCalls(); n != 0 {
		t.Fatalf("projection fired %d tool-call probe calls; want 0", n)
	}
	if modelsCalls != 0 {
		t.Fatalf("projection called Provider.Models %d times; want 0 (listing is supplied)", modelsCalls)
	}

	// POSITIVE CONTROL: plain Lookup on this same registry DOES fire the
	// fingerprint prober (TestModelRegistry_Lookup_RunsFingerprintProberFactory)
	// -- the counter works.
	if _, err := mr.Lookup(ctx, ModelKey{Provider: "prov", Model: "mystery-a"}); err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if n := fpProber.detectCalls + fpProber.chatCalls; n == 0 {
		t.Fatal("positive control failed: Lookup fired no fingerprint probes; the zero-assertion above is blind")
	}
	if modelsCalls == 0 {
		t.Fatal("positive control failed: Lookup did not route through Provider.Models; the zero-Models assertion above is blind")
	}
}

func TestProjectListedModels_DoesNotWriteProfileCache(t *testing.T) {
	ctx := context.Background()
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	if _, err := mr.ProjectListedModels(ctx, "prov", []ModelInfo{{Name: "m"}}); err != nil {
		t.Fatalf("ProjectListedModels() error: %v", err)
	}
	mr.mu.RLock()
	cached := len(mr.profiles)
	mr.mu.RUnlock()
	if cached != 0 {
		t.Fatalf("projection wrote %d profile cache entries; want 0", cached)
	}
}

func TestProjectListedModels_CancellationReturnsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reg := &mrMockProviderRegistry{providers: map[string]Provider{
		"prov": &mrMockProvider{name: "prov"},
	}}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	facts, err := mr.ProjectListedModels(ctx, "prov", []ModelInfo{{Name: "m"}})
	if err == nil || facts != nil {
		t.Fatalf("ProjectListedModels(cancelled) = %v, %v; want nil facts + context error", facts, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProjectListedModels(cancelled) error = %v; want context.Canceled", err)
	}
	// Post-loop barrier: pre-cancelled with an EMPTY listing must still
	// return the context error, not an empty slice with nil error.
	facts, err = mr.ProjectListedModels(ctx, "prov", nil)
	if facts != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("ProjectListedModels(cancelled, empty) = %v, %v; want nil facts + context.Canceled", facts, err)
	}
}
