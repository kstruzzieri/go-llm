# Full .gitignore Support Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace simplified gitignore matching in `IndexDirectory` with full spec support including nested `.gitignore` files, negation, `**` globs, path-scoped rules, and character classes.

**Architecture:** A self-contained `rag/gitignore.go` matching engine (unexported types) that parses `.gitignore` files into scoped rules, matches slash-normalized paths with `**` glob support, and resolves precedence via last-matching-rule-wins. `IndexDirectory` loads rules during `filepath.Walk` and checks both directories and files.

**Tech Stack:** Go stdlib only (`path`, `path/filepath`, `bufio`, `os`, `strings`). No new dependencies.

**Spec:** `docs/plans/2026-03-14-gitignore-support-design.md`

---

## File Structure

| File | Role | Change |
|------|------|--------|
| `rag/gitignore.go` | Pattern parsing, glob matching, matcher | NEW (~250 lines) |
| `rag/gitignore_test.go` | Unit tests for matching engine | NEW (~350 lines) |
| `rag/indexer.go` | Replace `loadGitignore`/`isIgnored`, integrate matcher into walk | MODIFY (lines 203-324) |
| `rag/indexer_test.go` | Integration tests for nested gitignore scenarios | MODIFY (append ~200 lines) |

---

## Chunk 1: Core Gitignore Engine

### Task 1: Pattern Parsing

**Files:**
- Create: `rag/gitignore.go`
- Create: `rag/gitignore_test.go`

- [ ] **Step 1: Write failing tests for parsePattern**

Create `rag/gitignore_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./rag/ -run TestParsePattern -v -count=1`
Expected: FAIL — `parsePattern` undefined

- [ ] **Step 3: Write parsePattern implementation**

Create `rag/gitignore.go`:

```go
package rag

import (
	"bufio"
	"os"
	"path"
	"strings"
)

// gitignorePattern represents a single parsed .gitignore rule.
type gitignorePattern struct {
	original string // raw line from .gitignore (for debugging)
	pattern  string // cleaned pattern (no leading !, no leading /, no trailing /)
	negation bool   // starts with ! → un-ignores a previously ignored path
	dirOnly  bool   // ends with / → only matches directories
	anchored bool   // leading / or internal / → match against scoped path from baseDir root
}

// matchRule pairs a pattern with the directory scope of its .gitignore file.
type matchRule struct {
	pattern gitignorePattern
	baseDir string // slash-normalized repo-relative directory containing the .gitignore
}

// gitignoreMatcher holds a stack of rules from multiple .gitignore files.
type gitignoreMatcher struct {
	rules []matchRule
}

// parsePattern parses a single .gitignore line into a pattern.
// Returns the pattern and true if the line is a valid pattern,
// or zero value and false if the line should be skipped.
func parsePattern(line string) (gitignorePattern, bool) {
	original := line

	// Rule 3: leading \ escapes # and !
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	}

	// Rule 1: blank lines and comments
	if strings.TrimSpace(line) == "" {
		return gitignorePattern{}, false
	}
	if strings.HasPrefix(line, "#") {
		return gitignorePattern{}, false
	}

	// Rule 2: trailing spaces (unless escaped with \)
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	// Unescape trailing space
	if strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-2] + " "
	}

	if line == "" {
		return gitignorePattern{}, false
	}

	// Rule 4: negation
	negation := false
	if strings.HasPrefix(line, "!") {
		negation = true
		line = line[1:]
	}

	// Rule 5: directory-only
	dirOnly := false
	if strings.HasSuffix(line, "/") {
		dirOnly = true
		line = strings.TrimRight(line, "/")
	}

	// Rules 6-9: anchoring
	anchored := false
	if strings.HasPrefix(line, "/") {
		anchored = true
		line = line[1:]
	} else {
		// Check if pattern contains / (other than in leading **/ prefix).
		// Leading **/ is special syntax meaning "match in all directories"
		// and its / does NOT trigger anchoring.
		checkStr := line
		if strings.HasPrefix(checkStr, "**/") {
			checkStr = checkStr[3:]
		}
		if strings.Contains(checkStr, "/") {
			anchored = true
		}
	}

	return gitignorePattern{
		original: original,
		pattern:  line,
		negation: negation,
		dirOnly:  dirOnly,
		anchored: anchored,
	}, true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./rag/ -run TestParsePattern -v -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add rag/gitignore.go rag/gitignore_test.go
git commit -m "feat: add gitignore pattern parsing (Issue #6)

Implements parsePattern() which handles: blank lines, comments,
negation (!), directory-only (/), leading-slash anchoring,
internal-slash anchoring, backslash escaping, and trailing space
stripping. Leading **/ does not trigger anchoring per gitignore spec."
```

