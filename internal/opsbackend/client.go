package opsbackend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Per-surface body caps (spec §4.3).
const (
	versionLimit = 4 << 10
	runningLimit = 256 << 10
	metricsLimit = 8 << 20
	modelsLimit  = 256 << 10
	psLimit      = 256 << 10
)

// requestTimeout bounds one request; the provider's configured timeout is
// never used (spec §4.5).
const requestTimeout = 2 * time.Second

// rootDestination validates a provider base URL for observation before any
// I/O (spec §4.1). A local destination's canonical form must have no path: a
// prefix such as /upstream/<model> would place every ops request under a
// llama-swap route that starts that model, so it is refused, never stripped.
// A remote destination is returned whatever its path: it reads remote, not
// invalid_configuration, and newClient refuses to build a client for it.
func rootDestination(providerName, baseURL string) (provider.Destination, error) {
	d, err := provider.NewDestination(providerName, baseURL)
	if err != nil {
		return provider.Destination{}, newCoded(CodeInvalidConfiguration, "provider base URL is not a valid destination")
	}
	u, err := url.Parse(d.BaseURL())
	if err != nil || (d.IsLocal() && u.Path != "") {
		return provider.Destination{}, newCoded(CodeInvalidConfiguration, "provider base URL has a path prefix")
	}
	return d, nil
}

// client issues allowlisted GETs to one destination under one purpose.
type client struct {
	hc      *http.Client
	gate    *provider.DestinationGate
	purpose string
	dest    provider.Destination
	apiKey  string // sent as a bearer token; never logged or rendered
}

// newClient builds, outer to inner, allowlist -> destination guard on an
// ops-owned single-edge gate with the zero policy (loopback only) -> a
// private stock transport (spec §4.2). The allowlist wraps the guarded
// transport because the guard refuses opaque delegates for loopback
// destinations. A remote destination fails at Install and reads denied.
func newClient(d provider.Destination, apiKey, purpose string, routes []route, refused *atomic.Int64) (*client, error) {
	m, err := provider.NewDestinationManifest(provider.DestinationEdge{Purpose: purpose, Destination: d})
	if err != nil {
		return nil, newCoded(CodeInvalidConfiguration, "destination manifest")
	}
	g := provider.NewDestinationGate()
	var loopbackOnly provider.DestinationPolicy
	if err := g.Install(loopbackOnly, m); err != nil {
		return nil, newCoded(CodeDenied, "destination not admitted")
	}
	// ponytail: a nil Transport makes the guard clone http.DefaultTransport
	// privately (falling back to a fresh one if it was replaced), so no
	// unchecked type assertion on the global is needed here.
	guarded, err := provider.GuardHTTPClient(g, d, &http.Client{Timeout: requestTimeout})
	if err != nil {
		return nil, newCoded(CodeDenied, "destination guard")
	}
	u, err := url.Parse(d.BaseURL())
	if err != nil {
		return nil, newCoded(CodeInvalidConfiguration, "destination URL")
	}
	hc := &http.Client{
		Transport:     newAllowlist(guarded.Transport, u.Scheme, u.Host, routes, refused),
		CheckRedirect: guarded.CheckRedirect,
		Timeout:       requestTimeout,
	}
	return &client{hc: hc, gate: g, purpose: purpose, dest: d, apiKey: apiKey}, nil
}

// get fetches path and returns at most limit bytes of a 2xx body. Errors are
// coded and never carry body bytes, the URL or the API key; caller
// cancellation returns ctx.Err() unclassified.
func (c *client) get(ctx context.Context, surface, path string, limit int64) ([]byte, error) {
	bctx, err := c.gate.Bind(ctx, c.purpose, c.dest.Provider())
	if err != nil {
		return nil, newCoded(CodeDenied, surface)
	}
	req, err := http.NewRequestWithContext(bctx, http.MethodGet, c.dest.BaseURL()+path, nil)
	if err != nil {
		return nil, newCoded(CodeInvalidConfiguration, surface)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, classify(ctx, surface, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, newCoded(CodeUnauthorized, surface)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, newCoded(CodeHTTPStatus, fmt.Sprintf("%s: status %d", surface, resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, classify(ctx, surface, err)
	}
	if int64(len(body)) > limit {
		return nil, newCoded(CodeTooLarge, surface)
	}
	return body, nil
}
