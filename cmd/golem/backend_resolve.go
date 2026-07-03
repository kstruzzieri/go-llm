package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// validateBaseURLOverride normalizes and validates an explicit backend URL
// override (-base-url flag or GO_LLM_BASE_URL env). source names the origin
// for error messages. The value is the openai-compat server ROOT — the client
// appends /v1 paths itself — so a path that ends in /v1 is rejected with a
// hint; other path prefixes stay allowed for reverse-proxy setups. A query or
// fragment is rejected: the client builds requests by string concatenation
// (base + "/v1/..."), so "http://h:8080?x=1" would swallow the API path into
// the query string and every call would silently hit "/". URLs in error
// messages are userinfo-redacted (never leak credentials into logs).
func validateBaseURLOverride(raw, source string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", fmt.Errorf("golem: %s: value is empty; pass the openai-compat server root, e.g. http://127.0.0.1:8081", source)
	}
	u, err := url.ParseRequestURI(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("golem: %s: URL %q must include scheme and host (server root, e.g. http://127.0.0.1:8081)", source, redactBaseURL(v))
	}
	// ParseRequestURI keeps "#" as a literal path byte (request-URIs carry no
	// fragment), but the client's own url.Parse of the final concatenated URL
	// WOULD treat it as a fragment delimiter — so check the raw value too.
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(v, "#") {
		return "", fmt.Errorf("golem: %s: URL %q must not carry a query or fragment (server root only, e.g. http://127.0.0.1:8081)", source, redactBaseURL(v))
	}
	if p := strings.TrimRight(u.Path, "/"); strings.HasSuffix(p, "/v1") {
		return "", fmt.Errorf("golem: %s: pass the server root without the /v1 suffix (the client appends API paths): got %q", source, redactBaseURL(v))
	}
	return v, nil
}

const (
	backendScanPortLow  = 8080
	backendScanPortHigh = 8090
	// backendProbeTimeout bounds each discovery probe. Loopback-only targets,
	// so a short timeout keeps the worst case — up to 12 probes (configured
	// URL + 11-port band) x 800ms, ~10s worst case — paid only when the
	// configured backend is already broken.
	backendProbeTimeout = 800 * time.Millisecond

	// sourceFlagBaseURL and sourceEnvBaseURL are the explicit-override source
	// labels shared by explicitBaseURL, diagSource, and validateBaseURLOverride
	// call sites, so the label used in diagnostics cannot drift from the label
	// stored on backendResolution.
	sourceFlagBaseURL = "-base-url"
	sourceEnvBaseURL  = "GO_LLM_BASE_URL"
)

// backendProber matches openaicompat.DiscoverBaseURL. A seam so policy tests
// never open sockets.
type backendProber func(ctx context.Context, candidates []string, wantModel string, opts ...openaicompat.ClientOption) (string, error)

// backendResolution is the single resolved backend value shared by provider
// bootstrap and preflight diagnostics, so client behavior and diagnostics
// cannot drift apart (spec: one small value used by both). A zero value means
// "use the configured URL as-is, nothing to report".
type backendResolution struct {
	providerKey string   // openai-compat provider to override; "" => none
	baseURL     string   // override URL; "" => none
	source      string   // sourceFlagBaseURL | sourceEnvBaseURL | "discovered" | ""
	notice      string   // positive startup line ("" => none)
	warns       []string // non-fatal problems to surface
}

// diagSource returns the source label preflight diagnostics should attach to
// the target provider's endpoint, naming explicit overrides only: a failing
// -base-url/GO_LLM_BASE_URL must be called out as such, while a discovered
// URL already got its own startup notice.
func (r backendResolution) diagSource() string {
	switch r.source {
	case sourceFlagBaseURL, sourceEnvBaseURL:
		return r.source
	}
	return ""
}

// backendResolveOpts carries resolveBackend's inputs. lookupEnv is
// os.LookupEnv in production (LookupEnv, not Getenv: a set-but-empty
// GO_LLM_BASE_URL must be a validation error, not silently ignored).
type backendResolveOpts struct {
	flagBaseURL string
	flagSet     bool
	noProbe     bool
	lookupEnv   func(string) (string, bool)
	prober      backendProber
}

