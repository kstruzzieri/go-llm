package completion

import "testing"

func TestResolveLanguage(t *testing.T) {
	tests := []struct {
		explicit string
		filePath string
		want     string
	}{
		{"go", "", "go"},
		{"Go", "", "go"},
		{"", "main.go", "go"},
		{"", "script.py", "python"},
		{"", "app.ts", "typescript"},
		{"", "app.tsx", "typescript"},
		{"", "index.js", "javascript"},
		{"", "index.jsx", "javascript"},
		{"", "Main.java", "java"},
		{"", "lib.rs", "rust"},
		{"", "app.rb", "ruby"},
		{"", "main.c", "c"},
		{"", "main.cpp", "cpp"},
		{"", "main.cc", "cpp"},
		{"", "query.sql", "sql"},
		{"", "config.yaml", "yaml"},
		{"", "config.yml", "yaml"},
		{"", "data.json", "json"},
		{"", "unknown.xyz", ""},
		{"", "", ""},
		{"python", "main.go", "python"}, // explicit wins
	}

	for _, tt := range tests {
		name := tt.explicit + ":" + tt.filePath
		if name == ":" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			got := resolveLanguage(tt.explicit, tt.filePath)
			if got != tt.want {
				t.Errorf("resolveLanguage(%q, %q) = %q, want %q", tt.explicit, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestLanguageHints(t *testing.T) {
	t.Run("known language has hints", func(t *testing.T) {
		hints := languageHintsFor("go")
		if len(hints.ImportKeywords) == 0 {
			t.Error("Go should have import keywords")
		}
		if len(hints.DeclarationKeywords) == 0 {
			t.Error("Go should have declaration keywords")
		}
		if len(hints.CommentPrefixes) == 0 {
			t.Error("Go should have comment prefixes")
		}
		if len(hints.StopPatterns) == 0 {
			t.Error("Go should have stop patterns")
		}
	})

	t.Run("unknown language returns generic hints", func(t *testing.T) {
		hints := languageHintsFor("brainfuck")
		if len(hints.DeclarationKeywords) == 0 {
			t.Error("unknown language should have generic declaration keywords")
		}
	})

	t.Run("all defined languages have stop patterns", func(t *testing.T) {
		for _, lang := range []string{"go", "python", "java", "typescript", "javascript", "rust", "ruby", "c", "cpp", "sql", "yaml", "json"} {
			hints := languageHintsFor(lang)
			if lang != "yaml" && lang != "json" && lang != "sql" && len(hints.StopPatterns) == 0 {
				t.Errorf("%s: expected stop patterns", lang)
			}
		}
	})
}

func TestAnalyzeCursor(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		suffix     string
		language   string
		wantCtx    CursorContext
		wantShape  CompletionShape
		wantConfGe float64
	}{
		{
			name:       "import block — Go",
			prefix:     "package main\n\nimport (\n\t\"fmt\"\n\t",
			suffix:     "\n)\n\nfunc main() {}",
			language:   "go",
			wantCtx:    ContextImportBlock,
			wantShape:  ShapeToken,
			wantConfGe: 0.75,
		},
		{
			name:       "import block — Python",
			prefix:     "import os\nimport ",
			suffix:     "\n\ndef main():\n    pass",
			language:   "python",
			wantCtx:    ContextImportBlock,
			wantShape:  ShapeToken,
			wantConfGe: 0.75,
		},
		{
			name:       "after open brace",
			prefix:     "func main() {\n\t",
			suffix:     "\n}",
			language:   "go",
			wantCtx:    ContextAfterOpenBrace,
			wantShape:  ShapeBlock,
			wantConfGe: 0.75,
		},
		{
			name:       "between declarations",
			prefix:     "func foo() {\n\treturn 1\n}\n\n",
			suffix:     "\nfunc bar() {\n\treturn 2\n}",
			language:   "go",
			wantCtx:    ContextBetweenDeclarations,
			wantShape:  ShapeDeclaration,
			wantConfGe: 0.75,
		},
		{
			name:       "comment block",
			prefix:     "func main() {\n\t// ",
			suffix:     "\n\tfmt.Println(\"hi\")\n}",
			language:   "go",
			wantCtx:    ContextCommentBlock,
			wantShape:  ShapeLine,
			wantConfGe: 0.75,
		},
		{
			name:       "expression after equals",
			prefix:     "func main() {\n\tx = ",
			suffix:     "\n}",
			language:   "go",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeExpression,
			wantConfGe: 0.5,
		},
		{
			name:       "expression after return",
			prefix:     "func foo() int {\n\treturn ",
			suffix:     "\n}",
			language:   "go",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeExpression,
			wantConfGe: 0.75,
		},
		{
			name:       "token after comma",
			prefix:     "func foo(a int, ",
			suffix:     ") {}",
			language:   "go",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeToken,
			wantConfGe: 0.5,
		},
		{
			name:       "token after dot",
			prefix:     "func main() {\n\tfmt.",
			suffix:     "\n}",
			language:   "go",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeToken,
			wantConfGe: 0.5,
		},
		{
			name:       "import keywords earlier in file do not force import context",
			prefix:     "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n\treturn ",
			suffix:     "\n}",
			language:   "go",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeExpression,
			wantConfGe: 0.75,
		},
		{
			name:       "empty prefix — start of file",
			prefix:     "",
			suffix:     "func main() {\n}",
			language:   "go",
			wantCtx:    ContextBetweenDeclarations,
			wantShape:  ShapeDeclaration,
			wantConfGe: 0.25,
		},
		{
			name:       "empty suffix — end of file",
			prefix:     "func main() {\n\treturn\n}\n",
			suffix:     "",
			language:   "go",
			wantCtx:    ContextBetweenDeclarations,
			wantShape:  ShapeDeclaration,
			wantConfGe: 0.5,
		},
		{
			name:       "both empty — new file",
			prefix:     "",
			suffix:     "",
			language:   "go",
			wantCtx:    ContextBetweenDeclarations,
			wantShape:  ShapeDeclaration,
			wantConfGe: 0.25,
		},
		{
			name:       "unknown language fallback",
			prefix:     "foo bar baz\n",
			suffix:     "\nqux",
			language:   "",
			wantCtx:    ContextFunctionBody,
			wantShape:  ShapeBlock,
			wantConfGe: 0.25,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AnalyzeCursor(tt.prefix, tt.suffix, tt.language)
			if result.Context != tt.wantCtx {
				t.Errorf("Context = %v, want %v (reason: %s)", result.Context, tt.wantCtx, result.Reason)
			}
			if result.Shape != tt.wantShape {
				t.Errorf("Shape = %v, want %v (reason: %s)", result.Shape, tt.wantShape, result.Reason)
			}
			if result.Confidence < tt.wantConfGe {
				t.Errorf("Confidence = %v, want >= %v", result.Confidence, tt.wantConfGe)
			}
			if result.Reason == "" {
				t.Error("Reason should not be empty")
			}
		})
	}
}
