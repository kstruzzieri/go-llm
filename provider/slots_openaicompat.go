package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// SlotBackend describes one governed backend endpoint.
type SlotBackend struct {
	// BaseURL is the server root, without /v1 (matching openaicompat.NewClient).
	BaseURL string
	// APIKey, when non-empty, is sent as a Bearer token — the same auth
	// convention as the provider's own client. Never logged.
	APIKey string
}

// slotsPropsResponse is the subset of llama-server's GET /props JSON we read.
type slotsPropsResponse struct {
	TotalSlots int `json:"total_slots"`
}

// fetchSlotCapacity queries {base}/props?model={model} and returns
// total_slots. The query is ALWAYS model-qualified: llama-swap (and
// llama-server's model-dispatched router mode) requires it to route to the
// right upstream, and single-model llama-server ignores the unknown
// parameter — one code path covers both. A response without a positive
// total_slots is an error; callers translate errors to fail-safe capacity 1.
func fetchSlotCapacity(ctx context.Context, hc *http.Client, be SlotBackend, model string) (int, error) {
	u := strings.TrimRight(be.BaseURL, "/") + "/props?model=" + url.QueryEscape(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: %w", model, err)
	}
	if be.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+be.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: %w", model, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("provider: slot probe %q: %s", model, resp.Status)
	}
	var props slotsPropsResponse
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: decode /props: %w", model, err)
	}
	if props.TotalSlots < 1 {
		return 0, fmt.Errorf("provider: slot probe %q: /props total_slots %d", model, props.TotalSlots)
	}
	return props.TotalSlots, nil
}