// resolveBackend resolves the openai-compat backend URL for the primary agent
// model before provider bootstrap. Precedence is first-explicit-source-wins:
// -base-url flag, then GO_LLM_BASE_URL, both absolute (no probe, no scan);
// otherwise the configured base_url is left alone unless discovery (loopback
// scan 8080..8090, identity-validated) finds the backend elsewhere. All
// failures are non-fatal except explicit-override validation errors; a broken
// backend is ultimately diagnosed by the existing tool-capability preflight.
func resolveBackend(ctx context.Context, cfg *config.Config, o backendResolveOpts) (backendResolution, error) {
	explicitURL, source, err := explicitBaseURL(o.flagBaseURL, o.flagSet, o.lookupEnv)
	if err != nil {
		return backendResolution{}, err
	}

	key, model, ok := openAICompatAgentTarget(cfg)
	if !ok {
		var res backendResolution
		if explicitURL != "" {
			res.warns = append(res.warns,
				source+" ignored: no openai-compat agent provider to apply it to (defaults.agent unset, unresolvable, or primary model's provider is not openai-compat)")
		}
		return res, nil
	}

	if explicitURL != "" {
		return backendResolution{
			providerKey: key,
			baseURL:     explicitURL,
			source:      source,
			notice: fmt.Sprintf("openai-compat backend: using %s from %s (discovery disabled)",
				redactBaseURL(explicitURL), source),
		}, nil
	}

	pc := cfg.Providers[key]
	if o.noProbe || !isLoopbackURL(pc.BaseURL) {
		// -no-probe, or a remote/non-loopback configured URL: never fall
		// forward to a local scan (silently redirecting a hosted provider to
		// a local service with the same model id would mask misconfiguration).
		return backendResolution{}, nil
	}

	candidates := scanCandidates(pc.BaseURL)
	copts := []openaicompat.ClientOption{
		openaicompat.WithHTTPClient(&http.Client{Timeout: backendProbeTimeout}),
	}
	// The provider's API key is deliberately sent to every loopback scan
	// candidate: if the backend moved to another local port but still
	// enforces auth, it must still answer the probe. This is bounded by the
	// literal-loopback gate above (isLoopbackURL) — the scan never leaves
	// 127.0.0.1/localhost, so the key never reaches a non-loopback host.
	// Spec-locked tradeoff; do not gate this on a "trusted candidate" check.
	if pc.APIKey != "" {
		copts = append(copts, openaicompat.WithAPIKey(pc.APIKey))
	}
	hit, derr := o.prober(ctx, candidates, model, copts...)
	if derr != nil {
		return backendResolution{warns: []string{
			fmt.Sprintf("openai-compat backend discovery: %v", derr),
		}}, nil
	}
	if hit == strings.TrimRight(pc.BaseURL, "/") {
		return backendResolution{}, nil // configured URL serves; nothing to do
	}
	return backendResolution{
		providerKey: key,
		baseURL:     hit,
		source:      "discovered",
		notice: fmt.Sprintf("openai-compat backend: resolved to %s (configured %s did not serve %q)",
			redactBaseURL(hit), redactBaseURL(pc.BaseURL), model),
	}, nil
}

// explicitBaseURL applies the explicit-override precedence: -base-url flag
// first, then GO_LLM_BASE_URL. Returns ("", "", nil) when neither is set.
//
// Validation is deliberately fatal even when no openai-compat target exists
// to apply the value to (where a VALID value is merely warn-and-ignored): a
// malformed override is a misconfiguration the user should notice, not one
// that starts failing only on the day the config gains an openai-compat
// provider.
func explicitBaseURL(flagVal string, flagSet bool, lookupEnv func(string) (string, bool)) (val, source string, err error) {
	if flagSet {
		v, err := validateBaseURLOverride(flagVal, sourceFlagBaseURL)
		if err != nil {
			return "", "", err
		}
		return v, sourceFlagBaseURL, nil
	}
	if raw, ok := lookupEnv(sourceEnvBaseURL); ok {
		v, err := validateBaseURLOverride(raw, sourceEnvBaseURL)
		if err != nil {
			return "", "", err
		}
		return v, sourceEnvBaseURL, nil
	}
	return "", "", nil
}

// openAICompatAgentTarget resolves the primary agent-chain model to its
// openai-compat provider: defaults.agent -> RoleFallbackChain[0] -> provider
// key + wire model name. ok=false (discovery and -base-url no-op) when cfg is
// nil, defaults.agent is absent (recommend mode), the chain cannot resolve,
// or the primary's provider is not api_format "openai-compat". Chain errors
// are NOT reported here — resolveAgentChain runs later on the effective
// config and stays the authority on fatal chain problems.
func openAICompatAgentTarget(cfg *config.Config) (providerKey, model string, ok bool) {
	if cfg == nil {
		return "", "", false
	}
	if _, present := cfg.Defaults["agent"]; !present {
		return "", "", false
	}
	chain, err := cfg.RoleFallbackChain("agent")
	if err != nil || len(chain) == 0 {
		return "", "", false
	}
	mk, selOK := parseSelector(chain[0])
	if !selOK {
		return "", "", false
	}
	pc, present := cfg.Providers[mk.Provider]
	if !present || pc.APIFormat != "openai-compat" {
		return "", "", false
	}
	return mk.Provider, mk.Model, true
}

// isLoopbackURL reports whether the URL's host is loopback. Only a literal
// qualifies: "localhost" or an IP for which IsLoopback holds. No DNS lookups
// — a hostname that merely resolves to 127.0.0.1 does not arm the scan.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname() // strips port and IPv6 brackets
	if strings.EqualFold(host, "localhost") {
		// Hostnames are case-insensitive (RFC 4343); "Localhost" must arm
		// the scan the same as "localhost".
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// scanCandidates builds the discovery candidate list: the configured URL
// first (it may serve after a transient failure), then the loopback scan
// band. String-deduped and order-preserving; equivalent hosts spelled
// differently (localhost vs 127.0.0.1) probe twice, which is harmless.
func scanCandidates(configured string) []string {
	seen := make(map[string]bool, backendScanPortHigh-backendScanPortLow+2)
	out := make([]string, 0, backendScanPortHigh-backendScanPortLow+2)
	add := func(u string) {
		u = strings.TrimRight(u, "/")
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	add(configured)
	for p := backendScanPortLow; p <= backendScanPortHigh; p++ {
		add(fmt.Sprintf("http://127.0.0.1:%d", p))
	}
	return out
}
