package rag

import "testing"

func TestExtractCodeQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Paths keep their structural separators so FTS5 sanitization splits them.
		{"path", "fix the bug in rag/retriever.go", "rag/retriever.go"},
		{"path only", "rag/retriever.go", "rag/retriever.go"},

		// snake_case identifiers.
		{"snake_case", "where is vector_space_id set", "vector_space_id"},

		// camelCase / PascalCase identifiers.
		{"camelCase", "please find the SearchMulti function", "SearchMulti"},
		{"PascalCase", "the VectorStore interface", "VectorStore"},
		{"acronym prefix", "the HTTPServer handler", "HTTPServer"},
		{"mixed case lower start", "uses iOS APIs", "iOS"},

		// Quoted strings are preserved verbatim (double quote and backtick).
		{"double quoted", `search for "hello world" now`, "hello world"},
		{"backtick quoted", "the `raw value` token", "raw value"},
		{"unclosed quote falls through", `the "unclosed token`, ""},

		// Alphanumeric identifiers.
		{"alphanumeric", "hash with sha256 please", "sha256"},
		{"model tag", "load qwen2.5:72b now", "qwen2.5:72b"},
		{"dotted code", "check x.y", "x.y"},

		// Full code snippet: identifiers survive, prose-like keywords are kept
		// only when code-shaped.
		{"code snippet", "func SearchMulti(ctx context.Context) error", "SearchMulti(ctx context.Context)"},

		// Plain natural language yields nothing -> caller falls back to original.
		{"pure prose", "how does retrieval work in this system", ""},
		{"empty", "", ""},
		{"punctuation only", "... --- ///", ""},
		{"numbers only", "i tried 3 times and 404", ""},
		{"sentence-leading caps", "Retrieval embeds the query", ""},
		{"all caps acronyms", "the API and HTTP layer", ""},
		{"dotted prose abbreviations", "e.g. i.e. (U.S.),", ""},

		// Mixed prose + code keeps only the code term.
		{"mixed", "please look at SearchMulti in rag/retriever.go", "SearchMulti rag/retriever.go"},
		{"abbreviation with code term", "e.g. SearchMulti", "SearchMulti"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCodeQuery(tt.input)
			if got != tt.want {
				t.Errorf("extractCodeQuery(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
