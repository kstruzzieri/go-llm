package tools

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false}, // * does not cross a separator (root-anchored)
		{"sub/*.go", "sub/main.go", true},
		{"**/*.go", "main.go", true},  // ** matches zero segments
		{"**/*.go", "a/b/c.go", true}, // ** matches many segments
		{"**", "anything/at/all.txt", true},
		{"a/**/d.go", "a/b/c/d.go", true},
		{"a/**/d.go", "a/d.go", true},    // ** zero segments in the middle
		{"a/**/d.go", "x/b/d.go", false}, // anchored: must start with a/
		{"?.txt", "a.txt", true},
		{"?.txt", "ab.txt", false},
		{"cmd/golem/*", "cmd/golem/main.go", true},
		{"cmd/golem/*", "cmd/other/main.go", false},
		{"[.go", "a.go", false},          // malformed bracket pattern: returns false, never panics
		{"*", ".env", true},              // path.Match matches dotfiles (no shell dot-exclusion)
		{"", "", true},                   // degenerate: both empty
		{"**/**/*.go", "a/b/c.go", true}, // redundant ** still matches
	}
	for _, tt := range tests {
		if got := matchGlob(tt.pattern, tt.name); got != tt.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}