---

### Task 2: Glob Matching with `**` Support

**Files:**
- Modify: `rag/gitignore_test.go`
- Modify: `rag/gitignore.go`

- [ ] **Step 1: Write failing tests for globMatch**

Append to `rag/gitignore_test.go`:

```go
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
		{"abc/**", "abc", false},        // dir itself not matched
		{"abc/**", "abcd/x", false},     // not a prefix match

		// Middle /**/
		{"a/**/b", "a/b", true},         // zero dirs
		{"a/**/b", "a/x/b", true},       // one dir
		{"a/**/b", "a/x/y/b", true},     // two dirs
		{"a/**/b", "a/x/y/z/b", true},   // three dirs
		{"a/**/b", "b", false},          // missing prefix
		{"a/**/b", "a/b/c", false},      // trailing mismatch
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./rag/ -run TestGlobMatch -v -count=1`
Expected: FAIL — `globMatch` undefined

- [ ] **Step 3: Write globMatch implementation**

Append to `rag/gitignore.go` (after `parsePattern`):

```go
// globMatch matches a slash-normalized path against a gitignore glob pattern
// with ** support. Returns false for malformed patterns.
func globMatch(pattern, name string) bool {
	if name == "" {
		return false
	}

	// Standalone **
	if pattern == "**" {
		return true
	}

	// Leading **/
	if strings.HasPrefix(pattern, "**/") {
		rest := pattern[3:]
		// Try matching the rest against the full name and every suffix
		if globMatch(rest, name) {
			return true
		}
		for i := 0; i < len(name); i++ {
			if name[i] == '/' && i+1 < len(name) {
				if globMatch(rest, name[i+1:]) {
					return true
				}
			}
		}
		return false
	}

	// Trailing /**
	if strings.HasSuffix(pattern, "/**") {
		prefix := pattern[:len(pattern)-3]
		return strings.HasPrefix(name, prefix+"/")
	}

	// Middle /**/
	if idx := strings.Index(pattern, "/**/"); idx >= 0 {
		before := pattern[:idx]
		after := pattern[idx+4:]

		// Path must start with before + /
		if !strings.HasPrefix(name, before+"/") {
			return false
		}
		remaining := name[len(before)+1:]

		// Try matching after against remaining and every suffix
		if globMatch(after, remaining) {
			return true
		}
		for i := 0; i < len(remaining); i++ {
			if remaining[i] == '/' && i+1 < len(remaining) {
				if globMatch(after, remaining[i+1:]) {
					return true
				}
			}
		}
		return false
	}

	// No ** — use path.Match for slash-aware globbing
	matched, err := path.Match(pattern, name)
	if err != nil {
		return false
	}
	return matched
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./rag/ -run TestGlobMatch -v -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add rag/gitignore.go rag/gitignore_test.go
git commit -m "feat: add globMatch with ** glob support (Issue #6)

Handles leading **/ (match at any depth), trailing /** (match
everything inside), middle /**/ (zero or more directories), and
falls back to path.Match for standard globs. Returns false for
malformed patterns."
```

---

### Task 3: Matcher — addFromFile and isIgnored

