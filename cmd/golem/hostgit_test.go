package main

import (
	"path/filepath"
	"testing"
)

func TestParseGitVersion(t *testing.T) {
	ok := []struct {
		in           string
		major, minor int
	}{
		{"git version 2.38.0", 2, 38},
		{"git version 2.38.0.rc1", 2, 38},
		{"git version 2.50.1 (Apple Git-155)", 2, 50},
		{"git version 2.45.1.windows.1", 2, 45},
		{"git version 2.45.GIT", 2, 45},
		{"git version 3.0.0", 3, 0},
		{"git version 2.56.0\n", 2, 56},
	}
	for _, c := range ok {
		maj, min, err := parseGitVersion(c.in)
		if err != nil || maj != c.major || min != c.minor {
			t.Errorf("parseGitVersion(%q) = %d,%d,%v; want %d,%d", c.in, maj, min, err, c.major, c.minor)
		}
	}
	for _, in := range []string{"", "git version", "git version x.y", "git version 2", "git version 2.38abc", "git version v2.40.0", "version 2.40.0", "git version -2.40"} {
		if _, _, err := parseGitVersion(in); err == nil {
			t.Errorf("parseGitVersion(%q) succeeded; want error", in)
		}
	}
}

func TestGitVersionSupported(t *testing.T) {
	for _, c := range []struct {
		major, minor int
		want         bool
	}{{2, 37, false}, {2, 38, true}, {2, 9, false}, {3, 0, true}, {1, 99, false}} {
		if got := gitVersionSupported(c.major, c.minor); got != c.want {
			t.Errorf("gitVersionSupported(%d,%d) = %v; want %v", c.major, c.minor, got, c.want)
		}
	}
}

// TestPathWithinLexicalHalf pins the lexical half of pathWithin: it must
// decide without touching the filesystem, so a platform whose file identity
// lookup fails cannot make a git inside the workspace look outside.
func TestPathWithinLexicalHalf(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "ws")
	tests := []struct {
		name, path string
		want       bool
	}{
		{"child", filepath.Join(ws, "git"), true},
		{"deep child", filepath.Join(ws, "a", "b", "git"), true},
		{"dot-dot prefixed child", filepath.Join(ws, "..foo", "git"), true},
		{"sibling", filepath.Join(filepath.Dir(ws), "git"), false},
		{"sibling sharing a prefix", ws + "x" + string(filepath.Separator) + "git", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Neither path exists, so only the lexical half can answer.
			got, err := pathWithin(tt.path, ws)
			if tt.want && (err != nil || !got) {
				t.Errorf("pathWithin(%q, %q) = %v, %v; want true, nil", tt.path, ws, got, err)
			}
			if !tt.want && got {
				t.Errorf("pathWithin(%q, %q) = true, want false", tt.path, ws)
			}
		})
	}
}
