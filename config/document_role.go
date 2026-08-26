package config

import "fmt"

// UnbindUseCase removes a use-case route from defaults. An unknown use
// case is refused loudly (use_case_not_found) — unbinding a route that is
// not bound is caller staleness, mirroring role_not_found. go-llm imposes
// no floor on which use cases must exist; consumer floors (e.g. Firn's
// agent route) stay host-side. Chain validity of the remaining defaults
// rides the finalize gate.
func (d *Document) UnbindUseCase(useCase string) error {
	return d.mutate(func(a *Config) error {
		// Inside the closure so the central read-only gate wins for
		// invalid requests too (established pattern).
		if useCase == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: unbind use case: empty use case"))
		}
		if _, ok := a.Defaults[useCase]; !ok {
			return diagWrap(CodeUseCaseNotFound, SubjectUseCase, useCase,
				fmt.Errorf("config: unbind use case: %q not bound", useCase))
		}
		delete(a.Defaults, useCase)
		return nil
	})
}

// AddRoleModel atomically creates a new role born complete: identity and
// capacity from facts, overrides only as re-asserted in opts (deep-copied;
// SetRoleModel option semantics, documented there). Description, Fallbacks,
// and Options start zero — the Slice B contract does not name them for
// creation; additive later if a consumer asks. The eligibility gate runs
// with full SetRoleModel semantics: an unrouted new role evaluates
// unknown/no_requirements and needs ConfirmUnknown; a supplied capabilities
// override joining an existing selector gates every selector role, because
// the override changes selector-wide persisted truth for live routes. The
// finalize gate (static selector conflicts included) validates the result;
// failure leaves the document unchanged.
func (d *Document) AddRoleModel(role string, facts ModelFacts, opts SetRoleModelOpts) error {
	return d.mutate(func(a *Config) error {
		if role == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: add role model: empty role"))
		}
		if err := validateRoleFacts("add role model", role, facts); err != nil {
			return err
		}
		if _, exists := a.Models[role]; exists {
			return diagWrap(CodeRoleExists, SubjectRole, role,
				fmt.Errorf("config: add role model: role %q already defined", role))
		}
		if _, ok := a.Providers[facts.Key.Provider]; !ok {
			return diagWrap(CodeProviderNotFound, SubjectProvider, facts.Key.Provider,
				fmt.Errorf("config: add role model %q: provider %q not configured", role, facts.Key.Provider))
		}
		if _, err := gateRoleEligibility(a, "add role model", role, facts, opts); err != nil {
			return err
		}
		m := ModelConfig{
			Name: facts.Key.Model, Provider: facts.Key.Provider,
			Type: facts.Type, Parameters: facts.Parameters,
			ContextWindow: facts.ContextWindow, Dimensions: facts.Dimensions,
		}
		applyRoleOverridesFromOpts(&m, opts)
		if a.Models == nil {
			a.Models = map[string]ModelConfig{}
		}
		a.Models[role] = m
		return nil
	})
}

// RemoveRole deletes a role nothing references. The explicit in-use
// pre-check names the first blocker in deterministic order — routed use
// cases (sorted) before fallback references (sorted) — mirroring
// RemoveProvider's discipline; the finalize gate remains the authority
// behind it. The mutate models diff records the tombstone so a later
// re-add of the same name cannot resurrect stale unknown raw members.
func (d *Document) RemoveRole(role string) error {
	return d.mutate(func(a *Config) error {
		if role == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: remove role: empty role"))
		}
		if _, ok := a.Models[role]; !ok {
			return diagWrap(CodeRoleNotFound, SubjectRole, role,
				fmt.Errorf("config: remove role: role %q not defined", role))
		}
		for _, uc := range sortedStringKeys(a.Defaults) {
			if a.Defaults[uc] == role {
				return diagWrap(CodeRoleInUse, SubjectUseCase, uc,
					fmt.Errorf("config: remove role %q: still routed by use case %q", role, uc))
			}
		}
		for _, other := range sortedStringKeys(a.Models) {
			if other == role {
				continue
			}
			for _, fb := range a.Models[other].Fallbacks {
				if fb == role {
					return diagWrap(CodeRoleInUse, SubjectRole, other,
						fmt.Errorf("config: remove role %q: still referenced by %q fallbacks", role, other))
				}
			}
		}
		delete(a.Models, role)
		return nil
	})
}