**Files:**
- Modify: `rag/gitignore_test.go`
- Modify: `rag/gitignore.go`

- [ ] **Step 1: Write failing tests for matcher**

Append to `rag/gitignore_test.go`:

```go
func TestGitignoreMatcherIsIgnored(t *testing.T) {
	tests := []struct {
		name    string
		rules   []struct {
			baseDir  string
			patterns []string
		}
		path   string
		isDir  bool
		want   bool
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./rag/ -run "TestGitignoreMatcher" -v -count=1`
Expected: FAIL — `newGitignoreMatcher` undefined

- [ ] **Step 3: Write matcher implementation**

Append to `rag/gitignore.go` (after `parsePattern` and `globMatch`):

```go
// newGitignoreMatcher creates an empty matcher.
func newGitignoreMatcher() *gitignoreMatcher {
	return &gitignoreMatcher{}
}

// addFromFile parses a .gitignore file and appends its rules scoped to baseDir.
// baseDir must be slash-normalized and repo-relative (e.g., "." for root, "src" for src/).
// Returns nil if the file doesn't exist. Other read errors are returned.
func (m *gitignoreMatcher) addFromFile(filePath string, baseDir string) error {
	f, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		pat, ok := parsePattern(scanner.Text())
		if !ok {
			continue
		}
		m.rules = append(m.rules, matchRule{
			pattern: pat,
			baseDir: baseDir,
		})
	}
	return scanner.Err()
}

// isIgnored checks if a slash-normalized repo-relative path should be ignored.
// isDir indicates whether the path is a directory (for directory-only patterns).
// Last matching rule wins — this is how negation works.
func (m *gitignoreMatcher) isIgnored(relPath string, isDir bool) bool {
	ignored := false
	for _, rule := range m.rules {
		// Directory-only patterns don't match files
		if rule.pattern.dirOnly && !isDir {
			continue
		}

		// Scope: pattern only applies to paths within baseDir
		var matchPath string
		if rule.baseDir == "." || rule.baseDir == "" {
			matchPath = relPath
		} else {
			if !strings.HasPrefix(relPath, rule.baseDir+"/") {
				continue
			}
			matchPath = relPath[len(rule.baseDir)+1:]
		}

		matched := false
		if rule.pattern.anchored {
			matched = globMatch(rule.pattern.pattern, matchPath)
		} else {
			// Unanchored: match against basename
			matched = globMatch(rule.pattern.pattern, path.Base(matchPath))
		}

		if matched {
			ignored = !rule.pattern.negation
		}
	}
	return ignored
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./rag/ -run "TestGitignoreMatcher" -v -count=1`
Expected: PASS

- [ ] **Step 5: Run all gitignore tests together**

Run: `go test ./rag/ -run "TestParsePattern|TestGlobMatch|TestGitignoreMatcher" -v -count=1`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
git add rag/gitignore.go rag/gitignore_test.go
git commit -m "feat: add gitignoreMatcher with scoped rules and precedence (Issue #6)

Implements newGitignoreMatcher, addFromFile (loads .gitignore with
graceful missing-file handling), and isIgnored (scoped pattern matching
with last-rule-wins precedence for negation support)."
```

---

## Chunk 2: IndexDirectory Integration

### Task 4: Replace Old Gitignore Code in IndexDirectory

**Files:**
- Modify: `rag/indexer.go:1-14` (imports)
- Modify: `rag/indexer.go:184-209` (IndexDirectory walk setup)
- Modify: `rag/indexer.go:215-247` (walk callback)
- Delete: `rag/indexer.go:287-324` (loadGitignore, isIgnored)

- [ ] **Step 1: Run existing tests to establish baseline**

Run: `go test ./rag/ -v -count=1`
Expected: All PASS

- [ ] **Step 2: Update imports in indexer.go**

In `rag/indexer.go`, add `"path/filepath"` is already imported. Add an import for the `path` package if needed — actually, `path` is only used in `gitignore.go`. No import changes needed in `indexer.go`.

Verify: imports remain:
```go
import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/kstruzzieri/go-llm/ollama"
	"golang.org/x/sync/errgroup"
)
```

- [ ] **Step 3: Replace IndexDirectory walk setup (lines 203-209)**

Replace lines 203-209 in `rag/indexer.go`:

```go
	// Load root .gitignore patterns if present.
	// NOTE: This is a simplified implementation that only reads the root .gitignore
	// and supports basic patterns (basename matching, directory suffixes).
	// Not supported: path-scoped rules, negation (!pattern), nested .gitignore
	// files, ** globs, or character classes. For full .gitignore compliance,
	// consider using a dedicated library.
	ignorePatterns := loadGitignore(filepath.Join(dir, ".gitignore"))
