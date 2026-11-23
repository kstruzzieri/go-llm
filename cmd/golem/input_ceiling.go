package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

type inputCeilingSource string

const (
	inputCeilingExplicit     inputCeilingSource = "explicit"
	inputCeilingChainMinimum inputCeilingSource = "chain minimum"
	inputCeilingSafeFallback inputCeilingSource = "safe fallback"
)

type inputCeilingResolution struct {
	ceiling      int
	source       inputCeilingSource
	fallbackFrom string // the chain entries that put the safe default in, comma-separated
}

type toolCallExplainer interface {
	ExplainToolCall(context.Context, provider.ModelKey) (provider.ToolCallExplanation, error)
}

// The eligibility filter reaches ExplainToolCall through a runtime type
// assertion that silently reports "not implemented" on signature drift; pin
// the production registry to it at compile time so drift breaks the build
// instead of quietly disabling every exclusion.
var _ toolCallExplainer = (*provider.ModelRegistry)(nil)

func (r inputCeilingResolution) line() string {
	source := string(r.source)
	if r.source == inputCeilingSafeFallback {
		source += "; model context metadata unavailable"
		if r.fallbackFrom != "" {
			// The names come from models.json, possibly the working
			// directory's: one inert line.
			source += " for " + rawArgsLine(r.fallbackFrom)
		}
	}
	return fmt.Sprintf("input ceiling: %d tokens (%s)", r.ceiling, source)
}

// resolveInputCeiling derives the per-turn input ceiling for the chain that
// will actually serve, under the use case it will actually be routed as.
//
// useCase is a parameter rather than a literal because both inputs below are
// use-case dependent, and the router applies the CALLER's use case, not
// "agent": EffectiveContextWindow honors a model's QualityCtxCeiling only for
// quality-sensitive use cases, and DefaultExpectedOutput varies per use case.
// A ceiling derived under one use case and spent under another sizes context
// against a budget the router never applies (#476 D4).
func resolveInputCeiling(ctx context.Context, reg capChecker, chain []string, useCase string, explicit, outputReserve int, canResolveToolCall bool) inputCeilingResolution {
	if explicit > 0 {
		return inputCeilingResolution{ceiling: explicit, source: inputCeilingExplicit}
	}

	ceiling := 0
	knownMin := 0 // smallest window with real metadata; 0 when none
	usedFallback := false
	var fallbackFrom []string
	// entry names the chain entry in the line when it falls back; "" when
	// no single entry is to blame.
	add := func(profile *provider.ModelProfile, entry string) {
		window := agent.DefaultInputCeiling
		eff := 0
		if profile != nil {
			eff = profile.EffectiveContextWindow(useCase)
		}
		if eff > 0 {
			// Mirror the router's admission budget: it validates input against
			// EffectiveContextWindow(useCase) minus the request's expected
			// output, and a zero -output-reserve leaves ExpectedOutput unset,
			// so the router reserves DefaultExpectedOutput(useCase) implicitly.
			// Without that reserve here, a long session assembles past the
			// router budget and routing fails with ErrBudgetAdaptationRequired
			// instead of compacting. A nonzero -output-reserve is already
			// subtracted on both sides (turnBudget and ExpectedOutput), so the
			// full window is correct then. The guard is the effective window,
			// not ContextWindow: with an unknown served window a configured
			// QualityCtxCeiling is the router's budget for a quality-sensitive
			// use case (#479), so Golem must size against it too.
			window = eff
			reserve := outputReserve
			if reserve <= 0 {
				reserve = provider.DefaultExpectedOutput(useCase)
			}
			if window <= reserve {
				// The router's budget for this model is zero: it can never be
				// admitted, so routing always serves from other chain entries.
				// Letting it set the chain minimum would crush the ceiling for
				// the models that actually carry the session.
				return
			}
			if outputReserve <= 0 {
				window -= reserve
			}
			if knownMin == 0 || window < knownMin {
				knownMin = window
			}
		} else {
			usedFallback = true
			if entry != "" && !slices.Contains(fallbackFrom, entry) {
				fallbackFrom = append(fallbackFrom, entry)
			}
		}
		if ceiling == 0 || window < ceiling {
			ceiling = window
		}
	}
	eligible := func(profile *provider.ModelProfile) bool {
		if profile == nil {
			return true
		}
		if !profile.Caps.Has(agent.ModelCallCapabilities(false)) {
			return false
		}
		if profileToolCapable(profile) {
			return true
		}
		if !canResolveToolCall {
			return false
		}
		explainer, ok := reg.(toolCallExplainer)
		if !ok {
			return true
		}
		explanation, err := explainer.ExplainToolCall(ctx, profile.Key)
		if err != nil {
			return true
		}
		if explanation.Source == "explicit" {
			return explanation.Has
		}
		if explanation.Source == "probe" && explanation.Valid {
			return explanation.State == fingerprint.CapProbeYes
		}
		if explanation.Source == "runtime" && !explanation.Has {
			return false // the provider said no, stored or live: routing rejects it
		}
		return true
	}

	if reg == nil {
		add(nil, "")
	} else if len(chain) == 0 {
		requiredCaps := agent.ModelCallCapabilities(true)
		if canResolveToolCall {
			requiredCaps &^= provider.CapToolCall
		}
		profiles, err := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: requiredCaps})
		if err != nil || len(profiles) == 0 {
			add(nil, "")
		} else {
			for _, profile := range profiles {
				if eligible(profile) {
					// No configured entry to name: the recommendation is
					// unbounded and not the user's to fix.
					add(profile, "")
				}
			}
		}
	} else {
		for _, selector := range chain {
			var profiles []*provider.ModelProfile
			var err error
			if key, qualified := parseSelector(selector); qualified {
				var profile *provider.ModelProfile
				profile, err = reg.Lookup(ctx, key)
				profiles = []*provider.ModelProfile{profile}
			} else {
				profiles, err = reg.LookupAny(ctx, selector)
			}
			switch {
			case errors.Is(err, provider.ErrModelNotFound):
				// The provider answered and does not list it, so routing
				// cannot pick it while that stays true: like a window the
				// reserve consumes, it must not set the chain minimum (#856).
				// Any other lookup error keeps the safe default below, since
				// an unreachable provider may come back mid-session.
			case err != nil || len(profiles) == 0:
				add(nil, selector)
			default:
				for _, profile := range profiles {
					if !eligible(profile) {
						continue
					}
					entry := selector
					if profile != nil {
						entry = profile.Key.String()
					}
					add(profile, entry)
				}
			}
		}
	}
	if ceiling == 0 {
		add(nil, "")
	}

	// The safe default is the reason only when it binds: a known window at
	// or below it sets the ceiling whatever the other entries do, and naming
	// them would point at a fix that cannot raise the budget.
	if usedFallback && (knownMin == 0 || knownMin > agent.DefaultInputCeiling) {
		return inputCeilingResolution{ceiling: ceiling, source: inputCeilingSafeFallback, fallbackFrom: strings.Join(fallbackFrom, ", ")}
	}
	return inputCeilingResolution{ceiling: ceiling, source: inputCeilingChainMinimum}
}
