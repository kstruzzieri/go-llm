package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

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
		if survivor, field := selectorStateDrop(a, role); survivor != "" {
			return diagWrap(CodeRoleInUse, SubjectRole, survivor,
				fmt.Errorf("config: remove role %q: still supplies selector-wide %s used by role %q", role, field, survivor))
		}
		delete(a.Models, role)
		return nil
	})
}

func selectorStateDrop(cfg *Config, removedRole string) (string, string) {
	models := cfg.Models
	removed := models[removedRole]
	removedProvider := removed.Provider
	if removedProvider == "" {
		removedProvider = "ollama"
	}
	var survivorNames []string
	var survivors []ModelConfig
	for _, role := range sortedStringKeys(models) {
		if role == removedRole {
			continue
		}
		m := models[role]
		providerName := m.Provider
		if providerName == "" {
			providerName = "ollama"
		}
		if providerName == removedProvider && m.Name == removed.Name {
			survivorNames = append(survivorNames, role)
			survivors = append(survivors, m)
		}
	}
	if len(survivors) == 0 {
		return "", ""
	}
	preserved := func(match func(ModelConfig) bool) bool {
		return slices.ContainsFunc(survivors, match)
	}
	removedCaps, removedCapsErr := provider.ParseCapsStrict(removed.Capabilities)
	switch {
	case removed.ContextWindow != 0 && !preserved(func(m ModelConfig) bool {
		return m.ContextWindow == removed.ContextWindow
	}):
		return survivorNames[0], "context_window"
	case removed.Options != nil && !preserved(func(m ModelConfig) bool {
		return m.Options != nil && reflect.DeepEqual(m.Options, removed.Options)
	}):
		return survivorNames[0], "options"
	case len(removed.Capabilities) > 0 && !preserved(func(m ModelConfig) bool {
		caps, err := provider.ParseCapsStrict(m.Capabilities)
		return len(m.Capabilities) > 0 && removedCapsErr == nil && err == nil && caps == removedCaps
	}):
		return survivorNames[0], "capabilities"
	case derivedCapabilitiesDrop(cfg.Providers[removedProvider], removed, survivors):
		return survivorNames[0], "capabilities"
	case removed.ThinkMode != "" && !preserved(func(m ModelConfig) bool {
		return m.ThinkMode != "" && strings.EqualFold(m.ThinkMode, removed.ThinkMode)
	}):
		return survivorNames[0], "think_mode"
	case removed.ThinkTags != nil && !preserved(func(m ModelConfig) bool {
		return m.ThinkTags != nil && *m.ThinkTags == *removed.ThinkTags
	}):
		return survivorNames[0], "think_tags"
	case removed.Slots != 0 && !preserved(func(m ModelConfig) bool {
		return m.Slots == removed.Slots
	}):
		return survivorNames[0], "slots"
	default:
		return "", ""
	}
}

func derivedCapabilitiesDrop(provider ProviderConfig, removed ModelConfig, survivors []ModelConfig) bool {
	if provider.APIFormat != "openai-compat" || len(removed.Capabilities) > 0 {
		return false
	}
	survivorCaps := make(map[string]bool)
	for _, survivor := range survivors {
		if len(survivor.Capabilities) > 0 {
			return false
		}
		for _, capability := range survivor.ResolvedCapabilities() {
			survivorCaps[capability] = true
		}
	}
	for _, capability := range removed.ResolvedCapabilities() {
		if !survivorCaps[capability] {
			return true
		}
	}
	return false
}

// ForkRoleModelOpts carries the SetRoleModel option semantics plus the
// exact drop confirmation for the source's projection-hidden fields.
type ForkRoleModelOpts struct {
	SetRoleModelOpts
	// ConfirmDrops must match the computed drop set exactly (compared
	// sorted and duplicate-free; closed vocabulary "slots" |
	// "think_tags"); otherwise the fork is refused before any mutation
	// and the required set is extractable from the error via DropSetOf.
	ConfirmDrops []string
}

// forkDropSet computes the projection-hidden fields the fork would drop,
// sorted ("slots" < "think_tags"): think_tags unless re-asserted; slots
// always when set (never re-assertable — the fork targets a different
// model).
func forkDropSet(src ModelConfig, opts SetRoleModelOpts) []string {
	var out []string
	if src.Slots != 0 {
		out = append(out, "slots")
	}
	if src.ThinkTags != nil && opts.ThinkTags == nil {
		out = append(out, "think_tags")
	}
	return out
}

