// Package providerbootstrap assembles the config→providers→model-registry→router
// wiring shared by the MCP server and the golem CLI. It owns provider
// construction, capability overrides, and the fingerprint prober factory; it does
// NOT own RAG, transcripts, or the server's degraded-mode startup policy.
package providerbootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// Options configures New. Config is optional; nil synthesizes a single default
// Ollama provider so callers can start with no discovered configuration.
type Options struct {
	Config           *config.Config
	FingerprintStore fingerprint.Store
	// FingerprintProfileStore supplies persisted profile enrichment without
	// enabling full profiling. FingerprintStore takes precedence when both are set.
	FingerprintProfileStore fingerprint.Store
	OllamaURLOverride       string
	RouterOptions           []provider.RouterOption

	// OpenAICompatURLOverrideProvider names the openai-compat provider whose
	// BaseURL is replaced by OpenAICompatURLOverride (provider keys are
	// arbitrary — "llamacpp", "local" — so a bare URL cannot address one).
	// Both fields must be set together; the named provider must exist in
	// Config with api_format "openai-compat". Unlike OllamaURLOverride, the
	// override is written into the returned Bundle.Config (on a copy — the
	// caller's Config is never mutated) so diagnostics read the same URL the
	// live client uses.
	OpenAICompatURLOverrideProvider string
	OpenAICompatURLOverride         string

	// CapabilityProbeStore wires capability-probe rows and on-demand
	// ResolveToolCall without enabling full fingerprint profiling. Golem
	// sets this and leaves FingerprintStore nil; MCP/full-profiler callers
	// can rely on FingerprintStore's SQLiteStore also satisfying
	// fingerprint.CapProbeStore (interface-assert fallback below).
	CapabilityProbeStore fingerprint.CapProbeStore

	// DestinationGate arms the destination-admission boundary (#477). When
	// non-nil, every provider HTTP client — ollama, openai-compat, and the
	// slot-probe client — is wrapped by the destination guard bound to that
	// provider's effective destination, the Router stamps the gate onto
	// every plan it builds, and RefreshModels runs ONLY for ActiveProviders
	// with a model-refresh capability bound per call. A denial anywhere in
	// bootstrap is FATAL, never a Bundle warning: an unadmitted destination
	// must produce zero requests, not a degraded start.
	//
	// nil keeps the pre-#477 behavior for callers that have not yet
	// migrated; each entry point must make that choice deliberately (I18).
	DestinationGate *provider.DestinationGate
	// ActiveProviders is the sorted provider set from the mode's frozen
	// network plan. Required when DestinationGate is set: a gate with no
	// active set would silently refresh nothing, and a wiring bug should be
	// loud.
	ActiveProviders []string
}

// Bundle is the assembled provider stack. Warnings collects non-fatal,
// best-effort failures (e.g. a provider whose RefreshModels failed at startup).
type Bundle struct {
	Config    *config.Config
	Providers *provider.Registry
	Models    *provider.ModelRegistry
	Router    *provider.Router
	Warnings  []error
}

// defaultOllamaURL is the fallback base URL for the nil-Config / default provider.
const defaultOllamaURL = "http://localhost:11434"

