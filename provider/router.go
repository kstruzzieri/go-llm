// router.go implements the top-level Router that composes candidate resolution,
// hard gates, scoring, sticky routing, and RoutePlan construction. It is the
// single entry point for consumers who want provider-agnostic model routing.
//
// Usage:
//
//	r, err := provider.NewRouter(modelReg, provReg, opts...)
//	plan, err := r.Route(ctx, req)
//	resp, err := plan.ExecuteChat(ctx)
//
// Or use the convenience methods which Route + Execute in one step:
//
//	resp, err := r.Chat(ctx, req)
package provider

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// routerDefaults
// ---------------------------------------------------------------------------

// routerDefaults holds configurable defaults for the Router.
type routerDefaults struct {
	stickyTTL        time.Duration
	hysteresisMargin float64
	maxStickyEntries int
	maxFallbacks     int
	defaultPriority  Priority
	weightOverrides  map[string]*WeightProfile
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// Router is the top-level orchestrator for model routing. It resolves
// candidates from the ModelRegistry, applies hard gates (capability, circuit
// breaker, RAM, budget), scores surviving candidates, applies sticky routing
// with hysteresis, and builds a RoutePlan with a fallback chain.
//
// Router implements RouteRecorder so that RoutePlans can feed post-execution
// signals back to the circuit breakers, warmth tracker, and sticky cache.
type Router struct {
	mu              sync.RWMutex
	registry        *ModelRegistry
	providers       *Registry
	breakers        map[string]*CircuitBreaker
	warmth          WarmthSource
	tokenBudget     *TokenBudgetValidator
	sticky          *stickyCache
	availableRAM    float64
	defaultOpts     routerDefaults
	closed          bool
	done            chan struct{}
	closeOnce       sync.Once
	routingFeedback *RoutingFeedback
	// feedbackScoringMode controls whether RoutingFeedback influences route
	// selection. PR3 default is FeedbackScoringOff (pre-PR3 behavior).
	// Independent of routingFeedback: recording (writes) is governed by
	// WithRoutingFeedback; reading + selection impact (reads) is governed
	// by this field. See routing_feedback.go and the FeedbackScoringMode
	// docs for the full truth table.
	feedbackScoringMode FeedbackScoringMode
	// feedbackLogger receives once-logged warnings about feedback store
	// failures and newRouteID RNG failures. Defaults to defaultFeedbackLogger
	// (wraps log.Default()). Tests in the same package can assign a
	// capturing implementation directly (router.feedbackLogger = cap)
	// after setupTestRouter returns — no exported option (would expose
	// the unexported feedbackLogger interface).
	feedbackLogger feedbackLogger

	// feedbackWarn holds the sync.Once guards for the three warning types
	// emitted by the feedback surface.
	feedbackWarn *feedbackWarningState
}

// Compile-time assertion that Router implements RouteRecorder.
var _ RouteRecorder = (*Router)(nil)

// ---------------------------------------------------------------------------
// RouterOption
// ---------------------------------------------------------------------------

// RouterOption configures a Router at construction time.
type RouterOption func(*Router)

// WithWarmthSource sets the warmth source used for warm-model scoring.
func WithWarmthSource(ws WarmthSource) RouterOption {
	return func(r *Router) {
		r.warmth = ws
	}
}

// WithStickyTTL sets the TTL for sticky routing cache entries.
func WithStickyTTL(d time.Duration) RouterOption {
	return func(r *Router) {
		if d > 0 {
			r.defaultOpts.stickyTTL = d
		}
	}
}

// WithHysteresis sets the score margin a challenger must exceed the incumbent
// by before replacing a sticky route. Values should be in [0, 1).
func WithHysteresis(margin float64) RouterOption {
	return func(r *Router) {
		if margin >= 0 && margin < 1 {
			r.defaultOpts.hysteresisMargin = margin
		}
	}
}

// WithMaxStickyEntries sets the maximum number of entries in the sticky cache.
func WithMaxStickyEntries(n int) RouterOption {
	return func(r *Router) {
		if n > 0 {
			r.defaultOpts.maxStickyEntries = n
		}
	}
}

// WithMaxFallbacks sets the maximum number of fallback candidates in a RoutePlan.
func WithMaxFallbacks(n int) RouterOption {
	return func(r *Router) {
		if n >= 0 {
			r.defaultOpts.maxFallbacks = n
		}
	}
}

// WithDefaultPriority sets the default priority for requests that don't specify one.
func WithDefaultPriority(p Priority) RouterOption {
	return func(r *Router) {
		r.defaultOpts.defaultPriority = p
	}
}

// WithWeightOverrides sets per-use-case weight profile overrides. The map
// keys are use case names (e.g. "chat", "fim").
func WithWeightOverrides(overrides map[string]*WeightProfile) RouterOption {
	return func(r *Router) {
		r.defaultOpts.weightOverrides = overrides
	}
}

// WithAvailableRAM sets the available RAM (in GB) for resource-aware filtering.
// Models whose RAMRequired exceeds this value are filtered out.
func WithAvailableRAM(gb float64) RouterOption {
	return func(r *Router) {
		if gb > 0 {
			r.availableRAM = gb
		}
	}
}

// WithTokenBudgetValidator sets a custom token budget validator.
func WithTokenBudgetValidator(v *TokenBudgetValidator) RouterOption {
	return func(r *Router) {
		r.tokenBudget = v
	}
}

// FeedbackScoringMode controls how the Router uses RoutingFeedback reads
// during candidate scoring and route selection. Independent of whether
// recording is enabled (WithRoutingFeedback); a deployment can write
// signals without ever letting them influence routing.
//
// Defaults to FeedbackScoringOff so the post-PR3 routing path is the
// pre-PR3 routing path until an operator opts in.
type FeedbackScoringMode int

const (
	// FeedbackScoringOff disables both feedback reads and feedback
	// breakdown emission. selectionActiveSignals / deltaActiveSignals
	// both exclude "feedback".
	FeedbackScoringOff FeedbackScoringMode = iota
	// FeedbackScoringShadow reads feedback per route, emits ScoreBreakdown
	// with raw/adjusted values, but route selection uses the
	// score-without-feedback path. Used to evaluate the impact of feedback
	// before enforcing it.
	FeedbackScoringShadow
	// FeedbackScoringEnforce reads feedback per route, emits the same
	// breakdown, AND lets the feedback signal participate in weighted
	// scoring for the route. Snapshot fail-open still disables feedback
	// for the whole route on any read error.
	FeedbackScoringEnforce
)

// String returns the operator-facing label for the mode. Unknown values
// return a labeled fallback so log lines never contain a bare integer.
func (m FeedbackScoringMode) String() string {
	switch m {
	case FeedbackScoringOff:
		return "off"
	case FeedbackScoringShadow:
		return "shadow"
	case FeedbackScoringEnforce:
		return "enforce"
	default:
		return fmt.Sprintf("unknown(%d)", int(m))
	}
}

// WithFeedbackScoringMode sets the FeedbackScoringMode at construction.
// Default is FeedbackScoringOff. Recording (writes) is configured
// separately via WithRoutingFeedback; this option controls only reads
// and selection impact.
func WithFeedbackScoringMode(mode FeedbackScoringMode) RouterOption {
	return func(r *Router) { r.feedbackScoringMode = mode }
}

// WithFeedbackScoring is compatibility sugar: true maps to
// FeedbackScoringEnforce, false maps to FeedbackScoringOff. Prefer
// WithFeedbackScoringMode when ramp-up via shadow mode is wanted.
func WithFeedbackScoring(enabled bool) RouterOption {
	return func(r *Router) {
		if enabled {
			r.feedbackScoringMode = FeedbackScoringEnforce
		} else {
			r.feedbackScoringMode = FeedbackScoringOff
		}
	}
}

// WithRoutingFeedback configures the router to record per-attempt
// outcomes via the RoutingFeedback wrapper. Default is nil (no recording).
// The wrapper itself is nil-safe — if a future caller passes a
// *RoutingFeedback constructed with a nil store, RecordOutcome returns
// ErrNilRoutingFeedbackStore which recordOutcomeFeedback swallows.
func WithRoutingFeedback(rf *RoutingFeedback) RouterOption {
	return func(r *Router) { r.routingFeedback = rf }
}

// ---------------------------------------------------------------------------
// Constructor
// ---------------------------------------------------------------------------

// NewRouter creates a Router with the given registries and options.
func NewRouter(registry *ModelRegistry, providers *Registry, opts ...RouterOption) *Router {
	r := &Router{
		registry:  registry,
		providers: providers,
		breakers:  make(map[string]*CircuitBreaker),
		defaultOpts: routerDefaults{
			stickyTTL:        5 * time.Minute,
			hysteresisMargin: 0.15,
			maxStickyEntries: 1024,
			maxFallbacks:     3,
			defaultPriority:  PriorityNormal,
			weightOverrides:  make(map[string]*WeightProfile),
		},
		done: make(chan struct{}),
	}
	r.feedbackLogger = defaultFeedbackLogger
	r.feedbackWarn = newFeedbackWarningState()

	for _, opt := range opts {
		opt(r)
	}

	// Create sticky cache with configured parameters.
	r.sticky = newStickyCache(r.defaultOpts.stickyTTL, r.defaultOpts.maxStickyEntries)

	// Create default token budget validator if none provided.
	if r.tokenBudget == nil {
		r.tokenBudget = NewTokenBudgetValidator()
	}

	return r
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

// Close shuts down the router. Subsequent calls to Route and convenience
// methods return ErrRouterClosed. Close is idempotent.
func (r *Router) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		close(r.done)

		if r.warmth != nil {
			err = r.warmth.Close()
		}
	})
	return err
}

