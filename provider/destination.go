package provider

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DestinationSchemeVersion labels the normalization below. It is recorded in
// the admission receipt so a grant issued under one interpretation is never
// silently reused under another: changing the normalization must change this
// constant, which forces re-consent instead of reinterpreting stored grants.
const DestinationSchemeVersion = "destination/v1"

// ErrDestinationInvalid marks a base URL or provider name that cannot be
// turned into a destination identity. It is a configuration error and is
// always fatal: an unclassifiable destination is never admitted.
var ErrDestinationInvalid = errors.New("provider: invalid destination")

// ErrDestinationDenied marks a request whose destination holds no grant.
// Callers must keep it distinguishable from an availability failure — a
// denied destination is a policy outcome, not an unreachable server.
var ErrDestinationDenied = errors.New("provider: destination denied")

// Destination is the canonical identity of one reachable model endpoint:
// a provider key plus its normalized base URL. Credentials, model names, and
// use cases are deliberately excluded, so the identity is safe to render and
// stable across the models a provider serves.
//
// The fields are unexported so a Destination can only come from
// NewDestination or ParseDestination. That makes the zero value unforgeable
// and, because every policy denies it, makes "forgot to construct one" fail
// closed rather than open. Destination stays comparable so it can key the
// grant set and the manifest directly.
type Destination struct {
	provider string
	baseURL  string
	local    bool
}

// Provider returns the config provider key.
func (d Destination) Provider() string { return d.provider }

// BaseURL returns the canonical server root under DestinationSchemeVersion.
func (d Destination) BaseURL() string { return d.baseURL }

// IsLocal reports whether the host is a literal loopback address. It is a
// lexical property only: no DNS was consulted, so a hostname that merely
// resolves to a loopback address is not local. Dial-time enforcement of that
// distinction belongs to the transport, not to this classification.
func (d Destination) IsLocal() bool { return d.local }

// IsZero reports whether this is the unconstructed value. Every policy denies
// it.
func (d Destination) IsZero() bool { return d.provider == "" || d.baseURL == "" }

// String renders the repeatable CLI grant form, "<provider>/<base URL>".
// Provider names cannot contain "/", so ParseDestination splits at the first
// one and recovers both halves unambiguously.
func (d Destination) String() string { return d.provider + "/" + d.baseURL }

// NewDestination validates and canonicalizes one endpoint identity. It
// performs no I/O: no DNS, no redirects, no model enumeration.
//
// Canonicalization exists to answer one question — are these two spellings
// the same endpoint? It may therefore merge only where they provably are
// (letter case, a default port, an equivalent IP literal, a trailing slash).
// Everything else stays distinct, because merging two different destinations
// silently transfers a grant from one to the other.
//
// Components that could carry a secret or change the request target —
// userinfo, query, fragment — are REJECTED rather than stripped. Stripping
// would quietly grant a destination the user never named, and echoing them
// back in a diagnostic would disclose the secret. For the same reason no
// error below includes the raw input.
func NewDestination(providerName, rawBaseURL string) (Destination, error) {
	if err := validateDestinationProviderName(providerName); err != nil {
		return Destination{}, fmt.Errorf("%w: %s", ErrDestinationInvalid, err)
	}
	canonical, local, err := canonicalizeEndpoint(rawBaseURL)
	if err != nil {
		return Destination{}, fmt.Errorf("%w: provider %q: %s", ErrDestinationInvalid, providerName, err)
	}
	return Destination{provider: providerName, baseURL: canonical, local: local}, nil
}

// validateDestinationProviderName accepts a strict SUBSET of what
// config.ValidateProviderName accepts. The rule is duplicated rather than
// called because config imports provider, so importing it back would cycle;
// destination_pin_test.go pins the subset relation so the two cannot drift
// into disagreement.
//
// Being stricter than config is the safe direction: it can only refuse to
// build a destination, never admit one config would have rejected. The extra
// rule is control characters. config permits them in a provider key, and the
// admission manifest is the line-oriented text a user reads before granting;
// a key containing a newline could forge manifest lines and misrepresent what
// is about to be admitted. This bounds structural forgery only -- it is not
// protection against visually confusable names, which #477 does not attempt.
func validateDestinationProviderName(name string) error {
	if name == "" {
		return errors.New("provider name is empty")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("provider name %q must not contain %q", name, "/")
	}
	if !utf8.ValidString(name) {
		return errors.New("provider name is not valid UTF-8")
	}
	for _, r := range name {
		// unicode.IsControl covers category Cc, which includes NUL, DEL, CR,
		// LF, and NEL -- but NOT U+2028 LINE SEPARATOR or U+2029 PARAGRAPH
		// SEPARATOR, which are Zl/Zp. Those are line breaks by definition and
		// do break line structure in some renderers, so a Cc-only check would
		// leave the guard incomplete against the forgery it exists to stop.
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			// %q escapes the offending rune, so the diagnostic cannot itself
			// carry the forgery into whatever renders it.
			return fmt.Errorf("provider name %q must not contain line-breaking or control characters", name)
		}
	}
	return nil
}

// ParseDestination reads the "<provider>/<base URL>" CLI grant form.
func ParseDestination(s string) (Destination, error) {
	name, raw, ok := strings.Cut(s, "/")
	if !ok {
		return Destination{}, fmt.Errorf(
			"%w: expected \"<provider>/<base URL>\"", ErrDestinationInvalid)
	}
	return NewDestination(name, raw)
}

