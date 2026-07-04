// provider/model_registry.go
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// capabilityToolCall is the canonical capability token persisted in
// capability_probes rows and resolved by ResolveToolCall.
const capabilityToolCall = "tool_call"

// ProviderResolver provides the subset of Registry methods needed by
// ModelRegistry to query providers and resolve model keys. This interface
// decouples ModelRegistry from the concrete Registry type for testability.
type ProviderResolver interface {
	// Resolve finds a provider by the ModelKey's Provider field.
	Resolve(key ModelKey) (Provider, error)

	// ProvidersForModel returns all providers that advertise the given model.
	ProvidersForModel(model string) ([]Provider, error)

	// All returns every registered provider.
	All() []Provider
}

// ModelRegistry maintains merged model profiles combining runtime provider
// data, static catalog knowledge, and fingerprint observations. It is the
// single source of truth for model capabilities and resource requirements.
//
// Profiles are cached by ModelKey after the first Lookup. The initial
// implementation returns the cached profile on cache hit without digest
// revalidation; callers that need freshness guarantees must use Refresh.
type ModelRegistry struct {
	mu              sync.RWMutex
	profiles        map[ModelKey]*ModelProfile
	catalog         *staticCatalog
	fpStore         fingerprint.Store
	fpProberFactory FingerprintProberFactory
	providers       ProviderResolver
	capOverride     CapabilityOverride
	capFloor        CapabilityFloor
	overrideVersion uint64 // bumped by SetCapabilityOverride AND SetCapabilityFloor; guards stale cache writes in buildProfile
	rejectionHook   OverrideRejectionHook

	// Capability-probe resolution (ResolveToolCall). Both fields are set
	// only at construction via options and never mutated afterward, so
	// they are read without holding mu.
	capProbeStore   fingerprint.CapProbeStore
	capProber       FingerprintProberFactory
	capResolveGroup singleflight.Group
}

// FingerprintProberSpec contains the model prober and digest identity the
// registry should use when refreshing a fingerprint profile.
type FingerprintProberSpec struct {
	Prober      fingerprint.ModelProber
	ModelDigest string
}

// FingerprintProberFactory builds the provider-specific ModelProber for a
// model. Returning nil means fingerprinting is unavailable for this provider.
type FingerprintProberFactory func(ctx context.Context, key ModelKey, runtime *ModelInfo, p Provider) (*FingerprintProberSpec, error)

// ModelRegistryOption configures ModelRegistry construction.
type ModelRegistryOption func(*ModelRegistry)

// WithFingerprintProberFactory installs provider-aware fingerprint prober
// selection. The factory is used only when NewModelRegistry also receives a
// non-nil fingerprint.Store.
func WithFingerprintProberFactory(fn FingerprintProberFactory) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.fpProberFactory = fn
	}
}

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
// named call sites (golem preflight, router candidate resolution).
func WithCapabilityProber(fn FingerprintProberFactory) ModelRegistryOption {
	return func(r *ModelRegistry) {
		r.capProber = fn
	}
}

// CapabilityOverride returns the user-declared canonical capability tokens
// for a model key, or nil when no override is configured for the key. When
// non-nil and well-formed, the returned slice REPLACES the merge-derived
// Caps on the resulting ModelProfile so users can carve down capabilities
// below what the static catalog or runtime probe claims — e.g. removing
// "generate" for an OpenAI-compatible server that lacks /v1/completions.
//
// Tokens MUST be canonical single-bit names (see CanonicalCapabilityNames /
// ParseCapsStrict). Non-canonical tokens — unknowns AND catalog aliases
// that would expand to multiple bits like "completion" or the catalog
// shorthand "insert" → generate+stream+insert — are REJECTED at the apply
// site, not silently expanded. On rejection the merge-derived caps are
// preserved unchanged so a misconfigured override cannot cripple a model
// for every router gate downstream. Install an OverrideRejectionHook via
// SetOverrideRejectionHook to surface rejections; otherwise the drop is
// silent by design (preserves backward compatibility for callers that
// only set the override).
//
// Replacement is wholesale and applies AFTER every other merge layer,
// including the runtime-template-driven CapInsert toggle in merge(). An
// override that omits "insert" drops runtime-detected CapInsert by design
// (the user's declaration wins); this is a sharp interaction worth knowing.
//
// Return semantics:
//   - nil slice: no override applied for this key; merge-derived caps used
//   - empty []string: ignored (no zero-out)
//   - any non-canonical token: ignored (no silent expansion)
//   - parses to zero caps: ignored (same crippling hazard)
type CapabilityOverride func(key ModelKey) []string

// CapabilityFloor returns baseline canonical capability tokens for a model
// key, or nil when none. Floor caps are OR-merged into the profile at the
// LOWEST precedence (with the static catalog layer): they guarantee a
// minimum capability set for models whose provider exposes no capability
// metadata (openai-compat), WITHOUT erasing catalog/fingerprint/runtime
// additions the way the REPLACE override does. Tokens must be canonical
// (ParseCapsStrict); invalid slices are dropped with the rejection hook
// fired and never zero or shrink the profile.
//
// CapInsert exception: the runtime layer sits ABOVE the floor and clears
// CapInsert (`&^= CapInsert`) when the model's template lacks suffix
// markers, so a floor-supplied "insert" bit may still be removed by
// runtime template detection. Every other floor bit is OR-only.
type CapabilityFloor func(key ModelKey) []string

// OverrideRejectionHook is called when a capability override returned a
// non-empty token slice that failed strict canonical-only parsing — i.e.
// the override was applied at the registry but had to be DROPPED because
// the tokens were unknown or were multi-bit aliases that would silently
// expand and violate the REPLACES contract.
//
// The merged profile preserves its pre-override caps in this case (fail
// safe), so the hook is the only signal the misconfiguration ever happened.
// Without it the override is silently swallowed and the operator sees
// router decisions that don't match the config they wrote.
//
// The hook also fires for rejected capability-floor slices: a floor whose
// tokens fail strict parsing is dropped wholesale (caps unchanged) with
// the same observability contract.
//
// Typical implementations log the rejection or emit a metric. Hooks MUST
// be cheap and non-blocking — they run inside merge() on every cache miss
// for the affected key until a Refresh clears the cached pre-override
// profile, so a slow hook would amplify into a hot-path stall.
type OverrideRejectionHook func(key ModelKey, tokens []string, err error)

