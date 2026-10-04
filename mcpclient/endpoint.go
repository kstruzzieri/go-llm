package mcpclient

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/provider"
)

// endpoint is one admitted streamable-HTTP endpoint (spec §5.5). Its path and
// query can carry credentials: sdkURL is for the SDK and comparison, never for
// display, and Format keeps every fmt verb to the origin. The struct is
// comparable; equal values are the same endpoint.
type endpoint struct {
	origin     string // canonical scheme://host[:port]
	path       string // escaped path, byte-for-byte; "/" when empty
	rawQuery   string
	forceQuery bool // "?" with an empty query
}

// sdkURL is the canonical string handed to the SDK. It carries the path and
// query verbatim, so it must never be rendered.
func (e endpoint) sdkURL() string {
	s := e.origin + e.path
	if e.rawQuery != "" || e.forceQuery {
		s += "?" + e.rawQuery
	}
	return s
}

// Format renders only the origin under every fmt verb; without it fmt would
// print the path and query (String, or the unexported fields). Value receiver
// so values and pointers both implement fmt.Formatter.
func (e endpoint) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, e.origin)
}

// canonicalEndpoint parses an operator-supplied endpoint. Errors never contain
// the input.
func canonicalEndpoint(raw string) (endpoint, error) {
	if strings.Contains(raw, "#") {
		return endpoint{}, errors.New("mcpclient: endpoint must not carry a fragment")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return endpoint{}, errors.New("mcpclient: endpoint is not a URL")
	}
	return endpointFromURL(u)
}

// endpointFromURL judges structured URL fields, never a string form, so the
// request guard and admission share one rule. u.Host must be a bare
// host[:port]: the origin is rebuilt from text and re-parsed, so a Host that
// smuggles a path or authority delimiter is rejected rather than trimmed into
// an admitted origin. Origins reuse provider's canonicalization only after
// non-ASCII hosts are rejected, so lowercasing cannot disagree with the HTTP
// client's IDNA handling.
func endpointFromURL(u *url.URL) (endpoint, error) {
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme != "http" && scheme != "https":
		return endpoint{}, errors.New("mcpclient: endpoint scheme must be http or https")
	case u.Opaque != "":
		return endpoint{}, errors.New("mcpclient: endpoint must not be opaque")
	case u.User != nil:
		return endpoint{}, errors.New("mcpclient: endpoint must not carry userinfo")
	case u.Fragment != "" || u.RawFragment != "":
		return endpoint{}, errors.New("mcpclient: endpoint must not carry a fragment")
	}
	host := u.Hostname()
	if host == "" {
		return endpoint{}, errors.New("mcpclient: endpoint must include a host")
	}
	for i := 0; i < len(host); i++ {
		if host[i] >= utf8.RuneSelf {
			return endpoint{}, errors.New("mcpclient: endpoint host must be ASCII; use the xn-- form")
		}
	}
	if strings.Contains(host, "%") {
		return endpoint{}, errors.New("mcpclient: endpoint host must not carry a zone ID")
	}
	if strings.ContainsAny(u.Host, "/\\?#@") {
		return endpoint{}, errors.New("mcpclient: endpoint host is invalid")
	}
	dest, err := provider.NewDestination("mcp", scheme+"://"+u.Host)
	if err != nil {
		return endpoint{}, errors.New("mcpclient: endpoint origin is invalid")
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return endpoint{}, errors.New("mcpclient: endpoint path must be absolute")
	}
	if strings.Contains(u.Path, `\`) {
		return endpoint{}, errors.New("mcpclient: endpoint path must not contain a backslash")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return endpoint{}, errors.New("mcpclient: endpoint path must not contain dot segments")
		}
	}
	return endpoint{origin: dest.BaseURL(), path: path, rawQuery: u.RawQuery, forceQuery: u.ForceQuery && u.RawQuery == ""}, nil
}

var (
	errRedirectRefused    = errors.New("mcpclient: HTTP redirect refused")
	errDestinationRefused = errors.New("mcpclient: HTTP request outside the admitted endpoint")
)

// httpRefusals records policy refusals on one transport, so admission can
// name them even where the SDK flattens the error chain.
type httpRefusals struct {
	redirect, destination atomic.Bool
}

// endpointGuard refuses every request that is not exactly the admitted
// endpoint, judged on URL fields, before any dial.
type endpointGuard struct {
	admitted endpoint
	refusals *httpRefusals
	next     http.RoundTripper
}

func (g endpointGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	allowed := req.URL != nil && (req.Host == "" || req.Host == req.URL.Host)
	if allowed {
		got, err := endpointFromURL(req.URL)
		allowed = err == nil && got == g.admitted
	}
	if !allowed {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		g.refusals.destination.Store(true)
		return nil, errDestinationRefused
	}
	return g.next.RoundTrip(req)
}

// newHTTPTransport builds the SDK transport for an admitted endpoint. Every
// redirect is refused, same-origin included: an admitted endpoint must not
// hand a request, its body or credentials to a Location target. The chain is
// guard, then httpSessionTransport, then the copied default client's
// Transport (http.DefaultTransport when nil). A host-installed
// http.DefaultClient.Transport sits below the guard and CheckRedirect, so it
// is trusted not to follow redirects or rewrite URLs: the same trust as
// Golem's own proxy environment.
func newHTTPTransport(ep endpoint) (gomcp.Transport, *httpRefusals) {
	refusals := new(httpRefusals)
	// Copy the default client, as before, so its Timeout still applies; drop
	// any cookie jar, whose behavior the guard cannot vouch for.
	client := *http.DefaultClient
	client.Jar = nil
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = endpointGuard{admitted: ep, refusals: refusals, next: httpSessionTransport{RoundTripper: base}}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		refusals.redirect.Store(true)
		return errRedirectRefused
	}
	// DisableStandaloneSSE: request/response only; no standalone SSE stream.
	return &gomcp.StreamableClientTransport{Endpoint: ep.sdkURL(), HTTPClient: &client, DisableStandaloneSSE: true}, refusals
}
