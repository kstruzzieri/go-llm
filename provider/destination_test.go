package provider

import (
	"errors"
	"strings"
	"testing"
)

// canaryURL carries every component D2 requires us to REJECT rather than
// strip, plus a credential-shaped secret. M20/I19: no diagnostic may echo it.
const (
	canarySecret = "SECRET-CANARY-91731"
	canaryURL    = "https://user:" + canarySecret + "@example.com/v1?token=" + canarySecret + "#frag"
)

func TestNewDestinationCanonicalizes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"already canonical", "https://api.example.com/v1", "https://api.example.com/v1"},
		{"uppercase scheme", "HTTPS://api.example.com/v1", "https://api.example.com/v1"},
		{"uppercase host", "https://API.Example.COM/v1", "https://api.example.com/v1"},
		{"default https port elided", "https://api.example.com:443/v1", "https://api.example.com/v1"},
		{"default http port elided", "http://api.example.com:80/v1", "http://api.example.com/v1"},
		{"nondefault port kept", "https://api.example.com:8443/v1", "https://api.example.com:8443/v1"},
		{"cross-scheme port kept", "https://api.example.com:80/v1", "https://api.example.com:80/v1"},
		{"trailing slash trimmed", "https://api.example.com/v1/", "https://api.example.com/v1"},
		{"root path emptied", "https://api.example.com/", "https://api.example.com"},
		{"no path", "https://api.example.com", "https://api.example.com"},
		{"multi-segment path kept", "https://opencode.ai/zen/go", "https://opencode.ai/zen/go"},
		{"escaped path byte preserved", "https://api.example.com/a%2Fb", "https://api.example.com/a%2Fb"},
		{"loopback ipv4", "http://127.0.0.1:8090", "http://127.0.0.1:8090"},
		{"ipv6 literal rebracketed", "http://[::1]:8090", "http://[::1]:8090"},
		{"ipv6 longhand canonicalized", "http://[0:0:0:0:0:0:0:1]:8090", "http://[::1]:8090"},
		{"ipv6 uppercase canonicalized", "http://[2001:DB8::1]/v1", "http://[2001:db8::1]/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDestination("p", tt.raw)
			if err != nil {
				t.Fatalf("NewDestination(%q) error = %v, want nil", tt.name, err)
			}
			if d.BaseURL() != tt.want {
				t.Errorf("BaseURL() = %q, want %q", d.BaseURL(), tt.want)
			}
			if d.Provider() != "p" {
				t.Errorf("Provider() = %q, want %q", d.Provider(), "p")
			}
		})
	}
}

// The canonical form must re-normalize to itself. Without this a "canonical"
// key could differ from the key derived on a later pass, and a grant would
// silently fail to match the destination it was issued for.
func TestNewDestinationCanonicalFormIsFixedPoint(t *testing.T) {
	raws := []string{
		"HTTPS://API.Example.COM:443/v1/",
		"http://[0:0:0:0:0:0:0:1]:8090",
		"http://127.0.0.1:8090/",
		"https://opencode.ai/zen/go",
		"https://api.example.com/a%2Fb",
		"http://api.example.com:80",
	}
	for _, raw := range raws {
		t.Run(raw, func(t *testing.T) {
			first, err := NewDestination("p", raw)
			if err != nil {
				t.Fatalf("NewDestination: %v", err)
			}
			second, err := NewDestination("p", first.BaseURL())
			if err != nil {
				t.Fatalf("re-normalizing %q: %v", first.BaseURL(), err)
			}
			if second != first {
				t.Errorf("not a fixed point: %q -> %q -> %q", raw, first.BaseURL(), second.BaseURL())
			}
		})
	}
}

