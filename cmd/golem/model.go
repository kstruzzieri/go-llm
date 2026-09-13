package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/config"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
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
	// newActiveChainCaller RETAINS the slice it is handed, so the caller this
	// function builds must never share backing storage with a chain its
	// caller still owns: a later switch that reused that array would reach
	// into a live caller. Every output below carries this owned copy.
	in.plan.chain = slices.Clone(in.plan.chain)
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

	// Frozen preparation inputs: the exact bundle and flag values startup
	// passed to BuildNetworkPlan, prepareModel, newDispatchTool, and
	// newOrchestratorFactory. A switch reuses them verbatim -- there is one
	// registry, one router, and one effective config for the process, and
	// re-resolving any of them could make the consumed route and the admitted
	// route disagree (#376 M2).
	effective       *providerbootstrap.Effective
	models          capChecker
	router          *provider.Router
	resolveEndpoint endpointResolver
	resolver        toolCallResolver // nil under -no-cap-probe: no active probing
	planOpts        providerbootstrap.PlanOptions
	flags           flags
	orchVerifier    agent.Verifier // the startup verifier, or the REPL's late-verifier slot
	dispatchNotify  func(string)   // newDispatchTool's completion-notice sink; nil when dispatch is off
}

// followsParentDispatch reports whether the default parent-following dispatch
// tool must be rebuilt for a new selection. An explicit -dispatch-role keeps
// its own independent startup route (#376 M5), so it is excluded.
func (s modelSelection) followsParentDispatch() bool {
	return s.flags.dispatch && s.flags.dispatchRole == ""
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

// modelStatusUsage is the single line every malformed /model form prints.
// Reaching it resolves nothing, so a malformed command performs no provider
// I/O (#376 M1).
const modelStatusUsage = "usage: /model [set <role|name>]"

// dispatchNotifySink adapts the late-bound dispatch notice sink for
// sess.selection. It is nil-safe because the notifier only exists when
// -dispatch is on, and a method value on a nil notifier would panic the first
// time a child finished.
func dispatchNotifySink(n *feedbackNotifier) func(string) {
	if n == nil {
		return nil
	}
	return n.notify
}

// destinationGrantRetainedNotice is appended to a /model set failure ONLY
// when the admission wrapper actually admitted new or renewed remote consent
// before that failure (#376 M6). Reusing an existing grant, a local
// destination, or an exact -allow-destination flag must not produce it:
// saying "a grant was retained" when none was taken teaches the user to run
// /grants clear for nothing.
const destinationGrantRetainedNotice = "destination grant retained for this session; use /grants clear to revoke"

// handleModel implements /model and /model set (#376 M6). The bare form is
// entirely read-only -- no lookup, probe, or model call -- and every
// malformed form stops at the usage line before anything is resolved.
func handleModel(ctx context.Context, out io.Writer, sess *replSession, fields []string) {
	switch {
	case len(fields) == 1:
		writeModelStatus(out, sess)
	case len(fields) == 3 && fields[1] == "set":
		handleModelSet(ctx, out, sess, fields[2])
	default:
		_, _ = fmt.Fprintln(out, modelStatusUsage)
	}
}

// modelSwitch is the fully prepared candidate one /model set publishes. Every
// field is allocated and validated BEFORE ReplaceConfiguration is called, so
// a failure anywhere above installs nothing and the bookkeeping that follows
// a successful publication is infallible (#376 M4 steps 7-8).
type modelSwitch struct {
	selection   modelSelection
	tools       []agent.Tool
	orch        *agent.Orchestrator
	newOrch     func() *agent.Orchestrator
	options     provider.ModelOptions
	budget      agent.Budget
	warnings    []string
	thinkNotice string
}

// handleModelSet runs the whole switch synchronously inside slash dispatch
// (#376 M6): the next goal is read only after this returns, under whichever
// configuration won. Ctrl-C scopes to this operation through interruptContext,
// whose deferred cancel joins the watcher before the prompt comes back.
//
// Publication decides success. A cancellation observed before
// ReplaceConfiguration preserves the old tuple; one arriving afterwards must
// not misreport a committed switch as a rollback, so nothing below the
// publication can fail.
func handleModelSet(ctx context.Context, out io.Writer, sess *replSession, arg string) {
	ctx, cancel := interruptContext(ctx, sess.interrupts)
	defer cancel()

	sw, newGrantAdmitted, err := prepareModelSwitch(ctx, sess, arg)
	if err == nil {
		// Last look before authority changes hands. ReplaceConfiguration
		// rechecks under its own publication lock; this check keeps a
		// cancellation raised during preparation from reaching it at all.
		if err = ctx.Err(); err == nil {
			err = sess.runtime.ReplaceConfiguration(ctx, golemruntime.Configuration{
				System:       sess.baseSystem,
				Tools:        sw.tools[sess.readToolCount:],
				ModelOptions: sw.options,
				Orchestrator: sw.orch,
				Budget:       sw.budget,
			})
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "model unchanged: %s\n", runFailureMessage("", err))
		if newGrantAdmitted {
			_, _ = fmt.Fprintln(out, destinationGrantRetainedNotice)
		}
		return
	}
	// Infallible CLI bookkeeping, in the synchronous handler, before the next
	// prompt (#376 M4 step 8).
	sess.selection = sw.selection
	sess.tools = sw.tools
	sess.orch = sw.orch
	sess.newOrchestrator = sw.newOrch
	sess.pressure = nil // the old sample described a request the old model assembled
	sess.lastModel = "" // nothing has routed on the new selection yet
	if sw.thinkNotice != "" {
		_, _ = fmt.Fprintln(out, sw.thinkNotice)
	}
	for _, w := range sw.warnings {
		_, _ = fmt.Fprintln(out, w)
	}
	writeModelStatus(out, sess)
}

// prepareModelSwitch resolves, admits, and builds a complete candidate
// without touching the session. The returned boolean is the admission
// wrapper's newGrantAdmitted: it is retained through every later step so a
// failure can say whether a remote grant is now standing.
func prepareModelSwitch(ctx context.Context, sess *replSession, arg string) (modelSwitch, bool, error) {
	sel := sess.selection
	if sess.runtime == nil {
		return modelSwitch{}, false, errors.New("golem: /model set: runtime unavailable")
	}

	// 1. Selector -> the ONE planned route everything downstream consumes.
	route, err := resolveModelSelection(sel.effective, arg)
	if err != nil {
		return modelSwitch{}, false, err
	}

	// 2. Candidate reachability, then ONE additive admission decision for the
	// complete proposal -- before any candidate metadata I/O.
	routes := []providerbootstrap.PlannedRoute{route}
	if sel.followsParentDispatch() {
		// Children follow the parent, so the dispatch purpose must be
		// admitted in the SAME decision; asking about it later would split
		// one consent question in two.
		routes = append(routes, providerbootstrap.PlannedRoute{UseCase: dispatchUseCase, Chain: route.Chain})
	}
	netPlan, err := providerbootstrap.BuildNetworkPlan(sel.effective, routes, sel.planOpts)
	if err != nil {
		return modelSwitch{}, false, err
	}
	newGrantAdmitted := false
	if sess.destAdmission != nil {
		if newGrantAdmitted, err = sess.destAdmission.extend(ctx, netPlan.Edges); err != nil {
			return modelSwitch{}, newGrantAdmitted, err
		}
	}

	// 3. The shared post-admission sequence. The thinking input is the
	// ACCEPTED current state, never the startup flag, so a value the session
	// rejected cannot come back with a new chain.
	prep, err := prepareModel(ctx, modelPreparation{
		models:          sel.models,
		router:          sel.router,
		plan:            chainPlanFor(route),
		resolveEndpoint: sel.resolveEndpoint,
		resolver:        sel.resolver,
		think:           thinkFlagValue(sess.runtime.ModelOptions()),
		inputCeiling:    sel.flags.inputCeiling,
		outputReserve:   sel.flags.outputReserve,
		pressureWarn:    sel.flags.pressureWarn,
	})
	if err != nil {
		return modelSwitch{}, newGrantAdmitted, err
	}

	// 4. Parent-following dispatch, replaced IN PLACE so every other tool
	// keeps its index and readToolCount/mountAt/writeToolCount still describe
	// the slice.
	newTools := sess.tools
	if sel.followsParentDispatch() {
		if newTools, err = rebuildDispatchTool(ctx, sess, prep.plan.chain); err != nil {
			return modelSwitch{}, newGrantAdmitted, err
		}
	}

	// 5. The orchestrator factory, rebound so no later rebuild can revive the
	// startup caller.
	newOrch := newOrchestratorFactory(prep.caller, sel.flags, sel.orchVerifier, sess.canary)

	next := sel
	next.requested = arg
	// A THIRD copy, not prep.plan.chain: that array is already retained by the
	// caller prepareModel built and, under default dispatch, by the child
	// caller rebuildDispatchTool built from it. The selection is the one
	// holder a later command can be tempted to rewrite in place, so it owns
	// its own (#376 M4 step 8).
	next.chain = slices.Clone(prep.plan.chain)
	next.useCase = prep.plan.useCase
	next.useRecommend = false // the first successful set is strict for the process lifetime
	next.ceilingSource = prep.ceiling.source
	return modelSwitch{
		selection:   next,
		tools:       newTools,
		orch:        newOrch(),
		newOrch:     newOrch,
		options:     applyThinkOptions(sess.runtime.ModelOptions(), prep.thinkOpts),
		budget:      prep.budget,
		warnings:    prep.warnings,
		thinkNotice: prep.thinkNotice,
	}, newGrantAdmitted, nil
}

// dispatchToolIndex locates the single registered dispatch tool. Exactly one
// must exist when the feature is enabled: zero means the candidate would
// publish a tool set the orchestrator's invocation limit names but cannot
// find, and two means an earlier rebuild appended instead of replacing.
func dispatchToolIndex(tools []agent.Tool) (int, error) {
	idx, found := -1, 0
	for i, t := range tools {
		if t != nil && t.Spec().Name == agenttools.DispatchToolName {
			found++
			if idx < 0 {
				idx = i
			}
		}
	}
	if found != 1 {
		return 0, fmt.Errorf("golem: /model set: dispatch is enabled but %d dispatch tools are registered; expected exactly one", found)
	}
	return idx, nil
}

// rebuildDispatchTool returns a CLONE of the session tools with the dispatch
// entry replaced at its own index by one built for the new chain: new caller,
// new child ceiling under the dispatch use case, and fan-out re-derived from
// the router's cached slot capacity. The child-visible tool set is the exact
// prefix that preceded dispatch at startup -- the read-only tools, before
// memory, write, exec, delegate, and MCP were appended -- so children keep
// seeing what they saw and nothing else.
func rebuildDispatchTool(ctx context.Context, sess *replSession, chain []string) ([]agent.Tool, error) {
	idx, err := dispatchToolIndex(sess.tools)
	if err != nil {
		return nil, err
	}
	sel := sess.selection
	// Same constant the dispatch caller routes with, so the child's ceiling
	// and the child's route can never disagree.
	childCeiling := resolveInputCeiling(ctx, sel.models, chain, dispatchUseCase,
		sel.flags.inputCeiling, sel.flags.outputReserve, sel.resolver != nil).ceiling
	fan := resolveDispatchFanout(sel.router.SlotCapacity, chain)
	caller := newRouterChainCallerFor(sel.router, chain, dispatchUseCase)
	dpt, err := newDispatchTool(caller, sel.flags,
		agent.Budget{InputCeiling: childCeiling, OutputReserve: sel.flags.outputReserve},
		fan, sel.dispatchNotify, sess.tools[:idx], sess.canary)
	if err != nil {
		return nil, err
	}
	next := slices.Clone(sess.tools)
	next[idx] = dpt
	return next, nil
}

// writeModelStatus renders the M6 status block. Each line comes from the
// authority that owns it: the selection for the requested selector, canonical
// chain, use case, strictness, and ceiling SOURCE; the runtime's current
// snapshot for the ceiling VALUE and the thinking mode; and the session for
// the last model actually routed. Reading the ceiling value from the runtime
// rather than from a remembered resolution is what keeps the display honest
// after a switch.
func writeModelStatus(out io.Writer, sess *replSession) {
	if sess.runtime == nil {
		// Same shape as /think and /context in a runtime-less session: one
		// line, no half-rendered block.
		_, _ = fmt.Fprintln(out, "model: runtime unavailable")
		return
	}
	sel := sess.selection
	requested := sel.requested
	if requested == "" {
		// Startup recommendation mode: no role was configured, so there is no
		// selector text to echo.
		requested = "none configured"
	}
	_, _ = fmt.Fprintf(out, "model: %s\n", requested)
	_, _ = fmt.Fprintf(out, "chain: %s\n", sel.chainLine())
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
