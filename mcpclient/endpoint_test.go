package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func mustEndpoint(t *testing.T, raw string) endpoint {
	t.Helper()
	e, err := canonicalEndpoint(raw)
	if err != nil {
		t.Fatalf("canonicalEndpoint(%q): %v", raw, err)
	}
	return e
}

// dump lists every field: endpoint's Format renders only the origin, so %v
// would hide exactly what a failing comparison needs to show.
func dump(e endpoint) string {
	return fmt.Sprintf("{origin=%q path=%q query=%q forceQuery=%t}", e.origin, e.path, e.rawQuery, e.forceQuery)
}

func TestCanonicalEndpointEquivalences(t *testing.T) {
	for _, pair := range [][2]string{
		{"HTTPS://Example.COM:443/mcp", "https://example.com/mcp"},
		{"http://example.com:80/mcp", "http://example.com/mcp"},
		{"http://[0:0:0:0:0:0:0:1]:8080/mcp", "http://[::1]:8080/mcp"},
		{"https://example.com", "https://example.com/"},
	} {
		if a, b := mustEndpoint(t, pair[0]), mustEndpoint(t, pair[1]); a != b {
			t.Errorf("%q and %q canonicalize differently: %s vs %s", pair[0], pair[1], dump(a), dump(b))
		}
	}
	// ForceQuery with a non-empty query serializes as "?a", exactly like the parsed form.
	built, err := endpointFromURL(&url.URL{Scheme: "https", Host: "example.com", Path: "/mcp", RawQuery: "a", ForceQuery: true})
	if err != nil {
		t.Fatalf("endpointFromURL(forced non-empty query): %v", err)
	}
	if want := mustEndpoint(t, "https://example.com/mcp?a"); built != want {
		t.Errorf("field-built forced query = %+v, want %s", dump(built), dump(want))
	}
	if got := mustEndpoint(t, "HTTPS://Example.COM:443/mcp?token=a").sdkURL(); got != "https://example.com/mcp?token=a" {
		t.Fatalf("canonical string = %q, want https://example.com/mcp?token=a", got)
	}
}

func TestCanonicalEndpointDistinctions(t *testing.T) {
	for _, pair := range [][2]string{
		{"https://example.com/mcp", "https://example.com/mcp/"},
		{"https://example.com/a%2Fb", "https://example.com/a%2fb"},
		{"https://example.com/mcp?a=1&b=2", "https://example.com/mcp?b=2&a=1"},
		{"https://example.com/mcp?", "https://example.com/mcp"},
		{"https://example.com:8443/mcp", "https://example.com/mcp"},
		{"https://example.com./mcp", "https://example.com/mcp"},
		{"http://example.com/mcp", "https://example.com/mcp"},
	} {
		if a, b := mustEndpoint(t, pair[0]), mustEndpoint(t, pair[1]); a == b {
			t.Errorf("%q and %q canonicalize to the same endpoint", pair[0], pair[1])
		}
	}
}

func TestCanonicalEndpointRejects(t *testing.T) {
	for _, raw := range []string{
		"https://user:canary-pw@example.com/mcp",
		"https://example.com/mcp#canary-frag",
		"https://example.com/mcp#",
		"ftp://example.com/mcp",
		"/relative/mcp",
		"https:///mcp",
		"https:canary-opaque",
		"https://İ.example/mcp",
		"https://%C4%B0.example/mcp",
		"https://[fe80::1%25en0]/mcp",
		"https://example.com/a/../mcp",
		"https://example.com/./mcp",
		"https://example.com/a/%2e%2e/mcp",
		"https://example.com/a%2F..%2Fmcp",
		"https://example.com/a%5Cb",
	} {
		_, err := canonicalEndpoint(raw)
		if err == nil {
			t.Errorf("canonicalEndpoint(%q) accepted", raw)
			continue
		}
		if strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "İ") {
			t.Errorf("canonicalEndpoint(%q) error echoes input: %v", raw, err)
		}
	}
	// Request URLs built field by field, as a hostile caller could.
	for name, u := range map[string]*url.URL{
		"slashless path":        {Scheme: "https", Host: "example.com", Path: "mcp"},
		"opaque path":           {Scheme: "https", Host: "example.com", Opaque: "/evil"},
		"opaque authority":      {Scheme: "https", Host: "example.com", Opaque: "//evil.example/x"},
		"fragment field":        {Scheme: "https", Host: "example.com", Path: "/mcp", Fragment: "x"},
		"host with slash":       {Scheme: "https", Host: "example.com/", Path: "/mcp"},
		"host with userinfo at": {Scheme: "https", Host: "example.com@evil.example", Path: "/mcp"},
		"raw fragment field":    {Scheme: "https", Host: "example.com", Path: "/mcp", RawFragment: "x"},
	} {
		if _, err := endpointFromURL(u); err == nil {
			t.Errorf("field-built %s accepted", name)
		}
	}
}

