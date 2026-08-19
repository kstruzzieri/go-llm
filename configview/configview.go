// Package configview builds a pure, projection-safe snapshot of a
// configuration for panels, CLIs, and MCP resources. Build performs no I/O,
// reads no clocks, and mutates nothing; all knowledge arrives via BuildInput.
//
// The JSON field names on the types below ARE the versioned wire contract
// (v1) — the golden test pins them; changing any is a contract break
// requiring a v2 resource.
package configview

import (
	"net/url"
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// Eligibility is the tri-state verdict for a candidate against a use case's
// declared capability requirements. Unknown is honest absence of knowledge —
// it must never be collapsed into ineligible.
type Eligibility string

const (
	EligibilityEligible   Eligibility = "eligible"
	EligibilityIneligible Eligibility = "ineligible"
	EligibilityUnknown    Eligibility = "unknown"
)

// DocSnapshot is the document-side input to Build, extracted by the caller
// from a config.Document (or assembled programmatically).
type DocSnapshot struct {
	Config   *config.Config
	Origin   config.Origin
	Revision string
}

// InventoryModel is one discovered model. KnownMask carries the bits that
// have a definitive answer; a set Caps bit always counts as known.
type InventoryModel struct {
	Key           provider.ModelKey   `json:"key"`
	Family        string              `json:"family,omitempty"`
	Caps          provider.Capability `json:"caps"`
	KnownMask     provider.Capability `json:"known_mask"`
	ProfileSource string              `json:"profile_source,omitempty"`
	ContextWindow int                 `json:"context_window,omitempty"`
}

// InventoryProvider reports provider reachability from an explicit refresh.
type InventoryProvider struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
}

// Inventory is a plain serializable value produced by an EXPLICIT refresh
// (spec slice 4) or a consumer-side adapter. The zero value renders a
// configured-only view: resolution and unprobed capabilities stay unknown.
type Inventory struct {
	Models    []InventoryModel    `json:"models,omitempty"`
	Providers []InventoryProvider `json:"providers,omitempty"`
}

// ProfileState is caller-owned profile bookkeeping (slice 3) passed through
// verbatim: Build never derives dirtiness or identity itself.
type ProfileState struct {
	ActiveID   string `json:"active_id,omitempty"`
	BaselineID string `json:"baseline_id,omitempty"`
	Dirty      bool   `json:"dirty"`
	Revision   string `json:"revision,omitempty"`
}

// BuildInput carries everything Build is allowed to know. Requirements maps
// use case → required capability bits, DECLARED BY THE CONSUMER (call shapes
// are consumer-specific); a use case absent from the map projects as unknown.
type BuildInput struct {
	Doc          DocSnapshot
	Inventory    Inventory
	Requirements map[string]provider.Capability
	Profile      ProfileState
}

// Candidate is one selectable model for a use case, with its tri-state
// eligibility and bounded reason codes.
type Candidate struct {
	Selector    string      `json:"selector"`
	Eligibility Eligibility `json:"eligibility"`
	Reasons     []string    `json:"reasons,omitempty"`
}

// RoleBinding is one use-case row: the configured role, its fallback chain,
// the resolution verdict, and the candidate list for a picker.
type RoleBinding struct {
	UseCase         string      `json:"use_case"`
	Role            string      `json:"role"`
	Resolved        string      `json:"resolved,omitempty"`
	ResolutionState string      `json:"resolution_state"` // resolved | unknown | none-eligible
	IsFallback      bool        `json:"is_fallback"`
	Chain           []string    `json:"chain"`
	Candidates      []Candidate `json:"candidates,omitempty"`
	Source          string      `json:"source"` // config-default | fallback | draft-edit
}