// M9/M10, I8. Merging is only safe where two spellings are provably the same
// endpoint. Everything else must stay distinct, because a merge silently
// transfers a grant from one destination to another.
func TestNewDestinationMergesOnlyProvablyIdenticalSpellings(t *testing.T) {
	t.Run("intended merges collapse", func(t *testing.T) {
		groups := [][]string{
			{"https://API.Example.com/v1", "https://api.example.com/v1", "HTTPS://api.example.COM/v1"},
			{"https://api.example.com/v1", "https://api.example.com:443/v1", "https://api.example.com/v1/"},
			{"http://[::1]/v1", "http://[0:0:0:0:0:0:0:1]/v1"},
		}
		for _, group := range groups {
			first, err := NewDestination("p", group[0])
			if err != nil {
				t.Fatalf("NewDestination(%q): %v", group[0], err)
			}
			for _, raw := range group[1:] {
				got, err := NewDestination("p", raw)
				if err != nil {
					t.Fatalf("NewDestination(%q): %v", raw, err)
				}
				if got != first {
					t.Errorf("%q and %q must be one destination, got %q vs %q",
						group[0], raw, first.BaseURL(), got.BaseURL())
				}
			}
		}
	})

	t.Run("differing components stay distinct", func(t *testing.T) {
		distinct := []string{
			"https://api.example.com/v1",
			"http://api.example.com/v1",       // scheme
			"https://api.example.com:8443/v1", // port
			"https://api.example.com/v2",      // path
			"https://api.example.com",         // no path
			"https://other.example.com/v1",    // host
			"https://api.example.com/v1/sub",  // deeper path
		}
		seen := make(map[string]string, len(distinct))
		for _, raw := range distinct {
			d, err := NewDestination("p", raw)
			if err != nil {
				t.Fatalf("NewDestination(%q): %v", raw, err)
			}
			if prev, dup := seen[d.BaseURL()]; dup {
				t.Errorf("over-merge: %q and %q both canonicalize to %q", prev, raw, d.BaseURL())
			}
			seen[d.BaseURL()] = raw
		}
	})

	t.Run("provider participates in identity", func(t *testing.T) {
		a, err := NewDestination("alpha", "https://api.example.com/v1")
		if err != nil {
			t.Fatal(err)
		}
		b, err := NewDestination("beta", "https://api.example.com/v1")
		if err != nil {
			t.Fatal(err)
		}
		if a == b {
			t.Error("two providers sharing an origin must not be one destination")
		}
	})
}

// D2/I10: reject rather than strip. Stripping could silently grant a different
// destination; rendering could disclose a secret.
func TestNewDestinationRejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"relative", "/v1"},
		{"no scheme", "api.example.com/v1"},
		{"unsupported scheme", "ftp://api.example.com/v1"},
		{"file scheme", "file:///etc/passwd"},
		{"opaque", "http:opaque"},
		{"userinfo", "https://user:pw@api.example.com/v1"},
		{"userinfo without password", "https://user@api.example.com/v1"},
		{"query", "https://api.example.com/v1?token=abc"},
		{"force query", "https://api.example.com/v1?"},
		{"fragment", "https://api.example.com/v1#frag"},
		{"empty host", "https:///v1"},
		{"ipv6 zone id", "http://[fe80::1%25en0]:8090"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDestination("p", tt.raw)
			if err == nil {
				t.Fatalf("NewDestination(%q) = %q, want error", tt.raw, d.BaseURL())
			}
			if !errors.Is(err, ErrDestinationInvalid) {
				t.Errorf("error %v does not match ErrDestinationInvalid", err)
			}
			if !d.IsZero() {
				t.Error("a rejected destination must be the zero value")
			}
		})
	}
}

func TestNewDestinationRejectsBadProviderName(t *testing.T) {
	// Control characters would forge lines in the admission manifest. Invalid
	// UTF-8 is rejected for the neighbouring reason: distinct byte sequences
	// all render as U+FFFD, so two different grant keys would be
	// indistinguishable on the surface the user reads before consenting.
	for _, name := range []string{
		"", "has/slash", "a\nb", "a\rb", "a\x00b", "a\x7fb",
		"a\xffb", "\xc3\x28", "a\u0085b", "a\u2028b", "a\u2029b",
	} {
		if _, err := NewDestination(name, "https://api.example.com/v1"); err == nil {
			t.Errorf("NewDestination(%q, ...) = nil error, want rejection", name)
		} else if !errors.Is(err, ErrDestinationInvalid) {
			t.Errorf("provider %q: error %v does not match ErrDestinationInvalid", name, err)
		}
	}
}

