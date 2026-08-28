package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// destinationTransport is the enforcement layer of #477: the outermost
// http.RoundTripper on a guarded client, bound at construction to ONE gate
// and ONE canonical destination. Every request must carry a capability the
// gate's current snapshot issued for exactly this destination, and must
// target the bound origin under the bound base path — otherwise it is denied
// before the delegate transport, dialer, or proxy is ever invoked.
//
// Purpose scope: the capability authorizes by {snapshot, provider,
// destination}. Sub-requests a provider client issues while serving one
// routed call — a model-list on a cache miss, a retry — share that call's
// capability toward the same destination; the purpose boundary is the edge
// the capability was issued for, not the individual HTTP request.
type destinationTransport struct {
	gate     *DestinationGate
	dest     Destination
	scheme   string // canonical scheme of the bound base URL
	hostPort string // canonical authority, default port elided
	basePath string // canonical escaped base path, "" for root
	delegate http.RoundTripper
}

// RoundTrip enforces capability, origin, and base-path binding, then
// delegates. Denial errors name only the canonical destination and purpose —
// never the request URL, whose query may carry request data (I19).
func (t *destinationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.gate.authorize(req.Context(), t.dest.Provider(), t.dest); err != nil {
		return nil, err
	}
	if !t.targetsBound(req.URL) {
		purpose := ""
		if cap := capabilityFromContext(req.Context()); cap != nil {
			purpose = cap.purpose
		}
		return nil, &DestinationDeniedError{Destination: t.dest, Purpose: purpose}
	}
	return t.delegate.RoundTrip(req)
}

// targetsBound reports whether u stays inside the bound destination:
// same canonical scheme and authority, no userinfo, no dot segments, and a
// path at or under the base path on a segment boundary. "Under" is only
// meaningful on dot-free text, which is why dot segments deny rather than
// resolve.
//
// Known limitation: containment is judged on the canonical text, so a server
// that applies its own non-standard normalization (the Tomcat-style "..;/"
// trick) could map an in-bounds path outside the base path. That crossing
// stays within one admitted ORIGIN and matters only when two providers share
// a host split by base path — both of which the user granted individually —
// so it is accepted rather than guessed at with server-specific rules.
func (t *destinationTransport) targetsBound(u *url.URL) bool {
	if u == nil || u.User != nil || u.Opaque != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != t.scheme {
		return false
	}
	hostPort, err := canonicalAuthority(scheme, u)
	if err != nil || hostPort != t.hostPort {
		return false
	}
	p := u.EscapedPath()
	if hasDotSegment(p) || hasDotSegment(u.Path) {
		return false
	}
	if t.basePath == "" {
		return true
	}
	p = strings.TrimRight(p, "/")
	return p == t.basePath || strings.HasPrefix(p, t.basePath+"/")
}

// canonicalAuthority renders a URL's authority the way destination/v1 does:
// lowercased host, IP literals collapsed to net.ParseIP's form and
// rebracketed, the scheme's default port elided. Zone IDs and empty hosts
// error — unclassifiable is never a match.
func canonicalAuthority(scheme string, u *url.URL) (string, error) {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("empty host")
	}
	if strings.Contains(host, "%") {
		return "", errors.New("zone ID in host")
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != defaultPortForScheme(scheme) {
		host += ":" + port
	}
	return host, nil
}

// lookupIPFunc is the resolver seam for localhost dial validation; tests
// inject one, production uses the default resolver.
type lookupIPFunc func(ctx context.Context, host string) ([]net.IP, error)

// GuardHTTPClient returns an http.Client whose every request is checked by
// the destination guard before any transport work. The guard is the
// OUTERMOST layer: a denied request never reaches the delegate transport,
// a dialer, or a proxy (D13).
//
// The returned client always refuses redirects, same-origin included (D6) —
// an admitted origin must not transfer authority or credentials to a
// Location target. Loopback destinations additionally bypass proxies and,
// for the "localhost" hostname, refuse to dial when the name resolves to
// anything that is not a loopback address (D3) — which requires an
// *http.Transport delegate; a loopback destination over an opaque
// RoundTripper is refused rather than left half-guarded. Remote destinations
// keep the base client's transport as-is, proxy included.
//
// base contributes its Timeout and Transport. A base carrying a cookie Jar
// is refused: the guard cannot vouch for jar-driven behavior.
func GuardHTTPClient(gate *DestinationGate, dest Destination, base *http.Client) (*http.Client, error) {
	return guardHTTPClient(gate, dest, base, nil)
}