// ModelSummary is one model card. Type is "" when config roles disagree on
// the selector's type (a selector_type_conflict diagnostic is emitted).
type ModelSummary struct {
	Selector      string   `json:"selector"`
	Provider      string   `json:"provider"`
	Type          string   `json:"type,omitempty"`
	Family        string   `json:"family,omitempty"`
	Caps          []string `json:"caps,omitempty"`
	ProfileSource string   `json:"profile_source,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
}

// ProviderSummary is one provider card. Endpoint is normalized to
// scheme://host[:port] — never userinfo, path, query, or fragment. HasKey is
// presence only; the key value never crosses this boundary.
type ProviderSummary struct {
	Name          string `json:"name"`
	Endpoint      string `json:"endpoint,omitempty"`
	APIFormat     string `json:"api_format,omitempty"`
	HasKey        bool   `json:"has_key"`
	SlotDiscovery bool   `json:"slot_discovery"`
	Reachable     *bool  `json:"reachable,omitempty"` // null = no inventory data
}

// Diagnostic carries a FIXED code plus a bounded subject (use case, selector,
// or provider name; hard-capped at 64 bytes). Codes: config_missing,
// chain_invalid, selector_type_conflict.
type Diagnostic struct {
	Code    string `json:"code"`
	Subject string `json:"subject,omitempty"`
}

// OriginProjection carries the origin's wire enum — never a path.
type OriginProjection struct {
	Source string `json:"source"`
}

// Snapshot is the v1 wire contract root.
type Snapshot struct {
	Ready       bool              `json:"ready"`
	Diagnostics []Diagnostic      `json:"diagnostics,omitempty"`
	Origin      OriginProjection  `json:"origin"`
	Bindings    []RoleBinding     `json:"bindings,omitempty"`
	Models      []ModelSummary    `json:"models,omitempty"`
	Providers   []ProviderSummary `json:"providers,omitempty"`
	Profile     ProfileState      `json:"profile"`
}

// originWire maps config origin sources onto the v1 wire enum. Exhaustive;
// anything unrecognized projects as "unknown" (never the raw value).
func originWire(src config.OriginSource) string {
	switch src {
	case config.OriginEnvOverride:
		return "env_override"
	case config.OriginWorkingDir:
		return "working_dir"
	case config.OriginUserConfig:
		return "user_config"
	case config.OriginLegacy:
		return "legacy"
	case config.OriginExplicit:
		return "explicit_path"
	case config.OriginProfile:
		return "profile"
	case config.OriginProgrammatic:
		return "programmatic"
	default:
		return "unknown"
	}
}

// Build produces the v1 snapshot. Pure: no I/O, no clocks, no mutation of in.
func Build(in BuildInput) Snapshot {
	s := Snapshot{
		Profile: in.Profile,
		Origin:  OriginProjection{Source: originWire(in.Doc.Origin.Source)},
	}
	if in.Doc.Config == nil {
		s.Diagnostics = append(s.Diagnostics, Diagnostic{Code: "config_missing"})
		return s
	}
	cfg := in.Doc.Config
	s.Ready = true

	invModels := make(map[string]InventoryModel, len(in.Inventory.Models))
	for _, m := range in.Inventory.Models {
		invModels[m.Key.Provider+"/"+m.Key.Model] = m
	}
	reach := make(map[string]bool, len(in.Inventory.Providers))
	for _, p := range in.Inventory.Providers {
		reach[p.Name] = p.Reachable
	}
	haveInventory := len(in.Inventory.Models) > 0 || len(in.Inventory.Providers) > 0

	selectors, types, typeConflicts := collectSelectors(cfg, invModels)
	for _, sub := range typeConflicts {
		s.Diagnostics = append(s.Diagnostics, Diagnostic{Code: "selector_type_conflict", Subject: bound64(sub)})
	}

	for _, uc := range sortedKeys(cfg.Defaults) {
		role := cfg.Defaults[uc]
		chain, err := cfg.RoleFallbackChain(uc)
		if err != nil {
			s.Diagnostics = append(s.Diagnostics, Diagnostic{Code: "chain_invalid", Subject: bound64(uc)})
			continue
		}
		b := RoleBinding{UseCase: uc, Role: role, Chain: chain, Source: "config-default"}
		b.Resolved, b.IsFallback, b.ResolutionState = resolveChain(chain, invModels, reach, haveInventory)
		req := in.Requirements[uc]
		for _, sel := range selectors {
			caps, known := capsFor(sel, cfg, invModels)
			el, reasons := eligibilityFor(req, caps, known)
			b.Candidates = append(b.Candidates, Candidate{Selector: sel, Eligibility: el, Reasons: reasons})
		}
		s.Bindings = append(s.Bindings, b)
	}

	s.Models = buildModelSummaries(cfg, invModels, selectors, types)

	for _, name := range sortedKeys(cfg.Providers) {
		p := cfg.Providers[name]
		ps := ProviderSummary{
			Name:          name,
			Endpoint:      normalizeEndpoint(p.BaseURL),
			APIFormat:     p.APIFormat,
			HasKey:        p.APIKey != "",
			SlotDiscovery: p.SlotDiscovery,
		}
		if r, ok := reach[name]; ok {
			r := r
			ps.Reachable = &r
		}
		s.Providers = append(s.Providers, ps)
	}
	return s
}

// resolveChain: no inventory at all → ("", false, "unknown") — Build refuses
// to claim resolution it cannot know. Otherwise the first chain selector
// present in inventory whose provider is not known-unreachable wins;
// none → ("", false, "none-eligible").
func resolveChain(chain []string, inv map[string]InventoryModel, reach map[string]bool, haveInventory bool) (string, bool, string) {
	if !haveInventory {
		return "", false, "unknown"
	}
	for i, sel := range chain {
		m, ok := inv[sel]
		if !ok {
			continue
		}
		if r, known := reach[m.Key.Provider]; known && !r {
			continue
		}
		return sel, i > 0, "resolved"
	}
	return "", false, "none-eligible"
}

// collectSelectors returns the sorted union of config-declared and inventory
// selectors, each selector's config-declared type, and the selectors whose
// config roles disagree on a non-empty type.
func collectSelectors(cfg *config.Config, inv map[string]InventoryModel) ([]string, map[string]string, []string) {
	seen := map[string]bool{}
	types := map[string]string{}
	conflictSet := map[string]bool{}
	for _, role := range sortedKeys(cfg.Models) {
		m := cfg.Models[role]
		sel := m.Provider + "/" + m.Name
		seen[sel] = true
		if m.Type != "" {
			if prev, ok := types[sel]; ok && prev != "" && prev != m.Type {
				conflictSet[sel] = true
				types[sel] = ""
			} else if !conflictSet[sel] {
				types[sel] = m.Type
			}
		}
	}
	for sel := range inv {
		seen[sel] = true
	}
	selectors := make([]string, 0, len(seen))
	for sel := range seen {
		selectors = append(selectors, sel)
	}
	sort.Strings(selectors)
	conflicts := make([]string, 0, len(conflictSet))
	for sel := range conflictSet {
		conflicts = append(conflicts, sel)
	}
	sort.Strings(conflicts)
	return selectors, types, conflicts
}

// buildModelSummaries renders one card per selector, sorted. Inventory wins
// on family/source/context; caps use the same config authority as eligibility.
func buildModelSummaries(cfg *config.Config, inv map[string]InventoryModel, selectors []string, types map[string]string) []ModelSummary {
	out := make([]ModelSummary, 0, len(selectors))
	for _, sel := range selectors {
		caps, _ := capsFor(sel, cfg, inv)
		ms := ModelSummary{Selector: sel, Type: types[sel], Caps: caps.Names()}
		if m, ok := inv[sel]; ok {
			ms.Provider = m.Key.Provider
			ms.Family = m.Family
			ms.ProfileSource = m.ProfileSource
			ms.ContextWindow = m.ContextWindow
		} else {
			ms.Provider = selectorProvider(sel)
			ms.ProfileSource = "config"
			if mc, ok := configModelBySelector(cfg, sel); ok {
				ms.ContextWindow = mc.ContextWindow
			}
		}
		out = append(out, ms)
	}
	return out
}

// normalizeEndpoint keeps scheme://host[:port] only — no userinfo, path,
// query, or fragment. Unparseable input yields "" (never a raw echo).
func normalizeEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// bound64 truncates a diagnostic subject to 64 bytes.
func bound64(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

func selectorProvider(sel string) string {
	for i := 0; i < len(sel); i++ {
		if sel[i] == '/' {
			return sel[:i]
		}
	}
	return ""
}
