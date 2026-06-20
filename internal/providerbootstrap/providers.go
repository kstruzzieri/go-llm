package providerbootstrap

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// buildProviders constructs every configured provider (sorted by config key).
// It returns the registered providers, the ollama clients keyed by provider name
// (for provider-specific fingerprint probers), and the EFFECTIVE config (the
// synthetic one when cfg==nil, else cfg) so callers reuse a single coherent
// config. nil cfg synthesizes one default ollama provider at override (or
// defaultOllamaURL). A non-nil cfg with zero Providers is an error (mcp parity).
func buildProviders(cfg *config.Config, override string) ([]provider.Provider, map[string]*ollama.Client, *config.Config, error) {
	effective := cfg
	if cfg == nil {
		url := override
		if url == "" {
			url = defaultOllamaURL
		}
		effective = &config.Config{Providers: map[string]config.ProviderConfig{
			"ollama": {BaseURL: url, APIFormat: "ollama"},
		}}
	} else if len(cfg.Providers) == 0 {
		return nil, nil, nil, fmt.Errorf("providerbootstrap: no providers configured")
	}

	keys := make([]string, 0, len(effective.Providers))
	for key := range effective.Providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	registered := make([]provider.Provider, 0, len(keys))
	ollamaClients := make(map[string]*ollama.Client)
	for _, key := range keys {
		if err := config.ValidateProviderName(key); err != nil {
			return nil, nil, nil, fmt.Errorf("providerbootstrap: provider config: %w", err)
		}
		pc := effective.Providers[key]
		if pc.APIFormat == "" {
			pc.APIFormat = "ollama"
		}
		// Explicit override pins the ollama base URL (mirrors mcp WithOllamaURL).
		if key == "ollama" && pc.APIFormat == "ollama" && override != "" {
			pc.BaseURL = override
		}
		prov, client, err := buildProvider(key, pc)
		if err != nil {
			return nil, nil, nil, err
		}
		registered = append(registered, prov)
		if pc.APIFormat == "ollama" && client != nil {
			ollamaClients[key] = client
		}
	}
	return registered, ollamaClients, effective, nil
}

// buildProvider constructs one provider from its config. For ollama it also
// returns the underlying client so the prober factory can reuse it.
func buildProvider(key string, cfg config.ProviderConfig) (provider.Provider, *ollama.Client, error) {
	switch cfg.APIFormat {
	case "", "ollama":
		opts := []ollama.Option{ollama.WithBaseURL(cfg.BaseURL)}
		if cfg.Timeout.Duration > 0 {
			opts = append(opts, ollama.WithTimeout(cfg.Timeout.Duration))
		}
		client := ollama.NewClient(opts...)
		return provider.NewOllamaProvider(client, provider.WithProviderName(key)), client, nil
	case "openai-compat":
		copts := []openaicompat.ClientOption{}
		if cfg.Timeout.Duration > 0 {
			copts = append(copts, openaicompat.WithHTTPClient(&http.Client{Timeout: cfg.Timeout.Duration}))
		}
		if cfg.APIKey != "" {
			copts = append(copts, openaicompat.WithAPIKey(cfg.APIKey))
		}
		return openaicompat.NewProvider(
			openaicompat.NewClient(cfg.BaseURL, copts...),
			openaicompat.WithProviderName(key),
		), nil, nil
	default:
		return nil, nil, fmt.Errorf("providerbootstrap: provider %q: unsupported api_format %q", key, cfg.APIFormat)
	}
}
