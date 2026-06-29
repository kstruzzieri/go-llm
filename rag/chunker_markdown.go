package rag

import (
	"path/filepath"
	"regexp"
	"strings"
)

// ATX heading: up to 3 leading spaces, 1-6 '#', then either end-of-line or a
// space/tab followed by the title. Group 2 (the remainder) must start with a
// space/tab or be absent, so "###Title" is not a heading. 4+ leading spaces
// fall outside the {0,3} cap and are treated as indented code, not a heading.
var headingRe = regexp.MustCompile(`^ {0,3}(#{1,6})([ \t].*)?$`)

// closingHashRe strips a CommonMark closing hash sequence: a run of '#' at end
// of line preceded by whitespace. It will NOT strip a '#' that is part of the
// title (e.g. "C#"), because that '#' is not preceded by whitespace.
var closingHashRe = regexp.MustCompile(`[ \t]+#+[ \t]*$`)

// fenceRe matches a fenced-code delimiter line: after up to 3 leading spaces, a
// run of 3 or more backticks OR 3 or more tildes.
var fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// isMarkdown reports whether path has a Markdown extension.
func isMarkdown(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// parseHeading reports whether line is an ATX heading. On match it returns the
// level (1-6) and the derived title. An empty title (bare "#") is normalized to
// level-many '#' characters so the section-path segment is deterministic and
// non-empty — otherwise a chunk built from it would carry no stable-key field.
func parseHeading(line string) (level int, title string, ok bool) {
	m := headingRe.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	level = len(m[1])
	rest := closingHashRe.ReplaceAllString(m[2], "")
	title = strings.TrimSpace(rest)
	if title == "" {
		title = strings.Repeat("#", level)
	}
	return level, title, true
}
