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
// gate issued for exactly this destination and not since revoked, and must
// target the bound origin under the bound base path — otherwise it is denied
// before the delegate transport, dialer, or proxy is ever invoked.
//
// Purpose scope: the capability authorizes by {revocation token, provider,
// destination}. The token is a private per-generation object the gate mints
// on Install and Narrow and drops on Clear, so a capability from another gate
// or a revoked generation names the same strings and still denies, and a
// capability carrying no token never authorizes. An additive Extend keeps the
// token (#376), so a request bound before a model switch survives one.
// Sub-requests a provider client issues while serving one routed call — a
// model-list on a cache miss, a retry — share that call's capability toward
// the same destination; the purpose boundary is the edge the capability was
// issued for, not the individual HTTP request.
type destinationTransport struct {
	gate     *DestinationGate
	dest     Destination
	scheme   string // canonical scheme of the bound base URL
	hostPort string // canonical authority, default port elided
	basePath string // canonical escaped base path, "" for root
	delegate http.RoundTripper
}

// RoundTrip enforces capability, origin, and base-path binding, then
// delegates. Guard-generated denial errors name only the canonical
// destination and purpose, never the request URL; delegate errors are
// returned unchanged.
//
// A denial closes the request body: the http.RoundTripper contract requires
// it on errors too, and http.Client never closes the body after a transport
// error, so skipping it leaks a pipe-backed body's writer goroutine (#655).
// An admitted request hands the body to the delegate unclosed.
func (t *destinationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.admit(req); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return t.delegate.RoundTrip(req)
}

// admit reports why req may not pass the guard, or nil when it may.
func (t *destinationTransport) admit(req *http.Request) error {
	if err := t.gate.authorize(req.Context(), t.dest.Provider(), t.dest); err != nil {
		return err
	}
	// NewRequest populates Host from URL; only a differing value is an override.
	if req.URL == nil || (req.Host != "" && req.Host != req.URL.Host) || !t.targetsBound(req.URL) {
		purpose := ""
		if cap := capabilityFromContext(req.Context()); cap != nil {
			purpose = cap.purpose
		}
		return &DestinationDeniedError{Destination: t.dest, Purpose: purpose}
	}
	return nil
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

// localhostIPs are the addresses a "localhost" destination dials, in order.
// RFC 6761 §6.3 lets an application treat the name as loopback without asking
// the resolver, as browsers do, so no hosts file can redirect a local
// destination or stop it from starting (#665). Local backends commonly bind
// 127.0.0.1 only, so it goes first.
var localhostIPs = []string{"127.0.0.1", "::1"}

// loopbackFallbackDelay is net.Dialer's default RFC 6555 fallback delay.
const loopbackFallbackDelay = 300 * time.Millisecond

// GuardHTTPClient returns an http.Client whose every request is checked by
// the destination guard before any transport work. The guard is the
// OUTERMOST layer: a denied request never reaches the delegate transport,
// a dialer, or a proxy (D13).
//
// The returned client always refuses redirects, same-origin included (D6) —
// an admitted origin must not transfer authority or credentials to a
// Location target. Loopback destinations additionally bypass proxies and
// dial the "localhost" hostname as 127.0.0.1, then ::1, without resolving it
// (D3 as amended by #665) — which requires an *http.Transport delegate; a
// loopback destination over an opaque RoundTripper is refused rather than
// left half-guarded. Remote destinations keep the base client's transport
// as-is, proxy included.
//
// base contributes its Timeout and Transport. A base carrying a cookie Jar
// is refused: the guard cannot vouch for jar-driven behavior.
func GuardHTTPClient(gate *DestinationGate, dest Destination, base *http.Client) (*http.Client, error) {
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
		inner, err = loopbackTransport(inner)
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
// the *http.Transport (or the default), reject TLS dial hooks that bypass
// DialContext, strip the proxy, and dial "localhost" as the loopback
// addresses. An opaque RoundTripper cannot be retrofitted with these
// properties, so it is refused rather than treated as guaranteed-local.
func loopbackTransport(inner http.RoundTripper) (*http.Transport, error) {
	var tr *http.Transport
	switch v := inner.(type) {
	case nil:
		tr = defaultTransportClone()
	case *http.Transport:
		tr = v.Clone()
	default:
		return nil, fmt.Errorf("%w: loopback destination requires an *http.Transport delegate", ErrDestinationInvalid)
	}
	if tr.DialTLSContext != nil || tr.DialTLS != nil { //nolint:staticcheck // DialTLS is deprecated but still bypasses DialContext when set.
		return nil, fmt.Errorf("%w: loopback destination does not support custom TLS dial hooks", ErrDestinationInvalid)
	}
	tr.Proxy = nil

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
			return dialLoopback(ctx, baseDial, network, port)
		}
		return baseDial(ctx, network, addr)
	}
	return tr, nil
}

// dialLoopback dials localhostIPs the way net.Dialer dials a dual-stack name
// (RFC 6555): each address starts when the previous one fails or after
// loopbackFallbackDelay, the first connection wins, and the rest are canceled
// and closed. The backend may listen on either stack, and a dropped attempt
// must not use up the request.
func dialLoopback(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), network, port string) (net.Conn, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	results := make(chan result, len(localhostIPs)) // buffered: no attempt blocks after a winner returns
	next := 0
	start := func() {
		addr := net.JoinHostPort(localhostIPs[next], port)
		next++
		go func() {
			conn, err := dial(ctx, network, addr)
			results <- result{conn, err}
		}()
	}
	start()
	fallback := time.NewTimer(loopbackFallbackDelay)
	defer fallback.Stop()
	var errs []error
	for pending := 1; pending > 0; {
		select {
		case <-fallback.C:
			if next < len(localhostIPs) {
				start()
				pending++
			}
		case r := <-results:
			pending--
			if r.err == nil {
				go func(n int) { // close any attempt that connects after the winner
					for ; n > 0; n-- {
						if late := <-results; late.conn != nil {
							_ = late.conn.Close()
						}
					}
				}(pending)
				return r.conn, nil
			}
			errs = append(errs, r.err)
			if next < len(localhostIPs) {
				start()
				pending++
				fallback.Reset(loopbackFallbackDelay)
			}
		}
	}
	return nil, errors.Join(errs...)
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