// validateConfirmDrops rejects tokens outside the vocabulary and
// duplicates, and returns the sorted copy used for comparison.
func validateConfirmDrops(confirm []string) ([]string, error) {
	out := append([]string(nil), confirm...)
	sort.Strings(out)
	for i, tok := range out {
		if tok != "slots" && tok != "think_tags" {
			return nil, diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: fork role model: unknown confirm drop %q", tok))
		}
		if i > 0 && out[i-1] == tok {
			return nil, diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: fork role model: duplicate confirm drop %q", tok))
		}
	}
	return out, nil
}

// ForkRoleModel atomically copies sourceRole's complete raw authored
// subtree — unknown/future JSON members included, via the raw seed
// mechanism (spec §1) — then starts from a deep clone of the complete typed
// source role and overlays the new selector/capacity plus opts re-assertions.
// Copy-by-default preserves future typed fields unless a later contract
// explicitly gives them replacement/drop semantics. The SetRoleModel
// preservation table defines the fields cleared or re-asserted. The
// source's projection-hidden ThinkTags/Slots follow the exact
// drop-confirmation rule: the computed set must be confirmed verbatim in
// opts.ConfirmDrops or the fork is refused before any mutation
// (drop_confirmation_required; set via DropSetOf).
// Eligibility semantics are AddRoleModel's (shared gate).
func (d *Document) ForkRoleModel(sourceRole, role string, facts ModelFacts, opts ForkRoleModelOpts) error {
	return d.mutateCommit(func(a *Config) error {
		if sourceRole == "" || role == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: fork role model: empty source or role"))
		}
		if err := validateRoleFacts("fork role model", role, facts); err != nil {
			return err
		}
		src, ok := a.Models[sourceRole]
		if !ok {
			return diagWrap(CodeRoleNotFound, SubjectRole, sourceRole,
				fmt.Errorf("config: fork role model: role %q not defined", sourceRole))
		}
		if _, exists := a.Models[role]; exists {
			return diagWrap(CodeRoleExists, SubjectRole, role,
				fmt.Errorf("config: fork role model: role %q already defined", role))
		}
		if _, ok := a.Providers[facts.Key.Provider]; !ok {
			return diagWrap(CodeProviderNotFound, SubjectProvider, facts.Key.Provider,
				fmt.Errorf("config: fork role model %q: provider %q not configured", role, facts.Key.Provider))
		}
		confirmed, err := validateConfirmDrops(opts.ConfirmDrops)
		if err != nil {
			return err
		}
		required := forkDropSet(src, opts.SetRoleModelOpts)
		if !slices.Equal(required, confirmed) {
			return dropSetWrap(required,
				diagWrap(CodeDropConfirmationRequired, SubjectRole, sourceRole,
					fmt.Errorf("config: fork role model %q: unconfirmed drops from %q: %s",
						role, sourceRole, strings.Join(required, ", "))))
		}
		if _, err := gateRoleEligibility(a, "fork role model", role, facts, opts.SetRoleModelOpts); err != nil {
			return err
		}
		// Config.clone is the existing generic deep-copy primitive. The extra
		// whole-config clone is acceptable on this rare user mutation and avoids
		// a field list that would silently lose future typed members.
		m := a.clone().Models[sourceRole]
		m.Name, m.Provider = facts.Key.Model, facts.Key.Provider
		m.Type = facts.Type
		m.Parameters = facts.Parameters
		m.ContextWindow = facts.ContextWindow
		m.Dimensions = facts.Dimensions
		applyRoleOverridesFromOpts(&m, opts.SetRoleModelOpts)
		a.Models[role] = m
		return nil
	}, func() {
		d.registerForkSeedLocked(sourceRole, role)
	})
}

// RoleOverrides is the explicit selector-wide override truth for the two
// projection-visible override fields. Replace semantics on exactly these
// fields; every other authored field — ThinkTags and Slots included — is
// preserved untouched (the mutation never reads or writes them).
type RoleOverrides struct {
	// Capabilities: nil clears the explicit override (capabilities derive
	// from Type again); non-empty sets it. An empty non-nil slice is
	// invalid_argument — a role cannot have zero capabilities.
	Capabilities []string
	// ThinkMode: "" clears the override; a value sets it (validated by
	// the finalize gate against the closed think-mode vocabulary).
	ThinkMode string
}

// SetRoleOverridesOpts is the eligibility-gate subset of the SetRoleModel
// options — there is nothing to re-assert because the overrides ARE the
// assertion. Caps/KnownMask are consulted only when clearing (evaluation
// falls back to derived-from-type facts); a set override is evaluated as
// selector-wide persisted truth with full knowledge (carve-down rule).
type SetRoleOverridesOpts struct {
	Requirements    map[string]provider.Capability
	Caps, KnownMask provider.Capability
	ConfirmUnknown  bool
}