// SetOverrideRejectionHook installs (or clears) the rejection-callback
// hook. Pass nil to disable. Safe for concurrent use.
//
// Unlike SetCapabilityOverride this does NOT invalidate the cache: the
// hook only fires when an override is rejected at merge time, and changing
// observability shouldn't churn the cache. Callers that need rejections
// surfaced for already-merged profiles must call Refresh themselves.
func (r *ModelRegistry) SetOverrideRejectionHook(fn OverrideRejectionHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rejectionHook = fn
}

// SetCapabilityOverride installs (or clears) the capability override hook.
// Pass nil to disable overrides. Safe for concurrent use.
//
// Installing or clearing an override invalidates the entire profile cache
// so the new policy applies to every subsequent Lookup, not just to
// previously-uncached keys. This matches the "Rebuild downstream objects
// when cache refreshes" principle that keeps the wiring sequence
// (build registry -> RefreshModels -> install override) correct regardless
// of which step warmed which entries.
//
// Concurrency model: cache invalidation alone is not enough. An in-flight
// buildProfile that read the OLD override before this call could otherwise
// finish, write the stale-policy profile to the freshly-cleared map, and
// shadow this swap until the next Refresh. To prevent that TOCTOU window
// the override version is bumped here; buildProfile snapshots the version
// alongside the override and only writes its result to the cache if the
// version is still current at write time. Stale results are returned to
// the caller (correct for that caller's snapshot) but never cached.
func (r *ModelRegistry) SetCapabilityOverride(fn CapabilityOverride) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capOverride = fn
	r.overrideVersion++
	// Cached profiles were merged under the previous override (possibly
	// nil); flush them so the next Lookup re-runs merge with the new
	// override in effect. Skipping this flush is the bug class where a
	// warm cache silently shadows config changes — see feedback memory
	// on rebuilding downstream state when a cache source changes.
	clear(r.profiles)
}

// SetCapabilityFloor installs (or clears) the capability floor hook.
// Pass nil to disable. Safe for concurrent use.
//
// Shares SetCapabilityOverride's invalidation + version-guard semantics:
// the profile cache is flushed and the single policy version counter is
// bumped so an in-flight buildProfile that snapshotted the OLD floor
// cannot repopulate the cache with a stale-policy profile.
func (r *ModelRegistry) SetCapabilityFloor(fn CapabilityFloor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capFloor = fn
	r.overrideVersion++
	clear(r.profiles)
}

// directModelInfoProvider is an optional provider capability that returns
// metadata for a specific model without relying on a full model-list scan.
// It is used as a resilience fallback when list queries fail or when the
// requested model name does not appear verbatim in the provider's tags list.
type directModelInfoProvider interface {
	ModelInfo(ctx context.Context, name string) (*ModelInfo, error)
}

