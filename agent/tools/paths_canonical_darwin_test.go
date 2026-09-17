//go:build darwin

package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceDescriptorNameDoesNotBorrowHardlink(t *testing.T) {
	for _, otherDir := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-parent", true: "different-parent"}[otherDir], func(t *testing.T) {
			root := t.TempDir()
			secret := filepath.Join(root, "Secret.txt")
			if err := os.WriteFile(secret, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(root, "Other.txt")
			if otherDir {
				if err := os.Mkdir(filepath.Join(root, "elsewhere"), 0700); err != nil {
					t.Fatal(err)
				}
				other = filepath.Join(root, "elsewhere", "Secret.txt")
			}
			if err := os.Link(secret, other); err != nil {
				t.Fatal(err)
			}
			parent, err := os.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = parent.Close() }()
			f, err := os.Open(secret)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			alias, err := os.Open(other)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = alias.Close() }()
			// Opening another hardlink changes Darwin's cached vnode path even
			// for f. Neither a different name nor parent is policy evidence.
			if name, err := workspaceDescriptorName(parent, f, "Secret.txt"); err == nil {
				t.Fatalf("borrowed hardlink path accepted as %q", name)
			}
		})
	}
}
