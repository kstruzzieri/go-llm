package main

import "testing"

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