// ---------------------------------------------------------------------------
// Route — core routing method
// ---------------------------------------------------------------------------

// Route selects the best provider/model for the given request and returns a
// RoutePlan with a fallback chain. The plan can then be executed via
// ExecuteChat, ExecuteGenerate, or ExecuteEmbed.
func (r *Router) Route(ctx context.Context, req RoutingRequest) (*RoutePlan, error) {
	// 1. Check closed.
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, ErrRouterClosed
	}
	r.mu.RUnlock()

	// Invariant: when PreferredChain is empty and the caller supplies BOTH
	// a qualified Model ("provider/model") and a Provider field, the two
	// must agree on provider identity. Mismatch is a caller-side bug — we
	// reject loudly with ErrProviderMismatch rather than silently routing
	// to one or the other. Under PreferredChain the chain selectors are
	// authoritative and Provider is suppressed; see RoutingRequest.Provider
	// docs and feedback_invariants_at_provider.
	if len(req.PreferredChain) == 0 && req.Provider != "" {
		if key, ok := parseModelSelector(req.Model); ok && key.Provider != req.Provider {
			return nil, fmt.Errorf("%w: model selector %q conflicts with Provider %q",
				ErrProviderMismatch, req.Model, req.Provider)
		}
	}

	if len(req.PreferredChain) > 0 {
		return r.routeChain(ctx, req)
	}

	// Priority is used as-is from the request. PriorityBackground (zero value)
	// is a valid explicit choice, so we do not override it with a default.
	// Convenience methods set appropriate priorities (e.g., FIM -> PriorityHigh).

	// 2. Resolve candidates.
	candidates, err := r.resolveCandidates(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, ErrNoViableCandidate
	}

	// 3. Hard gates + budget + score all candidates.
	//    PR3: build the per-route feedback snapshot exactly ONCE here and
	//    thread it through scoreAll. scoreCandidate stays I/O-free. For
	//    the single-pass branch the snapshot lives only across scoreAll;
	//    the chain-routing branch (routeChain) builds its own and extends
	//    it incrementally per step.
	snap := r.buildFeedbackSnapshot(ctx, candidates, req.UseCase)
	scored := r.scoreAll(ctx, candidates, req, snap)

	// 4. Partition into executable, truncatable, breakered, budgeted.
	var executable, truncatable []scoredCandidate
	var allBreakered, allBudgeted bool
	breakerCount, budgetCount := 0, 0

	for _, sc := range scored {
		if !sc.bd.capabilityGate {
			continue
		}
		if math.IsInf(sc.bd.breakerPenalty, -1) {
			breakerCount++
			continue
		}
		switch sc.budget.Decision {
		case BudgetOK:
			executable = append(executable, sc)
		case BudgetTruncate:
			truncatable = append(truncatable, sc)
		case BudgetReject:
			budgetCount++
		}
	}

	allBreakered = breakerCount > 0 && len(executable) == 0 && len(truncatable) == 0 && budgetCount == 0
	allBudgeted = budgetCount > 0 && len(executable) == 0 && len(truncatable) == 0

	// If no executable candidates, determine the best error.
	if len(executable) == 0 {
		if len(truncatable) > 0 {
			// Return best truncatable with adaptation error.
			sortScoredCandidates(truncatable, r.warmth)
			winner := truncatable[0]
			plan, buildErr := r.buildPlan(winner, nil, req, false, snap)
			if buildErr != nil {
				return nil, ErrNoViableCandidate
			}
			plan.Degraded = true
			return plan, ErrBudgetAdaptationRequired
		}
		if allBreakered {
			return nil, ErrAllBreakersOpen
		}
		if allBudgeted {
			return nil, ErrBudgetExceeded
		}
		return nil, ErrNoViableCandidate
	}

	// 5. Sort scored candidates.
	sortScoredCandidates(executable, r.warmth)

	// 6. Sticky routing.
	wasSticky := false
	stickyKey := ""
	if req.AffinityKey != "" && !req.DryRun {
		stickyKey = StickyKey(req)
		incumbent, found := r.sticky.get(stickyKey)
		if found {
			// Find incumbent in scored list.
			incumbentIdx := r.findIncumbentScore(incumbent.providerKey, executable)
			if incumbentIdx >= 0 {
				challengerScore := executable[0].score
				incumbentScore := executable[incumbentIdx].score

				// Hysteresis: challenger must exceed incumbent by margin.
				if challengerScore <= incumbentScore+r.defaultOpts.hysteresisMargin {
					// Keep incumbent: move it to front.
					if incumbentIdx != 0 {
						kept := executable[incumbentIdx]
						copy(executable[1:incumbentIdx+1], executable[:incumbentIdx])
						executable[0] = kept
					}
					wasSticky = true
					r.sticky.touch(stickyKey)
				}
				// else: challenger wins, replace incumbent below
			}
			// else: incumbent not in scored list (breaker opened, etc.) — let challenger win
		}
	}

	// 7. Build RoutePlan with fallback chain.
	var fallbacks []scoredCandidate
	maxFb := r.defaultOpts.maxFallbacks
	if maxFb > len(executable)-1 {
		maxFb = len(executable) - 1
	}
	if maxFb > 0 {
		fallbacks = executable[1 : 1+maxFb]
	}

	plan, buildErr := r.buildPlan(executable[0], fallbacks, req, wasSticky, snap)
	if buildErr != nil {
		return nil, fmt.Errorf("router: %w", buildErr)
	}

	// 8. Update sticky cache (skip if DryRun).
	if req.AffinityKey != "" && !req.DryRun && stickyKey != "" {
		now := time.Now()
		r.sticky.put(&routeSticky{
			key:         stickyKey,
			providerKey: executable[0].profile.Key,
			score:       executable[0].score,
			reason:      buildReason(executable[0]),
			createdAt:   now,
			lastUsedAt:  now,
			expiresAt:   now.Add(r.defaultOpts.stickyTTL),
		})
	}

	return plan, nil
}

