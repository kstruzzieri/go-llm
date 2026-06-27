package mcp

import (
	"encoding/json"
	"strings"

	"github.com/kstruzzieri/go-llm/rag"
)

// normalizeWS collapses internal whitespace runs to single spaces and trims
// both ends. strings.Fields splits on any Unicode whitespace and drops empties.
func normalizeWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quoteInChunk reports whether quote appears in the chunk's raw content after
// conservative whitespace normalization. Case-sensitive (code is). Matches
// against rag.Chunk.Content, never BuildContext output: BuildContext prefixes
// each line with "<n>| " (PR #227 line anchors), which would corrupt matching.
func quoteInChunk(chunk rag.Chunk, quote string) bool {
	q := normalizeWS(quote)
	if q == "" {
		return false
	}
	return strings.Contains(normalizeWS(chunk.Content), q)
}

// extractJSONObjects returns every top-level {...} object found in s, in order.
// It tracks string state and backslash escapes so braces inside JSON strings do
// not affect nesting depth. Text between objects is ignored. Byte iteration is
// sufficient because the structural characters are all ASCII.
func extractJSONObjects(s string) []json.RawMessage {
	var out []json.RawMessage
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, json.RawMessage(s[start:i+1]))
					start = -1
				}
			}
		}
	}
	return out
}
