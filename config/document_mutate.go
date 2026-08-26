package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// mutate applies fn to a clone of the authored config, re-derives the
// effective view through finalize (the same gate Load applies), and commits
// both views only if every stage succeeds — all under d.mu, so evaluation
// and commit are one atomic step and no save can interleave (saves also
// hold d.mu end-to-end; spec amendment 6). Any error leaves the document
// unchanged. Draft-only: rawBytes/revision/origin never change here.
func (d *Document) mutate(fn func(authored *Config) error) error {
	return d.mutateCommit(fn, nil)
}

// mutateCommit is mutate with an optional post-swap hook running under
// d.mu — bookkeeping that must commit atomically with the authored swap
// (fork raw seeds) and must NOT run on any failure path. The hook must
// not call Document methods (d.mu is not reentrant).
func (d *Document) mutateCommit(fn func(authored *Config) error, commit func()) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.readOnlyErrLocked(); err != nil {
		return err
	}
	authored := d.authored.clone()
	if err := fn(authored); err != nil {
		return err
	}
	effective := authored.clone()
	if err := effective.finalizeEnv(d.env); err != nil {
		return err
	}
	for name := range d.authored.Providers {
		if _, ok := authored.Providers[name]; ok {
			continue
		}
		if d.providerDrops == nil {
			d.providerDrops = make(map[string]struct{})
		}
		d.providerDrops[name] = struct{}{}
	}
	for role := range d.authored.Models {
		if _, ok := authored.Models[role]; ok {
			continue
		}
		if d.modelDrops == nil {
			d.modelDrops = make(map[string]struct{})
		}
		d.modelDrops[role] = struct{}{}
		// A removed forked role must not be re-inserted at render.
		delete(d.modelRawSeeds, role)
	}
	d.authored = authored
	d.effective = effective
	if commit != nil {
		commit()
	}
	return nil
}

// ModelFacts is the neutral DTO a caller supplies for SetRoleModel —
// identity plus known capacity facts (from a configview inventory entry or
// hand-entered).
type ModelFacts struct {
	Key provider.ModelKey
	// Type is REQUIRED: dense | moe | embedding (spec amendment 1). The
	// panel sources it from config entries or asks the user; inventory
	// cannot supply it.
	Type string
	// Parameters is replace-or-clear: "" = unknown, persisted as absent.
	Parameters string
	// ContextWindow/Dimensions are replace semantics (>= 0, shared validate).
	ContextWindow int
	Dimensions    int
}

// SetRoleModelOpts carries the caller's declared call shapes, the target's
// capability facts, and optional explicit re-assertions.
type SetRoleModelOpts struct {
	// Requirements: per-use-case call shapes — the same map fed to
	// configview.BuildInput. Aggregated over every use case whose
	// default/fallback chain contains the role; when Capabilities changes
	// selector-wide truth, this includes roles already sharing the target
	// selector (amendment 5). A missing affected entry evaluates as unknown.
	Requirements map[string]provider.Capability
	// Caps/KnownMask: capability facts for the TARGET model. Ignored when
	// the target selector has an explicit override — that override IS the
	// persisted truth and is evaluated with full knowledge (amendment 2 +
	// carve-down rule).
	Caps, KnownMask provider.Capability
	ConfirmUnknown  bool
	// Explicit re-assertions on the NEW model; absent = cleared per the
	// preservation table. Both are deep-copied, never aliased.
	Capabilities []string
	ThinkMode    string
	ThinkTags    *ThinkTagsConfig
}

// SetRoleModelResult reports non-fatal outcomes. Zero value on any error.
type SetRoleModelResult struct {
	DroppedCapabilityOverride bool
	DroppedThinkMode          bool
	DroppedThinkTags          bool
	DroppedSlots              bool
	Verdict                   provider.CapVerdict
}

// validateRoleFacts applies the shared facts argument checks. op names the
// calling operation for pinned message compatibility.
func validateRoleFacts(op, role string, facts ModelFacts) error {
	if facts.Key.Provider == "" || facts.Key.Model == "" {
		return diagWrap(CodeInvalidArgument, SubjectNone, "",
			fmt.Errorf("config: %s %q: empty provider or model", op, role))
	}
	if !validModelTypes[facts.Type] {
		return diagWrap(CodeInvalidArgument, SubjectNone, "",
			fmt.Errorf("config: %s %q: type must be one of dense, moe, embedding", op, role))
	}
	return nil
}

