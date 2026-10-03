package mcpclient

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
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
