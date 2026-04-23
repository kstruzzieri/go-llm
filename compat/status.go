package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// healthCheckTimeout bounds how long a single provider.Health call is allowed
// to run from the status handler. The check context is detached from the
// request so a client disconnect does not surface as a phantom degraded
// provider on the next poll. 2s is enough for a local TCP probe and short
// enough that a single hung provider cannot freeze the endpoint.
const healthCheckTimeout = 2 * time.Second

// StatusResponse is the payload for GET /v1/status. Non-standard extension;
// modeled loosely on OpenAI's health endpoints but carries go-llm-specific
// provider and warmth detail.
type StatusResponse struct {
	Status    string            `json:"status"` // "ready" | "degraded" | "unavailable"
	Providers []ProviderStatus  `json:"providers"`
	Warm      []WarmModelStatus `json:"warm,omitempty"`
	Uptime    string            `json:"uptime"`
}

// ProviderStatus describes one registered backend.
type ProviderStatus struct {
	Name         string   `json:"name"`
	Healthy      bool     `json:"healthy"`
	Capabilities []string `json:"capabilities"`
	Error        string   `json:"error,omitempty"`
}

// WarmModelStatus describes one model currently resident in a provider's memory.
type WarmModelStatus struct {
	Provider  string  `json:"provider"`
	Model     string  `json:"model"`
	Loaded    bool    `json:"loaded"`
	VRAMGB    float64 `json:"vram_gb,omitempty"`
	ExpiresAt string  `json:"expires_at,omitempty"`
}

// handleStatus implements GET /v1/status. It is a non-standard extension that
// surfaces provider health and warmth snapshots for operators. Status is
// "ready" when every provider reports healthy, "degraded" when at least one
// is unhealthy, "unavailable" when no providers are registered.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.providers == nil {
		writeError(w, http.StatusServiceUnavailable, "no_providers", "server has no provider registry")
		return
	}

	providers := s.providers.All()
	out := StatusResponse{
		Providers: make([]ProviderStatus, 0, len(providers)),
		Uptime:    time.Since(s.startedAt).Round(time.Second).String(),
	}
	healthy := 0
	for _, p := range providers {
		ps := ProviderStatus{
			Name:         p.Name(),
			Capabilities: splitCapabilities(p.Capabilities()),
		}
		hctx, hcancel := context.WithTimeout(context.Background(), healthCheckTimeout)
		err := p.Health(hctx)
		hcancel()
		if err != nil {
			ps.Error = err.Error()
		} else {
			ps.Healthy = true
			healthy++
		}
		out.Providers = append(out.Providers, ps)
	}
	switch {
	case len(providers) == 0:
		out.Status = "unavailable"
	case healthy == len(providers):
		out.Status = "ready"
	default:
		out.Status = "degraded"
	}

	if s.router != nil {
		for _, wm := range s.router.WarmthSnapshot() {
			ws := WarmModelStatus{
				Provider: wm.Key.Provider,
				Model:    wm.Key.Model,
				Loaded:   wm.Info.Loaded,
				VRAMGB:   wm.Info.VRAM,
			}
			if !wm.Info.ExpiresAt.IsZero() {
				ws.ExpiresAt = wm.Info.ExpiresAt.Format(time.RFC3339)
			}
			out.Warm = append(out.Warm, ws)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// splitCapabilities converts a Capability bitmask into ordered names. The
// returned slice is always non-nil (possibly empty) so JSON clients see
// "capabilities": [] rather than "capabilities": null — the latter breaks
// JavaScript callers that access .length.
//
// NOTE: keep this list in sync with provider.Capability in provider/types.go.
// A new bit added there is silently dropped from /v1/status output without
// this update.
func splitCapabilities(c provider.Capability) []string {
	all := []struct {
		bit  provider.Capability
		name string
	}{
		{provider.CapChat, "chat"},
		{provider.CapGenerate, "generate"},
		{provider.CapInsert, "insert"},
		{provider.CapEmbed, "embed"},
		{provider.CapStream, "stream"},
		{provider.CapToolCall, "tool_call"},
		{provider.CapThinking, "thinking"},
	}
	out := make([]string, 0, len(all))
	for _, a := range all {
		if c.Has(a.bit) {
			out = append(out, a.name)
		}
	}
	return out
}
