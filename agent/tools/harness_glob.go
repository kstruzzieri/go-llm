package tools

import (
	"path"
	"strings"
)

// opencodeInstructions reads the top-level "instructions" array of a decoded
// opencode.json (spec D5). Entries containing "://" are dropped; an entry with
// any of "*?[" is a pattern, any other a path. Non-array values and non-string
// or empty entries are ignored.
func opencodeInstructions(doc any) (paths, patterns []string) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, nil
	}
	list, ok := m["instructions"].([]any)
	if !ok {
		return nil, nil
	}
	for _, e := range list {
		s, ok := e.(string)
		if !ok || s == "" || strings.Contains(s, "://") {
			continue
		}
		if strings.ContainsAny(s, "*?[") {
			patterns = append(patterns, s)
		} else {
			paths = append(paths, s)
		}
	}
	return paths, patterns
}

// matchGlobBounded has matchGlob's semantics ("*", "?" and "[...]" within one
// segment, "**" for zero or more whole segments) but fills a table over
// (pattern segment, name segment), so it makes at most len(pat)*len(name)
// path.Match calls however many "**" the pattern holds.
func matchGlobBounded(pattern, name string) bool {
	pat, nm := strings.Split(pattern, "/"), strings.Split(name, "/")
	// ok[j] reports whether pat[i:] matches nm[j:]; rows are built from the end.
	ok := make([]bool, len(nm)+1)
	ok[len(nm)] = true // empty pattern matches only an empty name
	for i := len(pat) - 1; i >= 0; i-- {
		next := make([]bool, len(nm)+1)
		switch {
		case pat[i] == "**" && i == len(pat)-1:
			for j := range next {
				next[j] = true // trailing ** matches every remaining segment
			}
		case pat[i] == "**":
			for j := len(nm); j >= 0; j-- {
				next[j] = ok[j] || (j < len(nm) && next[j+1])
			}
		default:
			for j := 0; j < len(nm); j++ {
				if ok[j+1] {
					m, err := path.Match(pat[i], nm[j])
					next[j] = err == nil && m
				}
			}
		}
		ok = next
	}
	return ok[0]
}
