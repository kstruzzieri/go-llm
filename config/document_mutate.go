package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
)

// mutate applies fn to a clone of the authored config, re-derives the
// effective view through finalize (the same gate Load applies), runs the
// optional post hook against the FINALIZED effective candidate, and commits
// both views only if every stage succeeds — all under d.mu, so evaluation
// and commit are one atomic step and no save can interleave (saves also
// hold d.mu end-to-end; spec amendment 6). Any error leaves the document
// unchanged. Draft-only: rawBytes/revision/origin never change here.
func (d *Document) mutate(fn func(authored *Config) error, post func(effective *Config) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	authored := d.authored.clone()
	if err := fn(authored); err != nil {
		return err
	}
	effective := authored.clone()
	if err := effective.finalize(); err != nil {
		return err
	}
	if post != nil {
		if err := post(effective); err != nil {
			return err
		}
	}
	d.authored = authored
	d.effective = effective
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
	// default/fallback chain contains the role (amendment 5); a missing
	// entry for an affected use case evaluates as unknown.
	Requirements map[string]provider.Capability
	// Caps/KnownMask: capability facts for the TARGET model. Ignored when
	// Capabilities is supplied — an explicit override IS the persisted
	// truth and is evaluated with full knowledge (amendment 2 + carve-down
	// rule).
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

// SetRoleModel points role at a different provider/model. Preservation:
// Fallbacks/Description/Options preserved (role-level intent); identity and
// capacity replaced from facts (empty = unknown, never stale);
// Capabilities, ThinkMode, ThinkTags, Slots CLEARED unless re-asserted —
// a stale override carried across models could falsely assert tool_call or
// violate bootstrap's per-selector invariants. The eligibility gate, the
// finalize gate, and the finalized selector-conflict check all run under
// the document lock; any failure leaves the document unchanged.
func (d *Document) SetRoleModel(role string, facts ModelFacts, opts SetRoleModelOpts) (SetRoleModelResult, error) {
	var zero SetRoleModelResult
	if facts.Key.Provider == "" || facts.Key.Model == "" {
		return zero, fmt.Errorf("config: set role model %q: empty provider or model", role)
	}
	if !validModelTypes[facts.Type] {
		return zero, fmt.Errorf("config: set role model %q: type must be one of dense, moe, embedding", role)
	}
	var res SetRoleModelResult
	err := d.mutate(func(a *Config) error {
		m, ok := a.Models[role]
		if !ok {
			return fmt.Errorf("config: set role model: role %q not defined", role)
		}
		if _, ok := a.Providers[facts.Key.Provider]; !ok {
			return fmt.Errorf("config: set role model %q: provider %q not configured", role, facts.Key.Provider)
		}

		// Gate inside the lock, against the pre-mutation graph (the chains
		// as the user saw them when choosing). Carve-down rule: an explicit
		// override is the persisted truth — evaluate it with full knowledge
		// (REPLACE semantics, omissions definitive). Facts otherwise.
		evalCaps, evalKnown := opts.Caps, opts.KnownMask
		if len(opts.Capabilities) > 0 {
			parsed, perr := provider.ParseCapsStrict(opts.Capabilities)
			if perr != nil {
				return fmt.Errorf("config: set role model %q: capabilities: %w", role, perr)
			}
			evalCaps, evalKnown = parsed, provider.CanonicalCaps()
		}
		verdict, reasons := aggregateVerdict(a, role, opts.Requirements, evalCaps, evalKnown)
		switch verdict {
		case provider.CapIneligible:
			return fmt.Errorf("config: set role model %q: target %s/%s ineligible: %s",
				role, facts.Key.Provider, facts.Key.Model, strings.Join(reasons, ", "))
		case provider.CapUnknown:
			if !opts.ConfirmUnknown {
				return fmt.Errorf("config: set role model %q: target %s/%s eligibility unknown (%s); confirm to proceed",
					role, facts.Key.Provider, facts.Key.Model, strings.Join(reasons, ", "))
			}
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
		a.Models[role] = m
		res = drops
		return nil
	}, func(effective *Config) error {
		return selectorConflicts(effective, role)
	})
	if err != nil {
		return zero, err
	}
	return res, nil
}

// aggregateVerdict computes the affected use-case set — every default whose
// fallback chain (walked over Fallbacks, cycle-guarded) contains role — and
// takes the worst per-use-case verdict from provider.EvaluateCaps, with a
// missing Requirements entry evaluating as req==0 (unknown). ZERO affected
// use cases is unknown/no_requirements (amendment 5): a role nothing
// references cannot be vacuously eligible. Reasons are prefixed
// "uc=<use case>: ", deduplicated, sorted, capped.
func aggregateVerdict(a *Config, role string, reqs map[string]provider.Capability, caps, known provider.Capability) (provider.CapVerdict, []string) {
	affected := make([]string, 0, len(a.Defaults))
	for _, uc := range sortedStringKeys(a.Defaults) {
		if chainContainsRole(a, a.Defaults[uc], role) {
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
			if len(item) > 64 {
				cut := 64
				for cut > 0 && !utf8.RuneStart(item[cut]) {
					cut--
				}
				item = item[:cut]
			}
			reasonSet[item] = true
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

// chainContainsRole walks the fallback graph from start, cycle-guarded.
func chainContainsRole(a *Config, start, role string) bool {
	seen := map[string]bool{}
	stack := []string{start}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if cur == role {
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

// selectorConflicts pre-flights the per-selector invariants bootstrap
// rejects at load (internal/providerbootstrap/capabilities.go: conflicting
// context_window, sampling defaults, capability overrides, think_mode,
// think_tags, slots) so a retarget cannot author a config bootstrap will
// refuse. It runs on the FINALIZED candidate — providers defaulted,
// think_mode normalized — because authored-level comparison would miss
// implicit-provider collisions. Advisory pre-flight only; bootstrap remains
// the authority. Maintained together with the bootstrap list.
func selectorConflicts(effective *Config, changed string) error {
	cm, ok := effective.Models[changed]
	if !ok {
		return nil
	}
	for _, other := range sortedStringKeys(effective.Models) {
		if other == changed {
			continue
		}
		om := effective.Models[other]
		if om.Provider != cm.Provider || om.Name != cm.Name {
			continue
		}
		sel := cm.Provider + "/" + cm.Name
		if cm.ContextWindow != 0 && om.ContextWindow != 0 && cm.ContextWindow != om.ContextWindow {
			return fmt.Errorf("config: conflicting context_window for %s: models %q and %q", sel, changed, other)
		}
		if cm.Options != nil && om.Options != nil && !reflect.DeepEqual(cm.Options, om.Options) {
			return fmt.Errorf("config: conflicting sampling defaults for %s: models %q and %q; defaults are per provider/model, so use identical options or distinct provider keys", sel, changed, other)
		}
		if len(cm.Capabilities) > 0 && len(om.Capabilities) > 0 {
			// Parse errors cannot occur here — validate round-trips every
			// Capabilities list through ParseCapsStrict before this hook
			// runs. Skipping on error (not failing) keeps the advisory
			// direction: bootstrap stays the authority.
			cb, cerr := provider.ParseCapsStrict(cm.Capabilities)
			ob, oerr := provider.ParseCapsStrict(om.Capabilities)
			if cerr == nil && oerr == nil && cb != ob {
				return fmt.Errorf("config: conflicting capability overrides for %s: models %q and %q", sel, changed, other)
			}
		}
		if cm.ThinkMode != "" && om.ThinkMode != "" && cm.ThinkMode != om.ThinkMode {
			return fmt.Errorf("config: conflicting think_mode for %s: models %q and %q", sel, changed, other)
		}
		if cm.ThinkTags != nil && om.ThinkTags != nil && *cm.ThinkTags != *om.ThinkTags {
			return fmt.Errorf("config: conflicting think_tags for %s: models %q and %q", sel, changed, other)
		}
		// Unreachable via SetRoleModel today (it always clears Slots and
		// finalize never populates it) — kept so the bootstrap mirror stays
		// complete for any future mutation that authors slots.
		if cm.Slots != 0 && om.Slots != 0 && cm.Slots != om.Slots {
			return fmt.Errorf("config: conflicting slots for %s: models %q and %q", sel, changed, other)
		}
	}
	return nil
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
	if useCase == "" || role == "" {
		return fmt.Errorf("config: bind use case: empty use case or role")
	}
	return d.mutate(func(a *Config) error {
		if _, ok := a.Models[role]; !ok {
			return fmt.Errorf("config: bind use case %q: role %q not defined", useCase, role)
		}
		if a.Defaults == nil {
			a.Defaults = map[string]string{}
		}
		a.Defaults[useCase] = role
		return nil
	}, nil)
}
