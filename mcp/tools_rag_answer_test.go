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

func TestExtractJSONObjects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", `{"a":1}`, []string{`{"a":1}`}},
		{"prose around", "sure:\n{\"a\":1}\nthanks", []string{`{"a":1}`}},
		{"two objects", `{"a":1} then {"b":2}`, []string{`{"a":1}`, `{"b":2}`}},
		{"nested", `{"a":{"b":2}}`, []string{`{"a":{"b":2}}`}},
		{"brace in string", `{"a":"}{"}`, []string{`{"a":"}{"}`}},
		{"escaped quote in string", `{"a":"x\"}"}`, []string{`{"a":"x\"}"}`}},
		{"none", `no json here`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSONObjects(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d objects %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Errorf("obj[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
