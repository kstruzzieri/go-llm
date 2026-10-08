//go:build windows

package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewWorkspaceWithOpenDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	open, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	if _, err := NewWorkspace(root); err != nil {
		t.Errorf("NewWorkspace with another readable directory handle = %v, want nil", err)
	}
}

func TestNewWorkspaceLongPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for len(root) <= 260 {
		root = filepath.Join(root, "long-directory-name")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace with a %d-character path = %v, want nil", len(root), err)
	}
	if err := ws.VerifyRoot(); err != nil {
		t.Errorf("VerifyRoot with a %d-character path = %v, want nil", len(root), err)
	}
}
