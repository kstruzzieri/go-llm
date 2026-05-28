package main

import (
	"regexp"
	"strings"
)

// Patterns are intentionally narrow: a path must look like an absolute
// posix path with one of a few well-known sensitive roots. The point is
// not to catch every conceivable path — operator hygiene plus this
// renderer-side check is the defense-in-depth design from spec §5.2.
var (
	pathPattern         = regexp.MustCompile(`(?:(?:/tmp|/Users/[^/\s]+)(?:/[^\s]+)?|\$HOME)`)
	justificationPrefix = regexp.MustCompile(`(?i)\bjustification\s*:\s*(?:"[^"]*"|'[^']*'|[^;.\n]+)[;.]?`)
)

// redactString returns s with local paths and judge justification
// fragments replaced by sanitized placeholders. Idempotent — running it
// twice yields the same result. Used as the renderer's chokepoint per
// spec §5.2 sanitization rules.
func redactString(s string) string {
	s = pathPattern.ReplaceAllString(s, "<redacted-path>")
	s = justificationPrefix.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// redactErrorMessage maps a raw error string to a categorized stub.
// The category is best-effort; the goal is to record "what kind of
// failure" without leaking host/path/internal details.
func redactErrorMessage(s string) string {
	switch {
	case strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "timeout"):
		return "<error: timeout>"
	case strings.Contains(s, "connection refused") || strings.Contains(s, "no such host") || strings.Contains(s, "dial "):
		return "<error: network>"
	case strings.Contains(s, "cannot unmarshal") || strings.Contains(s, "json:") || strings.Contains(s, "parse"):
		return "<error: parse>"
	default:
		return "<error: other>"
	}
}