func TestCanonicalEndpointIsFixedPoint(t *testing.T) {
	for _, raw := range []string{
		"HTTPS://Example.COM:443/mcp?token=a",
		"https://example.com/a%2fb",
		"https://example.com/mcp?",
		"http://[0:0:0:0:0:0:0:1]:8080/x/",
		"https://example.com",
	} {
		first := mustEndpoint(t, raw)
		if again := mustEndpoint(t, first.sdkURL()); again != first {
			t.Errorf("not a fixed point: %q -> %s -> %s", raw, dump(first), dump(again))
		}
	}
}

func TestEndpointNeverRendersPathOrQuery(t *testing.T) {
	e := mustEndpoint(t, "https://example.com/mcp?token=canary")
	got := fmt.Sprintf("%v|%+v|%#v|%s", e, e, e, e)
	if strings.Contains(got, "canary") || strings.Contains(got, "/mcp") {
		t.Errorf("formatted endpoint leaks path or query: %q", got)
	}
	if got := fmt.Sprint(e); got != "https://example.com" {
		t.Errorf("fmt.Sprint(endpoint) = %q, want https://example.com", got)
	}
}

func TestEndpointGuardRefusesBeforeDelegate(t *testing.T) {
	admitted := mustEndpoint(t, "https://example.com/mcp?token=canary")
	var delegated atomic.Int32
	guard := endpointGuard{admitted: admitted, refusals: new(httpRefusals), next: mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		delegated.Add(1)
		return mcpHTTPResponse(req, http.StatusOK, ""), nil
	})}
	ok, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp?token=canary", nil)
	if _, err := guard.RoundTrip(ok); err != nil || delegated.Load() != 1 || guard.refusals.destination.Load() {
		t.Fatalf("exact request = (%v, %d delegated), want allowed", err, delegated.Load())
	}
	hostOverride, _ := http.NewRequest(http.MethodPost, "https://example.com/mcp?token=canary", nil)
	hostOverride.Host = "evil.example"
	for name, req := range map[string]*http.Request{
		"path":   mustRequest(t, "https://example.com/other?token=canary"),
		"query":  mustRequest(t, "https://example.com/mcp?token=other"),
		"port":   mustRequest(t, "https://example.com:8443/mcp?token=canary"),
		"host":   hostOverride,
		"opaque": {Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "evil.example", Opaque: "//example.com/mcp", RawQuery: "token=canary"}, Header: http.Header{}},
		"slash":  {Method: http.MethodPost, URL: &url.URL{Scheme: "https", Host: "example.com", Path: "mcp", RawQuery: "token=canary"}, Header: http.Header{}},
	} {
		// The opaque fixture is only meaningful if its string form is the
		// admitted endpoint: then only a field-based guard can refuse it.
		if name == "opaque" && req.URL.String() != admitted.sdkURL() {
			t.Fatalf("opaque fixture stringifies to %q, want %q", req.URL.String(), admitted.sdkURL())
		}
		body := &closeTrackingBody{Reader: strings.NewReader("{}")}
		req.Body = body
		if _, err := guard.RoundTrip(req); !errors.Is(err, errDestinationRefused) || !body.closed {
			t.Errorf("%s request = (%v, body closed %t), want errDestinationRefused and a closed body", name, err, body.closed)
		}
	}
	if delegated.Load() != 1 || !guard.refusals.destination.Load() {
		t.Fatalf("refused requests reached the delegate (%d) or were not recorded", delegated.Load())
	}
}

func mustRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestHTTPClientRefusesEveryRedirect(t *testing.T) {
	var elsewhere atomic.Int32
	// Each case redirects exactly once: if a mutation allows redirects, a
	// same-URL Location then gets 200 instead of looping forever (a custom
	// CheckRedirect replaces net/http's default ten-hop limit).
	var redirected atomic.Bool
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/mcp" {
			elsewhere.Add(1)
			return mcpHTTPResponse(req, http.StatusOK, ""), nil
		}
		resp := mcpHTTPResponse(req, http.StatusOK, "")
		if !redirected.Swap(true) {
			resp.StatusCode = statusFrom(req)
			resp.Header.Set("Location", req.Header.Get("X-Location"))
		}
		return resp, nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	tr, refusals := newHTTPTransport(mustEndpoint(t, "https://mcp.invalid/mcp"))
	client := tr.(*gomcp.StreamableClientTransport).HTTPClient
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, location := range []string{"/elsewhere", "https://mcp.invalid/mcp"} {
				redirected.Store(false)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				// strings.Reader sets GetBody, as the SDK's bytes.Reader does;
				// without it net/http silently declines to follow a 307/308.
				req, _ := http.NewRequestWithContext(ctx, method, "https://mcp.invalid/mcp", strings.NewReader("{}"))
				req.Header.Set("X-Status", itoa(status))
				req.Header.Set("X-Location", location)
				resp, err := client.Do(req)
				cancel()
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if !errors.Is(err, errRedirectRefused) {
					t.Errorf("%s %d -> %s: err = %v, want errRedirectRefused", method, status, location, err)
				}
			}
		}
	}
	if elsewhere.Load() != 0 || !refusals.redirect.Load() {
		t.Fatalf("redirect target requests = %d (want 0); recorded = %t", elsewhere.Load(), refusals.redirect.Load())
	}
}

func TestHTTPTransportKeepsDefaultClientTimeout(t *testing.T) {
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Timeout: 7 * time.Second, Jar: noJar{}}
	t.Cleanup(func() { http.DefaultClient = originalClient })
	tr, _ := newHTTPTransport(mustEndpoint(t, "https://mcp.invalid/mcp"))
	client := tr.(*gomcp.StreamableClientTransport).HTTPClient
	if client.Timeout != 7*time.Second || client.Jar != nil {
		t.Fatalf("client = (timeout %s, jar %v), want (7s, nil)", client.Timeout, client.Jar)
	}
}

// noJar is a do-nothing cookie jar, only to prove newHTTPTransport drops jars.
type noJar struct{}

func (noJar) SetCookies(*url.URL, []*http.Cookie) {}
func (noJar) Cookies(*url.URL) []*http.Cookie     { return nil }

func statusFrom(req *http.Request) int {
	n := 0
	for _, c := range req.Header.Get("X-Status") {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestAdmissionFailureNamesHTTPRefusals(t *testing.T) {
	for cause, want := range map[error]string{
		errRedirectRefused:    "redirect_refused",
		errDestinationRefused: "destination_refused",
	} {
		// Wrapped as net/http's client and then the SDK deliver it.
		err := fmt.Errorf("calling initialize: %w", &url.Error{Op: "Post", URL: "https://example.com/mcp", Err: cause})
		if got := admissionFailure("fs", "unavailable", err).Reason; got != want {
			t.Errorf("admissionFailure(%v).Reason = %q, want %q", cause, got, want)
		}
	}
}

// discover names a recorded refusal even when the SDK flattens the cause
// (spec F9): the dial error here carries no sentinel.
func TestDiscoverNamesRecordedRefusal(t *testing.T) {
	redirect, destination := new(httpRefusals), new(httpRefusals)
	redirect.redirect.Store(true)
	destination.destination.Store(true)
	for want, refusals := range map[string]*httpRefusals{
		"redirect_refused":    redirect,
		"destination_refused": destination,
		"unavailable":         nil,
	} {
		t.Run(want, func(t *testing.T) {
			p := preparedServer{alias: "fs", transport: &failingTransport{err: errors.New("flattened")}, refusals: refusals}
			_, _, _, _, err := discover(context.Background(), Implementation{Name: "test"}, p)
			var failure *AdmissionError
			if !errors.As(err, &failure) || failure.Reason != want {
				t.Fatalf("discover = %v, want reason %s", err, want)
			}
		})
	}
}