// ---------------------------------------------------------------------------
// RouteRecorder implementation
// ---------------------------------------------------------------------------

// RecordSuccess records a successful request to the circuit breaker.
func (r *Router) RecordSuccess(key ModelKey, _ LatencyInfo) {
	cb := r.getOrCreateBreaker(key.Provider)
	cb.RecordSuccess()
}

// RecordFailure records a failed request. Infrastructure errors trigger the
// circuit breaker and invalidate sticky routes for the provider.
func (r *Router) RecordFailure(key ModelKey, err error) {
	if IsInfrastructureError(err) {
		cb := r.getOrCreateBreaker(key.Provider)
		cb.RecordFailure(err)
		r.sticky.invalidateProvider(key.Provider)
	}
}

// RecordWarmthUse records a warmth use signal if a warmth source is configured.
func (r *Router) RecordWarmthUse(key ModelKey) {
	if r.warmth != nil {
		r.warmth.RecordUse(key)
	}
}

// ---------------------------------------------------------------------------
// Convenience methods
// ---------------------------------------------------------------------------

// Chat routes and executes a non-streaming chat request.
func (r *Router) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	rr := RoutingRequest{
		Model:    req.Model,
		Provider: req.Provider,
		UseCase:  "chat",
		Messages: req.Messages,
		Options:  req.Options,
		Tools:    req.Tools,
		Priority: r.defaultOpts.defaultPriority,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps = CapChat | CapToolCall
	} else {
		rr.RequiredCaps = CapChat
	}

	plan, err := r.Route(ctx, rr)
	if err != nil {
		return nil, err
	}
	return plan.ExecuteChat(ctx)
}

