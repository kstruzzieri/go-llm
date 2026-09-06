package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

func handleThink(ctx context.Context, out io.Writer, sess *replSession, fields []string) {
	if len(fields) == 1 {
		if sess.runtime == nil {
			_, _ = fmt.Fprintln(out, "think: runtime unavailable")
			return
		}
		_, _ = fmt.Fprintln(out, formatThinkOptions(sess.runtime.ModelOptions()))
		return
	}
	if len(fields) != 2 {
		_, _ = fmt.Fprintln(out, "usage: /think [off|on|low|medium|high|default]")
		return
	}
	value := strings.ToLower(fields[1])
	switch value {
	case "off", "on", "low", "medium", "high", "default":
	default:
		_, _ = fmt.Fprintln(out, "usage: /think [off|on|low|medium|high|default]")
		return
	}
	if sess.runtime == nil {
		_, _ = fmt.Fprintln(out, "think: runtime unavailable")
		return
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintf(out, "think: %v\n", err)
		return
	}
	resolveValue := value
	if value == "default" {
		resolveValue = ""
	} else if sess.thinkModels == nil {
		_, _ = fmt.Fprintln(out, "think: model configuration unavailable")
		return
	}
	resolved, notice := resolveThinkOptions(ctx, sess.thinkModels, sess.thinkChain, resolveValue)
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintf(out, "think: %v\n", err)
		return
	}
	if notice != "" {
		_, _ = fmt.Fprintln(out, notice)
		return
	}
	current := sess.runtime.ModelOptions()
	candidate := current
	candidate.Think = resolved.Think
	candidate.ThinkEffort = resolved.ThinkEffort
	if sameThinkOptions(current, candidate) {
		_, _ = fmt.Fprintln(out, formatThinkOptions(candidate))
		return
	}
	if err := ctx.Err(); err != nil {
		_, _ = fmt.Fprintf(out, "think: %v\n", err)
		return
	}
	if err := sess.runtime.Replace(sess.baseSystem, sess.tools[sess.readToolCount:], candidate); err != nil {
		_, _ = fmt.Fprintf(out, "think: %v\n", err)
		return
	}
	_, _ = fmt.Fprintln(out, formatThinkOptions(candidate))
}

func sameThinkOptions(a, b provider.ModelOptions) bool {
	if a.ThinkEffort != b.ThinkEffort || (a.Think == nil) != (b.Think == nil) {
		return false
	}
	return a.Think == nil || *a.Think == *b.Think
}

func formatThinkOptions(opts provider.ModelOptions) string {
	if opts.Think != nil && !*opts.Think {
		return "think: off"
	}
	if opts.ThinkEffort != "" {
		return "think: " + opts.ThinkEffort
	}
	if opts.Think != nil {
		return "think: on"
	}
	return "think: default (model decides)"
}

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
