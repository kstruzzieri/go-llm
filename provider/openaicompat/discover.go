package openaicompat

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// DiscoverBaseURL returns the first candidate base URL whose /v1/models
// endpoint serves wantModel. Candidates are tried in order; each probe is a
// single GET /v1/models against a Client built from opts (use WithHTTPClient
// for a short probe timeout and WithAPIKey when the server enforces auth). A
// candidate is accepted iff the response is 2xx, decodes to a well-formed
// models list, and some model id equals wantModel exactly — no alias or
// substring matching, so an unrelated local service listing different models
// is never selected.
//
// Candidate URLs are server roots WITHOUT the /v1 suffix (see NewClient).
// DiscoverBaseURL owns only the probe invariant; candidate ordering, port
// ranges, and loopback policy belong to the caller.
//
// The not-found error names every candidate tried with userinfo redacted;
// API keys never appear in the error. Context cancellation aborts the scan
// with the context's error.
func DiscoverBaseURL(ctx context.Context, candidates []string, wantModel string, opts ...ClientOption) (string, error) {
	if wantModel == "" {
		return "", fmt.Errorf("openaicompat: discover: wantModel is required")
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("openaicompat: discover: no candidates to probe")
	}
	tried := make([]string, 0, len(candidates))
	for _, cand := range candidates {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("openaicompat: discover: %w", err)
		}
		tried = append(tried, redactURLUserinfo(cand))
		var resp modelsResponse
		if err := NewClient(cand, opts...).getJSON(ctx, "/v1/models", &resp); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return "", fmt.Errorf("openaicompat: discover: %w", cerr)
			}
			continue // unreachable, non-2xx, or malformed body: not the backend
		}
		for _, m := range resp.Data {
			if m.ID == wantModel {
				return cand, nil
			}
		}
	}
	return "", fmt.Errorf("openaicompat: discover: no candidate serves model %q; tried %s",
		wantModel, strings.Join(tried, ", "))
}

// redactURLUserinfo strips any userinfo (user:pass@) from a URL before it is
// printed in an error, so credentials never leak into logs. A string that
// fails to parse and looks userinfo-shaped (contains "@") is replaced
// wholesale rather than printed raw. Mirrors cmd/golem's redactBaseURL.
func redactURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "@") {
			return "<invalid url>"
		}
		return raw
	}
	if strings.Contains(raw, "@") && u.User == nil && u.Host == "" {
		return "<invalid url>"
	}
	u.User = nil
	return u.String()
}
