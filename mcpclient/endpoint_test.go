package mcpclient

import (
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

func TestCanonicalEndpointEquivalences(t *testing.T) {
	for _, pair := range [][2]string{
		{"HTTPS://Example.COM:443/mcp", "https://example.com/mcp"},
		{"http://example.com:80/mcp", "http://example.com/mcp"},
		{"http://[0:0:0:0:0:0:0:1]:8080/mcp", "http://[::1]:8080/mcp"},
		{"https://example.com", "https://example.com/"},
	} {
		if a, b := mustEndpoint(t, pair[0]), mustEndpoint(t, pair[1]); a != b {
			t.Errorf("%q and %q canonicalize differently: %+v vs %+v", pair[0], pair[1], a, b)
		}
	}
	if got := mustEndpoint(t, "HTTPS://Example.COM:443/mcp?token=a").String(); got != "https://example.com/mcp?token=a" {
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
	// A request URL built field by field, as a hostile caller could.
	if _, err := endpointFromURL(&url.URL{Scheme: "https", Host: "example.com", Path: "mcp"}); err == nil {
		t.Error("slashless request path accepted")
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
		if again := mustEndpoint(t, first.String()); again != first {
			t.Errorf("not a fixed point: %q -> %+v -> %+v", raw, first, again)
		}
	}
}
