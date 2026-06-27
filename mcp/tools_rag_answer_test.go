package mcp

import (
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestQuoteInChunk(t *testing.T) {
	chunk := rag.Chunk{Content: "func BuildContext(results []SearchResult)  {\n\treturn x\n}"}
	tests := []struct {
		name  string
		quote string
		want  bool
	}{
		{"exact", "func BuildContext(results []SearchResult)", true},
		{"whitespace reflow", "func BuildContext(results []SearchResult) {", true}, // double space collapsed
		{"newline reflow", "[]SearchResult) { return x }", true},
		{"case mismatch", "func buildcontext", false},
		{"absent", "func NeverHere()", false},
		{"empty quote", "", false},
		{"line-anchor prefix not present", "42| func BuildContext", false}, // verify vs raw Content, not BuildContext output
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteInChunk(chunk, tc.quote); got != tc.want {
				t.Errorf("quoteInChunk(%q) = %v, want %v", tc.quote, got, tc.want)
			}
		})
	}
}
