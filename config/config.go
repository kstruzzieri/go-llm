// Package config loads and validates model configuration from models.json,
// providing model name resolution, provider lookups, and fallback chain walking.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Duration wraps time.Duration with JSON marshal/unmarshal support.
// It expects JSON strings in Go duration format (e.g. "5m", "30s", "1h30m").
type Duration struct {
	time.Duration
}

// UnmarshalJSON parses a quoted string as a Go duration.
// Returns an error for empty strings, non-string values, or invalid duration formats.
func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("config: duration must be a string: %w", err)
	}
	if s == "" {
		return fmt.Errorf("config: duration must not be empty")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", s, err)
	}
	if parsed < 0 {
		return fmt.Errorf("config: duration must not be negative: %q", s)
	}
	d.Duration = parsed
	return nil
}

// MarshalJSON returns the duration as a quoted string (e.g. "5m0s").
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// ProviderConfig holds connection settings for an LLM provider (e.g. Ollama, vLLM).
type ProviderConfig struct {
	BaseURL   string   `json:"base_url"`
	Timeout   Duration `json:"timeout"`
	APIKey    string   `json:"api_key,omitempty"`
	APIFormat string   `json:"api_format,omitempty"`
}

// ModelConfig describes a model's identity, capabilities, and fallback chain.
//
// Capabilities is an optional explicit override of the type-derived default
// capability set. When non-empty it REPLACES the derived set (not merges)
// so users can carve down capabilities for backends that don't expose every
// endpoint — e.g. ["chat", "stream"] for an OpenAI-compat server lacking
// /v1/completions. When empty, capabilities derive from Type per
// ResolvedCapabilities. See validCapabilityNames for the accepted vocabulary.
type ModelConfig struct {
	Name          string   `json:"name"`
	Provider      string   `json:"provider,omitempty"`
	Description   string   `json:"description,omitempty"`
	Type          string   `json:"type"`
	Parameters    string   `json:"parameters,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	Dimensions    int      `json:"dimensions,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Fallbacks     []string `json:"fallbacks,omitempty"`
}

// Config is the top-level configuration loaded from models.json.
type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
	Defaults  map[string]string         `json:"defaults"`
}

// ErrConfigNotFound indicates that Default could not find a models.json file
// in any of its discovery locations.
var ErrConfigNotFound = errors.New("config: no configuration file found")

// validModelTypes enumerates the allowed values for ModelConfig.Type.
var validModelTypes = map[string]bool{
	"dense":     true,
	"moe":       true,
	"embedding": true,
}

// validAPIFormats enumerates the allowed values for ProviderConfig.APIFormat.
// Empty is also allowed and is materialized to "ollama" by applyDefaults so
// existing configs that predate the field continue to load unchanged.
var validAPIFormats = map[string]bool{
	"ollama":        true,
	"openai-compat": true,
}

// validCapabilityNames is the schema vocabulary for ModelConfig.Capabilities,
// derived from provider.CanonicalCapabilityNames so the two never drift.
// User config accepts ONLY canonical single-bit names; catalog aliases like
// "completion" or "tools" that expand multiple bits are intentionally
// forbidden here so the "explicit Capabilities REPLACE derived" contract
// holds without surprise expansion at the override merge.
var validCapabilityNames = func() map[string]bool {
	m := make(map[string]bool, len(provider.CanonicalCapabilityNames))
	for _, name := range provider.CanonicalCapabilityNames {
		m[name] = true
	}
	return m
}()

// embeddingOnlyCapabilities are the capability tokens permitted on a model
// with Type=="embedding". Hybrid models that need additional capabilities
// must be configured as "dense" or "moe" with explicit capabilities; see
// the ModelConfig doc.
var embeddingOnlyCapabilities = map[string]bool{
	"embed": true,
}

// derivedCapabilitiesByType is the default capability vocabulary applied
// when ModelConfig.Capabilities is empty. Insert, tool_call, and thinking
// are never derived — they require explicit declaration or later runtime
// proof from the provider.
var derivedCapabilitiesByType = map[string][]string{
	"dense":     {"chat", "generate", "stream"},
	"moe":       {"chat", "generate", "stream"},
	"embedding": {"embed"},
}

