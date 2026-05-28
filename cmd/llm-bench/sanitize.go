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
	// pathPattern requires a path-start boundary so that identifiers
	// containing /tmp-like substrings (e.g. "org/tmp-model:v1") are not
	// incorrectly redacted. The leading group captures the boundary character
	// (whitespace, punctuation, or start-of-string); group 1 is the path.
	pathPattern = regexp.MustCompile(`(?:^|[\s,;'"(\[<])(/tmp(?:/[^\s]+)?|/Users/[^/\s]+(?:/[^\s]+)?|\$HOME)`)

	// justificationPrefix strips through the end of the line so that
	// multi-clause values like "completed.task; foo" are fully removed.
	justificationPrefix = regexp.MustCompile(`(?i)\bjustification\s*:\s*(?:"[^"]*"|'[^']*'|[^\n]+)`)
)

// redactPaths replaces local-path occurrences with <redacted-path> while
// preserving the boundary character (whitespace, comma, paren, etc.).
// Identifiers containing /tmp-like substrings (e.g. "org/tmp-model:v1")
// are left intact because the regex requires a path-start boundary.
func redactPaths(s string) string {
	return pathPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := pathPattern.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		boundary := strings.TrimSuffix(m, sub[1])
		return boundary + "<redacted-path>"
	})
}

// redactString returns s with local paths and judge justification
// fragments replaced by sanitized placeholders. Idempotent — running it
// twice yields the same result. Used as the renderer's chokepoint per
// spec §5.2 sanitization rules.
func redactString(s string) string {
	s = redactPaths(s)
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