// gateRoleEligibility applies the SetRoleModel eligibility semantics for
// role joining facts.Key's selector, gating inside the lock against the
// pre-mutation graph (the chains as the user saw them when choosing).
// Carve-down rule: an explicit override is selector-wide persisted truth
// (REPLACE semantics, omissions definitive). An inherited override governs
// the joining role; a supplied override also changes existing selector
// roles, so evaluate all of them. Facts otherwise. Verdict aggregation runs
// over affected use cases with zero-affected = unknown/no_requirements. On
// success it returns the verdict (CapEligible or the confirmed CapUnknown)
// for SetRoleModelResult. Moved verbatim from SetRoleModel; shared by
// AddRoleModel and ForkRoleModel (spec checkbox 2).
func gateRoleEligibility(a *Config, op, role string, facts ModelFacts, opts SetRoleModelOpts) (provider.CapVerdict, error) {
	gateRoles := map[string]bool{role: true}
	selectorRoles := map[string]bool{role: true}
	var inheritedCaps provider.Capability
	var inheritedRole string
	for _, siblingRole := range sortedStringKeys(a.Models) {
		if siblingRole == role {
			continue
		}
		sibling := a.Models[siblingRole]
		providerName := sibling.Provider
		if providerName == "" {
			providerName = "ollama"
		}
		if providerName != facts.Key.Provider || sibling.Name != facts.Key.Model {
			continue
		}
		selectorRoles[siblingRole] = true
		if len(opts.Capabilities) == 0 && len(sibling.Capabilities) > 0 {
			parsed, perr := provider.ParseCapsStrict(sibling.Capabilities)
			if perr != nil {
				return "", diagWrap(CodeModelInvalid, SubjectRole, role,
					fmt.Errorf("config: %s %q: capabilities: %w", op, role, perr))
			}
			// Siblings cannot disagree here: validate's static selector
			// check rejects any document holding conflicting capability
			// overrides on one selector. Retain the first.
			if inheritedRole == "" {
				inheritedCaps, inheritedRole = parsed, siblingRole
			}
		}
	}
	evalCaps, evalKnown := opts.Caps, opts.KnownMask
	if len(opts.Capabilities) > 0 {
		parsed, perr := provider.ParseCapsStrict(opts.Capabilities)
		if perr != nil {
			return "", diagWrap(CodeModelInvalid, SubjectRole, role,
				fmt.Errorf("config: %s %q: capabilities: %w", op, role, perr))
		}
		evalCaps, evalKnown = parsed, provider.CanonicalCaps()
		gateRoles = selectorRoles
	} else if inheritedRole != "" {
		evalCaps, evalKnown = inheritedCaps, provider.CanonicalCaps()
	}
	verdict, reasons := aggregateVerdict(a, gateRoles, opts.Requirements, evalCaps, evalKnown)
	switch verdict {
	case provider.CapIneligible:
		return "", diagWrap(CodeEligibilityIneligible, SubjectRole, role,
			fmt.Errorf("config: %s %q: target %s/%s ineligible: %s",
				op, role, facts.Key.Provider, facts.Key.Model, strings.Join(reasons, ", ")))
	case provider.CapUnknown:
		if !opts.ConfirmUnknown {
			return "", diagWrap(CodeEligibilityUnknown, SubjectRole, role,
				fmt.Errorf("config: %s %q: target %s/%s eligibility unknown (%s); confirm to proceed",
					op, role, facts.Key.Provider, facts.Key.Model, strings.Join(reasons, ", ")))
		}
	}
	return verdict, nil
}

// applyRoleOverridesFromOpts installs the deep-copied re-assertions per the
// cleared-unless-re-asserted preservation table; Slots is always cleared.
func applyRoleOverridesFromOpts(m *ModelConfig, opts SetRoleModelOpts) {
	if len(opts.Capabilities) > 0 {
		m.Capabilities = append([]string(nil), opts.Capabilities...)
	} else {
		m.Capabilities = nil
	}
	m.ThinkMode = opts.ThinkMode
	if opts.ThinkTags != nil {
		tt := *opts.ThinkTags
		m.ThinkTags = &tt
	} else {
		m.ThinkTags = nil
	}
	m.Slots = 0
}

