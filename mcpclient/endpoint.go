package mcpclient

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

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