```

With:

```go
	// Load .gitignore patterns. Nested .gitignore files are loaded during walk.
	ignore := newGitignoreMatcher()
	if err := ignore.addFromFile(filepath.Join(dir, ".gitignore"), "."); err != nil {
		return fmt.Errorf("rag: read root .gitignore in %q: %w", dir, err)
	}
```

- [ ] **Step 4: Replace walk callback directory handling (lines 225-233)**

Replace the directory handling block in the walk callback:

```go
		if info.IsDir() {
			name := info.Name()
			for _, excl := range cfg.exclude {
				if name == excl {
					return filepath.SkipDir
				}
			}
			return nil
		}
```

With:

```go
		if info.IsDir() {
			name := info.Name()
			// First-pass: WithExclude filter (cannot be overridden by .gitignore)
			for _, excl := range cfg.exclude {
				if name == excl {
					return filepath.SkipDir
				}
			}

			// Second-pass: gitignore check
			relDir, _ := filepath.Rel(dir, path)
			relDir = filepath.ToSlash(relDir)
			if relDir != "." && ignore.isIgnored(relDir, true) {
				return filepath.SkipDir
			}

			// Load nested .gitignore if present (skip root — already loaded pre-walk)
			if relDir != "." {
				nestedGitignore := filepath.Join(path, ".gitignore")
				if err := ignore.addFromFile(nestedGitignore, relDir); err != nil {
					walkErrors = append(walkErrors, fmt.Sprintf("read .gitignore in %q: %v", path, err))
				}
			}
			return nil
		}
```

- [ ] **Step 5: Replace walk callback file handling (lines 235-243)**

Replace the file filtering block:

```go
		ext := strings.ToLower(filepath.Ext(path))
		if !cfg.extensions[ext] {
			return nil
		}

		relPath, _ := filepath.Rel(dir, path)
		if isIgnored(relPath, ignorePatterns) {
			return nil
		}
```

With:

```go
		// Gitignore check (before extension filter)
		relPath, _ := filepath.Rel(dir, path)
		relPath = filepath.ToSlash(relPath)
		if ignore.isIgnored(relPath, false) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !cfg.extensions[ext] {
			return nil
		}
```

- [ ] **Step 6: Delete old loadGitignore and isIgnored functions (lines 287-324)**

Delete the entire `loadGitignore` function (lines 287-305) and the entire `isIgnored` function (lines 307-324).

- [ ] **Step 7: Remove unused `bufio` import**

`bufio` is no longer used in `indexer.go` (it moved to `gitignore.go`). Remove it from the import block.

- [ ] **Step 8: Run all existing tests**

Run: `go test ./rag/ -v -count=1`
Expected: All PASS — behavioral compatibility maintained

- [ ] **Step 9: Run vet**

Run: `go vet ./rag/`
Expected: No issues

- [ ] **Step 10: Commit**

```bash
git add rag/indexer.go
git commit -m "feat: integrate gitignoreMatcher into IndexDirectory (Issue #6)