// ChatStream routes and executes a streaming chat request.
func (r *Router) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	rr := RoutingRequest{
		Model:    req.Model,
		Provider: req.Provider,
		UseCase:  "chat",
		Messages: req.Messages,
		Options:  req.Options,
		Tools:    req.Tools,
		Priority: r.defaultOpts.defaultPriority,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps = CapChat | CapStream | CapToolCall
	} else {
		rr.RequiredCaps = CapChat | CapStream
	}

	plan, err := r.Route(ctx, rr)
	if err != nil {
		return err
	}
	return plan.ExecuteChatStream(ctx, fn)
}

// Generate routes and executes a non-streaming generate request.
// If the request includes a Suffix, FIM mode is inferred and priority is
// elevated to PriorityHigh.
func (r *Router) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	rr := RoutingRequest{
		Model:        req.Model,
		Provider:     req.Provider,
		UseCase:      "generate",
		Prompt:       req.Prompt,
		System:       req.System,
		Suffix:       req.Suffix,
		Options:      req.Options,
		RequiredCaps: CapGenerate,
		Priority:     r.defaultOpts.defaultPriority,
	}
	if req.Suffix != "" {
		rr.UseCase = "fim"
		rr.RequiredCaps = CapGenerate | CapInsert
		rr.Priority = PriorityHigh
	}

	plan, err := r.Route(ctx, rr)
	if err != nil {
		return nil, err
	}
	return plan.ExecuteGenerate(ctx)
}

