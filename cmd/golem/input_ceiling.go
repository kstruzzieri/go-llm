package main

import (
	"context"
	"fmt"

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
	ceiling int
	source  inputCeilingSource
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
	}
	return fmt.Sprintf("input ceiling: %d tokens (%s)", r.ceiling, source)
}

func resolveInputCeiling(ctx context.Context, reg capChecker, chain []string, explicit, outputReserve int, canResolveToolCall bool) inputCeilingResolution {
	if explicit > 0 {
		return inputCeilingResolution{ceiling: explicit, source: inputCeilingExplicit}
	}

	ceiling := 0
	usedFallback := false
	add := func(profile *provider.ModelProfile) {
		window := agent.DefaultInputCeiling
		if profile != nil && profile.ContextWindow > 0 {
			// Mirror the router's admission budget: it validates input against
			// EffectiveContextWindow("agent") minus the request's expected
			// output, and a zero -output-reserve leaves ExpectedOutput unset,
			// so the router reserves DefaultExpectedOutput("agent") implicitly.
			// Without that reserve here, a long session assembles past the
			// router budget and routing fails with ErrBudgetAdaptationRequired
			// instead of compacting. A nonzero -output-reserve is already
			// subtracted on both sides (turnBudget and ExpectedOutput), so the
			// full window is correct then.
			window = profile.EffectiveContextWindow("agent")
			reserve := outputReserve
			if reserve <= 0 {
				reserve = provider.DefaultExpectedOutput("agent")
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
		} else {
			usedFallback = true
		}
		if ceiling == 0 || window < ceiling {
			ceiling = window
		}
	}
	eligible := func(profile *provider.ModelProfile) bool {
		if profile == nil {
			return true
		}
		if !profile.Caps.Has(toolRouteCaps &^ provider.CapToolCall) {
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
		return true
	}

	if reg == nil {
		add(nil)
	} else if len(chain) == 0 {
		requiredCaps := toolRouteCaps
		if canResolveToolCall {
			requiredCaps &^= provider.CapToolCall
		}
		profiles, err := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: requiredCaps})
		if err != nil || len(profiles) == 0 {
			add(nil)
		} else {
			for _, profile := range profiles {
				if eligible(profile) {
					add(profile)
				}
			}
		}
	} else {
		for _, selector := range chain {
			if key, qualified := parseSelector(selector); qualified {
				profile, err := reg.Lookup(ctx, key)
				if err != nil {
					add(nil)
				} else if eligible(profile) {
					add(profile)
				}
				continue
			}
			profiles, err := reg.LookupAny(ctx, selector)
			if err != nil || len(profiles) == 0 {
				add(nil)
				continue
			}
			for _, profile := range profiles {
				if eligible(profile) {
					add(profile)
				}
			}
		}
	}
	if ceiling == 0 {
		add(nil)
	}

	source := inputCeilingChainMinimum
	if usedFallback {
		source = inputCeilingSafeFallback
	}
	return inputCeilingResolution{ceiling: ceiling, source: source}
}
