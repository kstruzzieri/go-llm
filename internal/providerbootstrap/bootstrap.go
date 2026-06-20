// Package providerbootstrap assembles the config→providers→model-registry→router
// wiring shared by the MCP server and the golem CLI. It owns provider
// construction, capability overrides, and the fingerprint prober factory; it does
// NOT own RAG, transcripts, or the server's degraded-mode startup policy.
package providerbootstrap

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// Options configures New. Config is optional; nil synthesizes a single default
// Ollama provider so callers can start with no discovered configuration.
type Options struct {
	Config            *config.Config
	FingerprintStore  fingerprint.Store
	OllamaURLOverride string
	RouterOptions     []provider.RouterOption
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
	return nil, fmt.Errorf("providerbootstrap: not implemented")
}

// Close releases router resources. Safe to call on a nil/zero Bundle.
func (b *Bundle) Close() error {
	if b == nil || b.Router == nil {
		return nil
	}
	return b.Router.Close()
}
