package mcp

import (
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
