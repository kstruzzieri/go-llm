// router_chain.go implements chain-first routing: walking an ordered list of
// model selectors from RoutingRequest.PreferredChain, applying hard gates and
// within-step scoring per entry, flattening across steps, then optionally
// appending a Recommend safety-net tail.
//
// Chain-first routing is selected by Router.Route when PreferredChain is
// non-empty. Sticky routing is unconditionally suppressed for chain routes
// to prevent hysteresis from reordering the user's declared preference.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// routeChain is the chain-first counterpart to the global-scoring branch in
// Router.Route. It walks PreferredChain in declared order, scores survivors
// within each step, flattens across steps, optionally appends a Recommend
// tail, and constructs a RoutePlan whose first entry is the winner and whose
// remainder is the fallback list (clamped by MaxFallbacks).
func (r *Router) routeChain(ctx context.Context, req RoutingRequest) (*RoutePlan, error) {
	r.mu.RLock()
	if r.closed {
		r.mu.RUnlock()
		return nil, ErrRouterClosed
	}
	r.mu.RUnlock()

	working := make([]scoredCandidate, 0, len(req.PreferredChain))
	truncatable := make([]scoredCandidate, 0)

	// PR3: build the route-level feedback snapshot ONCE here. It starts
	// active with empty byKey (or terminally inactive if Off / no_store /
	// empty_use_case); each per-step snap.readCandidates either extends
	// it OR latches fail-open. Either way scoreChainStep + the recommend
	// tail see the same *feedbackSnapshot for every step, so an early
	// step's read_error disables feedback for every candidate in the
	// route, including candidates resolved before the failed read.
	snap := r.buildFeedbackSnapshot(ctx, nil, req.UseCase)

	// Chain batching (#401): selectors resolve serially into entry
	// records -- a lookup error, or a [start,end) range into one flat
	// candidate slice -- then EVERY entry's tool_call resolution runs as
	// ONE bounded-parallel EnsureToolCallResolved invocation (distinct
	// keys, capResolveLimit-wide: roughly ceil(U/4) x ~30s on a cold
	// chain, plus admission queueing). All chain capability resolution
	// completes before the first feedback read; selector, profile, and
	// diagnostic ordering remain stable, while probe timing and
	// concurrent-feedback observation time are not serial-equivalent.
	type chainEntry struct {
		selector   string
		lookupErr  error
		start, end int
	}
	entries := make([]chainEntry, 0, len(req.PreferredChain))
	var flat []*ModelProfile
	for _, selector := range req.PreferredChain {
		profiles, err := r.resolveChainEntry(ctx, selector)
		if err != nil {
			r.recordChainLookupFailure(selector, err)
			entries = append(entries, chainEntry{selector: selector, lookupErr: err})
			continue
		}
		entries = append(entries, chainEntry{selector: selector, start: len(flat), end: len(flat) + len(profiles)})
		flat = append(flat, profiles...)
	}

	// Probe I/O runs strictly BEFORE any snap.readCandidates so
	// feedback-snapshot reads never interleave with probes and scorers
	// stay pure. Resolution never drops candidates; the capability gate
	// in scoreChainStep remains the single rejection point. diagAt is
	// index-addressed (nil when resolution is disabled or nothing was
	// unresolved) so the replay below can re-interleave diagnostics per
	// selector. Persisted verdicts amortize the cold cost, Task 9's
	// bounded-eager preflight keeps it rare (see
	// recommendWithToolCallResolution).
	resolved, diagAt := r.registry.ensureToolCallResolvedIndexed(ctx, flat, req.RequiredCaps)

	// Replay entry records in selector order: each selector contributes
	// its lookup error, or -- on successful lookup -- its candidates'
	// probe diagnostics, exactly as the per-selector loop did.
	var lookupErrs []error
	chainSteps := make([][]*ModelProfile, 0, len(req.PreferredChain))
	chainSeenForTail := make(map[ModelKey]bool)
	for _, e := range entries {
		if e.lookupErr != nil {
			lookupErrs = append(lookupErrs, fmt.Errorf("chain entry %q: %w", e.selector, e.lookupErr))
			continue
		}
		profiles := resolved[e.start:e.end]
		if diagAt != nil {
			for _, d := range diagAt[e.start:e.end] {
				if d != nil {
					lookupErrs = append(lookupErrs, d)
				}
			}
		}

		chainSteps = append(chainSteps, profiles)
		r.markChainSeenForTail(chainSeenForTail, profiles, req)

		// Extend the route-level snapshot with this step's profiles. All
		// chain scoring is deferred until after every chain/tail read has
		// had a chance to latch fail-open, so a later read_error cannot
		// leave earlier candidates scored with feedback.
		snap.readCandidates(ctx, r, profiles, req.UseCase)
	}

	var tailProfiles []*ModelProfile
	if !req.StrictChain {
		tp, tailDiags, terr := r.recommendTailProfiles(ctx, req, chainSeenForTail)
		if terr == nil {
			tailProfiles = tp
			lookupErrs = append(lookupErrs, tailDiags...)
			snap.readCandidates(ctx, r, tailProfiles, req.UseCase)
		}
	}

	seen := make(map[ModelKey]bool)
	breakerCount, budgetCount := 0, 0
	resolvedAny := false
	for _, profiles := range chainSteps {
		stepSurvivors := r.scoreChainStep(ctx, profiles, req, snap)
		for _, sc := range stepSurvivors {
			if seen[sc.profile.Key] {
				continue
			}
			if !sc.bd.capabilityGate {
				continue
			}
			resolvedAny = true
			seen[sc.profile.Key] = true
			if math.IsInf(sc.bd.breakerPenalty, -1) {
				breakerCount++
				continue
			}
			switch sc.budget.Decision {
			case BudgetOK:
				working = append(working, sc)
			case BudgetTruncate:
				truncatable = append(truncatable, sc)
			case BudgetReject:
				budgetCount++
				continue
			}
		}
	}

	if !req.StrictChain && len(tailProfiles) > 0 {
		tail := r.scoreRecommendTail(ctx, tailProfiles, req, seen, snap)
		working = append(working, tail...)
	}

	if len(working) == 0 {
		if len(truncatable) > 0 {
			sortScoredCandidates(truncatable, r.warmth)
			plan, buildErr := r.buildPlan(truncatable[0], nil, req, false /* not sticky */, snap)
			if buildErr != nil {
				return nil, ErrNoViableCandidate
			}
			plan.Degraded = true
			return plan, ErrBudgetAdaptationRequired
		}
		return nil, r.classifyChainExhaustion(resolvedAny, breakerCount, budgetCount, lookupErrs)
	}

	maxFb := r.defaultOpts.maxFallbacks
	if maxFb < 0 {
		maxFb = 0
	}
	if 1+maxFb > len(working) {
		maxFb = len(working) - 1
	}
	winner := working[0]
	var fallbacks []scoredCandidate
	if maxFb > 0 {
		fallbacks = working[1 : 1+maxFb]
	}

	plan, err := r.buildPlan(winner, fallbacks, req, false /* not sticky */, snap)
	if err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	return plan, nil
}