// ResolvedCapabilities returns the effective capability tokens for this
// model. Explicit Capabilities entries REPLACE the type-derived default set
// (not merge); an empty list yields the derive-from-type defaults. The
// returned slice is a fresh copy callers may freely mutate.
func (m ModelConfig) ResolvedCapabilities() []string {
	if len(m.Capabilities) > 0 {
		out := make([]string, len(m.Capabilities))
		copy(out, m.Capabilities)
		return out
	}
	derived := derivedCapabilitiesByType[m.Type]
	if len(derived) == 0 {
		return nil
	}
	out := make([]string, len(derived))
	copy(out, derived)
	return out
}

// typeCompatible reports whether a model of fromType can fall back to a model of toType.
// Embedding models can only fall back to other embedding models.
// Dense and MoE models are interchangeable as fallbacks.
func typeCompatible(fromType, toType string) bool {
	if !validModelTypes[fromType] || !validModelTypes[toType] {
		return false
	}
	if fromType == "embedding" || toType == "embedding" {
		return fromType == "embedding" && toType == "embedding"
	}
	// dense and moe are mutually compatible
	return true
}

// Provider returns a pointer to the ProviderConfig for the given key, or nil if not found.
func (c *Config) Provider(key string) *ProviderConfig {
	p, ok := c.Providers[key]
	if !ok {
		return nil
	}
	return &p
}

// RoleConfig returns a pointer to the ModelConfig for the given role, or nil if not found.
func (c *Config) RoleConfig(role string) *ModelConfig {
	m, ok := c.Models[role]
	if !ok {
		return nil
	}
	return &m
}

// ModelFor resolves a use-case to a model name through the defaults chain.
// It looks up useCase in Defaults to find the role, then looks up that role in Models
// to return the model Name. Returns "" if the use-case or its target role is not found.
func (c *Config) ModelFor(useCase string) string {
	role, ok := c.Defaults[useCase]
	if !ok {
		return ""
	}
	m, ok := c.Models[role]
	if !ok {
		return ""
	}
	return m.Name
}

// MustModelFor is like ModelFor but panics if the use-case cannot be resolved.
func (c *Config) MustModelFor(useCase string) string {
	name := c.ModelFor(useCase)
	if name == "" {
		panic(fmt.Sprintf("config: no model for use-case %q", useCase))
	}
	return name
}

// ProviderFor returns the provider config for a given role's model.
// It looks up the role in Models to find the Provider field, then returns
// the corresponding ProviderConfig. Returns nil if the role does not exist,
// the model has no Provider set (programmatic Config that bypassed
// applyDefaults), or the named provider is not present in cfg.Providers.
//
// This mirrors resolveRole's contract — empty Provider is not silently
// defaulted to "ollama". Load+applyDefaults guarantees a non-empty Provider
// for every model loaded from models.json; reaching this method with empty
// Provider means the caller constructed Config programmatically and
// skipped applyDefaults, in which case lying about the owner would
// misdirect downstream callers.
func (c *Config) ProviderFor(role string) *ProviderConfig {
	m, ok := c.Models[role]
	if !ok {
		return nil
	}
	if m.Provider == "" {
		return nil
	}
	return c.Provider(m.Provider)
}

