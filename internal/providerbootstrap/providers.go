package providerbootstrap

import (
	"fmt"
	"net/http"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// buildProviders materializes the effective config (see
// Materialize) and constructs every provider from it, sorted
// by config key. It returns the registered providers, the ollama clients
// keyed by provider name (for provider-specific fingerprint probers), and
// the effective config. All overrides are already IN the effective config by
// the time any client is constructed, so the returned config always reflects
// the live client URLs.
func buildProviders(cfg *config.Config, override, ocOverrideProvider, ocOverrideURL string) ([]provider.Provider, map[string]*ollama.Client, *config.Config, error) {
	eff, err := Materialize(cfg, override, ocOverrideProvider, ocOverrideURL)
	if err != nil {
		return nil, nil, nil, err
	}
	provs, ollamaClients, err := constructProviders(eff, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return provs, ollamaClients, eff.cfg, nil
}

// constructProviders builds one provider per effective config entry. It
// reads ONLY the effective config: every override was applied during
// materialization, so no construction-time rewriting can diverge from what
// the rest of the process displays and admits.
//
// A non-nil gate wraps every provider HTTP client with the destination
// guard bound to that provider's effective destination (#477): the guard is
// the outermost transport, so a request without a capability from the
// gate's current snapshot never reaches a dialer. The fingerprint probers
// reuse these same clients (the ollama client map and the openai-compat
// provider), so capability-probe traffic is guarded by construction rather
// than by a second wrapping site.
func constructProviders(eff *Effective, gate *provider.DestinationGate) ([]provider.Provider, map[string]*ollama.Client, error) {
	keys := eff.sortedProviderKeys()
	registered := make([]provider.Provider, 0, len(keys))
	ollamaClients := make(map[string]*ollama.Client)
	for _, key := range keys {
		pc := eff.cfg.Providers[key]
		prov, client, err := buildProvider(key, pc, gate, eff.dests[key])
		if err != nil {
			return nil, nil, err
		}
		registered = append(registered, prov)
		if pc.APIFormat == "ollama" && client != nil {
			ollamaClients[key] = client
		}
	}
	return registered, ollamaClients, nil
}

// slotBackends selects providers opted into slot discovery via
// slot_discovery: true. Managed-local is declared by the operator, never
// inferred from the host: loopback proves nothing (tunnels, Docker), and
// auto-enrolling generic loopback openai-compat runtimes without /props
// (vLLM, LM Studio) would serialize them through the capacity-1 fail-safe.
// A flag on a non-openai-compat provider is a loud config error.
func slotBackends(cfg *config.Config) (map[string]provider.SlotBackend, error) {
	out := make(map[string]provider.SlotBackend)
	for key, pc := range cfg.Providers {
		if !pc.SlotDiscovery {
			continue
		}
		apiFormat := pc.APIFormat
		if apiFormat == "" {
			apiFormat = "ollama"
		}
		if apiFormat != "openai-compat" {
			return nil, fmt.Errorf("providerbootstrap: provider %q: slot_discovery requires api_format \"openai-compat\", got %q", key, apiFormat)
		}
		out[key] = provider.SlotBackend{BaseURL: pc.BaseURL, APIKey: pc.APIKey}
	}
	return out, nil
}

// defaultProviderTimeout mirrors both clients' own default (ollama
// defaultTimeout and the openai-compat NewClient default are each 5
// minutes). The guarded path must build the base http.Client itself — the
// guard preserves the base's timeout — so the default lives here too;
// drifting from the client packages would change guarded timeouts only.
const defaultProviderTimeout = 5 * time.Minute

// defaultSlotProbeClientTimeout mirrors the slot source's own default probe
// client timeout; each probe is additionally ctx-bounded to the same value.
const defaultSlotProbeClientTimeout = 5 * time.Second

// buildProvider constructs one provider from its config. For ollama it also
// returns the underlying client so the prober factory can reuse it. A
// non-nil gate wraps the provider's HTTP client with the destination guard
// bound to dest; a nil gate keeps the ungated construction byte-identical.
func buildProvider(key string, cfg config.ProviderConfig, gate *provider.DestinationGate, dest provider.Destination) (provider.Provider, *ollama.Client, error) {
	timeout := defaultProviderTimeout
	if cfg.Timeout.Duration > 0 {
		timeout = cfg.Timeout.Duration
	}
	var guarded *http.Client
	if gate != nil {
		var err error
		guarded, err = provider.GuardHTTPClient(gate, dest, &http.Client{Timeout: timeout})
		if err != nil {
			return nil, nil, fmt.Errorf("providerbootstrap: provider %q: %w", key, err)
		}
	}
	switch cfg.APIFormat {
	case "", "ollama":
		opts := []ollama.Option{ollama.WithBaseURL(cfg.BaseURL)}
		if cfg.Timeout.Duration > 0 {
			opts = append(opts, ollama.WithTimeout(cfg.Timeout.Duration))
		}
		if guarded != nil {
			opts = append(opts, ollama.WithHTTPClient(guarded))
		}
		client := ollama.NewClient(opts...)
		return provider.NewOllamaProvider(client, provider.WithProviderName(key)), client, nil
	case "openai-compat":
		copts := []openaicompat.ClientOption{}
		if guarded != nil {
			copts = append(copts, openaicompat.WithHTTPClient(guarded))
		} else if cfg.Timeout.Duration > 0 {
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
