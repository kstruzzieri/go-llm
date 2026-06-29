// Package mcpstdio parses command strings for MCP stdio transports.
package mcpstdio

import (
	"fmt"
	"strings"
	"unicode"
)

// ParseCommand splits an MCP stdio command string into argv. It supports the
// small shell-like subset users expect in flags: whitespace separation, single
// and double quotes, and backslash escapes for delimiters.
func ParseCommand(command string) ([]string, error) {
	var parts []string
	var b strings.Builder
	var quote rune
	inToken := false

	flush := func() {
		parts = append(parts, b.String())
		b.Reset()
		inToken = false
	}

	runes := []rune(strings.TrimSpace(command))
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if quote != '\'' && r == '\\' {
			if i+1 < len(runes) && isEscapedRune(quote, runes[i+1]) {
				b.WriteRune(runes[i+1])
				i++
			} else {
				b.WriteRune(r)
			}
			inToken = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
				continue
			}
			b.WriteRune(r)
			inToken = true
			continue
		}

		switch {
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case unicode.IsSpace(r):
			if inToken {
				flush()
			}
		default:
			b.WriteRune(r)
			inToken = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %q quote", quote)
	}
	if inToken {
		flush()
	}
	return parts, nil
}

func isEscapedRune(quote rune, next rune) bool {
	if quote == '"' {
		return next == '"'
	}
	if quote == 0 {
		return unicode.IsSpace(next) || next == '\'' || next == '"'
	}
	return false
}
