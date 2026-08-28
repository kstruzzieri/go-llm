package tools

// Cross-platform tests for the bwrap policy layer (#441): the broad-root
// predicate and (from Task 3) the argv builder run on every CI platform, the
// same discipline seatbelt.go uses for SBPL rendering.

import (
	"testing"
)

func TestBwrapBroadRoot(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/", true},
		{"/bin", true},
		{"/boot", true},
		{"/dev", true},
		{"/etc", true},
		{"/home", true},
		{"/lib", true},
		{"/lib32", true},
		{"/lib64", true},
		{"/lost+found", true},
		{"/media", true},
		{"/mnt", true},
		{"/nix", true},
		{"/opt", true},
		{"/proc", true},
		{"/root", true},
		{"/run", true},
		{"/sbin", true},
		{"/snap", true},
		{"/srv", true},
		{"/sys", true},
		{"/tmp", true},
		{"/usr", true},
		{"/var", true},
		{"/home/user", false},
		{"/home/user/proj", false},
		{"/usr/bin", false},
		{"/data", false},
		{"/workspaces", false},
		{"usr", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := bwrapBroadRoot(tc.path); got != tc.want {
			t.Errorf("bwrapBroadRoot(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
