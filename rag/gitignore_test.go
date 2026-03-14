package rag

import (
	"testing"
)

func TestParsePattern(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantOK   bool
		pattern  string
		negation bool
		dirOnly  bool
		anchored bool
	}{
		// Blank lines and comments
		{name: "empty line", line: "", wantOK: false},
		{name: "comment", line: "# this is a comment", wantOK: false},
		{name: "whitespace only", line: "   ", wantOK: false},

		// Basic patterns
		{name: "simple glob", line: "*.log", wantOK: true, pattern: "*.log"},
		{name: "literal name", line: "foo", wantOK: true, pattern: "foo"},

		// Negation
		{name: "negation", line: "!important.go", wantOK: true, pattern: "important.go", negation: true},

		// Directory-only
		{name: "dir only", line: "build/", wantOK: true, pattern: "build", dirOnly: true},

		// Anchored via leading /
		{name: "leading slash", line: "/build", wantOK: true, pattern: "build", anchored: true},
		{name: "leading slash dir", line: "/dist/", wantOK: true, pattern: "dist", anchored: true, dirOnly: true},

		// Anchored via internal /
		{name: "internal slash", line: "src/generated", wantOK: true, pattern: "src/generated", anchored: true},
		{name: "deep path", line: "a/b/c", wantOK: true, pattern: "a/b/c", anchored: true},

		// ** patterns — leading **/ does NOT anchor
		{name: "leading doublestar", line: "**/foo", wantOK: true, pattern: "**/foo", anchored: false},
		{name: "leading doublestar deep", line: "**/src/foo", wantOK: true, pattern: "**/src/foo", anchored: true},
		{name: "middle doublestar", line: "a/**/b", wantOK: true, pattern: "a/**/b", anchored: true},
		{name: "trailing doublestar", line: "abc/**", wantOK: true, pattern: "abc/**", anchored: true},

		// Backslash escaping
		{name: "escaped hash", line: `\#not-a-comment`, wantOK: true, pattern: "#not-a-comment"},
		{name: "escaped bang", line: `\!literal-bang`, wantOK: true, pattern: "!literal-bang"},

		// Trailing space stripping
		{name: "trailing spaces", line: "foo   ", wantOK: true, pattern: "foo"},
		{name: "escaped trailing space", line: `foo\ `, wantOK: true, pattern: "foo "},

		// Negation + dir-only combo
		{name: "negate dir", line: "!build/", wantOK: true, pattern: "build", negation: true, dirOnly: true},

		// Character classes (pass through to globMatch)
		{name: "char class", line: "[Mm]akefile", wantOK: true, pattern: "[Mm]akefile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pat, ok := parsePattern(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parsePattern(%q) ok = %v, want %v", tt.line, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if pat.pattern != tt.pattern {
				t.Errorf("pattern = %q, want %q", pat.pattern, tt.pattern)
			}
			if pat.negation != tt.negation {
				t.Errorf("negation = %v, want %v", pat.negation, tt.negation)
			}
			if pat.dirOnly != tt.dirOnly {
				t.Errorf("dirOnly = %v, want %v", pat.dirOnly, tt.dirOnly)
			}
			if pat.anchored != tt.anchored {
				t.Errorf("anchored = %v, want %v", pat.anchored, tt.anchored)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Simple globs (no **)
		{"*.log", "app.log", true},
		{"*.log", "app.txt", false},
		{"*.log", "dir/app.log", false}, // * does not match /
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"[Mm]akefile", "Makefile", true},
		{"[Mm]akefile", "makefile", true},
		{"[Mm]akefile", "xakefile", false},
		{"foo", "foo", true},
		{"foo", "bar", false},

		// Leading **/
		{"**/foo", "foo", true},           // zero dirs
		{"**/foo", "a/foo", true},         // one dir
		{"**/foo", "a/b/foo", true},       // two dirs
		{"**/foo", "a/b/c/foo", true},     // three dirs
		{"**/foo", "bar", false},          // no match
		{"**/*.go", "main.go", true},      // zero dirs with glob
		{"**/*.go", "src/main.go", true},  // one dir with glob
		{"**/*.go", "src/main.py", false}, // wrong ext

		// Trailing /**
		{"abc/**", "abc/x", true},
		{"abc/**", "abc/x/y/z", true},
		{"abc/**", "abc", false},    // dir itself not matched
		{"abc/**", "abcd/x", false}, // not a prefix match

		// Middle /**/
		{"a/**/b", "a/b", true},       // zero dirs
		{"a/**/b", "a/x/b", true},     // one dir
		{"a/**/b", "a/x/y/b", true},   // two dirs
		{"a/**/b", "a/x/y/z/b", true}, // three dirs
		{"a/**/b", "b", false},        // missing prefix
		{"a/**/b", "a/b/c", false},    // trailing mismatch
		{"a/**/*.go", "a/main.go", true},
		{"a/**/*.go", "a/b/c/main.go", true},
		{"a/**/*.go", "a/b/c/main.py", false},

		// Standalone **
		{"**", "anything", true},
		{"**", "a/b/c", true},

		// No wildcards (literal)
		{"src/foo", "src/foo", true},
		{"src/foo", "src/bar", false},

		// Malformed pattern
		{"[unclosed", "x", false},

		// Edge: empty path
		{"*", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_vs_"+tt.path, func(t *testing.T) {
			got := globMatch(tt.pattern, tt.path)
			if got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}
