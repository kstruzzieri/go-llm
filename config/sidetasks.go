package config

import "sort"

// Auxiliary side-task use-case keys. These are use-case keys (the keys callers
// set in Config.Defaults), NOT model roles — Defaults maps use-case -> role.
// Untyped string constants so they drop into the existing string-typed APIs
// (ModelFor, Resolve, provider.RoutingRequest.UseCase) with no conversion.
const (
	UseCaseSummarize = "summarize"
	UseCaseRoute     = "route"
	UseCaseRerank    = "rerank"
	UseCaseVerify    = "verify"
	UseCaseExtract   = "extract"
	UseCaseApproval  = "approval"
	UseCaseVision    = "vision"
	// UseCasePlanning routes agent plan AUTHORING (#476). It names the TASK,
	// not a model property: "reasoning" is a model trait and appears below
	// only as a fallback target. Execution deliberately has no key of its own
	// -- "agent" already is the execution route, so a second name would be a
	// synonym for the route it falls back to.
	UseCasePlanning = "planning"
)

// sideTaskUseCaseFallbacks keeps auxiliary side-task use-cases optional.
// "route" here is the side-task model use-case, not provider.Router itself.
// Keys AND fallback entries are use-case keys looked up in Config.Defaults;
// explicit defaults win, these only pick an existing use-case when a side-task
// slot is absent from older models.json files.
var sideTaskUseCaseFallbacks = map[string][]string{
	UseCaseSummarize: {"analysis", "chat"},
	UseCaseRoute:     {"analysis", "chat"},
	UseCaseRerank:    {"analysis", "chat"},
	UseCaseVerify:    {"analysis", "chat"},
	UseCaseExtract:   {"analysis", "chat"},
	UseCaseApproval:  {"agent", "chat"},
	UseCaseVision:    {"chat"},
	// Planning degrades to a strong reasoner first, then the general analysis
	// route, and finally the agent route a runnable config always has. This
	// order is deliberately behavior-changing: a config that never mentions
	// planning can still author plans through an existing "reasoning" or
	// "analysis" role, which may live on a different provider than "agent".
	UseCasePlanning: {"reasoning", "analysis", "agent"},
}

// SideTaskUseCases returns the auxiliary side-task use-case keys, sorted.
// Derived from sideTaskUseCaseFallbacks so the enumerator and the fallback
// table cannot drift.
func SideTaskUseCases() []string {
	out := make([]string, 0, len(sideTaskUseCaseFallbacks))
	for k := range sideTaskUseCaseFallbacks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RoleForUseCase resolves a use-case to its configured model role. An explicit
// Config.Defaults entry wins; otherwise side-task use-cases fall back to
// existing defaults per sideTaskUseCaseFallbacks. ok is false when neither the
// use-case nor any of its fallbacks is configured.
func (c *Config) RoleForUseCase(useCase string) (string, bool) {
	if role, ok := c.Defaults[useCase]; ok {
		return role, true
	}
	for _, fallback := range sideTaskUseCaseFallbacks[useCase] {
		if role, ok := c.Defaults[fallback]; ok {
			return role, true
		}
	}
	return "", false
}