// M20/I19. The raw base URL may carry a credential; a diagnostic that echoes
// it turns a validation error into a disclosure.
//
// Every rejection branch gets its own secret-bearing input. A single canary
// input would only ever reach the first branch that matches it (the fragment
// check precedes the userinfo check, for instance), leaving every later branch
// free to leak while this test still passed.
func TestDestinationErrorsNeverEchoRawInput(t *testing.T) {
	tests := []struct {
		branch string
		raw    string
	}{
		{"not absolute", canarySecret + "-not-a-url"},
		{"fragment", "https://example.com/v1#" + canarySecret},
		{"scheme", "ftp://user:" + canarySecret + "@example.com/v1"},
		{"opaque", "http:" + canarySecret},
		{"userinfo", "https://user:" + canarySecret + "@example.com/v1"},
		{"query", "https://example.com/v1?token=" + canarySecret},
		{"empty host", "https:///" + canarySecret},
		{"zone id", "http://[fe80::1%25" + canarySecret + "]:8090"},
		{"everything at once", canaryURL},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			_, err := NewDestination("p", tt.raw)
			if err == nil {
				t.Fatalf("NewDestination(%q) must be rejected", tt.branch)
			}
			msg := err.Error()
			for _, forbidden := range []string{canarySecret, tt.raw} {
				if strings.Contains(msg, forbidden) {
					t.Errorf("branch %s leaked %q: %s", tt.branch, forbidden, msg)
				}
			}
		})
	}
}

// D3/I12. Literal classification only: no DNS, so a hostname that merely
// resolves to 127.0.0.1 is remote.
func TestDestinationLocalityIsLiteralOnly(t *testing.T) {
	tests := []struct {
		raw       string
		wantLocal bool
	}{
		{"http://localhost:8090", true},
		{"http://LOCALHOST:8090", true},
		{"http://127.0.0.1:11434", true},
		{"http://127.0.0.53:8090", true},
		{"http://[::1]:8090", true},
		{"http://[0:0:0:0:0:0:0:1]:8090", true},
		{"http://0.0.0.0:8090", false},
		{"http://192.168.1.10:8090", false},
		{"http://10.0.0.5:8090", false},
		{"https://opencode.ai/zen/go", false},
		{"http://localhost.evil.example", false},
		{"http://notlocalhost", false},
		{"http://[2001:db8::1]:8090", false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			d, err := NewDestination("p", tt.raw)
			if err != nil {
				t.Fatalf("NewDestination: %v", err)
			}
			if d.IsLocal() != tt.wantLocal {
				t.Errorf("IsLocal() = %v, want %v", d.IsLocal(), tt.wantLocal)
			}
		})
	}
}

// D1: the repeatable CLI grant form, split at the first slash.
func TestDestinationStringParseRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"https://opencode.ai/zen/go",
		"http://127.0.0.1:8090",
		"http://[::1]:8090/v1",
		"https://api.example.com",
	} {
		t.Run(raw, func(t *testing.T) {
			want, err := NewDestination("opencode", raw)
			if err != nil {
				t.Fatalf("NewDestination: %v", err)
			}
			got, err := ParseDestination(want.String())
			if err != nil {
				t.Fatalf("ParseDestination(%q): %v", want.String(), err)
			}
			if got != want {
				t.Errorf("round trip: %q -> %q -> %q", raw, want.String(), got.String())
			}
		})
	}
}

func TestParseDestinationRejectsMalformed(t *testing.T) {
	for _, s := range []string{
		"",
		"opencode",                    // no URL
		"https://opencode.ai/zen/go",  // no provider (splits into scheme "https:")
		"/https://opencode.ai",        // empty provider
		"opencode/not-a-url",          // unparseable remainder
		"opencode/https://u:p@h.com/", // userinfo survives no path
	} {
		t.Run(s, func(t *testing.T) {
			d, err := ParseDestination(s)
			if err == nil {
				t.Fatalf("ParseDestination(%q) = %q, want error", s, d.String())
			}
			if !errors.Is(err, ErrDestinationInvalid) {
				t.Errorf("error %v does not match ErrDestinationInvalid", err)
			}
			if !d.IsZero() {
				t.Error("a rejected destination must be the zero value")
			}
		})
	}
}