// canonicalizeEndpoint implements DestinationSchemeVersion. Its output is a
// fixed point: canonicalizing an already-canonical value returns it unchanged,
// so a key derived on a later pass always matches the grant issued earlier.
// Returned errors never contain raw.
func canonicalizeEndpoint(raw string) (canonical string, local bool, err error) {
	if raw == "" {
		return "", false, errors.New("base URL is empty")
	}
	// url.ParseRequestURI does not split fragments; it would fold "#frag" into
	// the path and we would canonicalize a destination the user never wrote.
	// Reject on the raw text instead. An escaped "%23" carries no literal "#"
	// and is unaffected.
	if strings.Contains(raw, "#") {
		return "", false, errors.New("base URL must not carry a fragment")
	}
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return "", false, errors.New("base URL is not an absolute URL")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false, errors.New("base URL scheme must be http or https")
	}
	if u.Opaque != "" {
		return "", false, errors.New("base URL must not be opaque")
	}
	if u.User != nil {
		return "", false, errors.New("base URL must not carry userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", false, errors.New("base URL must not carry a query")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false, errors.New("base URL must include a host")
	}
	// An IPv6 zone ID is meaningless as a consent identity, and its escaped
	// form ("%25en0" -> "%en0") would make the canonical output fail its own
	// re-normalization, breaking the fixed-point property.
	if strings.Contains(host, "%") {
		return "", false, errors.New("base URL host must not carry a zone ID")
	}
	// Equivalent IP spellings are the same endpoint, so collapsing them is a
	// safe merge: "::1" and "0:0:0:0:0:0:0:1" must not become two grants.
	ip := net.ParseIP(host)
	if ip != nil {
		host = ip.String()
	}
	hostPart := host
	if strings.Contains(host, ":") { // IPv6 literal: rebracket for the URL form
		hostPart = "[" + host + "]"
	}
	if port := u.Port(); port != "" && port != defaultPortForScheme(scheme) {
		hostPart += ":" + port
	}
	// EscapedPath preserves percent-encoding: "/a%2Fb" is not "/a/b", and
	// decoding here would merge two different paths into one grant.
	path := strings.TrimRight(u.EscapedPath(), "/")
	// A base path carrying "." or ".." segments misdescribes its own scope on
	// the surface the user consents from: "https://h/v1/../../admin" reads as
	// scoped under /v1 while the effective root is /admin. It also makes the
	// transport's "request path stays under the base path" check meaningless,
	// since "under" is not a property of unresolved text. Resolving the
	// segments instead would silently grant a root the user never wrote, so
	// reject. Both spellings are checked: a server may resolve "%2e%2e" as a
	// dot segment even though the escaped form does not look like one.
	if hasDotSegment(path) || hasDotSegment(strings.TrimRight(u.Path, "/")) {
		return "", false, errors.New("base URL path must not contain \".\" or \"..\" segments")
	}
	local = host == "localhost" || (ip != nil && ip.IsLoopback())
	return scheme + "://" + hostPart + path, local, nil
}

// hasDotSegment reports whether any "/"-separated segment is "." or "..".
func hasDotSegment(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func defaultPortForScheme(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// DestinationPolicy is the host- or CLI-supplied grant input: the set of
// remote destinations a caller has decided this process may reach.
//
// The zero value is the fail-closed default — it permits literal loopback and
// denies every remote destination. A caller that never chooses a policy
// therefore gets local-only behavior instead of accidental remote access.
// Remote access requires either an exact set or an explicit
// AllowAllDestinations, both of which are greppable at the call site.
//
// A concrete set was chosen over a callback or a matching language because it
// is auditable: the admitted destinations can be rendered into a receipt.
type DestinationPolicy struct {
	allowAll bool
	allowed  map[Destination]struct{}
}

// NewDestinationPolicy grants exactly the listed destinations. The input is
// copied, so later mutation of the caller's slice cannot widen the policy.
func NewDestinationPolicy(dests ...Destination) DestinationPolicy {
	allowed := make(map[Destination]struct{}, len(dests))
	for _, d := range dests {
		if d.IsZero() {
			continue // an unconstructed value grants nothing
		}
		allowed[d] = struct{}{}
	}
	return DestinationPolicy{allowed: allowed}
}

// AllowAllDestinations grants every valid destination. It is deliberately a
// named constructor rather than a nil or boolean default: opting out of the
// boundary should be visible in review, not implied by omission.
//
// It still grants nothing to an unconstructed destination, and it confers no
// authority over purposes absent from the active manifest or over redirect
// targets — those are enforced separately.
func AllowAllDestinations() DestinationPolicy {
	return DestinationPolicy{allowAll: true}
}

// Permits reports whether this policy grants d. Literal loopback always
// passes; a destination still needs a manifest-purpose capability at the
// transport, which this function does not supply.
func (p DestinationPolicy) Permits(d Destination) bool {
	if d.IsZero() {
		return false
	}
	if d.local {
		return true
	}
	if p.allowAll {
		return true
	}
	_, ok := p.allowed[d]
	return ok
}

// DestinationDeniedError reports that a destination reached the boundary
// without a grant. It names the destination and the purpose that reached it,
// so the diagnostic identifies which route to fix. It carries no credential,
// header, or payload.
type DestinationDeniedError struct {
	Destination Destination
	Purpose     string // routing use case or internal operation
}

func (e *DestinationDeniedError) Error() string {
	purpose := e.Purpose
	if purpose == "" {
		purpose = "unknown purpose"
	}
	return fmt.Sprintf("provider: destination %s not admitted for %s",
		e.Destination.String(), purpose)
}

// Unwrap makes errors.Is(err, ErrDestinationDenied) hold, so callers can
// detect a policy denial without depending on the concrete type.
func (e *DestinationDeniedError) Unwrap() error { return ErrDestinationDenied }