Replace simplified loadGitignore/isIgnored with full gitignoreMatcher.
Walk now loads nested .gitignore files, checks directories against
gitignore patterns (SkipDir for ignored dirs), and normalizes paths
to slash form before matching. WithExclude remains first-pass filter."
```

---

### Task 5: Integration Tests

**Files:**
- Modify: `rag/indexer_test.go`

- [ ] **Step 1: Write integration tests for nested gitignore**

Append to `rag/indexer_test.go`:

```go
func TestIndexerGitignoreNestedScoping(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root .gitignore ignores *.log
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n"), 0644)

	// src/.gitignore ignores *.tmp
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.tmp\n"), 0644)

	// lib/ has no .gitignore
	os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// Create test files
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "app.log"), []byte("log data\n"), 0644)          // ignored by root
	os.WriteFile(filepath.Join(tmpDir, "src", "util.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "cache.tmp"), []byte("temp\n"), 0644)     // ignored by src
	os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("log\n"), 0644)      // ignored by root
	os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "lib", "cache.tmp"), []byte("temp\n"), 0644)     // NOT ignored (src rule doesn't apply)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected indexed: main.go, src/util.go, lib/helper.go = 3 sources
	// lib/cache.tmp has .tmp extension which isn't in default extensions, so not indexed regardless
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources, got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreNegation(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore all .gen.go, but un-ignore important.gen.go
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.gen.go\n!important.gen.go\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "schema.gen.go"), []byte("package main\n"), 0644)     // ignored
	os.WriteFile(filepath.Join(tmpDir, "important.gen.go"), []byte("package main\n"), 0644)  // NOT ignored

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go + important.gen.go = 2 sources
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (negation should un-ignore important.gen.go), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreDirectorySkip(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore the build/ directory
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "build", "deep", "nested"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "build", "deep", "nested", "inner.go"), []byte("package nested\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Only main.go — entire build/ subtree skipped
	if stats.TotalSources != 1 {
		t.Errorf("expected 1 source (build/ dir skipped), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreWithExcludePriority(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// .gitignore tries to un-ignore vendor/
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("!vendor/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "vendor"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "vendor", "dep.go"), []byte("package dep\n"), 0644)

	// Default WithExclude includes "vendor"
	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Only main.go — WithExclude("vendor") takes priority over .gitignore negation
	if stats.TotalSources != 1 {
		t.Errorf("expected 1 source (WithExclude overrides .gitignore negation), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignorePathScoped(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Path-scoped rule: only ignore generated/ under src/
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("src/generated/\n"), 0644)

	os.MkdirAll(filepath.Join(tmpDir, "src", "generated"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "lib", "generated"), 0755)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "generated", "proto.go"), []byte("package gen\n"), 0644)  // ignored
	os.WriteFile(filepath.Join(tmpDir, "lib", "generated", "types.go"), []byte("package gen\n"), 0644)  // NOT ignored

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go, src/app.go, lib/generated/types.go = 3
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (path-scoped ignore), got %d", stats.TotalSources)
	}
}

func TestIndexerNoGitignore(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// No .gitignore file
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.py"), []byte("def helper():\n    pass\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (no gitignore), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreSiblingScoping(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.MkdirAll(filepath.Join(tmpDir, "lib"), 0755)

	// src/.gitignore ignores *.gen.go
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.gen.go\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "schema.gen.go"), []byte("package src\n"), 0644)   // ignored by src rule
	os.WriteFile(filepath.Join(tmpDir, "lib", "types.gen.go"), []byte("package lib\n"), 0644)    // NOT ignored
	os.WriteFile(filepath.Join(tmpDir, "lib", "helper.go"), []byte("package lib\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: src/app.go, lib/types.gen.go, lib/helper.go = 3
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (src/.gitignore should not affect lib/), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreDirectoryUnignore(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Ignore build/, then un-ignore it — subtree should be walked
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("build/\n!build/\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.MkdirAll(filepath.Join(tmpDir, "build"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "build", "output.go"), []byte("package build\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go + build/output.go = 2 (directory un-ignored, subtree walked)
	if stats.TotalSources != 2 {
		t.Errorf("expected 2 sources (build/ un-ignored), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreOverlappingNestedRules(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// Root: ignore *.log, then un-ignore *.log
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("*.log\n!*.log\n"), 0644)

	// src/: re-ignore *.log (overrides parent negation)
	os.MkdirAll(filepath.Join(tmpDir, "src"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "src", ".gitignore"), []byte("*.log\n"), 0644)

	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "root.log"), []byte("root log\n"), 0644)         // NOT ignored (root negation)
	os.WriteFile(filepath.Join(tmpDir, "src", "app.go"), []byte("package src\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "src", "debug.log"), []byte("src log\n"), 0644)  // ignored (nested re-ignore)

	err := idx.IndexDirectory(context.Background(), tmpDir,
		WithExtensions(".go", ".log")) // include .log in extensions
	if err != nil {
		t.Fatalf("IndexDirectory() error: %v", err)
	}

	stats, _ := store.Stats(context.Background())
	// Expected: main.go, root.log, src/app.go = 3 (src/debug.log re-ignored by nested rule)
	if stats.TotalSources != 3 {
		t.Errorf("expected 3 sources (nested re-ignore overrides parent negation), got %d", stats.TotalSources)
	}
}

func TestIndexerGitignoreAlreadyIndexedFileSurvives(t *testing.T) {
	srv := newMockEmbedServer(4)
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	store, _ := NewSQLiteStore(":memory:")
	defer store.Close()
	idx := NewIndexer(client, store)

	tmpDir := t.TempDir()

	// First run: no .gitignore, index everything
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "util.go"), []byte("package main\n\nfunc util() {}\n"), 0644)

	err := idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("first IndexDirectory() error: %v", err)
	}

	statsBefore, _ := store.Stats(context.Background())
	if statsBefore.TotalSources != 2 {
		t.Fatalf("expected 2 sources before gitignore, got %d", statsBefore.TotalSources)
	}

	// Second run: add .gitignore that ignores util.go
	os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("util.go\n"), 0644)

	err = idx.IndexDirectory(context.Background(), tmpDir)
	if err != nil {
		t.Fatalf("second IndexDirectory() error: %v", err)
	}

	statsAfter, _ := store.Stats(context.Background())
	// util.go chunks should still be in the store (prospective filtering only)
	if statsAfter.TotalSources != 2 {
		t.Errorf("expected 2 sources still (already-indexed data preserved), got %d", statsAfter.TotalSources)
	}
}
```

- [ ] **Step 2: Run integration tests**

Run: `go test ./rag/ -run "TestIndexerGitignore" -v -count=1`
Expected: All PASS

- [ ] **Step 3: Run full test suite**

Run: `go test ./rag/ -v -count=1`
Expected: All PASS (old + new)

- [ ] **Step 4: Commit**

```bash
git add rag/indexer_test.go
git commit -m "test: add integration tests for full .gitignore support (Issue #6)

Tests cover: nested scoping, negation, directory skip, directory
un-ignore, WithExclude priority, path-scoped rules, no-gitignore
fallback, sibling isolation, overlapping nested rules, and
already-indexed file preservation."
```

---

### Task 6: Final Verification and Cleanup

**Files:**
- None (verification only)

- [ ] **Step 1: Run full test suite across all packages**

Run: `go test ./... -count=1`
Expected: All packages PASS

- [ ] **Step 2: Run vet**

Run: `go vet ./...`
Expected: No issues

- [ ] **Step 3: Verify clean working tree**

Run: `git status`
Expected: Clean (all changes committed)

- [ ] **Step 4: Verify no unused imports**

Run: `go build ./...`
Expected: Success, no errors

- [ ] **Step 5: Review git log for commit history**

Run: `git log --oneline -10`
Expected: 5 new commits in logical order:
1. Pattern parsing
2. Glob matching
3. Matcher
4. IndexDirectory integration
5. Integration tests
