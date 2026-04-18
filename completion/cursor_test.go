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
