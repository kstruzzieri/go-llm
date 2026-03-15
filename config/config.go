package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"time"
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
		return fmt.Errorf("duration must be a string: %w", err)
	}
	if s == "" {
		return fmt.Errorf("duration must not be empty")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// MarshalJSON returns the duration as a quoted string (e.g. "5m0s").
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

// ProviderConfig holds connection settings for an LLM provider (e.g. Ollama, vLLM).
type ProviderConfig struct {
	BaseURL   string   `json:"base_url"`
	Timeout   Duration `json:"timeout"`
	APIKey    string   `json:"api_key"`
	APIFormat string   `json:"api_format"`
}

// ModelConfig describes a model's identity, capabilities, and fallback chain.
type ModelConfig struct {
	Name          string   `json:"name"`
	Provider      string   `json:"provider,omitempty"`
	Description   string   `json:"description,omitempty"`
	Type          string   `json:"type"`
	Parameters    string   `json:"parameters,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	Dimensions    int      `json:"dimensions,omitempty"`
	Fallbacks     []string `json:"fallbacks,omitempty"`
}

// Config is the top-level configuration loaded from models.json.
type Config struct {
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
	Defaults  map[string]string         `json:"defaults"`
}

// validModelTypes enumerates the allowed values for ModelConfig.Type.
var validModelTypes = map[string]bool{
	"dense":     true,
	"moe":       true,
	"embedding": true,
}

// typeCompatible reports whether a model of fromType can fall back to a model of toType.
// Embedding models can only fall back to other embedding models.
// Dense and MoE models are interchangeable as fallbacks.
func typeCompatible(fromType, toType string) bool {
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

// Load reads a models.json file from path, parses it, applies defaults, and validates.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// Apply defaults.
	cfg.applyDefaults()

	// Validate.
	if err := cfg.validate(); err != nil {
		return nil, err
	}

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
	// Default timeout to 5m for any provider that has a zero timeout.
	for key, p := range cfg.Providers {
		if p.Timeout.Duration == 0 {
			p.Timeout.Duration = 5 * time.Minute
			cfg.Providers[key] = p
		}
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
		if _, err := url.ParseRequestURI(p.BaseURL); err != nil {
			return fmt.Errorf("config: provider %q: invalid base_url: %w", key, err)
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

		// Check provider exists (provider was already materialized by applyDefaults).
		if _, ok := cfg.Providers[m.Provider]; !ok {
			if m.Provider == "ollama" {
				return fmt.Errorf("config: model %q: implicit provider %q not found", role, m.Provider)
			}
			return fmt.Errorf("config: model %q: provider %q not found", role, m.Provider)
		}

		// Validate fallbacks.
		for _, fb := range m.Fallbacks {
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
