package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// recheckExecPlan must copy the approved identities onto the spec so the
// scratch and sandbox checks have something to compare against.
func TestRecheckExecPlanCarriesIdentities(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec semantics")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "dir", "tool.sh")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	pp, err := prepareExecPlan(ws, []string{"./tool.sh"}, "dir", time.Minute, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := recheckExecPlan(ws, pp)
	if err != nil {
		t.Fatal(err)
	}
	if spec.ExeIdentity == nil || !os.SameFile(spec.ExeIdentity, pp.identity) {
		t.Fatalf("ExeIdentity = %v, want the approved executable identity", spec.ExeIdentity)
	}
	if spec.DirIdentity == nil || !os.SameFile(spec.DirIdentity, pp.dirIdentity) {
		t.Fatalf("DirIdentity = %v, want the approved cwd identity", spec.DirIdentity)
	}
	if spec.RootIdentity == nil || !os.SameFile(spec.RootIdentity, pp.workspaceIdentity) {
		t.Fatalf("RootIdentity = %v, want the approved root identity", spec.RootIdentity)
	}
}
