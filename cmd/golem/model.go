package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
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
