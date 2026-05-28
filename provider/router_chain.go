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
	seen := make(map[ModelKey]bool)

	// PR3: build the route-level feedback snapshot ONCE here. It starts
	// active with empty byKey (or terminally inactive if Off / no_store /
	// empty_use_case); each per-step snap.readCandidates either extends
	// it OR latches fail-open. Either way scoreChainStep + the recommend
	// tail see the same *feedbackSnapshot for every step, so an early
	// step's read_error disables feedback for every later step too.
	snap := r.buildFeedbackSnapshot(ctx, nil, req.UseCase)

	var lookupErrs []error
	breakerCount, budgetCount := 0, 0
	resolvedAny := false

	for _, selector := range req.PreferredChain {
		profiles, err := r.resolveChainEntry(ctx, selector)
		if err != nil {
			r.recordChainLookupFailure(selector, err)
			lookupErrs = append(lookupErrs, fmt.Errorf("chain entry %q: %w", selector, err))
			continue
		}

		// Extend the route-level snapshot with this step's profiles BEFORE
		// scoring. If an earlier step already latched read_error,
		// readCandidates is a no-op; if this step is the one that fails,
		// the snapshot latches and every later step plus the recommend
		// tail sees an inactive snapshot via the same pointer.
		snap.readCandidates(ctx, r, profiles, req.UseCase)

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

	if !req.StrictChain {
		tail, terr := r.appendRecommendTail(ctx, req, seen, snap)
		if terr == nil {
			working = append(working, tail...)
		}
	}

	if len(working) == 0 {
		if len(truncatable) > 0 {
			sortScoredCandidates(truncatable, r.warmth)
			plan, buildErr := r.buildPlan(truncatable[0], nil, req, false /* not sticky */)
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

	plan, err := r.buildPlan(winner, fallbacks, req, false /* not sticky */)
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

// scoreChainStep applies the same hard-gate + scoring pipeline that scoreAll
// uses for the global-scoring branch, but per chain step. The route-level
// feedback snapshot is built ONCE by routeChain at entry; callers extend
// it with this step's profiles via snap.readCandidates() before invoking
// scoreChainStep, which then stays I/O-free. The selection vs. delta
// active-signal split mirrors scoreAll.
//
// Because snap.readCandidates latches snap.failed=true on the first read
// error, an early step's fail-open carries forward to every later step
// AND the recommend tail in the same route — the spec's "any read error
// disables feedback for the whole route" invariant.
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

		bd.scoreWithoutFeedback = computeWeightedScore(bd, req.UseCase, baseActive, customWeights)
		bd.scoreWithFeedback = computeWeightedScore(bd, req.UseCase, deltaActive, customWeights)
		score := computeWeightedScore(bd, req.UseCase, selectActive, customWeights)

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

// appendRecommendTail runs ModelRegistry.Recommend with the request's
// constraints and returns scored candidates not already present in seen.
// Failures (e.g. registry index empty) return an error so the caller can
// decide whether to ignore (chain has survivors) or surface (chain empty).
//
// PR3: the route-level feedback snapshot is threaded in by routeChain. The
// tail's profiles are extended into the same snapshot via snap.readCandidates
// BEFORE scoreChainStep — keeping the snapshot's fail-open latch consistent
// across the whole route. If an earlier chain step already latched
// read_error, this extension is a no-op (and the tail sees inactive
// feedback identically to the latched chain steps).
func (r *Router) appendRecommendTail(ctx context.Context, req RoutingRequest, seen map[ModelKey]bool, snap *feedbackSnapshot) ([]scoredCandidate, error) {
	all, err := r.registry.Recommend(ctx, RecommendOpts{
		RequiredCaps: req.RequiredCaps,
		AvailableRAM: r.availableRAM,
		PreferWarm:   req.PreferWarm,
	})
	if err != nil {
		return nil, err
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
	snap.readCandidates(ctx, r, fresh, req.UseCase)
	scored := r.scoreChainStep(ctx, fresh, req, snap)

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
	return tail, nil
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
