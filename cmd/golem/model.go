package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// modelSetUseCase is the routing use case every /model set selection is
// planned under. The command switches the model this interactive agent runs
// on, so the use case is "agent" whatever the chosen ROLE is named -- a role
// called "coding" or "planning" still feeds the agent route, and stamping the
// role's name as a use case would make the planned route, the admitted
// destinations, and the constructed caller disagree (#476 I5).
const modelSetUseCase = "agent"

// resolveModelSelection turns a /model set argument into the ONE route the
// session would switch to, resolved against the process's frozen effective
// config (#376 M1). Pure: the only inputs are that frozen value and the
// user's text, so no registry, provider, or network access is reachable from
// here. Inventory and capability validation happen later, after the route's
// destinations have been admitted.
//
// Three rules, in order:
//
//  1. An exact configured role (a key of Config.Models) wins, and keeps its
//     complete ordered fallback chain including entries that are temporarily
//     unavailable -- availability is the Router's judgement, not the
//     selector's. A role literally named "recommend" is a role like any
//     other, never the recommend marker.
//  2. Otherwise, when the text before the FIRST slash is a configured
//     provider key, the entire remaining suffix is the model id: a slashed
//     id such as "meta-llama/Llama-3.3-70B-Instruct" survives whole.
//  3. Otherwise the whole input is a bare model id, qualified against the
//     sole configured provider. An unrecognized first segment is therefore
//     part of the id, NOT an unknown-provider error -- nothing here scans
//     model names or inventories to guess a provider, because Config.Models
//     keys are roles rather than aliases and a startup inventory would only
//     see providers that happened to refresh.
//
// The returned route is canonical before any shared parser sees it, which is
// what lets parseSelector/selectorProvider keep cutting at the first slash.
// Callers derive the CLI plan with chainPlanFor and feed this same value to
// network planning, so the consumed chain and the admitted one cannot
// diverge.
func resolveModelSelection(eff *providerbootstrap.Effective, input string) (providerbootstrap.PlannedRoute, error) {
	if eff == nil {
		return providerbootstrap.PlannedRoute{}, fmt.Errorf("golem: /model set: no effective configuration")
	}
	sel := strings.TrimSpace(input)
	if sel == "" {
		return providerbootstrap.PlannedRoute{}, fmt.Errorf("golem: /model set: empty selector")
	}
	cfg := eff.Config()

	if _, isRole := cfg.Models[sel]; isRole {
		route, err := providerbootstrap.PlanRoleRoute(cfg, sel, modelSetUseCase)
		if err != nil {
			return providerbootstrap.PlannedRoute{}, fmt.Errorf("golem: /model set: %w", err)
		}
		return route, nil
	}

	if prov, model, qualified := strings.Cut(sel, "/"); qualified {
		if _, configured := cfg.Providers[prov]; configured {
			if model == "" {
				return providerbootstrap.PlannedRoute{}, fmt.Errorf(
					"golem: /model set %q: empty model id after provider %q; use provider/model", sel, prov)
			}
			return providerbootstrap.PlannedRoute{UseCase: modelSetUseCase, Chain: []string{sel}}, nil
		}
	}

	// Materialize rejects a config with no providers, so the only ambiguity
	// left is "more than one".
	keys := make([]string, 0, len(cfg.Providers))
	for key := range cfg.Providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) != 1 {
		return providerbootstrap.PlannedRoute{}, fmt.Errorf(
			"golem: /model set %q: ambiguous with %d providers configured (%s); use provider/model",
			sel, len(keys), strings.Join(keys, ", "))
	}
	return providerbootstrap.PlannedRoute{UseCase: modelSetUseCase, Chain: []string{keys[0] + "/" + sel}}, nil
}

// modelPreparation is the input to the ONE post-admission preparation
// sequence startup and /model set share (#376 M4 steps 3-5). Every field is
// process state that is already frozen by the time it is read: the bundle's
// registry and router, the plan whose destinations admission just consented
// to, and the flag-sourced settings. Nothing here is re-resolved, and nothing
// here may run before the destination gate is installed -- preparation reads
// model metadata and may probe, so calling it earlier would perform the very
// I/O admission exists to gate.
type modelPreparation struct {
	models capChecker       // bundle.Models: the one registry
	router *provider.Router // bundle.Router: the one router the caller routes through
	plan   chainPlan        // the admitted chain AND the use case it routes as

	resolveEndpoint endpointResolver // provider base_url/discovery path for diagnostics
	resolver        toolCallResolver // nil under -no-cap-probe: no active probing

	// think is the RESOLVER input, not a flag: startup passes -think, and a
	// mid-session switch passes thinkFlagValue of the accepted current
	// options so a rejected value is never resurrected.
	think string

	inputCeiling  int // -input-ceiling: explicit override, 0 => derive
	outputReserve int // -output-reserve
	pressureWarn  int // -pressure-warn percent, 0 disables the band
}

// preparedModel is everything the post-admission sequence resolves for one
// route. It deliberately stops short of the orchestrator and the runtime: a
// caller validates this whole value first and only then publishes, so a
// failed preparation installs nothing.
type preparedModel struct {
	plan        chainPlan              // the plan these outputs were resolved for
	warnings    []string               // per-entry preflight diagnostics, in chain order
	thinkOpts   provider.ModelOptions  // ONLY Think/ThinkEffort are meaningful
	thinkNotice string                 // one-line explanation when thinking was cleared
	ceiling     inputCeilingResolution // value + source, for the startup/status line
	budget      agent.Budget           // derived from the ceiling, reserve, and warn percent
	caller      agent.ModelCaller      // strict chain caller for the orchestrator factory
}

