package mcp

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// maxHTTPBearerTokenLen is the longest accepted HTTP bearer token, in bytes.
const maxHTTPBearerTokenLen = 4096

// httpBearerTokenPattern is the RFC 6750 section 2.1 b64token grammar:
// one or more token characters, then optional "=" padding.
var httpBearerTokenPattern = regexp.MustCompile("^[A-Za-z0-9._~+/-]+=*$")

// ErrInvalidHTTPBearerToken is returned by NewServer when WithHTTPBearerToken
// is given an empty or malformed token. The error text names the setting and
// never the rejected value.
var ErrInvalidHTTPBearerToken = errors.New("mcp: invalid HTTP bearer token")

// WithHTTPBearerToken requires every HTTP request that reaches the MCP handler
// to present "Authorization: Bearer <token>". Omitting the option leaves the
// HTTP transport unauthenticated. An empty or malformed token is not treated
// as "off": NewServer fails with ErrInvalidHTTPBearerToken before it loads
// configuration or opens any database.
//
// The token must be 1-4096 bytes of RFC 6750 b64token characters
// (letters, digits, "-", ".", "_", "~", "+", "/", then optional trailing "=").
// It is not trimmed or normalized. NewServer keeps only the token's SHA-256
// digest. The token is one shared credential: it carries no per-user identity.
func WithHTTPBearerToken(token string) Option {
	return func(s *Server) {
		s.httpBearerSet = true
		s.httpBearerToken = token
	}
}

// validHTTPBearerToken reports whether token is 1-4096 bytes of RFC 6750
// b64token syntax.
func validHTTPBearerToken(token string) bool {
	return len(token) >= 1 && len(token) <= maxHTTPBearerTokenLen &&
		httpBearerTokenPattern.MatchString(token)
}

// finalizeHTTPBearer validates the token supplied by WithHTTPBearerToken,
// stores its SHA-256 digest and clears the plaintext. It is a no-op when the
// option was not supplied. Errors never contain the token.
func (s *Server) finalizeHTTPBearer() error {
	if !s.httpBearerSet {
		return nil
	}
	token := s.httpBearerToken
	s.httpBearerToken = ""
	if token == "" {
		return fmt.Errorf("%w: empty (omit WithHTTPBearerToken to disable authentication)", ErrInvalidHTTPBearerToken)
	}
	if !validHTTPBearerToken(token) {
		return fmt.Errorf("%w: must be 1-4096 bytes of RFC 6750 token characters, with \"=\" only as trailing padding", ErrInvalidHTTPBearerToken)
	}
	digest := sha256.Sum256([]byte(token))
	s.httpBearerDigest = &digest
	return nil
}

// presentedHTTPBearer returns the token from a request carrying exactly one
// Authorization header of the form "Bearer <token>": the scheme is matched
// case-insensitively, one or more spaces (and nothing else) separate it from
// the token, and the token must satisfy validHTTPBearerToken. The ok result is
// false for anything else, including a comma-joined list of credentials.
func presentedHTTPBearer(h http.Header) (token string, ok bool) {
	values := h.Values("Authorization")
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", false
	}
	scheme, rest, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimLeft(rest, " ")
	if !validHTTPBearerToken(token) {
		return "", false
	}
	return token, true
}

// digestEqual reports whether two token digests are equal in constant time.
func digestEqual(a, b *[sha256.Size]byte) bool {
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// hasHTTPBearer reports whether h carries a bearer token that hashes to want.
func hasHTTPBearer(h http.Header, want *[sha256.Size]byte) bool {
	token, ok := presentedHTTPBearer(h)
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(token))
	return digestEqual(&got, want)
}

// writeHTTPUnauthorized writes the one 401 every rejected request gets.
func writeHTTPUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="go-llm-mcp"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

// requireHTTPBearer serves next only for requests whose bearer token hashes to
// want. Every other request gets the same 401 whatever the failure. next sees
// a clone of the request without its Authorization header, so the SDK never
// copies the credential into tool handlers.
func requireHTTPBearer(next http.Handler, want *[sha256.Size]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasHTTPBearer(r.Header, want) {
			writeHTTPUnauthorized(w)
			return
		}
		authed := r.Clone(r.Context())
		authed.Header.Del("Authorization")
		next.ServeHTTP(w, authed)
	})
}

// requireHTTPBearerBeforeH2C wraps h2c.NewHandler so that a request without
// the bearer token gets the 401 before h2c sees it. h2c reads an HTTP/1.1
// upgrade's whole body and replies 101 before its child handler runs, so
// without this check an unauthenticated upgrade could make the server buffer
// up to the body limit. It only checks: next gets the request unchanged, and
// requireHTTPBearer checks the upgraded request again and strips the header.
// The prior-knowledge preface passes through, tested exactly as h2c tests it.
// Its header map is empty, so it can never take h2c's upgrade branch, and
// every stream on that connection reaches requireHTTPBearer.
func requireHTTPBearerBeforeH2C(next http.Handler, want *[sha256.Size]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		priorKnowledge := r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0"
		if !priorKnowledge && !hasHTTPBearer(r.Header, want) {
			writeHTTPUnauthorized(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}
