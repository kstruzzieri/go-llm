// router_score.go implements the candidate scoring engine for the Router.
// It evaluates each candidate model against a routing request, producing a
// weighted composite score. Signals include warmth (model already loaded),
// context headroom, feedback history, quality tier, speed tier, KV cache
// affinity, and cost. Active-signal normalization ensures absent subsystems
// (e.g. no warmth tracker, no feedback store) do not penalize candidates.
package provider

import (
	"math"
	"sort"
)

// warmthBonusMax is the maximum bonus applied to candidates whose model is
// currently loaded in provider memory ("warm"). This avoids cold-start latency.
const warmthBonusMax = 0.3

// ---------------------------------------------------------------------------
// WeightProfile
// ---------------------------------------------------------------------------

// WeightProfile holds per-signal integer weights for a specific use case.
// Higher values mean the signal matters more when computing the composite
// score. A weight of 0 effectively disables the signal.
type WeightProfile struct {
	Warmth   int
	Headroom int
	Feedback int
	Quality  int
	Speed    int
	KVCache  int
	Cost     int
}

// defaultWeightProfiles maps well-known use cases to their weight tuning.
// FIM heavily favors KV cache (prefix reuse) and speed. Chat favors quality
// and feedback. Embedding cares about quality and speed, not warmth/headroom.
// Reasoning maximizes quality with headroom for long outputs. Code-review
// balances quality and feedback. Agent (tool-calling) loops need speed per
// call (many small calls), headroom (accumulating tool trace), and feedback
// (tool-call accuracy is directly observable). "tool-use" is an alias for
// "agent" kept so callers can pick whichever name reads better locally.
var defaultWeightProfiles = map[string]*WeightProfile{
	"fim":         {Warmth: 2, Headroom: 1, Feedback: 1, Quality: 1, Speed: 2, KVCache: 3, Cost: 1},
	"chat":        {Warmth: 1, Headroom: 2, Feedback: 3, Quality: 4, Speed: 1, KVCache: 0, Cost: 1},
	"embedding":   {Warmth: 0, Headroom: 0, Feedback: 1, Quality: 3, Speed: 2, KVCache: 0, Cost: 2},
	"reasoning":   {Warmth: 1, Headroom: 3, Feedback: 3, Quality: 5, Speed: 0, KVCache: 0, Cost: 1},
	"analysis":    {Warmth: 1, Headroom: 3, Feedback: 3, Quality: 5, Speed: 1, KVCache: 0, Cost: 1},
	"code-review": {Warmth: 1, Headroom: 3, Feedback: 4, Quality: 3, Speed: 1, KVCache: 0, Cost: 1},
	"agent":       {Warmth: 2, Headroom: 2, Feedback: 4, Quality: 4, Speed: 3, KVCache: 1, Cost: 1},
	"tool-use":    {Warmth: 2, Headroom: 2, Feedback: 4, Quality: 4, Speed: 3, KVCache: 1, Cost: 1},
}

// defaultWeightProfile returns the weight profile for the given use case.
// Unknown use cases fall back to the "chat" profile.
func defaultWeightProfile(useCase string) *WeightProfile {
	if wp, ok := defaultWeightProfiles[useCase]; ok {
		return wp
	}
	return defaultWeightProfiles["chat"]
}

// ---------------------------------------------------------------------------
// scoreBreakdown
// ---------------------------------------------------------------------------

// scoreBreakdown holds the raw signal values computed for a single candidate.
// These values feed into computeWeightedScore for final ranking.
type scoreBreakdown struct {
	warmthBonus    float64 // 0.0–0.3
	headroomScore  float64 // 0.0–1.0
	feedbackScore  float64 // default 0.5 when nil
	qualityTier    float64 // 0.25–1.0
	speedTier      float64 // 0.25–1.0
	kvCacheBonus   float64 // default 0
	costPenalty    float64 // default 0
	breakerPenalty float64 // 0 or -Inf
	capabilityGate bool    // false = eliminated
}

// ---------------------------------------------------------------------------
// scoredCandidate
// ---------------------------------------------------------------------------

// scoredCandidate pairs a ModelProfile with its scoring result for sorting
// and selection by the Router.
type scoredCandidate struct {
	profile *ModelProfile
	budget  BudgetResult
	score   float64
	bd      scoreBreakdown
}

// ---------------------------------------------------------------------------
// tierToFloat
// ---------------------------------------------------------------------------

// tierToFloat converts a Tier enum to a normalized float64 value.
// Basic=0.25, Good=0.50, Great=0.75, Best=1.00. Unknown tiers default to
// Basic (0.25).
func tierToFloat(t Tier) float64 {
	switch t {
	case TierBasic:
		return 0.25
	case TierGood:
		return 0.50
	case TierGreat:
		return 0.75
	case TierBest:
		return 1.00
	default:
		return 0.25
	}
}

// ---------------------------------------------------------------------------
// scoreCandidate
// ---------------------------------------------------------------------------