func guardHTTPClient(gate *DestinationGate, dest Destination, base *http.Client, lookup lookupIPFunc) (*http.Client, error) {
	if gate == nil {
		return nil, fmt.Errorf("%w: guard requires a gate", ErrDestinationInvalid)
	}
	if dest.IsZero() {
		return nil, fmt.Errorf("%w: guard requires a constructed destination", ErrDestinationInvalid)
	}

	var timeout time.Duration
	var inner http.RoundTripper
	if base != nil {
		if base.Jar != nil {
			return nil, fmt.Errorf("%w: guarded clients do not support cookie jars", ErrDestinationInvalid)
		}
		timeout = base.Timeout
		inner = base.Transport
	}

	// dest.BaseURL is canonical (a fixed point of destination/v1), so this
	// parse cannot fail and its parts need no re-validation.
	u, err := url.Parse(dest.BaseURL())
	if err != nil {
		return nil, fmt.Errorf("%w: destination base URL failed to re-parse", ErrDestinationInvalid)
	}
	scheme := u.Scheme
	hostPort, err := canonicalAuthority(scheme, u)
	if err != nil {
		return nil, fmt.Errorf("%w: destination authority: %s", ErrDestinationInvalid, err)
	}

	if dest.IsLocal() {
		inner, err = loopbackTransport(inner, lookup)
		if err != nil {
			return nil, err
		}
	} else if inner == nil {
		inner = defaultTransportClone()
	}

	guard := &destinationTransport{
		gate:     gate,
		dest:     dest,
		scheme:   scheme,
		hostPort: hostPort,
		basePath: strings.TrimRight(u.EscapedPath(), "/"),
		delegate: inner,
	}
	return &http.Client{
		Transport: guard,
		Timeout:   timeout,
		// Refuse every redirect before the follow-up request is issued;
		// the target — foreign or same-origin — receives zero requests.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("%w: redirect refused by destination guard", ErrDestinationDenied)
		},
	}, nil
}

// loopbackTransport prepares the delegate for a loopback destination: clone
// the *http.Transport (or the default), strip the proxy, and validate
// "localhost" resolution at dial time. An opaque RoundTripper cannot be
// retrofitted with either property, so it is refused — silently passing it
// through would ship an unguarded proxy path under a guaranteed-local label.
func loopbackTransport(inner http.RoundTripper, lookup lookupIPFunc) (*http.Transport, error) {
	var tr *http.Transport
	switch v := inner.(type) {
	case nil:
		tr = defaultTransportClone()
	case *http.Transport:
		tr = v.Clone()
	default:
		return nil, fmt.Errorf("%w: loopback destination requires an *http.Transport delegate", ErrDestinationInvalid)
	}
	tr.Proxy = nil

	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			ips := make([]net.IP, len(addrs))
			for i, a := range addrs {
				ips[i] = a.IP
			}
			return ips, nil
		}
	}
	baseDial := tr.DialContext
	if baseDial == nil {
		baseDial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(host, "localhost") {
			ips, err := lookup(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("provider: destination guard: resolve localhost: %w", err)
			}
			if len(ips) == 0 {
				return nil, errors.New("provider: destination guard: localhost resolved to no addresses")
			}
			// ANY non-loopback mapping fails the whole dial. Filtering down
			// to the loopback half would happily dial through a poisoned
			// hosts file — the presence of an off-host mapping for
			// "localhost" is the tampering signal itself.
			for _, ip := range ips {
				if !ip.IsLoopback() {
					return nil, fmt.Errorf("provider: destination guard: localhost resolved to non-loopback %s; refusing", ip)
				}
			}
			addr = net.JoinHostPort(ips[0].String(), port)
		}
		return baseDial(ctx, network, addr)
	}
	return tr, nil
}

// defaultTransportClone returns a private copy of http.DefaultTransport so
// guard adjustments never mutate shared state.
func defaultTransportClone() *http.Transport {
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		return tr.Clone()
	}
	// http.DefaultTransport replaced by something exotic: fall back to a
	// fresh transport with stdlib-comparable defaults rather than aliasing
	// unknown global state.
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}
