package opsbackend

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
)

type acceptRT struct{ calls int }

func (a *acceptRT) RoundTrip(*http.Request) (*http.Response, error) {
	a.calls++
	return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
}

func validURL(path string) *url.URL {
	return &url.URL{Scheme: "http", Host: "127.0.0.1:8090", Path: path}
}

func TestAllowlistPermitsExactRoutes(t *testing.T) {
	for _, path := range []string{"/api/version", "/running", "/api/metrics", "/v1/models"} {
		next := &acceptRT{}
		var refused atomic.Int64
		a := newAllowlist(next, "http", "127.0.0.1:8090", llamaSwapRoutes, &refused)
		resp, err := a.RoundTrip(&http.Request{Method: http.MethodGet, URL: validURL(path), Header: http.Header{}})
		if err != nil || resp == nil || next.calls != 1 || refused.Load() != 0 {
			t.Fatalf("%s: err=%v calls=%d refused=%d", path, err, next.calls, refused.Load())
		}
	}
}

func TestAllowlistRefusesBeforeDelegate(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func() *http.Request
	}{
		{"query", func() *http.Request { u := validURL("/running"); u.RawQuery = "model=x"; return get(u) }},
		{"force query", func() *http.Request { u := validURL("/running"); u.ForceQuery = true; return get(u) }},
		{"fragment", func() *http.Request { u := validURL("/running"); u.Fragment = "x"; return get(u) }},
		{"raw fragment", func() *http.Request { u := validURL("/running"); u.RawFragment = "x"; return get(u) }},
		{"opaque", func() *http.Request { u := validURL("/running"); u.Opaque = "//127.0.0.1:8090/running"; return get(u) }},
		{"userinfo", func() *http.Request { u := validURL("/running"); u.User = url.User("a"); return get(u) }},
		{"encoded slash", func() *http.Request { u := validURL("/api/metrics"); u.RawPath = "/api%2Fmetrics"; return get(u) }},
		{"encoded dot", func() *http.Request { u := validURL("/v1/../props"); u.RawPath = "/v1/%2e%2e/props"; return get(u) }},
		{"backslash", func() *http.Request { return get(validURL("/v1\\models")) }},
		// EscapedPath discards an invalid RawPath, so only the raw-path
		// backslash test refuses this one.
		{"raw path backslash", func() *http.Request { u := validURL("/running"); u.RawPath = "/running\\"; return get(u) }},
		{"double slash", func() *http.Request { return get(validURL("//running")) }},
		{"dot segment", func() *http.Request { return get(validURL("/v1/../running")) }},
		{"trailing slash", func() *http.Request { return get(validURL("/running/")) }},
		{"host override", func() *http.Request { r := get(validURL("/running")); r.Host = "127.0.0.1:9999"; return r }},
		{"wrong scheme", func() *http.Request { u := validURL("/running"); u.Scheme = "https"; return get(u) }},
		{"wrong host", func() *http.Request { u := validURL("/running"); u.Host = "127.0.0.1:9999"; return get(u) }},
		{"post", func() *http.Request { r := get(validURL("/running")); r.Method = http.MethodPost; return r }},
		{"head", func() *http.Request { r := get(validURL("/running")); r.Method = http.MethodHead; return r }},
		{"lowercase method", func() *http.Request { r := get(validURL("/running")); r.Method = "get"; return r }},
		{"props", func() *http.Request { return get(validURL("/props")) }},
		{"chat", func() *http.Request { return get(validURL("/v1/chat/completions")) }},
		{"upstream", func() *http.Request { return get(validURL("/upstream/m/props")) }},
		{"unload", func() *http.Request { return get(validURL("/unload")) }},
		{"api unload", func() *http.Request { return get(validURL("/api/models/unload")) }},
		{"nil url", func() *http.Request { return &http.Request{Method: http.MethodGet, Header: http.Header{}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &acceptRT{}
			var refused atomic.Int64
			a := newAllowlist(next, "http", "127.0.0.1:8090", llamaSwapRoutes, &refused)
			resp, err := a.RoundTrip(tc.req())
			if !errors.Is(err, ErrRefused) || resp != nil {
				t.Fatalf("RoundTrip = %v, %v; want nil, ErrRefused", resp, err)
			}
			if next.calls != 0 || refused.Load() != 1 {
				t.Fatalf("delegate calls = %d, refused = %d; want 0, 1", next.calls, refused.Load())
			}
		})
	}
}

func TestIdentifyStateAllowsOnlyVersion(t *testing.T) {
	next := &acceptRT{}
	var refused atomic.Int64
	a := newAllowlist(next, "http", "127.0.0.1:8090", identifyRoutes, &refused)
	if _, err := a.RoundTrip(get(validURL("/running"))); !errors.Is(err, ErrRefused) {
		t.Fatalf("identify state allowed /running: %v", err)
	}
	if _, err := a.RoundTrip(get(validURL("/api/version"))); err != nil || next.calls != 1 {
		t.Fatalf("identify state refused /api/version: %v calls=%d", err, next.calls)
	}
}

// TestRouteTablesAreExact pins every adapter state's table. Requests are built
// with http.NewRequest, as the client builds them, so req.Host equals URL.Host.
func TestRouteTablesAreExact(t *testing.T) {
	paths := []string{"/api/version", "/running", "/api/metrics", "/v1/models", "/api/ps"}
	for _, tc := range []struct {
		name   string
		routes []route
		want   []string
	}{
		{"identify", identifyRoutes, []string{"/api/version"}},
		{"llama-swap", llamaSwapRoutes, []string{"/api/version", "/running", "/api/metrics", "/v1/models"}},
		{"ollama", ollamaRoutes, []string{"/api/ps"}},
	} {
		// The probes below only try GET on known paths; this pin also
		// catches an added method or an unprobed path (spec §4.2: GET only).
		pin := make([]route, len(tc.want))
		for i, p := range tc.want {
			pin[i] = route{http.MethodGet, p}
		}
		if !slices.Equal(tc.routes, pin) {
			t.Errorf("%s routes = %v; want exactly %v", tc.name, tc.routes, pin)
		}
		for _, p := range paths {
			req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8090"+p, nil)
			if err != nil {
				t.Fatal(err)
			}
			next := &acceptRT{}
			var refused atomic.Int64
			_, err = newAllowlist(next, "http", "127.0.0.1:8090", tc.routes, &refused).RoundTrip(req)
			if permitted := err == nil && next.calls == 1; permitted != slices.Contains(tc.want, p) {
				t.Errorf("%s state, GET %s: permitted = %t (err=%v)", tc.name, p, permitted, err)
			}
		}
	}
}

type closeTracker struct{ closed bool }

func (c *closeTracker) Read([]byte) (int, error) { return 0, io.EOF }
func (c *closeTracker) Close() error             { c.closed = true; return nil }

// TestAllowlistClosesRefusedBody pins the RoundTripper contract: the body is
// closed even when the request never reaches the delegate.
func TestAllowlistClosesRefusedBody(t *testing.T) {
	body := &closeTracker{}
	r := get(validURL("/running"))
	r.Method, r.Body = http.MethodPost, body
	var refused atomic.Int64
	_, err := newAllowlist(&acceptRT{}, "http", "127.0.0.1:8090", llamaSwapRoutes, &refused).RoundTrip(r)
	if !errors.Is(err, ErrRefused) || !body.closed {
		t.Fatalf("refused POST: err=%v, body closed=%t; want ErrRefused, true", err, body.closed)
	}
}

func get(u *url.URL) *http.Request {
	return &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}
}
