package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// planKeyRC builds a RunCommand rooted in a temp workspace and returns the
// root (the Workspace exposes no root accessor). nil runner: Plan never spawns.
func planKeyRC(t *testing.T) (*RunCommand, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunCommand(ws, nil), root
}

func execPlanKey(t *testing.T, rc *RunCommand, raw string) string {
	t.Helper()
	p, err := rc.Plan(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Plan(%s): %v", raw, err)
	}
	return p.ApprovalKey
}

func TestExecApprovalKeyStableAndNamespaced(t *testing.T) {
	rc, _ := planKeyRC(t)
	const raw = `{"argv":["go","version"]}`
	k1 := execPlanKey(t, rc, raw)
	k2 := execPlanKey(t, rc, raw)
	if k1 == "" || k1 != k2 {
		t.Fatalf("identical plans must share one non-empty key: %q vs %q", k1, k2)
	}
	// White-box namespace + recipe pin: an exec key must never be
	// constructible as a class key, and the recipe tag makes any future
	// fingerprint change an explicit migration.
	if !strings.HasPrefix(k1, "exec:v2:") {
		t.Fatalf("exec key %q must be namespaced with \"exec:v2:\"", k1)
	}
}

func TestExecApprovalKeyVariesByEachInput(t *testing.T) {
	t.Setenv("LANG", "C") // pin env shape so the baseline is deterministic
	rc, root := planKeyRC(t)
	base := execPlanKey(t, rc, `{"argv":["go","version"]}`)
	variants := map[string]string{
		"argv":    `{"argv":["go","env"]}`,
		"timeout": `{"argv":["go","version"],"timeout_seconds":30}`,
	}
	for name, raw := range variants {
		if got := execPlanKey(t, rc, raw); got == base {
			t.Errorf("%s change did not change the key (%q)", name, got)
		}
	}
	// cwd: same argv from a subdirectory.
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := execPlanKey(t, rc, `{"argv":["go","version"],"dir":"sub"}`); got == base {
		t.Errorf("cwd change did not change the key (%q)", got)
	}
	// env VALUE: same shape (LANG still present), different value (D13/REV 4:
	// values materially change behavior; "exact command" hashes them).
	t.Setenv("LANG", "en_US.UTF-8")
	if got := execPlanKey(t, rc, `{"argv":["go","version"]}`); got == base {
		t.Errorf("env-value change did not change the key (%q)", got)
	}
	// env shape: drop LANG from the allowlisted parent env entirely.
	if err := os.Unsetenv("LANG"); err != nil {
		t.Fatal(err)
	}
	if got := execPlanKey(t, rc, `{"argv":["go","version"]}`); got == base {
		t.Errorf("env-shape change did not change the key (%q)", got)
	}
}

func TestExecApprovalKeyVariesByRequestedTimeout(t *testing.T) {
	rc, _ := planKeyRC(t)
	omitted := execPlanKey(t, rc, `{"argv":["go","version"]}`)
	explicitDefault := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":60}`)
	if omitted == explicitDefault {
		t.Fatalf("omitted and explicit default timeouts must not share a key (%q)", omitted)
	}
	key601 := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":601}`)
	key999 := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":999}`)
	if key601 == key999 {
		t.Fatalf("different requested timeouts must not share a key (%q)", key601)
	}
}

func TestExecApprovalKeyVariesWithResolvedExecutable(t *testing.T) {
	// Same argv and environment, but a new binary shadows the old executable in
	// an earlier PATH directory. The resolved path must independently change the
	// key or an old grant silently authorizes a different executable.
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH-resolution fixture")
	}
	rc, _ := planKeyRC(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dirA+string(os.PathListSeparator)+dirB)
	keyB := execPlanKey(t, rc, `{"argv":["mytool"]}`)
	if err := os.WriteFile(filepath.Join(dirA, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	keyA := execPlanKey(t, rc, `{"argv":["mytool"]}`)
	if keyA == keyB {
		t.Fatalf("a different resolved executable must change the key (%q)", keyA)
	}
}

func TestWriteAndEditShareTheWriteClassKey(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	wp, err := NewWriteFile(ws, nil).Plan(context.Background(),
		json.RawMessage(`{"path":"f.txt","content":"new\n"}`))
	if err != nil {
		t.Fatalf("write Plan: %v", err)
	}
	ep, err := NewEditFile(ws, nil).Plan(context.Background(),
		json.RawMessage(`{"path":"f.txt","old_string":"old\n","new_string":"new\n"}`))
	if err != nil {
		t.Fatalf("edit Plan: %v", err)
	}
	if wp.ApprovalKey != WriteClassApprovalKey || ep.ApprovalKey != WriteClassApprovalKey {
		t.Fatalf("write=%q edit=%q, want both == WriteClassApprovalKey (%q)",
			wp.ApprovalKey, ep.ApprovalKey, WriteClassApprovalKey)
	}
}
