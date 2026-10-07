package opsbackend

import (
	"net/http"
	"strings"
	"sync/atomic"
)

// route is one permitted method and exact path.
type route struct{ method, path string }

// Route tables per adapter state (spec §4.2). They are the only paths golem
// ops ever requests.
var (
	identifyRoutes  = []route{{http.MethodGet, "/api/version"}}
	llamaSwapRoutes = []route{
		{http.MethodGet, "/api/version"},
		{http.MethodGet, "/running"},
		{http.MethodGet, "/api/metrics"},
		{http.MethodGet, "/v1/models"},
	}
	// Ollama answers /api/version from a constant, outside the scheduler
	// locks /api/ps waits on; it is only a liveness probe after a timeout.
	ollamaRoutes = []route{{http.MethodGet, "/api/version"}, {http.MethodGet, "/api/ps"}}
)

// allowlist refuses every request that is not an exact permitted method and
// path on the destination's own scheme and host. It sits outside the
// destination guard, so a refused request reaches no transport at all.
type allowlist struct {
	next    http.RoundTripper
	scheme  string
	host    string
	routes  map[route]bool
	refused *atomic.Int64
}

func newAllowlist(next http.RoundTripper, scheme, host string, routes []route, refused *atomic.Int64) *allowlist {
	set := make(map[route]bool, len(routes))
	for _, r := range routes {
		set[r] = true
	}
	return &allowlist{next: next, scheme: scheme, host: host, routes: set, refused: refused}
}

// RoundTrip implements http.RoundTripper.
func (a *allowlist) RoundTrip(req *http.Request) (*http.Response, error) {
	if !a.permits(req) {
		a.refused.Add(1)
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, ErrRefused
	}
	return a.next.RoundTrip(req)
}

func (a *allowlist) permits(req *http.Request) bool {
	u := req.URL
	if u == nil || u.Scheme != a.scheme || u.Host != a.host {
		return false
	}
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return false
	}
	if req.Host != "" && req.Host != u.Host {
		return false
	}
	// Exact routes never need percent-encoding or a backslash, and either can
	// carry a separator a server reinterprets, so both refuse outright. The
	// backslash test covers RawPath too (spec §4.2: "anywhere in the raw
	// path"), so it does not lean on EscapedPath discarding an invalid one.
	if strings.ContainsRune(u.Path, '\\') || strings.ContainsRune(u.RawPath, '\\') || u.EscapedPath() != u.Path {
		return false
	}
	return a.routes[route{req.Method, u.Path}]
}