// resolveChainEntry parses a chain selector and returns profiles via the
// existing ModelRegistry.Lookup / LookupAny paths, mirroring the selector
// semantics already implemented in Router.resolveCandidates.
func (r *Router) resolveChainEntry(ctx context.Context, selector string) ([]*ModelProfile, error) {
	if key, ok := parseModelSelector(selector); ok {
		profile, err := r.registry.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		return []*ModelProfile{profile}, nil
	}
	return r.registry.LookupAny(ctx, selector)
}

func (r *Router) markChainSeenForTail(seen map[ModelKey]bool, profiles []*ModelProfile, req RoutingRequest) {
	for _, profile := range profiles {
		if seen[profile.Key] {
			continue
		}
		if r.availableRAM > 0 && profile.Resources.RAMRequired > r.availableRAM {
			continue
		}
		if !profileSatisfiesRequiredCaps(profile, req.RequiredCaps) {
			continue
		}
		seen[profile.Key] = true
	}
}

// scoreChainStep applies the same hard-gate + scoring pipeline that scoreAll
// uses for the global-scoring branch, but per chain step. The route-level
// feedback snapshot is built ONCE by routeChain at entry; routeChain extends
// it with every chain/tail profile before invoking scoreChainStep, which
// then stays I/O-free. The selection vs. delta active-signal split mirrors
// scoreAll.
//
// Because snap.readCandidates latches snap.failed=true on the first read
// error, any step's fail-open applies to every chain step and the recommend
// tail in the same route — the spec's "any read error disables feedback for
// the whole route" invariant.
//
// Returns survivors in score-descending order so the caller can append
// them to the working list.
//
// ctx is checked once up-front so a cancelled route doesn't pay the
// full per-candidate loop. All store I/O already happened in
// snap.readCandidates, so per-iteration ctx checks aren't needed.
func (r *Router) scoreChainStep(ctx context.Context, profiles []*ModelProfile, req RoutingRequest, snap *feedbackSnapshot) []scoredCandidate {
	if err := ctx.Err(); err != nil {
		return nil
	}
	selectActive := r.selectionActiveSignals(snap)
	deltaActive := r.deltaActiveSignals(snap)
	baseActive := r.baseActiveSignals()
	var customWeights *WeightProfile
	if wp, ok := r.defaultOpts.weightOverrides[req.UseCase]; ok {
		customWeights = wp
	}

	scored := make([]scoredCandidate, 0, len(profiles))
	for _, profile := range profiles {
		if r.availableRAM > 0 && profile.Resources.RAMRequired > r.availableRAM {
			continue
		}
		budget := r.tokenBudget.Validate(req, profile)
		breaker := r.getOrCreateBreaker(profile.Key.Provider)
		cf := snap.lookup(FeedbackKey{Provider: profile.Key.Provider, Model: profile.Key.Model, UseCase: req.UseCase})
		bd := scoreCandidate(profile, req, budget, r.warmth, cf, breaker)

		neutralBD := scoreBreakdownWithNeutralFeedback(bd)
		bd.scoreWithoutFeedback = computeWeightedScore(neutralBD, req.UseCase, baseActive, customWeights)
		bd.scoreWithFeedback = computeWeightedScore(bd, req.UseCase, deltaActive, customWeights)
		scoreBD := scoreBreakdownForSelection(bd, snap)
		score := computeWeightedScore(scoreBD, req.UseCase, selectActive, customWeights)

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

	sortScoredCandidates(scored, r.warmth)
	return scored
}

// recordChainLookupFailure records a per-selector lookup error against the
// owning provider's circuit breaker when the selector identifies a provider
// and the error is classified as infrastructure. Unqualified selectors and
// non-infrastructure errors are not recorded — the breaker is provider-scoped
// and we do not have a single provider key to attribute generic lookup
// failures to.
func (r *Router) recordChainLookupFailure(selector string, err error) {
	if !IsInfrastructureError(err) {
		return
	}
	key, ok := parseModelSelector(selector)
	if !ok {
		return
	}
	// Guard against selectors like "/qwen3:8b" where parseModelSelector
	// succeeds but the provider half is empty. Without this we would
	// create and persist a breaker named "" in the breakers map — a
	// phantom entry that surfaces in observability dumps and that no
	// legitimate route could ever match.
	if key.Provider == "" {
		return
	}
	cb := r.getOrCreateBreaker(key.Provider)
	cb.RecordFailure(err)
}

// recommendTailProfiles runs ModelRegistry.Recommend with the request's
// constraints and returns candidate profiles not already present in seen.
// Failures (e.g. registry index empty) return an error so the caller can
// keep the chain-only result.
//
// PR3: the route-level feedback snapshot is threaded in by routeChain. The
// tail's profiles are extended into the same snapshot before any chain/tail
// scoring happens, keeping the snapshot's fail-open latch consistent across
// the whole route.
func (r *Router) recommendTailProfiles(ctx context.Context, req RoutingRequest, seen map[ModelKey]bool) ([]*ModelProfile, []error, error) {
	// Same strip-resolve-filter dance as resolveCandidates (shared helper):
	// probe I/O runs here, before routeChain's snap.readCandidates call on
	// the tail profiles, preserving feedback-snapshot purity. Probe
	// diagnostics bubble to routeChain, which joins them into lookupErrs.
	all, diags, err := r.recommendWithToolCallResolution(ctx, RecommendOpts{
		AvailableRAM: r.availableRAM,
		PreferWarm:   req.PreferWarm,
	}, req.RequiredCaps)
	if err != nil {
		return nil, nil, err
	}
	// Pre-filter Recommend output against the chain's `seen` set before
	// extending the route-level snapshot. Without this, every candidate
	// already selected by an earlier chain step would trigger a wasted
	// store.Score read in readCandidates — costs O(seen ∩ tail) reads
	// per route once the store is SQLite-backed. Capability/breaker/
	// budget filtering still happens inside scoreChainStep + the loop
	// below; we only dedupe what's cheap to dedupe here.
	fresh := make([]*ModelProfile, 0, len(all))
	for _, p := range all {
		if seen[p.Key] {
			continue
		}
		fresh = append(fresh, p)
	}
	return fresh, diags, nil
}

func (r *Router) scoreRecommendTail(
	ctx context.Context,
	profiles []*ModelProfile,
	req RoutingRequest,
	seen map[ModelKey]bool,
	snap *feedbackSnapshot,
) []scoredCandidate {
	scored := r.scoreChainStep(ctx, profiles, req, snap)

	tail := make([]scoredCandidate, 0, len(scored))
	for _, sc := range scored {
		if seen[sc.profile.Key] {
			continue
		}
		if !sc.bd.capabilityGate {
			continue
		}
		if math.IsInf(sc.bd.breakerPenalty, -1) {
			continue
		}
		if sc.budget.Decision != BudgetOK {
			continue
		}
		seen[sc.profile.Key] = true
		tail = append(tail, sc)
	}
	return tail
}

// classifyChainExhaustion picks the most informative error when chain routing
// produced no viable candidate. Mirrors the partition logic in the existing
// global-scoring path.
func (r *Router) classifyChainExhaustion(resolvedAny bool, breakerCount, budgetCount int, lookupErrs []error) error {
	if !resolvedAny {
		if len(lookupErrs) > 0 {
			return fmt.Errorf("%w: %s", ErrNoViableCandidate, errors.Join(lookupErrs...))
		}
		return ErrNoViableCandidate
	}
	if breakerCount > 0 && budgetCount == 0 {
		return ErrAllBreakersOpen
	}
	if budgetCount > 0 && breakerCount == 0 {
		return ErrBudgetExceeded
	}
	return ErrNoViableCandidate
}