// prepareModel runs the post-admission preparation for one already-admitted
// route: tool-capability preflight, thinking resolution, input-ceiling
// derivation, and the budget/caller the orchestrator factory consumes.
//
// The order matters. Preflight runs first so a chain that cannot route a
// tool-capable model fails before any ceiling or thinking decision is derived
// from it, and its error is returned UNWRAPPED so the caller's classification
// (isPreflightCapabilityError => caller misuse, anything else => provider
// pre-run failure) keeps working. Per-entry warnings are returned on the
// failure path too, because the caller folds them into its own warning list
// before returning.
func prepareModel(ctx context.Context, in modelPreparation) (preparedModel, error) {
	warnings, err := preflightToolCapable(ctx, in.models, in.plan.chain, in.plan.useCase, in.resolveEndpoint, in.resolver)
	if err != nil {
		return preparedModel{plan: in.plan, warnings: warnings}, err
	}
	thinkOpts, thinkNotice := resolveThinkOptions(ctx, in.models, in.plan.chain, in.think)
	ceiling := resolveInputCeiling(ctx, in.models, in.plan.chain, in.plan.useCase,
		in.inputCeiling, in.outputReserve, in.resolver != nil)
	budget := agent.Budget{InputCeiling: ceiling.ceiling, OutputReserve: in.outputReserve}
	if in.pressureWarn > 0 {
		// The agent package owns the band layout (single source of truth for
		// the monotonic clamp + defaults); golem only supplies the fraction.
		budget.Pressure = agent.PressureThresholdsForWarn(float64(in.pressureWarn) / 100)
	}
	return preparedModel{
		plan:        in.plan,
		warnings:    warnings,
		thinkOpts:   thinkOpts,
		thinkNotice: thinkNotice,
		ceiling:     ceiling,
		budget:      budget,
		caller:      newActiveChainCaller(in.router, in.plan),
	}, nil
}

// modelSelection is the CLI's private record of WHICH model this session runs
// on (#376 M4 step 8). It is initialized at startup from exactly the values
// startup itself used and replaced wholesale after a successful publication.
//
// chain is OWNED. newActiveChainCaller keeps the slice it is handed, so a
// selection that aliased a caller's slice would let a later switch reach into
// a live caller.
type modelSelection struct {
	requested     string             // the selector text: the configured role at startup, the /model set argument after a switch
	chain         []string           // canonical chain, owned copy; empty under useRecommend
	useCase       string             // the routing use case the chain is served under
	useRecommend  bool               // startup recommendation mode; the first successful set clears it for good
	ceilingSource inputCeilingSource // which rule produced the live input ceiling
}

// startupSelector is the selector text the /model status echoes before any
// switch: the ROLE the active route resolved from, which is exactly what a
// user would type at /model set to reproduce it. Recommendation mode and a
// config-less run have no such role, so they report the empty string.
func startupSelector(cfg *config.Config, route providerbootstrap.PlannedRoute) string {
	if cfg == nil || route.Recommend || route.SuppliedByUseCase == "" {
		return ""
	}
	return cfg.Defaults[route.SuppliedByUseCase]
}

// chainLine renders the /model "chain:" line. Recommendation mode names the
// strategy rather than a head selector: calling a chain head the serving
// model is exactly the confusion M6 forbids.
func (s modelSelection) chainLine() string {
	if s.useRecommend || len(s.chain) == 0 {
		return fmt.Sprintf("model recommendation (use case: %s)", s.useCase)
	}
	return fmt.Sprintf("%s (strict; use case: %s)", strings.Join(s.chain, " -> "), s.useCase)
}

// modelStatusUsage is the single line every malformed /model form prints. It
// performs no provider I/O, by construction: it is reached before anything is
// resolved.
const modelStatusUsage = "usage: /model [set <role|name>]"

// handleModel implements /model (#376 M6). The bare form is entirely
// read-only -- no lookup, probe, or model call -- and every other form stops
// at the usage line before anything is resolved.
func handleModel(out io.Writer, sess *replSession, fields []string) {
	if len(fields) != 1 {
		_, _ = fmt.Fprintln(out, modelStatusUsage)
		return
	}
	writeModelStatus(out, sess)
}

// writeModelStatus renders the M6 status block. Each line comes from the
// authority that owns it: the selection for the requested selector, canonical
// chain, use case, strictness, and ceiling SOURCE; the runtime's current
// snapshot for the ceiling VALUE and the thinking mode; and the session for
// the last model actually routed. Reading the ceiling value from the runtime
// rather than from a remembered resolution is what keeps the display honest
// after a switch.
func writeModelStatus(out io.Writer, sess *replSession) {
	sel := sess.selection
	requested := sel.requested
	if requested == "" {
		// Startup recommendation mode: no role was configured, so there is no
		// selector text to echo.
		requested = "none configured"
	}
	_, _ = fmt.Fprintf(out, "model: %s\n", requested)
	_, _ = fmt.Fprintf(out, "chain: %s\n", sel.chainLine())
	if sess.runtime == nil {
		_, _ = fmt.Fprintln(out, "model: runtime unavailable")
		return
	}
	_, _ = fmt.Fprintln(out, inputCeilingResolution{
		ceiling: sess.runtime.Budget().InputCeiling,
		source:  sel.ceilingSource,
	}.line())
	_, _ = fmt.Fprintln(out, formatThinkOptions(sess.runtime.ModelOptions()))
	last := sess.lastModel
	if last == "" {
		last = "not yet routed"
	}
	_, _ = fmt.Fprintf(out, "last routed: %s\n", last)
}