// D4/I17/M18. The zero value is the fail-closed default: any caller that
// forgets to choose a policy denies remote access rather than allowing it.
func TestDestinationPolicyZeroValueDeniesRemotePermitsLocal(t *testing.T) {
	var p DestinationPolicy // deliberately zero

	local, err := NewDestination("ollama", "http://127.0.0.1:11434")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewDestination("opencode", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}

	if !p.Permits(local) {
		t.Error("zero policy must permit literal loopback")
	}
	if p.Permits(remote) {
		t.Error("zero policy must deny remote")
	}
	if p.Permits(Destination{}) {
		t.Error("zero policy must deny the zero destination")
	}
}

func TestDestinationPolicyExactSet(t *testing.T) {
	granted, err := NewDestination("opencode", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}
	otherProvider, err := NewDestination("other", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}
	otherPath, err := NewDestination("opencode", "https://opencode.ai/zen/other")
	if err != nil {
		t.Fatal(err)
	}

	p := NewDestinationPolicy(granted)
	if !p.Permits(granted) {
		t.Error("exact grant must be permitted")
	}
	if p.Permits(otherProvider) {
		t.Error("a grant must not transfer across providers")
	}
	if p.Permits(otherPath) {
		t.Error("a grant must not transfer across base paths")
	}

	// A grant spelled differently still matches, because both canonicalize.
	alias, err := NewDestination("opencode", "HTTPS://OpenCode.ai:443/zen/go/")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Permits(alias) {
		t.Error("an equivalent spelling of a granted destination must be permitted")
	}
}

func TestDestinationPolicyIsImmutableAfterConstruction(t *testing.T) {
	granted, err := NewDestination("opencode", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}
	input := []Destination{granted}
	p := NewDestinationPolicy(input...)

	other, err := NewDestination("other", "https://other.example.com")
	if err != nil {
		t.Fatal(err)
	}
	input[0] = other // mutate the caller's slice after construction

	if p.Permits(other) {
		t.Error("policy must copy its input; caller mutation changed the grant set")
	}
	if !p.Permits(granted) {
		t.Error("policy lost its original grant after caller mutation")
	}
}

func TestAllowAllDestinationsPermitsValidRemote(t *testing.T) {
	p := AllowAllDestinations()
	remote, err := NewDestination("opencode", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Permits(remote) {
		t.Error("AllowAllDestinations must permit a valid remote destination")
	}
	// D4: allow-all still does not admit an unconstructed destination.
	if p.Permits(Destination{}) {
		t.Error("AllowAllDestinations must not permit the zero destination")
	}
}

// M22 groundwork: denial must stay a typed, distinguishable error so callers
// cannot collapse it into a generic availability failure.
func TestDestinationDeniedErrorIsTypedAndSafe(t *testing.T) {
	d, err := NewDestination("opencode", "https://opencode.ai/zen/go")
	if err != nil {
		t.Fatal(err)
	}
	denied := &DestinationDeniedError{Destination: d, Purpose: "summarize"}

	if !errors.Is(denied, ErrDestinationDenied) {
		t.Error("DestinationDeniedError must match ErrDestinationDenied")
	}

	var target *DestinationDeniedError
	if !errors.As(error(denied), &target) {
		t.Fatal("errors.As must recover the concrete denial")
	}
	if target.Destination != d || target.Purpose != "summarize" {
		t.Error("denial must retain destination and purpose")
	}

	// I6: the diagnostic names the destination AND the purpose that reached it.
	msg := denied.Error()
	for _, want := range []string{"opencode", "https://opencode.ai/zen/go", "summarize"} {
		if !strings.Contains(msg, want) {
			t.Errorf("denial message missing %q: %s", want, msg)
		}
	}
}

func TestDestinationSchemeVersionIsPinned(t *testing.T) {
	// D1: the receipt schema labels this normalization. Changing it requires
	// re-consent, so the constant must not drift silently.
	if DestinationSchemeVersion != "destination/v1" {
		t.Errorf("DestinationSchemeVersion = %q; changing it invalidates existing grants",
			DestinationSchemeVersion)
	}
}
