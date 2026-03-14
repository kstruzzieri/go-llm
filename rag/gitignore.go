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

	// Rule 1: blank lines
	if strings.TrimSpace(line) == "" {
		return gitignorePattern{}, false
	}

	// Rule 3: leading \ escapes # and ! (must check before comment/negation)
	escaped := false
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
		escaped = true
	}

	// Rule 1: comments (skip if backslash-escaped)
	if !escaped && strings.HasPrefix(line, "#") {
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

	// Rule 4: negation (skip if backslash-escaped)
	negation := false
	if !escaped && strings.HasPrefix(line, "!") {
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
