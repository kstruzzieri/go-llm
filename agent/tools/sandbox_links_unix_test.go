//go:build darwin || linux

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Direct unit coverage for the shared workspace hard-link validator (#442,
// relocated for #441). Backend-level coverage of the darwin call site lives in
// exec_darwin_test.go; the Linux call site is covered in exec_linux_test.go.

func TestValidateSandboxWorkspaceLinksAllowsInternalLinks(t *testing.T) {
	ws := t.TempDir()
	first := filepath.Join(ws, "first")
	if err := os.WriteFile(first, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(ws, "second")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(ws, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "sub", "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateSandboxWorkspaceLinks(ws); err != nil {
		t.Fatalf("wholly internal links rejected: %v", err)
	}
}

func TestValidateSandboxWorkspaceLinksRejectsExternalLink(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside-canary")
	if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(ws, "pre-linked")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	err := validateSandboxWorkspaceLinks(ws)
	if err == nil {
		t.Fatal("externally linked workspace entry accepted")
	}
	if !strings.Contains(err.Error(), "linked outside the workspace") ||
		!strings.Contains(err.Error(), `"pre-linked"`) {
		t.Fatalf("error must name the escaping entry: %v", err)
	}
}

func TestValidateSandboxWorkspaceLinksSkipsSymlinks(t *testing.T) {
	ws := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "multi")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(outsideDir, "multi2")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "ptr")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validateSandboxWorkspaceLinks(ws); err != nil {
		t.Fatalf("symlink treated as hard link: %v", err)
	}
}
