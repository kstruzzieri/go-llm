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

// Suppress unused import warnings — these are used by later functions in this file.
var _ = path.Match
var _ = bufio.NewScanner
var _ = os.Open
