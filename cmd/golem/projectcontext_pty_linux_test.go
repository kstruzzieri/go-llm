//go:build linux

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectTrustGoalLinuxTerminalFailsBeforeConfig(t *testing.T) {
	p := openPTY(t)
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeTrustDocument(t, root, "terminal-guidance")
	_, out, diag := runTestFiles(t)
	err := run([]string{"-root", root, "-config", filepath.Join(root, "missing"), "-goal", "inspect", "-trust-project-context", "sha256:" + strings.Repeat("0", 64)}, p.slave, out, diag)
	if err == nil || exitCodeFor(err) != 1 || !strings.Contains(err.Error(), "project context untrusted") {
		t.Fatalf("TTY preflight=%v, want trust exit1", err)
	}
}
