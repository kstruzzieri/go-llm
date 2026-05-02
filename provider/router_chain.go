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
	"strings"
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

		stepSurvivors := r.scoreChainStep(profiles, req)
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
		tail, terr := r.appendRecommendTail(ctx, req, seen)
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
	if strings.Contains(selector, "/") {
		parts := strings.SplitN(selector, "/", 2)
		key := ModelKey{Provider: parts[0], Model: parts[1]}
		profile, err := r.registry.Lookup(ctx, key)
		if err != nil {
			return nil, err
		}
		return []*ModelProfile{profile}, nil
	}
	return r.registry.LookupAny(ctx, selector)
}

// scoreChainStep applies the same hard-gate + scoring pipeline that scoreAll
// uses for the global-scoring branch, but per chain step. Returns survivors
// in score-descending order so the caller can append them to the working list.
func (r *Router) scoreChainStep(profiles []*ModelProfile, req RoutingRequest) []scoredCandidate {
	active := r.activeSignals()
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
		bd := scoreCandidate(profile, req, budget, r.warmth, nil, breaker)
		score := computeWeightedScore(bd, req.UseCase, active, customWeights)
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
	if !strings.Contains(selector, "/") {
		return
	}
	parts := strings.SplitN(selector, "/", 2)
	cb := r.getOrCreateBreaker(parts[0])
	cb.RecordFailure(err)
}

// appendRecommendTail runs ModelRegistry.Recommend with the request's
// constraints and returns scored candidates not already present in seen.
// Failures (e.g. registry index empty) return an error so the caller can
// decide whether to ignore (chain has survivors) or surface (chain empty).
func (r *Router) appendRecommendTail(ctx context.Context, req RoutingRequest, seen map[ModelKey]bool) ([]scoredCandidate, error) {
	all, err := r.registry.Recommend(ctx, RecommendOpts{
		RequiredCaps: req.RequiredCaps,
		AvailableRAM: r.availableRAM,
		PreferWarm:   req.PreferWarm,
	})
	if err != nil {
		return nil, err
	}
	scored := r.scoreChainStep(all, req)

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