// NewModelRegistry creates a ModelRegistry backed by the given provider
// resolver and fingerprint store. The static catalog is loaded from the
// embedded catalog.json at construction time. The fingerprint store may
// be nil if fingerprint enrichment is not available.
func NewModelRegistry(providers ProviderResolver, fpStore fingerprint.Store, opts ...ModelRegistryOption) (*ModelRegistry, error) {
	catData, err := loadCatalog()
	if err != nil {
		return nil, fmt.Errorf("provider: new model registry: %w", err)
	}

	r := &ModelRegistry{
		profiles:  make(map[ModelKey]*ModelProfile),
		catalog:   newStaticCatalog(catData),
		fpStore:   fpStore,
		providers: providers,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// Lookup returns the merged ModelProfile for a provider-qualified model key.
// Results are cached by ModelKey. The initial implementation returns the cached
// profile on cache hit; callers that need digest revalidation must use Refresh.
func (r *ModelRegistry) Lookup(ctx context.Context, key ModelKey) (*ModelProfile, error) {
	// Check cache first.
	r.mu.RLock()
	if cached, ok := r.profiles[key]; ok {
		r.mu.RUnlock()
		return cached, nil
	}
	r.mu.RUnlock()

	return r.buildProfile(ctx, key)
}

// Refresh forces a re-merge for the given model key, bypassing the cache.
// Used when the caller knows or suspects the model has been updated.
func (r *ModelRegistry) Refresh(ctx context.Context, key ModelKey) (*ModelProfile, error) {
	// Remove cached entry.
	r.mu.Lock()
	delete(r.profiles, key)
	r.mu.Unlock()

	return r.buildProfile(ctx, key)
}

// ResolveToolCall resolves the tri-state tool-call capability for a model
// key on demand. Precedence:
//  1. Explicit capability override -- authoritative in both directions
//     (declared set contains tool_call => yes, otherwise no), no probe.
//  2. Merged profile already carries CapToolCall (catalog/floor/runtime/
//     cached probe row) => yes, no probe.
//  3. Resolution disabled (no prober or no store) => "" (unknown) -- the
//     empty state is never a claim.
//  4. Active probe, singleflight-deduplicated per key: valid cached row
//     wins, otherwise probe, persist the verdict, and invalidate the
//     cached profile so the next Lookup re-merges.
//
// A non-nil error means the probe was transient (network/auth); nothing
// is persisted and the state is "".
func (r *ModelRegistry) ResolveToolCall(ctx context.Context, key ModelKey) (fingerprint.CapProbeState, error) {
	// 1. Explicit declaration is authoritative both directions. A rejected
	// override (non-canonical tokens / zero caps) is a no-op here exactly
	// as it is in merge(): fall through rather than fabricate a verdict.
	r.mu.RLock()
	override := r.capOverride
	r.mu.RUnlock()
	if override != nil {
		if tokens := override(key); len(tokens) > 0 {
			if caps, err := ParseCapsStrict(tokens); err == nil && caps != 0 {
				if caps.Has(CapToolCall) {
					return fingerprint.CapProbeYes, nil
				}
				return fingerprint.CapProbeNo, nil
			}
		}
	}

	// 2. Merged profile short-circuit (cache READ only; Lookup never probes).
	profile, err := r.Lookup(ctx, key)
	if err != nil {
		return "", err
	}
	if profile.Caps.Has(CapToolCall) {
		return fingerprint.CapProbeYes, nil
	}

	// 3. Resolution disabled: unknown is never a claim.
	if r.capProber == nil || r.capProbeStore == nil {
		return "", nil
	}

	// 4. Deduplicated slow path. The singleflight fn holds no registry
	// locks; invalidateProfile takes mu only briefly after probe IO.
	// Flight key uses an explicit NUL separator (not key.String(), whose
	// "provider/model" join could collide across keys containing slashes).
	// Follower semantics: the leader's ctx governs the probe; followers
	// block until the shared flight completes and receive its result even
	// if their own ctx was canceled meanwhile.
	flightKey := key.Provider + "\x00" + key.Model
	v, err, _ := r.capResolveGroup.Do(flightKey, func() (interface{}, error) {
		return r.resolveToolCallSlow(ctx, key)
	})
	if err != nil {
		return "", err
	}
	return v.(fingerprint.CapProbeState), nil
}

// resolveToolCallSlow is the probe path behind ResolveToolCall's
// singleflight: cache check, active probe, persistence, and profile-cache
// invalidation. Must not be called while holding r.mu.
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
	if err != nil {
		return "", err
	}
	if spec == nil || spec.Prober == nil {
		return "", nil
	}
	prober, ok := spec.Prober.(fingerprint.ToolCallProber)
	if !ok {
		return "", nil
	}

	// Cap-probe rows deliberately IGNORE spec.ModelDigest: the real factory
	// (providerbootstrap's fingerprintDigestForModel) synthesizes
	// "config-caps:<caps>" digests when the runtime digest is empty --
	// exactly the openai-compat case -- and the read side (capProbeCaps:
	// runtimeInfo.Digest fallback key.String()) can never reproduce that
	// value. Rows keyed by it would never merge into profiles, and
	// synthetic-digest negatives would dodge the digestless TTL cap below.
	// Using runtimeInfo.Digest with the key fallback keeps the write and
	// read digests byte-symmetric by construction.
	digest := capProbeDigest(key, runtimeInfo)
	backendID := key.Provider // EnsureProfile's existing backend_id convention

	now := time.Now()
	if row, gErr := r.capProbeStore.GetCapProbe(ctx, backendID, key.Model, capabilityToolCall); gErr == nil && row != nil && row.Valid(digest, now) {
		return row.State, nil
	}

	outcome, err := prober.ProbeToolCall(ctx, key.Model)
	if err != nil {
		// Transient/diagnostic: never persisted, never a claim.
		return "", err
	}

	row := fingerprint.CapProbe{
		BackendID:    backendID,
		ModelName:    key.Model,
		Capability:   capabilityToolCall,
		State:        outcome.State,
		ModelDigest:  digest,
		ProbeVersion: fingerprint.CurrentToolProbeVersion,
		TestedAt:     now,
	}
	if outcome.TTL > 0 {
		row.ExpiresAt = now.Add(outcome.TTL)
	}
	// Digestless negatives expire: a wedged "no" keyed only by name would
	// silently block usage until a manual reprobe. A real digest keeps the
	// prober-chosen TTL (ollama's curated "no" stays unbounded).
	if outcome.State == fingerprint.CapProbeNo && digest == key.String() {
		row.ExpiresAt = now.Add(fingerprint.CapProbeDigestlessNoTTL)
	}
	// Persistence failure is non-fatal: the verdict stands for this caller.
	_ = r.capProbeStore.SaveCapProbe(ctx, row)

	if outcome.State == fingerprint.CapProbeYes {
		r.invalidateProfile(key)
	}
	return outcome.State, nil
}

// invalidateProfile drops one cached profile so the next Lookup re-merges.
//
// It also bumps the shared policy version counter: a buildProfile in
// flight for this key may have read the store BEFORE the probe row was
// saved; without the bump its cache write would pass the version guard
// and re-cache the stale no-toolcall profile permanently (ResolveToolCall's
// row-hit fast path never invalidates, so it would never self-heal).
// Bumping fences those in-flight writes exactly like
// SetCapabilityOverride/SetCapabilityFloor do.
func (r *ModelRegistry) invalidateProfile(key ModelKey) {
	r.mu.Lock()
	delete(r.profiles, key)
	r.overrideVersion++
	r.mu.Unlock()
}

// canResolveToolCall reports whether on-demand tool-call resolution is
// fully wired (prober factory AND probe store).
func (r *ModelRegistry) canResolveToolCall() bool {
	return r != nil && r.capProber != nil && r.capProbeStore != nil
}

// EnsureToolCallResolved resolves tool_call for candidate profiles that
// lack CapToolCall, returning a slice where probe-yes candidates are
// replaced by their refreshed profiles. Candidates are NEVER removed --
// probe-no/inconclusive/error entries are returned unchanged so the
// caller's RequiredCaps gate stays the single rejection point. The input
// slice and its profiles are never modified; replacements land in a copy.
// No-op (input returned as-is) when resolution is disabled or required
// does not include CapToolCall.
//
// The returned errors are per-candidate probe diagnostics labeled with
// the model key (transient failures: network, auth, ...). They are
// diagnostics ONLY -- never a rejection signal; the affected candidates
// stay in the returned slice unchanged -- so operators can distinguish
// "probe failed (e.g. 401)" from "genuinely not tool-capable" when a
// route comes up empty. Callers thread them into the route-failure error.
func (r *ModelRegistry) EnsureToolCallResolved(ctx context.Context, profiles []*ModelProfile, required Capability) ([]*ModelProfile, []error) {
	if !r.canResolveToolCall() || !required.Has(CapToolCall) {
		return profiles, nil
	}
	var diags []error
	out := make([]*ModelProfile, len(profiles))
	copy(out, profiles)
	for i, p := range out {
		if p == nil || p.Caps.Has(CapToolCall) {
			continue
		}
		state, err := r.ResolveToolCall(ctx, p.Key)
		if err != nil {
			diags = append(diags, fmt.Errorf("resolve tool_call %s: %w", p.Key, err))
			continue
		}
		if state != fingerprint.CapProbeYes {
			continue
		}
		if refreshed, lErr := r.Lookup(ctx, p.Key); lErr == nil && refreshed.Caps.Has(CapToolCall) {
			out[i] = refreshed
			continue
		}
		// The probe said yes but the refreshed profile lacks the bit --
		// e.g. SaveCapProbe failed (swallowed as non-fatal) so the merge
		// layer had no row to read. A lost store write must not erase the
		// verdict for this caller: patch a copy, never the caller's input.
		cp := *p
		cp.Caps |= CapToolCall
		out[i] = &cp
	}
	return out, diags
}

// LookupAny returns merged profiles for an unqualified model name across
// all providers that advertise it. Returns one ModelProfile per provider.
func (r *ModelRegistry) LookupAny(ctx context.Context, model string) ([]*ModelProfile, error) {
	providers, err := r.providers.ProvidersForModel(model)
	if err != nil {
		return nil, err
	}

	var profiles []*ModelProfile
	var firstErr error

	for _, p := range providers {
		key := ModelKey{Provider: p.Name(), Model: model}
		profile, lookupErr := r.Lookup(ctx, key)
		if lookupErr != nil {
			if firstErr == nil {
				firstErr = lookupErr
			}
			continue
		}
		profiles = append(profiles, profile)
	}

	if len(profiles) == 0 {
		return nil, fmt.Errorf("provider: lookup any %q: all providers failed: %w", model, firstErr)
	}

	return profiles, nil
}

// All returns merged profiles for every model across every registered
// provider. Individual lookup errors are skipped to provide best-effort
// results, but if ALL providers fail, an error is returned.
func (r *ModelRegistry) All(ctx context.Context) ([]*ModelProfile, error) {
	allProviders := r.providers.All()
	if len(allProviders) == 0 {
		return nil, nil
	}

	var profiles []*ModelProfile
	var providerErrors int

	for _, p := range allProviders {
		models, err := p.Models(ctx)
		if err != nil {
			providerErrors++
			continue
		}
		for _, mi := range models {
			key := ModelKey{Provider: p.Name(), Model: mi.Name}
			profile, err := r.Lookup(ctx, key)
			if err != nil {
				continue
			}
			profiles = append(profiles, profile)
		}
	}

	if len(profiles) == 0 && providerErrors == len(allProviders) {
		return nil, fmt.Errorf("provider: model registry: all %d providers failed", providerErrors)
	}

	return profiles, nil
}

// Recommend returns a ranked list of model profiles matching the given
// criteria. Results are filtered by required capabilities and available
// RAM, then sorted by quality tier descending. The Router makes the
// final selection from this candidate list.
//
// When opts.RestrictToProvider is non-empty, Recommend builds the initial
// candidate set from only the named provider (via recommendSourceProfiles)
// rather than walking All() and filtering after probing unrelated providers.
// An unknown RestrictToProvider value surfaces as a provider resolution
// error rather than degrading silently to an empty result.
func (r *ModelRegistry) Recommend(ctx context.Context, opts RecommendOpts) ([]*ModelProfile, error) {
	all, err := r.recommendSourceProfiles(ctx, opts.RestrictToProvider)
	if err != nil {
		return nil, fmt.Errorf("provider: recommend: %w", err)
	}

	var candidates []*ModelProfile

	for _, p := range all {
		// Filter by required capabilities.
		if opts.RequiredCaps != 0 && (p.Caps&opts.RequiredCaps) != opts.RequiredCaps {
			continue
		}

		// Filter by RAM.
		if opts.AvailableRAM > 0 {
			ramNeeded := p.Resources.RAMRequired
			if opts.ContextSize > 0 {
				ramNeeded = p.Resources.RAMAtContext(opts.ContextSize)
			}
			if ramNeeded > opts.AvailableRAM {
				continue
			}
		}

		// Filter by context size.
		if opts.ContextSize > 0 && p.ContextWindow > 0 && p.ContextWindow < opts.ContextSize {
			continue
		}

		candidates = append(candidates, p)
	}

	// Sort by quality descending, then speed descending for ties.
	sortProfiles(candidates)

	return candidates, nil
}

// recommendSourceProfiles produces the initial profile set Recommend will
// filter and rank. When providerName is empty, it returns the cross-provider
// All() view. When non-empty, it scopes to that single provider instance —
// resolving and probing only that provider, so unrelated provider outages
// cannot break a scoped recommendation. An unknown providerName surfaces
// as a provider resolution error.
func (r *ModelRegistry) recommendSourceProfiles(ctx context.Context, providerName string) ([]*ModelProfile, error) {
	if providerName == "" {
		return r.All(ctx)
	}

	prov, err := r.providers.Resolve(ModelKey{Provider: providerName})
	if err != nil {
		return nil, fmt.Errorf("restricted provider %q: %w", providerName, err)
	}

	models, err := prov.Models(ctx)
	if err != nil {
		return nil, fmt.Errorf("query models from restricted provider %q: %w", providerName, err)
	}

	profiles := make([]*ModelProfile, 0, len(models))
	for _, mi := range models {
		profile, lookupErr := r.Lookup(ctx, ModelKey{Provider: providerName, Model: mi.Name})
		if lookupErr != nil {
			continue
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

// FIMConfigFor returns the local FIM policy for a given model, or nil if no
// stop-token or budget overrides are known. Capability detection is resolved
// separately from runtime model metadata.
func (r *ModelRegistry) FIMConfigFor(ctx context.Context, key ModelKey) (*FIMConfig, error) {
	profile, err := r.Lookup(ctx, key)
	if err != nil {
		return nil, err
	}
	return profile.FIM, nil
}

// buildProfile performs the three-layer merge for a model key and caches
// the result. The merge layers in order of ascending precedence are:
//  1. Static catalog: FIM policy, think_mode, quality/speed tiers, RAM estimates
//  2. Fingerprint: capability probing data, benchmarked resource observations
//  3. Runtime: context_window, parameter_size, quant_level, digest (freshest)
func (r *ModelRegistry) buildProfile(ctx context.Context, key ModelKey) (*ModelProfile, error) {
	// Snapshot (override, floor, version, rejectionHook) FIRST so the
	// policy a single buildProfile applies is fixed at start, and the
	// cache write at the end can detect any concurrent
	// SetCapabilityOverride or SetCapabilityFloor (both bump the shared
	// version counter). Reading
	// later (after slow IO like queryRuntime) would shrink the visible
	// TOCTOU window but also leave the same race against the cache write
	// itself; reading first keeps the contract simple: one buildProfile ->
	// one policy version.
	r.mu.RLock()
	override := r.capOverride
	floor := r.capFloor
	overrideVer := r.overrideVersion
	rejectionHook := r.rejectionHook
	r.mu.RUnlock()

	// Layer 1: Runtime query.
	runtimeInfo, err := r.queryRuntime(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("provider: lookup %v: %w", key, err)
	}

	// Layer 2: Static catalog match.
	parsed := ParseModelName(key.Model)
	staticProfile := r.catalogProfileFor(parsed, runtimeInfo)

	// Layer 3: Fingerprint enrichment.
	fpProfile := r.fingerprintProfile(ctx, key, runtimeInfo)

	// Cap-probe merge layer: READ ONLY -- never probes. A valid cached
	// "yes" row ORs CapToolCall into the profile. Computed here (not in
	// merge) so merge stays IO-free and applies exactly the bits this
	// buildProfile snapshot observed.
	capProbeCaps := r.capProbeCaps(ctx, key, runtimeInfo)

	// Merge layers using the (override, floor, rejectionHook) snapshot
	// taken at function entry.
	profile := r.merge(key, runtimeInfo, staticProfile, fpProfile, parsed, override, floor, rejectionHook, capProbeCaps)

	// Cache the result iff the override snapshot is still current.
	r.mu.Lock()
	if r.overrideVersion == overrideVer {
		r.profiles[key] = profile
	}
	r.mu.Unlock()

	return profile, nil
}

// catalogProfileFor resolves the static-catalog profile for a parsed model
// name, applying the same fallback ladder buildProfile uses: exact
// family+param, family+tag, family-only, then runtime-reported family.
// Returns nil when the catalog knows nothing about the model.
func (r *ModelRegistry) catalogProfileFor(parsed ParsedModel, runtimeInfo *ModelInfo) *ModelProfile {
	staticProfile := r.catalog.lookup(parsed.NormalizedFamily(), parsed.ParamSize)
	if staticProfile == nil && parsed.ParamSize == "" && parsed.Tag != "" {
		// Some catalog-only variants are keyed by tag (for example ":latest")
		// rather than by parsed parameter size.
		staticProfile = r.catalog.lookup(parsed.NormalizedFamily(), parsed.Tag)
	}
	if staticProfile == nil {
		// Try family-only lookup for family-level metadata (FIM, think mode).
		staticProfile = r.catalog.lookupFamily(parsed.NormalizedFamily())
	}
	if staticProfile == nil && runtimeInfo != nil && runtimeInfo.Family != "" {
		// Fall back to runtime-reported family.
		staticProfile = r.catalog.lookupFamily(strings.ToLower(runtimeInfo.Family))
	}
	return staticProfile
}

// ToolCallExplanation is diagnostic provenance for one model's tool_call
// capability, consumed by `golem models`. Deliberately narrow -- not a general
// provenance API.
type ToolCallExplanation struct {
	Caps     Capability
	Has      bool
	Source   string                    // "explicit" | "catalog" | "runtime" | "probe" | "unknown"
	State    fingerprint.CapProbeState // cached probe state, "" if none
	TestedAt time.Time                 // zero when no probe row
}

// ExplainToolCall reports where a model's tool_call bit comes from (or why it
// is absent). READ-ONLY: it consults the explicit override, the merged profile
// (a cache read; Lookup never probes), the static catalog, and the cap-probe
// cache -- it never triggers an active probe. Precedence mirrors
// ResolveToolCall / merge():
//  1. explicit override => "explicit" (present or absent per the declared set).
//  2. merged profile carries CapToolCall => catalog / probe / runtime,
//     distinguished by re-running the pure catalog lookup and the cached row.
//  3. profile lacks tool_call but a probe row (any state) exists => "probe".
//  4. otherwise => "unknown".
//
// State + TestedAt are populated from the cap-probe row whenever one exists,
// regardless of the chosen Source, so operators see the last verdict and when.
func (r *ModelRegistry) ExplainToolCall(ctx context.Context, key ModelKey) (ToolCallExplanation, error) {
	// 1. Explicit override is authoritative in both directions.
	r.mu.RLock()
	override := r.capOverride
	r.mu.RUnlock()
	if override != nil {
		if tokens := override(key); len(tokens) > 0 {
			if caps, err := ParseCapsStrict(tokens); err == nil && caps != 0 {
				return ToolCallExplanation{
					Caps:   caps,
					Has:    caps.Has(CapToolCall),
					Source: "explicit",
				}, nil
			}
		}
	}

	profile, err := r.Lookup(ctx, key)
	if err != nil {
		return ToolCallExplanation{}, err
	}

	// Read the cap-probe row (any state) once so State/TestedAt are always
	// populated when a row exists. Uses the write-side digest convention.
	row := r.capProbeRow(ctx, key)

	exp := ToolCallExplanation{Caps: profile.Caps, Has: profile.Caps.Has(CapToolCall)}
	if row != nil {
		exp.State = row.State
		exp.TestedAt = row.TestedAt
	}

	if profile.Caps.Has(CapToolCall) {
		// Distinguish catalog vs probe vs runtime for a present bit. The catalog
		// lookup is a pure in-memory read (no probe); if the catalog profile
		// carries tool_call, that is the source.
		// This queryRuntime is deliberate: Lookup above ran its own internal
		// queryRuntime but does not expose the digest, and the probe-row Valid
		// check below needs it (via capProbeDigest). It is a metadata read, not
		// a tool-call probe -- the READ-ONLY contract holds.
		runtimeInfo, _ := r.queryRuntime(ctx, key)
		parsed := ParseModelName(key.Model)
		if static := r.catalogProfileFor(parsed, runtimeInfo); static != nil && static.Caps.Has(CapToolCall) {
			exp.Source = "catalog"
			return exp, nil
		}
		// "probe" only when the row is the one the merge layer would actually
		// honor: a VALID "yes" (same digest + probe version, unexpired). A stale
		// yes row cannot be the source of a live bit -- that came from floor /
		// fingerprint / runtime -- so gate on Valid() exactly like capProbeCaps,
		// using the identical digest fallback (runtimeInfo.Digest -> key.String()).
		if row != nil && row.State == fingerprint.CapProbeYes && row.Valid(capProbeDigest(key, runtimeInfo), time.Now()) {
			exp.Source = "probe"
			return exp, nil
		}
		// Floor/fingerprint/runtime-supplied bit: registry/provider-layer
		// knowledge, labeled "runtime" for operator diagnostics.
		exp.Source = "runtime"
		return exp, nil
	}

	// Bit absent: a probe row (no/inconclusive) is still a "probe" source so
	// operators can see the verdict that blocked it.
	if row != nil {
		exp.Source = "probe"
		return exp, nil
	}
	exp.Source = "unknown"
	return exp, nil
}

// capProbeDigest returns the digest a cap-probe row is keyed by: the runtime
// content digest, or key.String() when the runtime is digestless (the
// openai-compat case). Write side (resolveToolCallSlow) and read sides
// (capProbeCaps, ExplainToolCall) MUST agree byte-for-byte, so the convention
// lives in one place.
func capProbeDigest(key ModelKey, runtimeInfo *ModelInfo) string {
	if runtimeInfo != nil && runtimeInfo.Digest != "" {
		return runtimeInfo.Digest
	}
	return key.String()
}

// capProbeRow reads the persisted tool-call probe row for a key (any state),
// or nil when the store is absent or has no row. Uses the same digest-agnostic
// read the merge layer uses: the row is returned even when stale so callers can
// surface State/TestedAt; validity is the caller's concern. Pure cache read.
func (r *ModelRegistry) capProbeRow(ctx context.Context, key ModelKey) *fingerprint.CapProbe {
	if r.capProbeStore == nil {
		return nil
	}
	row, err := r.capProbeStore.GetCapProbe(ctx, key.Provider, key.Model, capabilityToolCall)
	if err != nil || row == nil {
		return nil
	}
	return row
}

func (r *ModelRegistry) fingerprintProfile(ctx context.Context, key ModelKey, runtimeInfo *ModelInfo) *fingerprint.Profile {
	if r.fpStore == nil {
		return nil
	}

	if r.fpProberFactory != nil {
		if p, err := r.providers.Resolve(key); err == nil {
			spec, err := r.fpProberFactory(ctx, key, runtimeInfo, p)
			if err == nil && spec != nil && spec.Prober != nil {
				modelDigest := spec.ModelDigest
				if modelDigest == "" && runtimeInfo != nil {
					modelDigest = runtimeInfo.Digest
				}
				if modelDigest == "" {
					modelDigest = key.String()
				}
				profile, err := fingerprint.NewProfiler(r.fpStore, spec.Prober).EnsureProfile(ctx, key.Provider, key.Model, modelDigest)
				if err == nil {
					return profile
				}
			}
		}
	}

	fpProfile, _ := r.fpStore.Get(ctx, key.Provider, key.Model)
	// Ignore errors -- fingerprint is optional enrichment.
	return fpProfile
}

// capProbeCaps reads the persisted tool-call probe verdict for a key and
// returns the capability bits it contributes to the merge (CapToolCall for
// a valid "yes" row, zero otherwise). Pure cache read; never probes.
func (r *ModelRegistry) capProbeCaps(ctx context.Context, key ModelKey, runtimeInfo *ModelInfo) Capability {
	if r.capProbeStore == nil {
		return 0
	}
	digest := capProbeDigest(key, runtimeInfo)
	row, err := r.capProbeStore.GetCapProbe(ctx, key.Provider, key.Model, capabilityToolCall)
	if err != nil {
		if errors.Is(err, fingerprint.ErrNotFound) {
			return 0 // expected miss: never probed
		}
		// Real store error (corruption, IO): fail closed to "no bits" --
		// enrichment is optional and must never block a profile build.
		return 0
	}
	if row == nil {
		return 0
	}
	if row.State == fingerprint.CapProbeYes && row.Valid(digest, time.Now()) {
		return CapToolCall
	}
	return 0
}

// queryRuntime asks the provider for model information by resolving the
// provider and scanning its model list.
func (r *ModelRegistry) queryRuntime(ctx context.Context, key ModelKey) (*ModelInfo, error) {
	p, err := r.providers.Resolve(key)
	if err != nil {
		return nil, err
	}
	direct, _ := p.(directModelInfoProvider)

	models, err := p.Models(ctx)
	if err != nil {
		if direct != nil {
			if detail, detailErr := direct.ModelInfo(ctx, key.Model); detailErr == nil {
				return detail, nil
			}
		}
		return nil, fmt.Errorf("query models from %q: %w", key.Provider, err)
	}

	for i := range models {
		if models[i].Name == key.Model {
			return &models[i], nil
		}
	}
	if direct != nil {
		if detail, err := direct.ModelInfo(ctx, key.Model); err == nil {
			return detail, nil
		}
	}

	return nil, fmt.Errorf("model %q not found on %q", key.Model, key.Provider)
}

// merge combines runtime, static, and fingerprint data into a single
// ModelProfile. Precedence (lowest to highest):
//   - Static catalog: FIM policy, think_mode, quality/speed tiers, RAM estimates
//   - Fingerprint: capability probing data, benchmarked resource observations
//   - Runtime: context_window, dimensions, parameter_size, quant_level (freshest)
func (r *ModelRegistry) merge(
	key ModelKey,
	runtime *ModelInfo,
	static *ModelProfile,
	fp *fingerprint.Profile,
	parsed ParsedModel,
	override CapabilityOverride,
	floor CapabilityFloor,
	rejectionHook OverrideRejectionHook,
	capProbeCaps Capability,
) *ModelProfile {
	profile := &ModelProfile{
		Key:       key,
		Name:      key.Model,
		Provider:  key.Provider,
		Source:    SourceMerged,
		UpdatedAt: time.Now(),
	}

	// Start with static catalog (lowest precedence).
	if static != nil {
		profile.Family = static.Family
		profile.FIM = static.FIM
		profile.ThinkMode = static.ThinkMode
		profile.ThinkTags = static.ThinkTags
		profile.Quality = static.Quality
		profile.Speed = static.Speed
		profile.Caps = static.Caps
		profile.Dimensions = static.Dimensions
		profile.Resources = static.Resources

		if static.ContextWindow > 0 {
			profile.ContextWindow = static.ContextWindow
		}
	}

	// Capability floor (lowest precedence, OR-merge). Guarantees baseline
	// caps for providers that expose no capability metadata without the
	// REPLACE override's erasure semantics. Invalid tokens are dropped
	// wholesale with the rejection hook fired — a floor must never zero,
	// shrink, or partially apply to the profile. Passed in by buildProfile
	// (not read from r) for the same TOCTOU reason as the override.
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

	// Apply fingerprint enrichment (medium precedence).
	if fp != nil {
		fpCaps := parseCaps(fp.Capabilities)
		if fpCaps != 0 {
			profile.Caps |= fpCaps
		}
		if fp.PeakMemoryMB > 0 {
			// Fingerprint observed actual memory -- more accurate than catalog estimate.
			observedGB := float64(fp.PeakMemoryMB) / 1024.0
			if observedGB > profile.Resources.RAMRequired {
				profile.Resources.RAMRequired = observedGB
			}
		}
	}

	// Cap-probe verdict (read-only layer, computed by buildProfile). OR-only
	// like the floor; the REPLACE override below still wins both directions.
	profile.Caps |= capProbeCaps

	// Apply runtime data (highest precedence).
	if runtime != nil {
		if runtime.Family != "" && profile.Family == "" {
			profile.Family = strings.ToLower(runtime.Family)
		}
		if runtime.ParameterSize != "" {
			profile.Resources.ParameterSize = runtime.ParameterSize
		}
		if runtime.QuantLevel != "" {
			profile.Resources.QuantLevel = runtime.QuantLevel
		}
		if runtime.ContextWindow > 0 {
			profile.ContextWindow = runtime.ContextWindow
		}
		if runtime.Digest != "" {
			profile.Digest = runtime.Digest
		}
		if runtime.Template != "" {
			profile.Template = runtime.Template
		}

		// Merge runtime capabilities.
		rtCaps := parseCaps(runtime.Capabilities)
		if rtCaps != 0 {
			profile.Caps |= rtCaps
		}
		if runtime.Template != "" {
			if templateUsesSuffix(runtime.Template) {
				profile.Caps |= CapInsert
			} else {
				profile.Caps &^= CapInsert
			}
		}
	}

	// Dynamic inference fallback for unknown models.
	if static == nil {
		inferred := inferProfile(runtime)
		if inferred != nil {
			if profile.FIM == nil {
				profile.FIM = inferred.FIM
			}
			if profile.ThinkMode == ThinkNone {
				profile.ThinkMode = inferred.ThinkMode
			}
			if profile.Quality == TierBasic && inferred.Quality > TierBasic {
				profile.Quality = inferred.Quality
			}
			if profile.Speed == TierBasic && inferred.Speed > TierBasic {
				profile.Speed = inferred.Speed
			}
		}

		// Estimate resources if catalog didn't provide them.
		if profile.Resources.RAMRequired == 0 && runtime != nil {
			profile.Resources = estimateResources(runtime.ParameterSize, runtime.QuantLevel)
			profile.Resources.ParameterSize = runtime.ParameterSize
			profile.Resources.QuantLevel = runtime.QuantLevel
		}
	}

	// Set family from parsed name if still empty.
	if profile.Family == "" {
		profile.Family = parsed.NormalizedFamily()
	}

	// Config capability override (final precedence). A non-nil, non-empty
	// result that parses to a non-zero bitmask REPLACES the merged Caps
	// wholesale — this is the escape hatch the design contract guarantees,
	// e.g. ["chat", "stream"] genuinely removes CapGenerate even if the
	// static catalog or runtime template detection claims it.
	//
	// Three corner cases are deliberately ignored rather than applied:
	//   1. Non-nil but empty slice — would silently zero out the profile.
	//      Treated as anomaly; keep merged caps and skip the replace.
	//   2. Non-canonical tokens (catalog aliases like "completion" or
	//      "insert"-as-FIM-shorthand) — these would silently expand to
	//      multiple bits and defeat the REPLACES contract. ParseCapsStrict
	//      rejects them; on error we keep merged caps and skip the replace.
	//   3. Non-empty slice that parses to zero (theoretically reachable
	//      only via a programmatic bypass) — same crippling hazard as
	//      case 1. Refuse the replacement rather than zero the profile.
	//
	// ParseCapsStrict (canonical-only) is used here so the parser at the
	// apply site matches the parser at the validation site (config.validate
	// also routes through ParseCapsStrict). Using lenient parseCaps would
	// re-introduce alias expansion for tokens like "insert" (single bit
	// under strict, three bits under lenient), reopening the contract gap
	// that motivated the canonical-only split in the first place.
	//
	// The override is passed in by buildProfile (not read here from r)
	// so the merge applies the SAME override the caller version-checks
	// at cache-write time. Reading r.capOverride here would reintroduce
	// the TOCTOU window SetCapabilityOverride's version counter exists
	// to close.
	if override != nil {
		if cfgCaps := override(key); len(cfgCaps) > 0 {
			parsedCaps, err := ParseCapsStrict(cfgCaps)
			switch {
			case err != nil:
				// Non-canonical tokens (e.g. catalog aliases that would
				// expand to multiple bits) — fire the rejection hook so
				// the operator can see the misconfiguration. Without this
				// signal the drop is silent and the only symptom is router
				// decisions that don't match the config.
				if rejectionHook != nil {
					rejectionHook(key, cfgCaps, err)
				}
			case parsedCaps != 0:
				profile.Caps = parsedCaps
			}
		}
	}

	return profile
}

// inferProfile generates a basic ModelProfile from runtime model info for
// models not found in the static catalog. It uses heuristics based on
// family name patterns and capabilities.
func inferProfile(info *ModelInfo) *ModelProfile {
	if info == nil {
		return nil
	}

	p := &ModelProfile{}

	// Infer think mode from family patterns.
	family := strings.ToLower(info.Family)
	switch {
	case strings.Contains(family, "deepseek") && strings.Contains(family, "r1"):
		p.ThinkMode = ThinkAlways
	case strings.HasPrefix(family, "qwen3") || strings.HasPrefix(family, "gemma3"):
		p.ThinkMode = ThinkToggle
	default:
		p.ThinkMode = ThinkAuto // safe default for unknown models
	}

	// Estimate quality tier from parameter count.
	paramCount := parseParamCount(info.ParameterSize)
	switch {
	case paramCount >= 30:
		p.Quality = TierGreat
		p.Speed = TierBasic // slow
	case paramCount >= 7:
		p.Quality = TierGood
		p.Speed = TierGood // medium
	default:
		p.Quality = TierBasic
		p.Speed = TierGreat // fast
	}

	return p
}

// estimateResources estimates RAM requirements from parameter size and
// quantization level for models not in the catalog.
//
// RAM formula: bytes_per_param * param_count_billions * 1.2 (overhead for
// KV cache and runtime buffers).
//
// Quantization multipliers (bytes per parameter):
//   - Q4_K_M: ~0.55
//   - Q8_0:   ~1.0
//   - FP16:   ~2.0
//   - Default: ~0.55 (most Ollama models are Q4)
func estimateResources(paramSize, quantLevel string) ResourceProfile {
	paramCount := parseParamCount(paramSize) // billions
	if paramCount == 0 {
		return ResourceProfile{}
	}

	bytesPerParam := quantBytesPerParam(quantLevel)
	const overhead = 1.2

	ramGB := paramCount * bytesPerParam * overhead
	recommendedGB := ramGB * 1.3

	return ResourceProfile{
		ParameterSize:  paramSize,
		QuantLevel:     quantLevel,
		RAMRequired:    math.Round(ramGB*10) / 10,
		RAMRecommended: math.Round(recommendedGB*10) / 10,
	}
}

// parseParamCount extracts the numeric parameter count in billions from
// strings like "8B", "70B", "8x7b", "3.8B", "137M".
func parseParamCount(s string) float64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0
	}

	// Handle MoE format: "8x7b" -> 56
	if idx := strings.Index(s, "x"); idx > 0 {
		experts, _ := strconv.ParseFloat(s[:idx], 64)
		rest := strings.TrimSuffix(s[idx+1:], "b")
		perExpert, _ := strconv.ParseFloat(rest, 64)
		if experts > 0 && perExpert > 0 {
			return experts * perExpert
		}
	}

	// Handle millions: "137M" -> 0.137
	if strings.HasSuffix(s, "m") {
		val, err := strconv.ParseFloat(strings.TrimSuffix(s, "m"), 64)
		if err == nil {
			return val / 1000.0
		}
	}

	// Handle billions: "8B", "3.8B", "70B"
	s = strings.TrimSuffix(s, "b")
	val, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return val
}

// quantBytesPerParam returns the approximate bytes per parameter for a
// given quantization level.
func quantBytesPerParam(quant string) float64 {
	quant = strings.ToLower(strings.TrimSpace(quant))
	switch {
	case strings.HasPrefix(quant, "q4"):
		return 0.55
	case strings.HasPrefix(quant, "q5"):
		return 0.7
	case strings.HasPrefix(quant, "q6"):
		return 0.8
	case strings.HasPrefix(quant, "q8"):
		return 1.0
	case quant == "fp16" || quant == "f16":
		return 2.0
	case quant == "fp32" || quant == "f32":
		return 4.0
	default:
		return 0.55 // default to Q4 (most common on Ollama)
	}
}

// sortProfiles sorts profiles by quality descending, then speed descending.
// Uses sort.Slice for clarity and correctness.
func sortProfiles(profiles []*ModelProfile) {
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Quality != profiles[j].Quality {
			return profiles[i].Quality > profiles[j].Quality
		}
		return profiles[i].Speed > profiles[j].Speed
	})
}
