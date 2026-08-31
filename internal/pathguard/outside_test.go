package pathguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateOutsideRejectsRootAndDescendants(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	for _, path := range []string{
		root,
		filepath.Join(root, "sessions.db"),
		filepath.Join(root, "nested", "missing", "sessions.db"),
	} {
		if err := ValidateOutside(path, root); err == nil {
			t.Errorf("ValidateOutside(%q) = nil, want inside-root rejection", path)
		}
	}
}

func TestValidateOutsideAllowsSiblingsAndParents(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	for _, path := range []string{
		filepath.Join(base, "repo-data", "sessions.db"), // sibling sharing a name prefix
		filepath.Join(base, "sessions.db"),
		filepath.Join(t.TempDir(), "golem", "sessions.db"),
	} {
		if err := ValidateOutside(path, root); err != nil {
			t.Errorf("ValidateOutside(%q) = %v, want nil", path, err)
		}
	}
}

func TestValidateOutsideRejectsSymlinkedAncestorIntoRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	link := filepath.Join(base, "data")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := ValidateOutside(filepath.Join(link, "golem", "sessions.db"), root); err == nil {
		t.Fatal("want symlinked path into root rejected")
	}
}

// TestValidateOutsideRejectsCaseAliasOfRoot pins the on-disk identity check:
// on case-insensitive filesystems (APFS default) a case-variant spelling of the
// root must not smuggle the data path inside the workspace.
func TestValidateOutsideRejectsCaseAliasOfRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	alias := filepath.Join(base, "WORKSPACE")
	rootInfo, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(rootInfo, aliasInfo) {
		t.Skip("filesystem is case-sensitive; alias does not resolve to root")
	}
	if err := ValidateOutside(filepath.Join(alias, "golem", "sessions.db"), root); err == nil {
		t.Fatal("want case-variant path into root rejected")
	}
}

func TestValidateOutsideNonexistentRootUsesLexicalCheck(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "not-created-yet")
	if err := ValidateOutside(filepath.Join(root, "sessions.db"), root); err == nil {
		t.Fatal("want path lexically inside nonexistent root rejected")
	}
	if err := ValidateOutside(filepath.Join(base, "elsewhere", "sessions.db"), root); err != nil {
		t.Fatalf("path outside nonexistent root rejected: %v", err)
	}
}
