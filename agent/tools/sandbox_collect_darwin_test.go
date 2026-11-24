//go:build darwin

package tools

import "testing"

// TestSeatbeltInspectionCollectsNothingForCheck: on macOS the workspace walk
// Seatbelt runs before a spawn reads only what it protects. It records no
// layout, HEAD, identity, folded name or link the post-run check (Linux only)
// would use, and so reads no HEAD's contents.
func TestSeatbeltInspectionCollectsNothingForCheck(t *testing.T) {
	root := canonGitTempDir(t)
	writeGitFixture(t, root)
	runIn(t, root, `mkdir -p bare.git/objects bare.git/refs link && printf 'ref: refs/heads/main\n' > bare.git/HEAD && `+
		`ln -s ../nested/.git link/.git`)
	l, err := inspectSandboxWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.dotGitDirs) == 0 || len(l.metadata) == 0 {
		t.Fatalf("the walk found no git layout to protect: %+v", l)
	}
	if l.baseline != nil {
		t.Fatal("collected a post-run baseline")
	}
	for name, n := range map[string]int{
		"layoutHEADs": len(l.layoutHEADs), "headIDs": len(l.headIDs), "repoLayouts": len(l.repoLayouts),
		"innerLayouts": len(l.innerLayouts), "ids": len(l.ids), "dotGitLinks": len(l.dotGitLinks),
		"containerLinks": len(l.containerLinks),
	} {
		if n != 0 {
			t.Errorf("%s has %d entries", name, n)
		}
	}
}