// SetRoleModel points role at a different provider/model. Preservation:
// Fallbacks/Description/Options preserved (role-level intent); identity and
// capacity replaced from facts (empty = unknown, never stale);
// Capabilities, ThinkMode, ThinkTags, Slots CLEARED unless re-asserted —
// a stale override carried across models could falsely assert tool_call or
// violate the per-selector invariants. The eligibility gate and the
// finalize gate (whose validate includes the static selector-conflict
// check) run under the document lock; any failure leaves the document
// unchanged.
func (d *Document) SetRoleModel(role string, facts ModelFacts, opts SetRoleModelOpts) (SetRoleModelResult, error) {
	var zero SetRoleModelResult
	var res SetRoleModelResult
	err := d.mutate(func(a *Config) error {
		// Argument checks live inside the closure so the central read-only
		// gate wins even for invalid requests; order for valid documents is
		// unchanged (args first, then role, then provider).
		if err := validateRoleFacts("set role model", role, facts); err != nil {
			return err
		}
		m, ok := a.Models[role]
		if !ok {
			return diagWrap(CodeRoleNotFound, SubjectRole, role,
				fmt.Errorf("config: set role model: role %q not defined", role))
		}
		if _, ok := a.Providers[facts.Key.Provider]; !ok {
			return diagWrap(CodeProviderNotFound, SubjectProvider, facts.Key.Provider,
				fmt.Errorf("config: set role model %q: provider %q not configured", role, facts.Key.Provider))
		}

		verdict, gerr := gateRoleEligibility(a, "set role model", role, facts, opts)
		if gerr != nil {
			return gerr
		}

		drops := SetRoleModelResult{
			DroppedCapabilityOverride: len(m.Capabilities) > 0 && len(opts.Capabilities) == 0,
			DroppedThinkMode:          m.ThinkMode != "" && opts.ThinkMode == "",
			DroppedThinkTags:          m.ThinkTags != nil && opts.ThinkTags == nil,
			DroppedSlots:              m.Slots != 0,
			Verdict:                   verdict,
		}

		m.Name, m.Provider = facts.Key.Model, facts.Key.Provider
		m.Type = facts.Type
		m.Parameters = facts.Parameters
		m.ContextWindow = facts.ContextWindow
		m.Dimensions = facts.Dimensions
		applyRoleOverridesFromOpts(&m, opts)
		a.Models[role] = m
		res = drops
		return nil
	})
	if err != nil {
		return zero, err
	}
	return res, nil
}

// aggregateVerdict computes the affected use-case set — every default whose
// fallback chain (walked over Fallbacks, cycle-guarded) contains any role — and
// takes the worst per-use-case verdict from provider.EvaluateCaps, with a
// missing Requirements entry evaluating as req==0 (unknown). ZERO affected
// use cases is unknown/no_requirements (amendment 5): unreferenced roles
// cannot be vacuously eligible. Reasons are prefixed
// "uc=<use case>: ", deduplicated, sorted, capped.
func aggregateVerdict(a *Config, roles map[string]bool, reqs map[string]provider.Capability, caps, known provider.Capability) (provider.CapVerdict, []string) {
	affected := make([]string, 0, len(a.Defaults))
	for _, uc := range sortedStringKeys(a.Defaults) {
		if chainContainsAnyRole(a, a.Defaults[uc], roles) {
			affected = append(affected, uc)
		}
	}
	if len(affected) == 0 {
		return provider.CapUnknown, []string{"no_requirements"}
	}
	verdict := provider.CapEligible
	reasonSet := map[string]bool{}
	for _, uc := range affected {
		v, reasons := provider.EvaluateCaps(reqs[uc], caps, known)
		switch v {
		case provider.CapIneligible:
			verdict = provider.CapIneligible
		case provider.CapUnknown:
			if verdict != provider.CapIneligible {
				verdict = provider.CapUnknown
			}
		}
		for _, r := range reasons {
			item := "uc=" + uc + ": " + r
			// Bounds (16 items, 64 bytes each) mirror provider.maxReasons
			// and its item discipline; keep in lockstep. Cut on a rune
			// boundary — use-case keys are user-authored and may be
			// multi-byte.
			reasonSet[truncateRuneSafe64(item)] = true
		}
	}
	if verdict == provider.CapEligible {
		return provider.CapEligible, nil
	}
	out := make([]string, 0, len(reasonSet))
	for r := range reasonSet {
		out = append(out, r)
	}
	sort.Strings(out)
	if len(out) > 16 {
		out = out[:16]
	}
	return verdict, out
}

// chainContainsAnyRole walks the fallback graph from start, cycle-guarded.
func chainContainsAnyRole(a *Config, start string, roles map[string]bool) bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if roles[cur] {
			return true
		}
		m, ok := a.Models[cur]
		if !ok {
			continue
		}
		stack = append(stack, m.Fallbacks...)
	}
	return false
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// BindUseCase points a use case at an EXISTING role (edits defaults). Role
// existence is checked explicitly; chain validity rides the finalize gate.
func (d *Document) BindUseCase(useCase, role string) error {
	return d.mutate(func(a *Config) error {
		// Inside the closure so the central read-only gate wins for invalid
		// requests too.
		if useCase == "" || role == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: bind use case: empty use case or role"))
		}
		if _, ok := a.Models[role]; !ok {
			return diagWrap(CodeRoleNotFound, SubjectRole, role,
				fmt.Errorf("config: bind use case %q: role %q not defined", useCase, role))
		}
		if a.Defaults == nil {
			a.Defaults = map[string]string{}
		}
		a.Defaults[useCase] = role
		return nil
	})
}
