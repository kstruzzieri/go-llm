//go:build darwin || linux

package tools

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Compare both walks. A stale common-directory pointer must not turn the
// existing HEAD into a hold; an uncertain baseline must protect it as well.
func TestCommonDirLayoutTransitions(t *testing.T) {
	collectForCheck(t)
	for _, tc := range []struct {
		name    string
		pointer func(root string) string
		valid   bool
		unknown bool
	}{
		{"relative worktree pointer", func(string) string { return "../common\n" }, true, false},
		{"absolute worktree pointer", func(root string) string { return root + "/common\r\n" }, true, false},
		{"stale target", func(string) string { return "../missing\n" }, false, false},
		{"empty pointer", func(string) string { return "\n" }, false, false},
		{"stale intermediate", func(string) string { return "../missing/../common\n" }, false, false},
		{"common refs absent", func(string) string { return "../partial\n" }, false, false},
		{"outside target", func(string) string { return "../../outside\n" }, false, true},
		{"symlink component", func(string) string { return "../alias/../common\n" }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := canonGitTempDir(t)
			runIn(t, root, "mkdir -p work common/objects common/refs partial/objects && ln -s common alias && "+
				validHEADBody+" > work/HEAD")
			before, err := inspectSandboxWorkspace(root)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(root, "work", "commondir")
			if err := os.WriteFile(p, []byte(tc.pointer(root)), 0600); err != nil {
				t.Fatal(err)
			}
			after := inspectSandboxWorkspaceAfter(root)
			work := filepath.Dir(p)
			items := newGitMachinery(before, after)
			if slices.Contains(after.layoutHEADs, filepath.Join(work, "HEAD")) != tc.valid ||
				(len(items) > 0) != tc.valid || slices.Contains(after.unlisted, work) != tc.unknown {
				t.Fatalf("layouts %v, holds %v, unchecked %v", after.layoutHEADs, items, after.unlisted)
			}
			if tc.unknown {
				before, err := inspectSandboxWorkspace(root)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("../common"), 0600); err != nil {
					t.Fatal(err)
				}
				if items := newGitMachinery(before, inspectSandboxWorkspaceAfter(root)); len(items) > 0 {
					t.Fatalf("moved a HEAD whose original layout was uncertain: %v", items)
				}
			}
		})
	}
}

// A valid local objects/refs pair cannot override a stale commondir pointer.
func TestCommonDirSupersedesLocalMarkers(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir refs objects && "+validHEADBody+" > HEAD && echo missing > commondir")
	if got := inspectSandboxWorkspaceAfter(root); len(got.layoutHEADs) > 0 {
		t.Fatalf("stale common directory still produced a layout: %v", got.layoutHEADs)
	}
}

func TestLayoutObjectDirectoryOverride(t *testing.T) {
	root := canonGitTempDir(t)
	runIn(t, root, "mkdir refs && "+validHEADBody+" > HEAD")
	for _, tc := range []struct {
		path    string
		valid   bool
		unknown bool
	}{
		{root, true, false}, {filepath.Join(root, "absent"), false, false},
		{"", false, false}, {"relative-objects", false, true},
	} {
		t.Setenv("GIT_OBJECT_DIRECTORY", tc.path)
		got := inspectSandboxWorkspaceAfter(root)
		if (len(got.layoutHEADs) > 0) != tc.valid || (len(got.unlisted) > 0) != tc.unknown {
			t.Errorf("override %q: layouts %v, unchecked %v", tc.path, got.layoutHEADs, got.unlisted)
		}
	}
}