// SetRoleOverrides applies ov to EVERY role whose effective selector
// (implicit-ollama rule applied) equals sel, in one atomic mutation —
// uniform values keep the static selector-conflict invariant satisfiable
// by construction. Zero matching roles is role_not_found with no subject
// (nothing to override). Firn uses this for same-selector capability/
// think edits instead of SetRoleModel, whose replace semantics clear
// omitted ThinkTags/Slots.
func (d *Document) SetRoleOverrides(sel provider.ModelKey, ov RoleOverrides, opts SetRoleOverridesOpts) error {
	return d.mutate(func(a *Config) error {
		if sel.Provider == "" || sel.Model == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: set role overrides: empty provider or model"))
		}
		if ov.Capabilities != nil && len(ov.Capabilities) == 0 {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: set role overrides: empty capabilities override (nil clears)"))
		}
		var matching []string
		for _, role := range sortedStringKeys(a.Models) {
			m := a.Models[role]
			providerName := m.Provider
			if providerName == "" {
				providerName = "ollama"
			}
			if providerName == sel.Provider && m.Name == sel.Model {
				matching = append(matching, role)
			}
		}
		if len(matching) == 0 {
			return diagWrap(CodeRoleNotFound, SubjectNone, "",
				fmt.Errorf("config: set role overrides: no role uses %s/%s", sel.Provider, sel.Model))
		}
		evalCaps, evalKnown := opts.Caps, opts.KnownMask
		if ov.Capabilities != nil {
			parsed, perr := provider.ParseCapsStrict(ov.Capabilities)
			if perr != nil {
				return diagWrap(CodeModelInvalid, SubjectRole, matching[0],
					fmt.Errorf("config: set role overrides: capabilities: %w", perr))
			}
			evalCaps, evalKnown = parsed, provider.CanonicalCaps()
		}
		gate := make(map[string]bool, len(matching))
		for _, r := range matching {
			gate[r] = true
		}
		verdict, reasons := aggregateVerdict(a, gate, opts.Requirements, evalCaps, evalKnown)
		switch verdict {
		case provider.CapIneligible:
			return diagWrap(CodeEligibilityIneligible, SubjectRole, matching[0],
				fmt.Errorf("config: set role overrides: %s/%s ineligible: %s",
					sel.Provider, sel.Model, strings.Join(reasons, ", ")))
		case provider.CapUnknown:
			if !opts.ConfirmUnknown {
				return diagWrap(CodeEligibilityUnknown, SubjectRole, matching[0],
					fmt.Errorf("config: set role overrides: %s/%s eligibility unknown (%s); confirm to proceed",
						sel.Provider, sel.Model, strings.Join(reasons, ", ")))
			}
		}
		for _, role := range matching {
			m := a.Models[role]
			if ov.Capabilities != nil {
				m.Capabilities = append([]string(nil), ov.Capabilities...)
			} else {
				m.Capabilities = nil
			}
			m.ThinkMode = ov.ThinkMode
			a.Models[role] = m
		}
		return nil
	})
}

// registerForkSeedLocked captures the source's raw continuity for the new
// role (spec §1). Runs as a mutateCommit hook: under d.mu, after the
// authored swap, only on success.
func (d *Document) registerForkSeedLocked(sourceRole, role string) {
	seed, ok := d.forkSeedSourceLocked(sourceRole)
	if !ok {
		return
	}
	if d.modelRawSeeds == nil {
		d.modelRawSeeds = make(map[string]json.RawMessage)
	}
	d.modelRawSeeds[role] = seed
}

// forkSeedSourceLocked resolves sourceRole's raw continuity: a pending
// seed (chained fork), else the rawBytes models entry unless the name is
// tombstoned (a role re-created after in-draft removal has no raw
// continuity). The result is always a copy, never an alias.
func (d *Document) forkSeedSourceLocked(sourceRole string) (json.RawMessage, bool) {
	if seed, ok := d.modelRawSeeds[sourceRole]; ok {
		return append(json.RawMessage(nil), seed...), true
	}
	if _, dropped := d.modelDrops[sourceRole]; dropped {
		return nil, false
	}
	var raw struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	// rawBytes is parse-valid by Document invariant. Mirror Config.clone's
	// bug backstop: never silently turn invariant corruption into a lossy fork.
	if err := json.Unmarshal(d.rawBytes, &raw); err != nil {
		panic(fmt.Sprintf("config: fork seed parse raw: %v", err))
	}
	entry, ok := raw.Models[sourceRole]
	if !ok {
		return nil, false
	}
	return append(json.RawMessage(nil), entry...), true
}