// New builds the provider stack. It errors only when no provider can be
// constructed/registered or the model registry/router cannot be built.
func New(ctx context.Context, opts Options) (*Bundle, error) {
	gate := opts.DestinationGate
	if gate != nil && len(opts.ActiveProviders) == 0 {
		return nil, fmt.Errorf("providerbootstrap: DestinationGate set without ActiveProviders; a gated bootstrap that refreshes nothing is a wiring bug, not a mode")
	}

	// effCfg is the synthetic config when opts.Config is nil; reuse it for the
	// prober factory, capability overrides, and Bundle.Config so all four see the
	// same providers New actually built.
	eff, err := materializeEffectiveConfig(opts.Config, opts.OllamaURLOverride,
		opts.OpenAICompatURLOverrideProvider, opts.OpenAICompatURLOverride)
	if err != nil {
		return nil, err
	}
	provs, ollamaClients, err := constructProviders(eff, gate)
	if err != nil {
		return nil, err
	}
	effCfg := eff.cfg

	// Validate slot_discovery before any provider registration or
	// RefreshModels I/O: user config fails loud and fails FAST — an
	// invalid config must error without touching any server.
	slotBEs, err := slotBackends(effCfg)
	if err != nil {
		return nil, err
	}

	active := make(map[string]bool, len(opts.ActiveProviders))
	for _, name := range opts.ActiveProviders {
		active[name] = true
	}
	if gate != nil {
		// I7: an inactive provider must see zero requests, so its backend
		// leaves slot governance entirely rather than fail-safe-probing.
		// Nothing routes to it either (it is on no planned route), so the
		// ungoverned-unlimited admission default is unreachable for it.
		for name := range slotBEs {
			if !active[name] {
				delete(slotBEs, name)
			}
		}
	}

	pReg := provider.NewRegistry()
	registered := 0
	var warnings []error
	for _, p := range provs {
		if err := pReg.Register(p); err != nil {
			warnings = append(warnings, fmt.Errorf("providerbootstrap: register %q: %w", p.Name(), err))
			continue
		}
		registered++
		// Gated bootstrap refreshes ONLY the active providers (I7), each
		// call bound to a model-refresh capability. A denial — at the bind
		// or surfacing through the guarded client — is fatal (never a
		// warning): it means the admitted manifest and this refresh
		// disagree, and degrading would start a session whose consent
		// receipt does not match its traffic.
		rctx := ctx
		if gate != nil {
			if !active[p.Name()] {
				continue
			}
			rctx, err = gate.Bind(ctx, provider.DestinationPurposeModelRefresh, p.Name())
			if err != nil {
				return nil, fmt.Errorf("providerbootstrap: refresh %q: %w", p.Name(), err)
			}
		}
		if err := pReg.RefreshModels(rctx, p.Name()); err != nil {
			if errors.Is(err, provider.ErrDestinationDenied) {
				return nil, fmt.Errorf("providerbootstrap: refresh %q: %w", p.Name(), err)
			}
			warnings = append(warnings, fmt.Errorf("providerbootstrap: refresh %q: %w", p.Name(), err))
		}
	}
	if registered == 0 {
		return nil, fmt.Errorf("providerbootstrap: no providers registered: %v", warnings)
	}

	mrOpts := []provider.ModelRegistryOption{}
	if gate != nil {
		// Registry-initiated traffic (live model queries during routing,
		// active capability probes) binds its own metadata purposes (#477).
		mrOpts = append(mrOpts, provider.WithModelRegistryDestinationGate(gate))
	}
	factory := proberFactory(effCfg, ollamaClients)
	if opts.FingerprintStore != nil {
		mrOpts = append(mrOpts, provider.WithFingerprintProberFactory(factory))
	} else if opts.FingerprintProfileStore != nil {
		mrOpts = append(mrOpts, provider.WithReadOnlyFingerprintProfiles(factory))
	}
	fpStore := opts.FingerprintStore
	if fpStore == nil {
		// Read persisted profiles without installing the full profiling factory.
		fpStore = opts.FingerprintProfileStore
	}
	// Capability-only resolution (ResolveToolCall): explicit store wins;
	// otherwise a FingerprintStore that also satisfies CapProbeStore (the
	// SQLite store does) provides it for free. The prober factory is shared —
	// WithCapabilityProber never triggers full profiling from Lookup.
	capProbeStore := opts.CapabilityProbeStore
	if capProbeStore == nil {
		if s, ok := opts.FingerprintStore.(fingerprint.CapProbeStore); ok {
			capProbeStore = s
		}
	}
	if capProbeStore != nil {
		mrOpts = append(mrOpts,
			provider.WithCapabilityProbeStore(capProbeStore),
			provider.WithCapabilityProber(factory),
		)
	}
	mr, err := provider.NewModelRegistry(pReg, fpStore, mrOpts...)
	if err != nil {
		return nil, fmt.Errorf("providerbootstrap: model registry: %w", err)
	}
	if err := installContextWindowOverrides(mr, effCfg); err != nil {
		return nil, err
	}
	if err := installCapabilityOverrides(mr, effCfg); err != nil {
		return nil, err
	}
	if err := installCapabilityFloors(mr, effCfg); err != nil {
		return nil, err
	}
	if err := installThinkOverrides(mr, effCfg); err != nil {
		return nil, err
	}

	modelDefaults, err := buildModelDefaults(effCfg)
	if err != nil {
		return nil, err
	}
	routerOpts := make([]provider.RouterOption, 0, len(opts.RouterOptions)+2)
	if len(modelDefaults) > 0 {
		routerOpts = append(routerOpts, provider.WithModelDefaults(modelDefaults))
	}
	// A caller-supplied WithSlotSource (below, applied last) replaces the
	// config-derived source, which holds no goroutines until its RecordUse
	// is called and is simply collected — nothing to close.
	if len(slotBEs) > 0 {
		slotOverrides, err := buildSlotOverrides(effCfg, slotBEs)
		if err != nil {
			return nil, err
		}
		ssOpts := []provider.SlotSourceOption{}
		if len(slotOverrides) > 0 {
			ssOpts = append(ssOpts, provider.WithSlotCapacityOverrides(slotOverrides))
		}
		if gate != nil {
			// Slot probes are repository-owned metadata traffic (I14):
			// per-backend guarded clients, and a slot-probe capability
			// bound freshly per probe so re-admission after a revoke
			// restores probing instead of leaving a dead cached context.
			slotClients := make(map[string]*http.Client, len(slotBEs))
			for name := range slotBEs {
				gc, gerr := provider.GuardHTTPClient(gate, eff.dests[name],
					&http.Client{Timeout: defaultSlotProbeClientTimeout})
				if gerr != nil {
					return nil, fmt.Errorf("providerbootstrap: slot probe client %q: %w", name, gerr)
				}
				slotClients[name] = gc
			}
			ssOpts = append(ssOpts,
				provider.WithSlotClientFor(func(name string) *http.Client { return slotClients[name] }),
				provider.WithSlotProbeBinder(func(ctx context.Context, name string) (context.Context, error) {
					return gate.Bind(ctx, provider.DestinationPurposeSlotProbe, name)
				}),
			)
		}
		routerOpts = append(routerOpts, provider.WithSlotSource(provider.NewOpenAICompatSlotSource(slotBEs, ssOpts...)))
	} else if _, err := buildSlotOverrides(effCfg, slotBEs); err != nil {
		// slots overrides with NO slot-discovery provider at all is the
		// same loud config error, not a silent no-op.
		return nil, err
	}
	if gate != nil {
		// Router plans bind per-attempt destination capabilities (#477).
		routerOpts = append(routerOpts, provider.WithDestinationGate(gate))
	}
	// Explicit constructor options apply last, so caller-supplied defaults
	// override config for matching model keys while preserving other config keys.
	routerOpts = append(routerOpts, opts.RouterOptions...)
	router := provider.NewRouter(mr, pReg, routerOpts...)

	return &Bundle{
		Config:    effCfg,
		Providers: pReg,
		Models:    mr,
		Router:    router,
		Warnings:  warnings,
	}, nil
}

// Close releases router resources. Safe to call on a nil/zero Bundle.
func (b *Bundle) Close() error {
	if b == nil || b.Router == nil {
		return nil
	}
	return b.Router.Close()
}