// Default discovers and loads the configuration file from standard locations.
// It searches in order:
//  1. $GO_LLM_CONFIG environment variable (absolute path)
//  2. ./models.json (current working directory)
//  3. ~/.config/go-llm/models.json (user config directory)
//
// Returns a descriptive error if no configuration file is found at any location.
func Default() (*Config, error) {
	// 1. $GO_LLM_CONFIG env var (if set and non-empty).
	if envPath, ok := os.LookupEnv("GO_LLM_CONFIG"); ok {
		if envPath == "" {
			return nil, fmt.Errorf("config: GO_LLM_CONFIG is set but empty")
		}
		return Load(envPath)
	}

	// 2. ./models.json in current working directory.
	if _, err := os.Stat("models.json"); err == nil {
		return Load("models.json")
	}

	// 3. Platform-standard user config directory (e.g., ~/.config on Linux,
	// ~/Library/Application Support on macOS, %AppData% on Windows).
	configDir, configDirErr := os.UserConfigDir()
	if configDirErr == nil {
		configPath := filepath.Join(configDir, "go-llm", "models.json")
		if _, err := os.Stat(configPath); err == nil {
			return Load(configPath)
		}
	}

	// 4. Legacy fallback: ~/.config/go-llm/models.json. Preserves backward
	// compatibility for users who created configs at the old hardcoded path
	// on platforms where os.UserConfigDir() returns a different directory
	// (e.g., macOS returns ~/Library/Application Support, not ~/.config).
	if home, err := os.UserHomeDir(); err == nil {
		legacyPath := filepath.Join(home, ".config", "go-llm", "models.json")
		if _, err := os.Stat(legacyPath); err == nil {
			return Load(legacyPath)
		}
	}

	// Build an actionable error message with the resolved config path when available.
	configHint := "<user-config-dir>/go-llm/models.json"
	if configDirErr == nil {
		configHint = filepath.Join(configDir, "go-llm", "models.json")
	}
	return nil, fmt.Errorf("%w; set GO_LLM_CONFIG, "+
		"place models.json in the working directory, or create %s", ErrConfigNotFound, configHint)
}

// Load reads a models.json file from path, parses it, applies defaults, and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %q: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %q: %w", path, err)
	}

	// Validate first (before materializing defaults).
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	// Apply defaults after validation passes.
	cfg.applyDefaults()

	return &cfg, nil
}

// MustLoad is like Load but panics on error.
func MustLoad(path string) *Config {
	cfg, err := Load(path)
	if err != nil {
		panic(err)
	}
	return cfg
}

// applyDefaults materializes implicit provider assignments and timeout defaults.
func (cfg *Config) applyDefaults() {
	// Default timeout to 5m for any provider that has a zero timeout; default
	// empty APIFormat to "ollama" so configs predating the field keep working.
	for key, p := range cfg.Providers {
		if p.Timeout.Duration == 0 {
			p.Timeout.Duration = 5 * time.Minute
		}
		if p.APIFormat == "" {
			p.APIFormat = "ollama"
		}
		cfg.Providers[key] = p
	}

	// Materialize implicit provider: models without an explicit provider get "ollama".
	for role, m := range cfg.Models {
		if m.Provider == "" {
			m.Provider = "ollama"
			cfg.Models[role] = m
		}
	}
}

