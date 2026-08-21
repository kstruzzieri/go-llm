//go:build unix

package profiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Group/world-accessible profiles dir → store_unsafe (the private boundary
// is profiles/, not the shared root).
func TestProfilesDirInsecureModeRefused(t *testing.T) {
	root := t.TempDir()
	seedUserProfile(t, root, "x", storeFixtureBody)
	if err := os.Chmod(filepath.Join(root, "profiles"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(root).List(context.Background()); CodeOf(err) != CodeStoreUnsafe {
		t.Fatal("world-readable profiles dir must be refused")
	}
}
