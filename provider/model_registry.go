// provider/model_registry.go
package provider

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

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
	mu          sync.RWMutex
	profiles    map[ModelKey]*ModelProfile
	catalog     *staticCatalog
	fpStore     fingerprint.Store
	providers   ProviderResolver
	capOverride CapabilityOverride
}

// CapabilityOverride returns the user-declared canonical capability tokens
// for a model key, or nil when no override is configured for the key. When
// non-nil, the returned slice REPLACES the merge-derived Caps on the resulting
// ModelProfile so users can carve down capabilities below what the static
// catalog or runtime probe claims — e.g. removing "generate" for an
// OpenAI-compatible server that lacks /v1/completions.
//
// Returning a non-nil empty slice yields a profile with zero capabilities;
// callers that want "no override applied" must return nil.
type CapabilityOverride func(key ModelKey) []string

// SetCapabilityOverride installs (or clears) the capability override hook.
// Pass nil to disable overrides. Safe for concurrent use; the new function
// takes effect on subsequent Refresh or Lookup calls that miss the cache.
func (r *ModelRegistry) SetCapabilityOverride(fn CapabilityOverride) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capOverride = fn
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
func NewModelRegistry(providers ProviderResolver, fpStore fingerprint.Store) (*ModelRegistry, error) {
	catData, err := loadCatalog()
	if err != nil {
		return nil, fmt.Errorf("provider: new model registry: %w", err)
	}

	return &ModelRegistry{
		profiles:  make(map[ModelKey]*ModelProfile),
		catalog:   newStaticCatalog(catData),
		fpStore:   fpStore,
		providers: providers,
	}, nil
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
func (r *ModelRegistry) Recommend(ctx context.Context, opts RecommendOpts) ([]*ModelProfile, error) {
	all, err := r.All(ctx)
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
	// Layer 1: Runtime query.
	runtimeInfo, err := r.queryRuntime(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("provider: lookup %v: %w", key, err)
	}

	// Layer 2: Static catalog match.
	parsed := ParseModelName(key.Model)
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
	if staticProfile == nil && runtimeInfo.Family != "" {
		// Fall back to runtime-reported family.
		staticProfile = r.catalog.lookupFamily(strings.ToLower(runtimeInfo.Family))
	}

	// Layer 3: Fingerprint enrichment.
	var fpProfile *fingerprint.Profile
	if r.fpStore != nil {
		fpProfile, _ = r.fpStore.Get(ctx, key.Provider, key.Model)
		// Ignore errors -- fingerprint is optional enrichment.
	}

	// Merge layers.
	profile := r.merge(key, runtimeInfo, staticProfile, fpProfile, parsed)

	// Cache the result.
	r.mu.Lock()
	r.profiles[key] = profile
	r.mu.Unlock()

	return profile, nil
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

	// Config capability override (final precedence). A non-nil result REPLACES
	// the merged Caps wholesale — this is the escape hatch the design contract
	// guarantees, e.g. ["chat", "stream"] genuinely removes CapGenerate even
	// if the static catalog claims it. Lenient parseCaps matches the other
	// merge sources; config validation already rejected unknown tokens.
	r.mu.RLock()
	override := r.capOverride
	r.mu.RUnlock()
	if override != nil {
		if cfgCaps := override(key); cfgCaps != nil {
			profile.Caps = parseCaps(cfgCaps)
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
