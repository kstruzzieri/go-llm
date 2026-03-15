package rag

import (
	"os"
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

		// Trailing /** with glob prefix
		{"build-*/**", "build-debug/output.go", true},
		{"build-*/**", "build-debug/sub/output.go", true},
		{"build-*/**", "other/output.go", false},
		{"[Tt]mp/**", "Tmp/cache.dat", true},
		{"[Tt]mp/**", "tmp/cache.dat", true},
		{"[Tt]mp/**", "xmp/cache.dat", false},

		// Trailing /** with /**/ in the prefix
		{"a/**/b/**", "a/b/out.go", true},         // zero dirs in /**/
		{"a/**/b/**", "a/x/y/b/out.go", true},     // multiple dirs in /**/
		{"a/**/b/**", "a/x/y/b/d/e/f.go", true},   // deep under b/
		{"a/**/b/**", "a/b", false},                // b itself not matched (no trailing content)
		{"a/**/b/**", "other/b/out.go", false},     // wrong prefix

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

		// Middle /**/ with glob prefix
		{"build-*/**/output.go", "build-debug/output.go", true},
		{"build-*/**/output.go", "build-debug/sub/deep/output.go", true},
		{"build-*/**/output.go", "other/output.go", false},

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

func TestGitignoreMatcherIsIgnored(t *testing.T) {
	tests := []struct {
		name  string
		rules []struct {
			baseDir  string
			patterns []string
		}
		path  string
		isDir bool
		want  bool
	}{
		// Basic ignore
		{
			name: "simple glob match",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log"}},
			},
			path: "app.log", isDir: false, want: true,
		},
		{
			name: "no match",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log"}},
			},
			path: "app.go", isDir: false, want: false,
		},

		// Unanchored matches at any depth
		{
			name: "unanchored deep match",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log"}},
			},
			path: "a/b/app.log", isDir: false, want: true,
		},

		// Anchored matches only at scope level
		{
			name: "anchored match",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"src/foo"}},
			},
			path: "src/foo", isDir: false, want: true,
		},
		{
			name: "anchored no match elsewhere",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"src/foo"}},
			},
			path: "lib/src/foo", isDir: false, want: false,
		},

		// Negation
		{
			name: "negation un-ignores",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log", "!important.log"}},
			},
			path: "important.log", isDir: false, want: false,
		},
		{
			name: "negation does not affect others",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log", "!important.log"}},
			},
			path: "debug.log", isDir: false, want: true,
		},

		// Directory-only
		{
			name: "dir-only matches dir",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"build/"}},
			},
			path: "build", isDir: true, want: true,
		},
		{
			name: "dir-only skips file",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"build/"}},
			},
			path: "build", isDir: false, want: false,
		},

		// Nested .gitignore scoping
		{
			name: "nested rule applies within scope",
			rules: []struct{ baseDir string; patterns []string }{
				{"src", []string{"*.tmp"}},
			},
			path: "src/cache.tmp", isDir: false, want: true,
		},
		{
			name: "nested rule does NOT affect sibling",
			rules: []struct{ baseDir string; patterns []string }{
				{"src", []string{"*.tmp"}},
			},
			path: "lib/cache.tmp", isDir: false, want: false,
		},

		// Leading **/ pattern
		{
			name: "leading doublestar",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"**/test"}},
			},
			path: "a/b/test", isDir: false, want: true,
		},
		{
			name: "leading doublestar root",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"**/test"}},
			},
			path: "test", isDir: false, want: true,
		},

		// Last rule wins across files
		{
			name: "nested negation overrides parent",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.gen.go"}},
				{"src", []string{"!important.gen.go"}},
			},
			path: "src/important.gen.go", isDir: false, want: false,
		},
		{
			name: "nested re-ignore after parent negation",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"*.log", "!*.log"}},
				{"src", []string{"*.log"}},
			},
			path: "src/debug.log", isDir: false, want: true,
		},

		// Leading slash anchoring
		{
			name: "leading slash root only",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"/build"}},
			},
			path: "build", isDir: false, want: true,
		},
		{
			name: "leading slash no deep match",
			rules: []struct{ baseDir string; patterns []string }{
				{".", []string{"/build"}},
			},
			path: "src/build", isDir: false, want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newGitignoreMatcher()
			for _, r := range tt.rules {
				for _, line := range r.patterns {
					pat, ok := parsePattern(line)
					if ok {
						m.rules = append(m.rules, matchRule{pattern: pat, baseDir: r.baseDir})
					}
				}
			}
			got := m.isIgnored(tt.path, tt.isDir)
			if got != tt.want {
				t.Errorf("isIgnored(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestGitignoreMatcherAddFromFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a .gitignore file
	gitignorePath := tmpDir + "/.gitignore"
	content := "*.log\n# comment\n!important.log\nbuild/\n"
	os.WriteFile(gitignorePath, []byte(content), 0644)

	m := newGitignoreMatcher()
	err := m.addFromFile(gitignorePath, ".")
	if err != nil {
		t.Fatalf("addFromFile() error: %v", err)
	}

	if len(m.rules) != 3 { // *.log, !important.log, build/
		t.Errorf("expected 3 rules, got %d", len(m.rules))
	}

	// Missing file returns nil error
	err = m.addFromFile(tmpDir+"/nonexistent", ".")
	if err != nil {
		t.Errorf("missing file should return nil, got: %v", err)
	}
}
