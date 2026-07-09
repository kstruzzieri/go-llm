package agentflow

import (
	"regexp"
	"strings"
	"sync"
)

// MatchesPath mirrors agentflow validation.py::matches_path exactly: for each
// non-blank pattern, a trailing-"/" pattern matches by prefix; otherwise an exact
// string match or an fnmatch-style glob match (where '*' crosses '/', unlike
// Go's path.Match) counts. Blank patterns are skipped.
func MatchesPath(path string, patterns []string) bool {
	for _, pat := range patterns {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
		if path == p || fnmatch(path, p) {
			return true
		}
	}
	return false
}

var fnmatchCache sync.Map // pattern -> *regexp.Regexp

func fnmatch(name, pattern string) bool {
	re := compileFnmatch(pattern)
	return re != nil && re.MatchString(name)
}

// compileFnmatch translates a Python fnmatch-style glob into an anchored Go
// regexp, mirroring fnmatch.translate: '*' -> any run of characters (including
// '/' and newlines - Python wraps the whole pattern in "(?s:...)", i.e. DOTALL),
// '?' -> any single character, and '[...]'/'[!...]' -> a (possibly negated)
// character class. Everything else is a literal.
func compileFnmatch(pattern string) *regexp.Regexp {
	if v, ok := fnmatchCache.Load(pattern); ok {
		return v.(*regexp.Regexp)
	}
	var b strings.Builder
	b.WriteString("(?s)^") // (?s): '.' matches '\n' too, matching Python's (?s:...) wrap
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			b.WriteString(".*") // fnmatch: '*' matches everything, including '/'
		case '?':
			b.WriteString(".")
		case '[':
			// find the closing ']'; if none, treat '[' literally (fnmatch behavior)
			j := i + 1
			negate := false
			if j < len(pattern) && pattern[j] == '!' {
				negate = true
				j++
			}
			if j < len(pattern) && pattern[j] == ']' {
				j++
			}
			for j < len(pattern) && pattern[j] != ']' {
				j++
			}
			if j >= len(pattern) {
				b.WriteString(regexp.QuoteMeta("["))
			} else {
				start := i + 1
				if negate {
					start++
				}
				class := pattern[start:j]
				if negate {
					class = "^" + class
				}
				b.WriteString("[" + class + "]")
				i = j
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	fnmatchCache.Store(pattern, re)
	return re
}

// EffectiveScope mirrors validation.py::effective_scope: (step.files intersected
// with top-level allowed_files) and the top-level blocked_files.
func EffectiveScope(plan *Plan, stepID string) (allowed, blocked []string) {
	var stepFiles []string
	for _, s := range plan.Steps {
		if s.ID == stepID {
			stepFiles = s.Files
			break
		}
	}
	for _, f := range stepFiles {
		for _, a := range plan.AllowedFiles {
			if MatchesPath(f, []string{a}) {
				allowed = append(allowed, f)
				break
			}
		}
	}
	return allowed, plan.BlockedFiles
}
