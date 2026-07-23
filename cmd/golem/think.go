package main

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/provider"
)

// thinkModelOptions maps the validated -think value to per-run model options.
// Empty input returns zero options (model decides; no wire fields sent).
func thinkModelOptions(v string) provider.ModelOptions {
	switch v {
	case "":
		return provider.ModelOptions{}
	case "off":
		off := false
		return provider.ModelOptions{Think: &off}
	case "on":
		on := true
		return provider.ModelOptions{Think: &on}
	default: // low, medium, high — validated at the flag boundary
		on := true
		return provider.ModelOptions{Think: &on, ThinkEffort: v}
	}
}

// resolveThinkOptions gates -think on the configured agent chain's effective
// ThinkMode. Empty chain = recommend mode => fail open (selection happens at
// routing). If every resolved configured candidate is ThinkNone => zero
// options + one-line notice. Mixed chains and lookup failures fail open;
// RoutePlan clears wire controls for a selected ThinkNone fallback. Keys off
// ThinkMode, not CapThinking (openai-compat never advertises CapThinking).
// Selector semantics match the tool-capability preflight: provider/model via
// Lookup, bare model via LookupAny.
func resolveThinkOptions(ctx context.Context, src capChecker, chain []string, flagVal string) (provider.ModelOptions, string) {
	if flagVal == "" {
		return provider.ModelOptions{}, ""
	}
	opts := thinkModelOptions(flagVal)
	if len(chain) == 0 {
		return opts, ""
	}
	for _, sel := range chain {
		if key, parsed := parseSelector(sel); parsed {
			p, err := src.Lookup(ctx, key)
			if err != nil || p == nil {
				return opts, "" // fail open: diagnostics must not make startup brittle
			}
			if p.ThinkMode != provider.ThinkNone {
				return opts, "" // mixed chain: routing may select a thinking model
			}
			continue
		}
		// Bare model name: resolve across providers; the candidate counts as
		// ThinkNone only when every matching profile is ThinkNone.
		profs, err := src.LookupAny(ctx, sel)
		if err != nil || len(profs) == 0 {
			return opts, "" // fail open (error or unknown selector)
		}
		for _, p := range profs {
			if p == nil || p.ThinkMode != provider.ThinkNone {
				return opts, ""
			}
		}
	}
	// Every configured candidate resolved and none supports thinking: suppress
	// the options and tell the user once. chain[0] is a ThinkNone example.
	return provider.ModelOptions{}, fmt.Sprintf("think: model %s does not support thinking; -think ignored", chain[0])
}