// scoreCandidate evaluates a single candidate model against a routing request
// and returns a scoreBreakdown. This is a standalone function, not a Router
// method, so it can be tested and used independently.
//
// Parameters:
//   - profile: the candidate model's metadata
//   - req: the routing request with capability requirements and use case
//   - budget: the token budget validation result (headroom feeds scoring)
//   - warmth: optional warmth source; nil means warmth signal is inactive
//   - _ : feedback placeholder (reserved for Phase 3)
//   - breaker: circuit breaker for the candidate; never nil (lazily created)
func scoreCandidate(
	profile *ModelProfile,
	req RoutingRequest,
	budget BudgetResult,
	warmth WarmthSource,
	_ interface{},
	breaker *CircuitBreaker,
) scoreBreakdown {
	var bd scoreBreakdown

	// 1. Compute tier scores.
	bd.qualityTier = tierToFloat(profile.Quality)
	bd.speedTier = tierToFloat(profile.Speed)

	// 2. Headroom from budget validation.
	bd.headroomScore = budget.HeadroomScore

	// 3. Neutral feedback default (Phase 3 will replace this).
	bd.feedbackScore = 0.5

	// 4. Capability gate: if the request requires capabilities the model
	//    doesn't have, eliminate the candidate immediately. CapInsert is
	//    satisfied by either the explicit capability bit or a live template that
	//    proves native suffix insertion support; this keeps Router admission in
	//    sync with ModelProfile.SupportsFIM.
	if !profileSatisfiesRequiredCaps(profile, req.RequiredCaps) {
		bd.capabilityGate = false
		return bd
	}
	bd.capabilityGate = true

	// 5. Circuit breaker gate: if the breaker blocks the request, the
	//    candidate is penalized with -Inf so it sorts to the bottom.
	//    Allow() is used intentionally — it transitions Open→HalfOpen after
	//    cooldown, enabling one probe request. Without this, a tripped
	//    breaker could never recover. The Router calls scoreCandidate once
	//    per candidate per Route(), so at most one probe is granted per
	//    provider per routing pass.
	if breaker != nil && !breaker.Allow() {
		bd.breakerPenalty = math.Inf(-1)
		return bd
	}

	// 6. Warmth bonus: models already loaded get a latency advantage.
	if warmth != nil && warmth.IsWarm(profile.Key) {
		bd.warmthBonus = warmthBonusMax
	}

	return bd
}

func profileSatisfiesRequiredCaps(profile *ModelProfile, required Capability) bool {
	if required == 0 {
		return true
	}
	caps := profile.Caps
	if required.Has(CapInsert) && profile.SupportsFIM() {
		caps |= CapInsert
	}
	return caps.Has(required)
}

// ---------------------------------------------------------------------------
// computeWeightedScore
// ---------------------------------------------------------------------------

// computeWeightedScore combines the raw signal values from a scoreBreakdown
// into a single composite score using the weight profile for the given use
// case. Only signals present in activeSignals with non-zero weight contribute
// to the result. This active-signal normalization ensures that absent
// subsystems (no warmth tracker, no feedback store) do not penalize candidates.
//
// If no signals are active, the function returns 0.
func computeWeightedScore(
	bd scoreBreakdown,
	useCase string,
	activeSignals map[string]bool,
	customWeights *WeightProfile,
) float64 {
	wp := customWeights
	if wp == nil {
		wp = defaultWeightProfile(useCase)
	}

	// Signal name → (normalized value in [0,1], weight).
	type signal struct {
		name   string
		value  float64
		weight int
	}

	signals := []signal{
		{"warmth", bd.warmthBonus / warmthBonusMax, wp.Warmth},
		{"headroom", bd.headroomScore, wp.Headroom},
		{"feedback", bd.feedbackScore, wp.Feedback},
		{"quality", bd.qualityTier, wp.Quality},
		{"speed", bd.speedTier, wp.Speed},
		{"kvcache", bd.kvCacheBonus, wp.KVCache},
		{"cost", 1.0 - bd.costPenalty, wp.Cost},
	}

	var weightedSum float64
	var totalWeight float64

	for _, s := range signals {
		if !activeSignals[s.name] || s.weight <= 0 {
			continue
		}
		w := float64(s.weight)
		weightedSum += s.value * w
		totalWeight += w
	}

	if totalWeight == 0 {
		return 0
	}

	return weightedSum / totalWeight
}

// ---------------------------------------------------------------------------
// sortScoredCandidates
// ---------------------------------------------------------------------------

// sortScoredCandidates sorts candidates by descending composite score with
// deterministic tiebreaking:
//  1. Prefer warm models (if warmth source is available)
//  2. Higher context window
//  3. Alphabetical by ModelKey string representation
func sortScoredCandidates(candidates []scoredCandidate, warmth WarmthSource) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]

		// Primary: higher score wins.
		if a.score != b.score {
			return a.score > b.score
		}

		// Tiebreak 1: prefer warm model.
		if warmth != nil {
			aWarm := warmth.IsWarm(a.profile.Key)
			bWarm := warmth.IsWarm(b.profile.Key)
			if aWarm != bWarm {
				return aWarm
			}
		}

		// Tiebreak 2: higher context window.
		if a.profile.ContextWindow != b.profile.ContextWindow {
			return a.profile.ContextWindow > b.profile.ContextWindow
		}

		// Tiebreak 3: alphabetical by key.
		return a.profile.Key.String() < b.profile.Key.String()
	})
}
