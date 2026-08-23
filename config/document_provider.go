package config

import "fmt"

// ProviderSpec is the api_key-free authored provider value used by Add and
// Update (spec §2). There is no field for the dedicated API key; key entry
// is single-pathed through SetProviderAPIKey. BaseURL stays backend-only in
// consumers because userinfo/query data may itself be sensitive.
type ProviderSpec struct {
	BaseURL       string
	Timeout       Duration // zero = provider default (5m) per existing semantics
	APIFormat     string   // "" = default ("ollama")
	SlotDiscovery bool
}

// AddProvider adds a new provider entry. The full finalize gate (including
// static slot policy) validates the candidate; failure leaves the document
// unchanged.
func (d *Document) AddProvider(name string, p ProviderSpec) error {
	return d.mutate(func(a *Config) error {
		// Keep argument validation inside mutate so the central read-only
		// guard always wins on an ambiguous document.
		if err := ValidateProviderName(name); err != nil {
			return diagWrap(CodeProviderNameInvalid, SubjectProvider, name,
				fmt.Errorf("config: %w", err))
		}
		// Duration's non-negative invariant otherwise only lives in
		// UnmarshalJSON, where the mutate clone round-trip turns it into a
		// process panic.
		if p.Timeout.Duration < 0 {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: provider timeout must not be negative"))
		}
		if _, exists := a.Providers[name]; exists {
			return diagWrap(CodeProviderExists, SubjectProvider, name,
				fmt.Errorf("config: add provider: %q already configured", name))
		}
		if a.Providers == nil {
			a.Providers = map[string]ProviderConfig{}
		}
		a.Providers[name] = ProviderConfig{
			BaseURL: p.BaseURL, Timeout: p.Timeout,
			APIFormat: p.APIFormat, SlotDiscovery: p.SlotDiscovery,
		}
		return nil
	})
}

// UpdateProvider replaces the four non-api_key fields wholesale (form
// semantics — no patch language). It preserves the existing authored APIKey
// by updating the current value in place. Callers needing the current
// authored base use AuthoredProvider and overlay their edits.
func (d *Document) UpdateProvider(name string, p ProviderSpec) error {
	return d.mutate(func(a *Config) error {
		if p.Timeout.Duration < 0 {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: provider timeout must not be negative"))
		}
		cur, ok := a.Providers[name]
		if !ok {
			return providerNotFoundErr("update provider", name)
		}
		cur.BaseURL, cur.Timeout = p.BaseURL, p.Timeout
		cur.APIFormat, cur.SlotDiscovery = p.APIFormat, p.SlotDiscovery
		a.Providers[name] = cur
		return nil
	})
}

// RemoveProvider deletes a provider no model references. The explicit
// in-use pre-check names the first referencing role in sorted order; the
// finalize gate (>= 1 provider, model references) remains the authority.
func (d *Document) RemoveProvider(name string) error {
	return d.mutate(func(a *Config) error {
		if _, ok := a.Providers[name]; !ok {
			return providerNotFoundErr("remove provider", name)
		}
		for _, role := range sortedStringKeys(a.Models) {
			providerName := a.Models[role].Provider
			if providerName == "" {
				providerName = "ollama"
			}
			if providerName == name {
				return diagWrap(CodeProviderInUse, SubjectRole, role,
					fmt.Errorf("config: remove provider %q: still referenced by model %q", name, role))
			}
		}
		delete(a.Providers, name)
		return nil
	})
}

// AuthoredProvider returns an api_key-free copy of the AUTHORED provider
// fields — no default materialization, no expansion. Works on read-only
// documents. The result is backend-only: BaseURL may carry sensitive
// userinfo/query data and must not be projected or logged.
func (d *Document) AuthoredProvider(name string) (ProviderSpec, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cur, ok := d.authored.Providers[name]
	if !ok {
		return ProviderSpec{}, providerNotFoundErr("authored provider", name)
	}
	return ProviderSpec{
		BaseURL: cur.BaseURL, Timeout: cur.Timeout,
		APIFormat: cur.APIFormat, SlotDiscovery: cur.SlotDiscovery,
	}, nil
}

// SetProviderAPIKey stores value verbatim as the provider's authored
// api_key: a raw literal or a ${ENV} template (every ${NAME} occurrence is
// a template; raw literals containing "${" are not representable). The
// finalize gate validates templates against the document's environment
// lookup; failure leaves the document unchanged. An empty value is
// rejected — clearing is a distinct operation, ClearProviderAPIKey.
// Rejections carry CodeInvalidArgument, CodeKeyReferenceMalformed, or
// CodeKeyReferenceUnavailable, and rejection errors never echo the
// submitted value, so they are safe to surface and log.
func (d *Document) SetProviderAPIKey(name, value string) error {
	return d.mutate(func(a *Config) error {
		if value == "" {
			return diagWrap(CodeInvalidArgument, SubjectNone, "",
				fmt.Errorf("config: set provider api key: empty value (use ClearProviderAPIKey)"))
		}
		cur, ok := a.Providers[name]
		if !ok {
			return providerNotFoundErr("set provider api key", name)
		}
		cur.APIKey = value
		a.Providers[name] = cur
		return nil
	})
}

// ClearProviderAPIKey removes the provider's stored key. Deliberately
// separate from Set: an accidentally blank write-only field can never
// erase a stored credential. Clearing a provider that holds no key
// succeeds — consumers cannot read key presence, so Clear must be
// unconditional.
func (d *Document) ClearProviderAPIKey(name string) error {
	return d.mutate(func(a *Config) error {
		cur, ok := a.Providers[name]
		if !ok {
			return providerNotFoundErr("clear provider api key", name)
		}
		cur.APIKey = ""
		a.Providers[name] = cur
		return nil
	})
}

func providerNotFoundErr(op, name string) error {
	return diagWrap(CodeProviderNotFound, SubjectProvider, name,
		fmt.Errorf("config: %s: provider %q not configured", op, name))
}
