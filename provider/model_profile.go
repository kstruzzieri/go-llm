package provider

import "time"

// ---------------------------------------------------------------------------
// Tier
// ---------------------------------------------------------------------------

// Tier represents a quality or speed classification for a model.
// Quality tiers rank model output fidelity; speed tiers rank throughput.
type Tier int

const (
	// TierBasic is the lowest tier -- small/fast models with limited quality.
	TierBasic Tier = iota
	// TierGood represents capable models suitable for most tasks.
	TierGood
	// TierGreat represents high-quality models for demanding tasks.
	TierGreat
	// TierBest represents the highest quality models available.
	TierBest
)

// String returns the human-readable tier name.
func (t Tier) String() string {
	switch t {
	case TierBasic:
		return "basic"
	case TierGood:
		return "good"
	case TierGreat:
		return "great"
	case TierBest:
		return "best"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// ProfileSource
// ---------------------------------------------------------------------------

// ProfileSource indicates the origin of a ModelProfile's data.
type ProfileSource int

const (
	// SourceStatic means the profile came from the embedded catalog.
	SourceStatic ProfileSource = iota
	// SourceFingerprint means the profile came from the fingerprint store.
	SourceFingerprint
	// SourceRuntime means the profile came from a live provider query.
	SourceRuntime
	// SourceMerged means the profile was assembled from multiple sources.
	SourceMerged
)

// String returns the human-readable source name.
func (s ProfileSource) String() string {
	switch s {
	case SourceStatic:
		return "static"
	case SourceFingerprint:
		return "fingerprint"
	case SourceRuntime:
		return "runtime"
	case SourceMerged:
		return "merged"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// ResourceProfile
// ---------------------------------------------------------------------------

// ResourceProfile describes a model's resource requirements.
type ResourceProfile struct {
	ParameterSize   string  // "8B", "70B"
	QuantLevel      string  // "Q4_K_M", "Q8_0"
	RAMRequired     float64 // GB minimum to load
	RAMRecommended  float64 // GB for good performance
	VRAMRecommended float64 // GB if GPU offload available
}

// kvScaleFactor is the RAM scale factor per 1024 context tokens for KV cache.
// This default assumes approximately 0.5 GB of additional RAM per 1K context
// tokens, which is a reasonable estimate across common quantization levels.
const kvScaleFactor = 0.5

// RAMAtContext estimates RAM usage at a specific context window size.
// It accounts for KV cache scaling using the formula:
//
//	base_ram + (ctx_size / 1024) * kv_scale_factor
//
// where base_ram is RAMRequired and kv_scale_factor is 0.5 GB per 1K tokens.
// When ctxSize is zero the method returns RAMRequired unchanged.
func (rp ResourceProfile) RAMAtContext(ctxSize int) float64 {
	return rp.RAMRequired + (float64(ctxSize)/1024.0)*kvScaleFactor
}

// ---------------------------------------------------------------------------
// ModelProfile
// ---------------------------------------------------------------------------

// ModelProfile holds the complete, merged metadata for a specific model
// on a specific provider. This is the central data structure that the
// Router, completion engine, and consumers use for decision-making.
//
// Profiles are constructed by the ModelRegistry's three-layer merge:
// static catalog defaults, enriched by fingerprint observations, then
// overridden by live runtime data from the provider. The Source field
// records which layers contributed to this profile.
type ModelProfile struct {
	Key           ModelKey        // canonical provider-qualified identity
	Name          string          // display name
	Family        string          // normalized: "qwen3", "deepseek-r1", "codellama"
	Provider      string          // provider name
	Resources     ResourceProfile // resource requirements
	Caps          Capability      // authoritative per-model capabilities after merge
	FIM           *FIMConfig      // nil if model doesn't support FIM
	ThinkMode     ThinkMode       // reasoning behavior
	ThinkTags     *ThinkTags      // nil uses default <think></think>
	Quality       Tier            // basic, good, great, best
	Speed         Tier            // basic=slow, good=medium, great=fast, best=fastest
	ContextWindow int             // max context tokens
	Dimensions    int             // embedding dimensions, 0 if not embedding model
	Source        ProfileSource   // static, fingerprint, runtime, merged
	Digest        string          // model digest for staleness detection
	UpdatedAt     time.Time       // when this profile was last computed
}

// ---------------------------------------------------------------------------
// RecommendOpts
// ---------------------------------------------------------------------------

// RecommendOpts configures the ModelRegistry.Recommend method, specifying
// constraints and preferences for model selection. The Router uses this to
// request a ranked list of provider-qualified candidates.
type RecommendOpts struct {
	// RequestedModel is an optional canonical selector or unqualified model name.
	// Reserved for Phase 2 (Router) — not yet used by Recommend.
	RequestedModel string

	// UseCase describes the intended use: "chat", "fim", "embedding",
	// "reasoning", "code-review". Reserved for Phase 2 — not yet used by Recommend.
	UseCase string

	// AvailableRAM is the amount of RAM (in GB) the caller can allocate.
	// Models whose RAMAtContext(ContextSize) exceeds this are filtered out.
	AvailableRAM float64

	// ContextSize is the desired context window in tokens. Used together
	// with AvailableRAM for resource-aware filtering via RAMAtContext.
	ContextSize int

	// PreferWarm, when true, biases ranking toward models that are already
	// loaded in the provider's memory. Reserved for Phase 2 (WarmthTracker).
	PreferWarm bool

	// PreferredProviders is a soft preference for specific backends.
	// Reserved for Phase 2 — not yet used by Recommend.
	PreferredProviders []string

	// RequiredCaps is a bitmask of capabilities the model must support.
	// Models missing any of the required capabilities are filtered out.
	RequiredCaps Capability
}
