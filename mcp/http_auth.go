package mcp

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
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