// GenerateStream routes and executes a streaming generate request.
func (r *Router) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	rr := RoutingRequest{
		Model:        req.Model,
		Provider:     req.Provider,
		UseCase:      "generate",
		Prompt:       req.Prompt,
		System:       req.System,
		Suffix:       req.Suffix,
		Options:      req.Options,
		RequiredCaps: CapGenerate | CapStream,
		Priority:     r.defaultOpts.defaultPriority,
	}
	if req.Suffix != "" {
		rr.UseCase = "fim"
		rr.RequiredCaps = CapGenerate | CapInsert | CapStream
		rr.Priority = PriorityHigh
	}

	plan, err := r.Route(ctx, rr)
	if err != nil {
		return err
	}
	return plan.ExecuteGenerateStream(ctx, fn)
}

// Embed routes and executes an embedding request.
func (r *Router) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	rr := RoutingRequest{
		Model:        req.Model,
		Provider:     req.Provider,
		UseCase:      "embedding",
		Input:        req.Input,
		RequiredCaps: CapEmbed,
		Priority:     r.defaultOpts.defaultPriority,
	}

	plan, err := r.Route(ctx, rr)
	if err != nil {
		return nil, err
	}
	return plan.ExecuteEmbed(ctx)
}

// ---------------------------------------------------------------------------
// Observability
// ---------------------------------------------------------------------------

// BreakerInfo returns the circuit breaker state for the named provider.
// Returns false if no breaker exists yet.
func (r *Router) BreakerInfo(provider string) (BreakerInfo, bool) {
	r.mu.RLock()
	cb, ok := r.breakers[provider]
	r.mu.RUnlock()
	if !ok {
		return BreakerInfo{}, false
	}
	return cb.Info(), true
}

// StickyRoutes returns a snapshot of all active sticky routing entries.
func (r *Router) StickyRoutes() map[string]StickyRouteInfo {
	return r.sticky.snapshot()
}

