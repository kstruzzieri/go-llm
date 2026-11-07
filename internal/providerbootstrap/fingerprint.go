package providerbootstrap

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint/probers"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// proberFactory builds the provider-aware fingerprint prober factory. Ollama
// clients are keyed by provider name so multi-Ollama configs probe the same
// backend that owns the requested model key. obs, when non-nil, receives each
// openai-compat probe call's usage under the probed key (the prober itself
// knows only the bare model name).
func proberFactory(cfg *config.Config, ollamaClients map[string]*ollama.Client, obs func(provider.ModelKey, provider.Usage)) provider.FingerprintProberFactory {
	return func(ctx context.Context, key provider.ModelKey, runtime *provider.ModelInfo, p provider.Provider) (*provider.FingerprintProberSpec, error) {
		apiFormat := "ollama"
		if cfg != nil {
			pc, ok := cfg.Providers[key.Provider]
			if !ok {
				return nil, nil
			}
			apiFormat = pc.APIFormat
			if apiFormat == "" {
				apiFormat = "ollama"
			}
		}

		// Ollama's digest comes from /api/tags. Without it this lookup has no real
		// identity: a fallback would re-run the benchmark now, and again when the
		// digest returns. An openai-compat server reports none, so its fallback is
		// its identity.
		if apiFormat == "ollama" && (runtime == nil || runtime.Digest == "") {
			return nil, nil
		}

		caps := capabilitiesForKey(cfg, key)
		in := probers.ProberFactoryInput{
			APIFormat:    apiFormat,
			OllamaClient: ollamaClients[key.Provider],
			Capabilities: caps,
		}
		if apiFormat == "openai-compat" {
			oc, ok := p.(*openaicompat.Provider)
			if !ok {
				return nil, fmt.Errorf("providerbootstrap: fingerprint prober for %s/%s: provider has type %T, want *openaicompat.Provider", key.Provider, key.Model, p)
			}
			in.OpenAICompatProvider = oc
			if obs != nil {
				in.UsageObserver = func(_ string, u provider.Usage) { obs(key, u) }
			}
		}

		prober, err := probers.NewProberForAPIFormat(in)
		if err != nil {
			return nil, err
		}
		return &provider.FingerprintProberSpec{
			Prober:      prober,
			ModelDigest: fingerprintDigestForModel(key, runtime, caps),
		}, nil
	}
}

// fingerprintDigestForModel returns the canonical model digest for fingerprint
// cache keying. Mirrors mcp.fingerprintDigestForModel exactly.
func fingerprintDigestForModel(key provider.ModelKey, runtime *provider.ModelInfo, caps []string) string {
	if runtime != nil && runtime.Digest != "" {
		return runtime.Digest
	}
	if len(caps) > 0 {
		return "config-caps:" + strings.Join(caps, ",")
	}
	return key.String()
}
