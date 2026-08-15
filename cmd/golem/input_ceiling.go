package main

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/agent"
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

func (r inputCeilingResolution) line() string {
	source := string(r.source)
	if r.source == inputCeilingSafeFallback {
		source += "; model context metadata unavailable"
	}
	return fmt.Sprintf("input ceiling: %d tokens (%s)", r.ceiling, source)
}

func resolveInputCeiling(ctx context.Context, reg capChecker, chain []string, explicit int) inputCeilingResolution {
	if explicit > 0 {
		return inputCeilingResolution{ceiling: explicit, source: inputCeilingExplicit}
	}

	ceiling := 0
	usedFallback := false
	add := func(profile *provider.ModelProfile) {
		window := agent.DefaultInputCeiling
		if profile != nil && profile.ContextWindow > 0 {
			window = profile.ContextWindow
		} else {
			usedFallback = true
		}
		if ceiling == 0 || window < ceiling {
			ceiling = window
		}
	}

	if reg == nil {
		add(nil)
	} else if len(chain) == 0 {
		profiles, err := reg.Recommend(ctx, provider.RecommendOpts{RequiredCaps: toolRouteCaps})
		if err != nil || len(profiles) == 0 {
			add(nil)
		} else {
			for _, profile := range profiles {
				add(profile)
			}
		}
	} else {
		for _, selector := range chain {
			if key, qualified := parseSelector(selector); qualified {
				profile, err := reg.Lookup(ctx, key)
				if err != nil {
					add(nil)
				} else {
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
				add(profile)
			}
		}
	}

	source := inputCeilingChainMinimum
	if usedFallback {
		source = inputCeilingSafeFallback
	}
	return inputCeilingResolution{ceiling: ceiling, source: source}
}