// WarmthSnapshot returns all currently warm models. Returns nil if no
// warmth source is configured.
func (r *Router) WarmthSnapshot() []WarmModel {
	if r.warmth == nil {
		return nil
	}
	return r.warmth.Snapshot()
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// parseModelSelector parses a "provider/model" string into a ModelKey,
// returning ok=false when the input is unqualified (no "/" separator).
// Shared by Router.Route, Router.resolveCandidates, and the chain-routing
// path so selector semantics cannot drift between them.
func parseModelSelector(selector string) (ModelKey, bool) {
	providerName, model, ok := strings.Cut(selector, "/")
	if !ok {
		return ModelKey{}, false
	}
	return ModelKey{Provider: providerName, Model: model}, true
}

// resolveCandidates resolves model profiles from the request's Model field,
// optionally scoped by req.Provider to a specific provider instance:
//   - Empty Model: use Recommend (scoped by Provider when non-empty)
//   - Qualified Model ("provider/model"): Lookup by ModelKey
//   - Unqualified Model + Provider set: Lookup by ModelKey{Provider, Model}
//   - Unqualified Model + Provider empty: LookupAny across all providers
//
// The qualified-Model + non-empty Provider conflict case is rejected upstream
// in Router.Route before this is called; by the time we reach the qualified
// branch here, either req.Provider is empty or it agrees with the qualified
// prefix, so we do not need to re-read req.Provider in that branch.
func (r *Router) resolveCandidates(ctx context.Context, req RoutingRequest) ([]*ModelProfile, error) {
	if req.Model == "" {
		return r.registry.Recommend(ctx, RecommendOpts{
			RequiredCaps:       req.RequiredCaps,
			AvailableRAM:       r.availableRAM,
			PreferWarm:         req.PreferWarm,
			RestrictToProvider: req.Provider, // empty == unrestricted
		})
	}

	if key, ok := parseModelSelector(req.Model); ok {
		profile, err := r.registry.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		return []*ModelProfile{profile}, nil
	}

	if req.Provider != "" {
		key := ModelKey{Provider: req.Provider, Model: req.Model}
		profile, err := r.registry.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		return []*ModelProfile{profile}, nil
	}
	return r.registry.LookupAny(ctx, req.Model)
}

// scoreAll evaluates all candidates against the routing request, applying
// hard gates (capability, breaker, RAM, budget) and computing weighted
// scores. The caller (Route()) builds the per-route feedback snapshot
// exactly once and threads it in; scoreCandidate stays I/O-free.
//
// Each candidate gets THREE weighted-score computations:
//   - selection score (chosen via selectionActiveSignals: feedback in
//     activeSignals iff Enforce + snap.active)
//   - scoreWithFeedback (deltaActiveSignals: feedback in activeSignals
//     iff snap.active, regardless of mode — so Shadow exposes a real
//     delta in the breakdown)
//   - scoreWithoutFeedback (baseActiveSignals: feedback never in
//     activeSignals — the PR2 baseline)
func (r *Router) scoreAll(_ context.Context, candidates []*ModelProfile, req RoutingRequest, snap *feedbackSnapshot) []scoredCandidate {
	selectActive := r.selectionActiveSignals(snap)
	deltaActive := r.deltaActiveSignals(snap)
	baseActive := r.baseActiveSignals()
	var customWeights *WeightProfile
	if wp, ok := r.defaultOpts.weightOverrides[req.UseCase]; ok {
		customWeights = wp
	}

	scored := make([]scoredCandidate, 0, len(candidates))

	for _, profile := range candidates {
		// RAM gate: skip models that exceed available RAM.
		if r.availableRAM > 0 && profile.Resources.RAMRequired > r.availableRAM {
			continue
		}

		// Budget validation.
		budget := r.tokenBudget.Validate(req, profile)

		// Score the candidate.
		breaker := r.getOrCreateBreaker(profile.Key.Provider)
		cf := snap.lookup(FeedbackKey{Provider: profile.Key.Provider, Model: profile.Key.Model, UseCase: req.UseCase})
		bd := scoreCandidate(profile, req, budget, r.warmth, cf, breaker)

		// Compute the three weighted score flavors.
		bd.scoreWithoutFeedback = computeWeightedScore(bd, req.UseCase, baseActive, customWeights)
		bd.scoreWithFeedback = computeWeightedScore(bd, req.UseCase, deltaActive, customWeights)
		score := computeWeightedScore(bd, req.UseCase, selectActive, customWeights)

		// Apply breaker penalty (overrides score if -Inf).
		if math.IsInf(bd.breakerPenalty, -1) {
			score = math.Inf(-1)
		}

		scored = append(scored, scoredCandidate{
			profile: profile,
			budget:  budget,
			score:   score,
			bd:      bd,
		})
	}

	return scored
}

// baseActiveSignals returns the always-on scoring signals (everything
// except "feedback", which is conditional on snapshot/mode). Signals
// backed by absent subsystems (e.g. no warmth tracker) are excluded so
// they don't penalize candidates.
func (r *Router) baseActiveSignals() map[string]bool {
	active := map[string]bool{
		"headroom": true,
		"quality":  true,
		"speed":    true,
		"kvcache":  true,
		"cost":     true,
	}
	if r.warmth != nil {
		active["warmth"] = true
	}
	return active
}

// selectionActiveSignals is the set used for the score that actually
// drives candidate selection. Feedback participates only in Enforce
// mode with a live (non-fail-open) snapshot; Off/Shadow/fail-open all
// exclude it so pre-PR3 selection math is preserved.
func (r *Router) selectionActiveSignals(snap *feedbackSnapshot) map[string]bool {
	active := r.baseActiveSignals()
	if r.routingFeedback != nil && snap != nil && snap.active && snap.mode == FeedbackScoringEnforce {
		active["feedback"] = true
	}
	return active
}

// deltaActiveSignals is the set used to compute ScoreWithFeedback for
// the breakdown. Feedback participates whenever the snapshot is active
// (Shadow OR Enforce) so the breakdown can expose the would-be-applied
// delta even in Shadow. Fail-open (snap.active == false) still excludes
// feedback so the delta reports the same scores as PR2 in that case.
func (r *Router) deltaActiveSignals(snap *feedbackSnapshot) map[string]bool {
	active := r.baseActiveSignals()
	if r.routingFeedback != nil && snap != nil && snap.active {
		active["feedback"] = true
	}
	return active
}

// ---------------------------------------------------------------------------
// Feedback snapshot
// ---------------------------------------------------------------------------

// feedbackSnapshotStatus is the operator-facing reason why a route's
// feedback snapshot was (or was not) active. Unexported per the spec's
// PR3-public-surface restriction; the public ScoreBreakdown surfaces
// the same value as a plain string field so operators can distinguish
// "feedback off by design" from "feedback off because the store failed"
// without parsing logs.
type feedbackSnapshotStatus string

const (
	// feedbackSnapshotStatusOff indicates the FeedbackScoringMode was Off.
	feedbackSnapshotStatusOff feedbackSnapshotStatus = "off"
	// feedbackSnapshotStatusNoStore indicates Shadow/Enforce mode but no
	// RoutingFeedback was configured (WithRoutingFeedback never called,
	// or called with nil).
	feedbackSnapshotStatusNoStore feedbackSnapshotStatus = "no_store"
	// feedbackSnapshotStatusEmptyUseCase indicates the route had an
	// empty req.UseCase, which cannot form a FeedbackKey.
	feedbackSnapshotStatusEmptyUseCase feedbackSnapshotStatus = "empty_use_case"
	// feedbackSnapshotStatusReadError indicates the store returned a
	// non-nil error during snapshot construction; fail-open disabled
	// feedback for the route.
	feedbackSnapshotStatusReadError feedbackSnapshotStatus = "read_error"
	// feedbackSnapshotStatusActive indicates the snapshot read every
	// candidate key successfully and feedback is in effect for the route.
	feedbackSnapshotStatusActive feedbackSnapshotStatus = "active"
)

// feedbackSnapshot is the per-Route()-call view of routing-feedback
// aggregates for all candidate keys touched by the route. All store I/O
// happens through the snapshot — initially in buildFeedbackSnapshot for
// the first candidate set, then incrementally via readCandidates for
// chain routing's later steps. Consumers (scoreCandidate) read via
// lookup() and never touch the store directly. The snapshot lives only
// for the duration of one Route() (or routeChain() walk); a future TTL
// cache would slot in here without changing scoreCandidate.
//
// Fail-open is route-level and latching: the first store read error
// flips failed=true, sets status=read_error, clears byKey, and prevents
// any further reads for the rest of the route. This preserves the spec
// contract that one read failure disables feedback for the whole route
// — including any later chain steps or the recommend tail. The
// non-Off / non-no_store / non-empty_use_case states (active, read_error)
// are terminal once set; only the initial unconfigured states can
// transition to active.
type feedbackSnapshot struct {
	active bool
	failed bool // latches true on first read error; bars further reads
	mode   FeedbackScoringMode
	status feedbackSnapshotStatus
	byKey  map[FeedbackKey]*candidateFeedback
}

// lookup returns the per-key feedback for a candidate, or nil if the
// key was not in the snapshot (or the snapshot is inactive).
func (s *feedbackSnapshot) lookup(key FeedbackKey) *candidateFeedback {
	if s == nil || !s.active {
		return nil
	}
	return s.byKey[key]
}

// readCandidates extends the snapshot with reads for any profile whose
// FeedbackKey is not yet known. Called by routeChain before each
// scoreChainStep / appendRecommendTail so that lazily-resolved chain
// step profiles get read into the same route-level snapshot. Fail-open
// latches inactive for the whole route on the first read error: once
// snap.failed == true, the snapshot stays inactive and subsequent calls
// are no-ops, so an early read_error disables feedback for every later
// chain step AND the recommend tail.
//
// No-op when the snapshot is already inactive (off / no_store /
// empty_use_case / read_error) — nothing to add and nothing to lose.
func (s *feedbackSnapshot) readCandidates(ctx context.Context, r *Router, profiles []*ModelProfile, useCase string) {
	if s == nil || s.failed || !s.active || r.routingFeedback == nil || useCase == "" {
		return
	}
	seen := make(map[FeedbackKey]struct{}, len(profiles))
	for _, p := range profiles {
		key := FeedbackKey{Provider: p.Key.Provider, Model: p.Key.Model, UseCase: useCase}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if _, already := s.byKey[key]; already {
			continue
		}
		agg, err := r.routingFeedback.Score(ctx, key)
		if err != nil {
			if r.feedbackWarn != nil {
				r.feedbackWarn.warnFeedbackReadOnce(r.feedbackLogger, key, err)
			}
			s.active = false
			s.failed = true
			s.status = feedbackSnapshotStatusReadError
			s.byKey = nil
			return
		}
		s.byKey[key] = &candidateFeedback{
			raw:      agg,
			adjusted: adjustFeedbackScore(agg),
		}
	}
}

// buildFeedbackSnapshot constructs the per-route feedback snapshot. The
// initial candidate set is optional: pass `nil` for chain routing (where
// chain step profiles are resolved lazily and added incrementally via
// snap.readCandidates), or pass the full candidate slice for the
// single-pass scoreAll path.
//
// Returns an inactive (active=false) snapshot when:
//   - mode is FeedbackScoringOff           -> status = "off"
//   - routingFeedback is nil               -> status = "no_store"
//   - useCase is empty                     -> status = "empty_use_case"
//   - any store read returns an error      -> status = "read_error", failed=true
//
// Fail-open writes a once-logged warning via feedbackLogger so a
// persistently broken store is visible without flooding logs.
//
// IMPORTANT: this is called exactly ONCE per Route() — the same snapshot
// is then threaded into every scoreAll / scoreChainStep / appendRecommendTail
// call for the same route. That guarantees an early read error disables
// feedback for the whole route, including any later chain steps.
func (r *Router) buildFeedbackSnapshot(ctx context.Context, candidates []*ModelProfile, useCase string) *feedbackSnapshot {
	snap := &feedbackSnapshot{mode: r.feedbackScoringMode}
	switch {
	case r.feedbackScoringMode == FeedbackScoringOff:
		snap.status = feedbackSnapshotStatusOff
		return snap
	case r.routingFeedback == nil:
		snap.status = feedbackSnapshotStatusNoStore
		return snap
	case useCase == "":
		snap.status = feedbackSnapshotStatusEmptyUseCase
		return snap
	}
	// Begin in the "active with empty byKey" state so readCandidates can
	// extend it. The initial readCandidates call below either populates
	// byKey with the supplied candidates or latches read_error.
	snap.active = true
	snap.status = feedbackSnapshotStatusActive
	snap.byKey = make(map[FeedbackKey]*candidateFeedback, len(candidates))
	if len(candidates) > 0 {
		snap.readCandidates(ctx, r, candidates, useCase)
	}
	return snap
}

// getOrCreateBreaker returns the circuit breaker for the named provider,
// creating one if it doesn't exist. Uses double-check locking.
func (r *Router) getOrCreateBreaker(provider string) *CircuitBreaker {
	r.mu.RLock()
	cb, ok := r.breakers[provider]
	r.mu.RUnlock()
	if ok {
		return cb
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Double-check after acquiring write lock.
	if cb, ok = r.breakers[provider]; ok {
		return cb
	}

	cb = NewCircuitBreaker()
	r.breakers[provider] = cb
	return cb
}

// findIncumbentScore finds the index of the incumbent model in the scored
// list. Returns -1 if not found.
func (r *Router) findIncumbentScore(key ModelKey, scored []scoredCandidate) int {
	for i, sc := range scored {
		if sc.profile.Key == key {
			return i
		}
	}
	return -1
}

// buildPlan constructs a RoutePlan from the winning candidate, fallback
// candidates, the original request, the sticky state, and the route-level
// feedback snapshot. The snapshot is read for its mode and status fields
// only — store I/O is the caller's responsibility (Route() / routeChain()
// build the snapshot exactly once and thread it through). A nil snap is
// tolerated defensively but should never happen in production.
func (r *Router) buildPlan(winner scoredCandidate, fallbacks []scoredCandidate, req RoutingRequest, wasSticky bool, snap *feedbackSnapshot) (*RoutePlan, error) {
	prov, err := r.providers.Resolve(winner.profile.Key)
	if err != nil {
		return nil, fmt.Errorf("router: provider resolution failed for %s: %w", winner.profile.Key, err)
	}

	plan := &RoutePlan{
		Kind:     inferRouteKind(req),
		Provider: prov,
		Model:    winner.profile.Key.Model,
		Profile:  winner.profile,
		Request:  req,
		Score:    winner.score,
		Budget:   winner.budget,
		Reason:   buildReason(winner),
	}
	plan.SetWasSticky(wasSticky)
	plan.SetRecorder(r)
	plan.SetFeedback(r.routingFeedback) // ← PR2 addition; nil is fine, SetFeedback handles it
	// Stamp the per-Router warn state and logger onto the plan so
	// newRouteIDWithWarn (called from buildOutcome) and
	// recordOutcomeFeedback can fire once-logged warnings on RNG /
	// store failures. Wired unconditionally — the warn state is per-
	// Router and doesn't track feedback scoring on/off; a nil
	// routingFeedback still wants RNG-failure warnings.
	plan.setFeedbackTelemetry(r.feedbackWarn, r.feedbackLogger)

	// Stamp the winner's breakdown and the mode used for selection so
	// buildOutcome can render the public ScoreBreakdown. Off mode skips
	// the stamp entirely so RouteOutcome.ScoreBreakdown stays nil. A nil
	// snap (defensive — Route() and routeChain() always pass one) also
	// skips the stamp.
	if r.feedbackScoringMode != FeedbackScoringOff && snap != nil {
		bd := winner.bd
		plan.setScoreBreakdown(&bd)
		plan.setBuiltUnderMode(r.feedbackScoringMode)
		plan.setFeedbackStatus(snap.status)
	}

	// Build fallback chain.
	for _, fb := range fallbacks {
		fbProv, fbErr := r.providers.Resolve(fb.profile.Key)
		if fbErr != nil {
			continue // skip unresolvable fallbacks
		}
		fbPlan := RoutePlan{
			Kind:     plan.Kind,
			Provider: fbProv,
			Model:    fb.profile.Key.Model,
			Profile:  fb.profile,
			Request:  req,
			Score:    fb.score,
			Budget:   fb.budget,
			Reason:   buildReason(fb),
		}
		plan.Fallbacks = append(plan.Fallbacks, fbPlan)
	}

	return plan, nil
}

// buildReason creates a human-readable reason string from a scored candidate.
func buildReason(sc scoredCandidate) string {
	parts := []string{
		fmt.Sprintf("score=%.3f", sc.score),
		fmt.Sprintf("quality=%s", sc.profile.Quality),
		fmt.Sprintf("speed=%s", sc.profile.Speed),
	}
	if sc.budget.Decision != BudgetOK {
		parts = append(parts, fmt.Sprintf("budget=%s", sc.budget.Decision))
	}
	return strings.Join(parts, ", ")
}

// inferRouteKind determines the RouteKind from the request's fields.
func inferRouteKind(req RoutingRequest) RouteKind {
	if len(req.Input) > 0 {
		return RouteKindEmbed
	}
	if req.Prompt != "" || req.Suffix != "" {
		return RouteKindGenerate
	}
	return RouteKindChat
}
