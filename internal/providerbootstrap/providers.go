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
	provs, ollamaClients, err := constructProviders(eff, nil, nil)
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
//
// A non-nil match is the outbound content guard (#424); buildProvider installs
// it on the openai-compat clients that need it.
func constructProviders(eff *Effective, gate *provider.DestinationGate, match func(string) bool) ([]provider.Provider, map[string]*ollama.Client, error) {
	keys := eff.sortedProviderKeys()
	registered := make([]provider.Provider, 0, len(keys))
	ollamaClients := make(map[string]*ollama.Client)
	for _, key := range keys {
		pc := eff.cfg.Providers[key]
		prov, client, err := buildProvider(key, pc, gate, eff.dests[key], match)
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

// ProviderTimeout is the per-request timeout a provider built from cfg runs
// with: its configured timeout when positive, else the clients' 5-minute
// default.
func ProviderTimeout(cfg config.ProviderConfig) time.Duration {
	if cfg.Timeout.Duration > 0 {
		return cfg.Timeout.Duration
	}
	return defaultProviderTimeout
}

// BuildGuardedProvider constructs one provider from cfg whose every request
// passes the destination guard bound to gate and dest, as bootstrap builds a
// gated provider. For ollama it also returns the underlying client. A non-nil
// transport is the guarded client's base transport; nil uses the default. A
// loopback destination still clones it, strips its proxy and dials localhost
// as the loopback addresses. match is the content guard, as in bootstrap. A
// nil gate is an error: this constructor never builds an ungated client.
//
// closeIdle closes the idle connections of the transport the guard made for
// this provider: its clone for a loopback destination, and its copy of the
// default for a remote one built with a nil transport. For a remote
// destination built with a transport, which the guard uses as is, it does
// nothing: those connections are the caller's.
func BuildGuardedProvider(key string, cfg config.ProviderConfig, gate *provider.DestinationGate, dest provider.Destination, match func(string) bool, transport *http.Transport) (prov provider.Provider, client *ollama.Client, closeIdle func(), err error) {
	if gate == nil {
		return nil, nil, nil, fmt.Errorf("providerbootstrap: provider %q: a guarded provider requires a gate", key)
	}
	guarded, err := guardedClient(key, cfg, gate, dest, transport)
	if err != nil {
		return nil, nil, nil, err
	}
	if prov, client, err = providerWith(key, cfg, guarded, dest, match); err != nil {
		return nil, nil, nil, err
	}
	closeIdle = func() {}
	if dest.IsLocal() || transport == nil {
		closeIdle = guarded.CloseIdleConnections
	}
	return prov, client, closeIdle, nil
}

// buildProvider constructs one provider from its config. For ollama it also
// returns the underlying client so the prober factory can reuse it. A
// non-nil gate wraps the provider's HTTP client with the destination guard
// bound to dest; a nil gate keeps the ungated construction byte-identical.
func buildProvider(key string, cfg config.ProviderConfig, gate *provider.DestinationGate, dest provider.Destination, match func(string) bool) (provider.Provider, *ollama.Client, error) {
	var guarded *http.Client
	if gate != nil {
		var err error
		if guarded, err = guardedClient(key, cfg, gate, dest, nil); err != nil {
			return nil, nil, err
		}
	}
	return providerWith(key, cfg, guarded, dest, match)
}

// guardedClient is an HTTP client with cfg's timeout behind the destination
// guard bound to gate and dest. A non-nil transport is its base transport.
func guardedClient(key string, cfg config.ProviderConfig, gate *provider.DestinationGate, dest provider.Destination, transport *http.Transport) (*http.Client, error) {
	base := &http.Client{Timeout: ProviderTimeout(cfg)}
	if transport != nil { // a nil *http.Transport in the interface field is not a nil RoundTripper
		base.Transport = transport
	}
	guarded, err := provider.GuardHTTPClient(gate, dest, base)
	if err != nil {
		return nil, fmt.Errorf("providerbootstrap: provider %q: %w", key, err)
	}
	return guarded, nil
}

// providerWith constructs the provider over guarded, or over the clients' own
// HTTP client when guarded is nil.
//
// A non-nil match is installed as the content guard on an openai-compat client
// whose destination is not loopback. Loopback is decided on the materialized
// destination, so a URL override moves a provider across the boundary; ollama
// clients take no guard.
func providerWith(key string, cfg config.ProviderConfig, guarded *http.Client, dest provider.Destination, match func(string) bool) (provider.Provider, *ollama.Client, error) {
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
		// An empty cache_affinity lets the client infer the mode from its host.
		copts := []openaicompat.ClientOption{openaicompat.WithCacheAffinity(cfg.CacheAffinity)}
		if guarded != nil {
			copts = append(copts, openaicompat.WithHTTPClient(guarded))
		} else if cfg.Timeout.Duration > 0 {
			copts = append(copts, openaicompat.WithHTTPClient(&http.Client{Timeout: cfg.Timeout.Duration}))
		}
		if cfg.APIKey != "" {
			copts = append(copts, openaicompat.WithAPIKey(cfg.APIKey))
		}
		if match != nil && !dest.IsLocal() {
			copts = append(copts, openaicompat.WithContentGuard(match))
		}
		return openaicompat.NewProvider(
			openaicompat.NewClient(cfg.BaseURL, copts...),
			openaicompat.WithProviderName(key),
		), nil, nil
	default:
		return nil, nil, fmt.Errorf("providerbootstrap: provider %q: unsupported api_format %q", key, cfg.APIFormat)
	}
}