// validate checks all config invariants and returns the first error found.
func (cfg *Config) validate() error {
	// At least one provider is required.
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("config: at least one provider is required")
	}

	// Validate providers (sorted for deterministic errors).
	providerKeys := make([]string, 0, len(cfg.Providers))
	for key := range cfg.Providers {
		providerKeys = append(providerKeys, key)
	}
	sort.Strings(providerKeys)
	for _, key := range providerKeys {
		p := cfg.Providers[key]
		if p.BaseURL == "" {
			return fmt.Errorf("config: provider %q: base_url is required", key)
		}
		u, err := url.ParseRequestURI(p.BaseURL)
		if err != nil {
			return fmt.Errorf("config: provider %q: invalid base_url: %w", key, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: provider %q: base_url must include scheme and host", key)
		}
		// Empty api_format defaults to "ollama" via applyDefaults; only
		// reject explicit unknown values so a typo like "ollma" surfaces
		// at load time instead of silently degrading.
		if p.APIFormat != "" && !validAPIFormats[p.APIFormat] {
			return fmt.Errorf("config: provider %q: invalid api_format %q", key, p.APIFormat)
		}
	}

	// Validate models (sorted for deterministic errors).
	modelKeys := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		modelKeys = append(modelKeys, role)
	}
	sort.Strings(modelKeys)
	for _, role := range modelKeys {
		m := cfg.Models[role]
		if m.Name == "" {
			return fmt.Errorf("config: model %q: name is required", role)
		}
		if m.Type == "" {
			return fmt.Errorf("config: model %q: type is required", role)
		}
		if !validModelTypes[m.Type] {
			return fmt.Errorf("config: model %q: invalid type %q", role, m.Type)
		}

		// Validate explicit capabilities (only when provided; empty defers to
		// type-driven derivation). Strict-reject unknowns so typos surface at
		// load time, and reject multi-bit aliases like "completion" so the
		// "explicit Capabilities REPLACE derived" contract holds without
		// surprise expansion at the override merge. Then enforce the
		// embedding-type-must-only-embed invariant per the design contract.
		// Tokens are lowercased to match provider.ParseCapsStrict's behavior;
		// JSON case ("Chat" vs "chat") must not cause silent disagreement.
		if len(m.Capabilities) > 0 {
			for _, cap := range m.Capabilities {
				lower := strings.ToLower(cap)
				if !validCapabilityNames[lower] {
					return fmt.Errorf("config: model %q: unknown or non-canonical capability %q (canonical names: %v)", role, cap, provider.CanonicalCapabilityNames)
				}
				if m.Type == "embedding" && !embeddingOnlyCapabilities[lower] {
					return fmt.Errorf("config: model %q: type %q must declare only embedding capabilities, got %q", role, m.Type, cap)
				}
			}
			// Final round-trip check: provider must accept the same tokens.
			// This catches drift if CanonicalCapabilityNames and the parser
			// ever diverge — a class of bug the consume-shared-list pattern
			// is designed to make impossible, but worth asserting in case
			// someone bypasses the list.
			if _, err := provider.ParseCapsStrict(m.Capabilities); err != nil {
				return fmt.Errorf("config: model %q: %w", role, err)
			}
		}

		// Check provider exists. Use a local name distinct from the imported
		// "provider" package — shadowing would silently break any future
		// edit that needs to call provider.X inside this loop body.
		providerKey := m.Provider
		if providerKey == "" {
			providerKey = "ollama"
		}
		if _, ok := cfg.Providers[providerKey]; !ok {
			if m.Provider == "" {
				return fmt.Errorf("config: model %q: implicit provider \"ollama\" not found", role)
			}
			return fmt.Errorf("config: model %q: provider %q not found", role, providerKey)
		}

		// Validate fallbacks.
		for _, fb := range m.Fallbacks {
			if fb == role {
				return fmt.Errorf("config: model %q: lists itself as a fallback", role)
			}
			fbModel, ok := cfg.Models[fb]
			if !ok {
				return fmt.Errorf("config: model %q: fallback %q references unknown role", role, fb)
			}
			if !typeCompatible(m.Type, fbModel.Type) {
				return fmt.Errorf("config: model %q: fallback %q has incompatible type", role, fb)
			}
		}
	}

	// Validate defaults (sorted for deterministic errors).
	defaultKeys := make([]string, 0, len(cfg.Defaults))
	for key := range cfg.Defaults {
		defaultKeys = append(defaultKeys, key)
	}
	sort.Strings(defaultKeys)
	for _, key := range defaultKeys {
		role := cfg.Defaults[key]
		if _, ok := cfg.Models[role]; !ok {
			return fmt.Errorf("config: default %q references unknown role %q", key, role)
		}
	}

	// Detect circular fallback chains.
	// Sort roles for deterministic error reporting.
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if err := cfg.detectCycle(role); err != nil {
			return err
		}
	}

	return nil
}

// detectCycle checks for circular fallback chains starting from startRole.
// It uses on-path tracking to avoid false positives on diamond-shaped graphs.
func (cfg *Config) detectCycle(startRole string) error {
	onPath := make(map[string]bool)

	var walk func(role string) error
	walk = func(role string) error {
		if onPath[role] {
			return fmt.Errorf("config: model %q: circular fallback chain", startRole)
		}
		m, ok := cfg.Models[role]
		if !ok {
			return nil
		}
		onPath[role] = true
		defer delete(onPath, role)

		for _, fb := range m.Fallbacks {
			if err := walk(fb); err != nil {
				return err
			}
		}
		return nil
	}

	return walk(startRole)
}
