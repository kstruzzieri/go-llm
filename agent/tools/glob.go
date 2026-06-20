package tools

import (
	"path"
	"strings"
)

// matchGlob matches a slash-separated relative path against a glob pattern,
// anchored at the workspace root. "*" and "?" match within a single segment
// (they never cross "/"); "**" matches zero or more whole segments. This is a
// deliberately small, root-anchored matcher — unlike rag's gitignore matcher,
// which matches a basename at any depth.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

// matchSegments matches pattern segments against name segments. Each "**"
// fans out across every possible skip count, so the worst case is combinatorial
// in (path-depth, number of "**" segments). In practice the matched names come
// from a real filesystem walk (bounded directory depth) and patterns carry 1–3
// "**", so the cost is small; this matcher is not intended for adversarial
// unbounded pattern/path input.
func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				return true // trailing ** matches every remaining segment
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}
