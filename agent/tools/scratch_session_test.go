package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestScratchRuntime builds an enabled runtime rooted at a fresh canonical
// fixture, with the temp base pointed at a private test dir.
func newTestScratchRuntime(t *testing.T, cfg ScratchConfig) (*scratchRuntime, string) {
	t.Helper()
	canon := t.TempDir()
	for rel, data := range map[string]string{
		"main.go":        "package main\n",
		"scripts/run.sh": "#!/bin/sh\n",
		"dir/data.txt":   "data",
	} {
		p := filepath.Join(canon, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := newScratchRuntime(canon, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt.tempBase = t.TempDir()
	// Production specs carry the canonical root (Workspace.root); mirror it.
	return rt, rt.root
}

func testSpec(canon string) execSpec {
	return execSpec{
		Path:          "/bin/sh",
		Argv:          []string{"sh", "-c", "true"},
		Dir:           canon,
		Env:           []string{"PATH=/usr/bin:/bin", "HOME=/home/u", "TMPDIR=/ambient/tmp"},
		WorkspaceRoot: canon,
	}
}

func TestScratchSessionBeginLayoutAndRewrite(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	spec := testSpec(canon)
	spec.Dir = filepath.Join(canon, "dir")
	spec.Path = filepath.Join(canon, "scripts/run.sh")
	session, rewritten, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.discard()

	// Two independently guarded roots; the execution parent holds only
	// workspace/ and tmp/, and cannot reach the pristine reference by cd ..
	if filepath.Dir(session.reference) == filepath.Dir(session.work) {
		t.Fatalf("reference %q and work %q must live under separate parents", session.reference, session.work)
	}
	execParent := filepath.Dir(session.work)
	entries, err := os.ReadDir(execParent)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !slicesContains(names, "workspace") || !slicesContains(names, "tmp") {
		t.Fatalf("execution parent must hold exactly workspace/ and tmp/, got %v", names)
	}
	for _, p := range []string{filepath.Dir(session.reference), execParent} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s perm = %v, want 0700", p, fi.Mode().Perm())
		}
	}

	// Spec rewrite: root, deep cwd, workspace-local executable, TMPDIR.
	if rewritten.WorkspaceRoot != session.work {
		t.Fatalf("WorkspaceRoot = %q, want %q", rewritten.WorkspaceRoot, session.work)
	}
	if rewritten.Dir != filepath.Join(session.work, "dir") {
		t.Fatalf("Dir = %q", rewritten.Dir)
	}
	if rewritten.Path != filepath.Join(session.work, "scripts/run.sh") {
		t.Fatalf("workspace-local executable not remapped: %q", rewritten.Path)
	}
	wantTmp := filepath.Join(execParent, "tmp")
	var sawTmp bool
	for _, kv := range rewritten.Env {
		if strings.HasPrefix(kv, "TMPDIR=") {
			sawTmp = true
			if kv != "TMPDIR="+wantTmp {
				t.Fatalf("TMPDIR = %q, want %q", kv, "TMPDIR="+wantTmp)
			}
		}
	}
	if !sawTmp {
		t.Fatal("rewritten env must carry the scratch-private TMPDIR")
	}
	if !slicesContains(rewritten.Env, "PATH=/usr/bin:/bin") || !slicesContains(rewritten.Env, "HOME=/home/u") {
		t.Fatalf("unrelated env must be preserved: %v", rewritten.Env)
	}
	// Cloned content present in the work tree.
	if data, err := os.ReadFile(filepath.Join(session.work, "main.go")); err != nil || string(data) != "package main\n" {
		t.Fatalf("work tree content: %q err=%v", data, err)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestScratchSessionExternalExecutableUnchanged(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	spec := testSpec(canon)
	session, rewritten, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.discard()
	if rewritten.Path != "/bin/sh" {
		t.Fatalf("external executable must stay the rechecked approved path, got %q", rewritten.Path)
	}
}

func TestScratchSessionWorkspaceBinding(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	// A sub-root spoof is the distinguishing input: it survives the cwd
	// containment rewrite (Dir stays under the runtime root), so only the
	// explicit binding check can reject it.
	sub := filepath.Join(canon, "dir")
	spec := testSpec(sub)
	if _, _, err := beginScratchSession(context.Background(), rt, spec); err == nil {
		t.Fatal("a spec rooted at a sub-root of the runtime's canonical root must be rejected")
	}
	other := t.TempDir()
	if _, _, err := beginScratchSession(context.Background(), rt, testSpec(other)); err == nil {
		t.Fatal("a spec rooted outside the runtime's canonical root must be rejected")
	}
}

func TestScratchSessionAdmissionCap(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true, MaxConcurrentSessions: 2})
	spec := testSpec(canon)
	s1, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.discard()
	s2, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.discard()
	before, err := os.ReadDir(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := beginScratchSession(context.Background(), rt, spec); err == nil {
		t.Fatal("third concurrent session must be rejected")
	}
	after, err := os.ReadDir(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatal("rejected admission must not create temp roots")
	}
}

func TestScratchSessionSlotReleasedOnBeginFailure(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true, MaxConcurrentSessions: 1})
	rt.clone = func(f *os.File, dst string) error { return errors.New("boom") }
	spec := testSpec(canon)
	if _, _, err := beginScratchSession(context.Background(), rt, spec); err == nil {
		t.Fatal("clone failure must fail begin")
	}
	entries, err := os.ReadDir(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed begin must remove every partial root, found %v", entries)
	}
	// The single slot must have been released: a working begin now succeeds.
	rt.clone = cloneFile
	s, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatalf("slot leaked by failed begin: %v", err)
	}
	s.discard()
}

func TestScratchSessionFinishCapturesAndRemoves(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true, MaxConcurrentSessions: 1})
	spec := testSpec(canon)
	session, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.work, "artifact.txt"), []byte("made"), 0o644); err != nil {
		t.Fatal(err)
	}
	session.finish(context.Background())
	out, status := rt.store.get(session.id)
	if status != scratchStatusCaptured {
		t.Fatalf("status = %v, want captured", status)
	}
	found := false
	for _, c := range out.changes {
		if c.path == "artifact.txt" && c.kind == scratchChangeCreate {
			found = true
		}
	}
	if !found {
		t.Fatalf("capture missing artifact: %+v", out.changes)
	}
	for _, p := range []string{filepath.Dir(session.reference), filepath.Dir(session.work)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("session root %s must be removed after finish, err=%v", p, err)
		}
	}
	// Slot released exactly once: a new session fits the cap of 1.
	s2, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatalf("slot not released by finish: %v", err)
	}
	s2.discard()
}

func TestScratchSessionFinishCaptureErrorStillCleans(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	session, _, err := beginScratchSession(context.Background(), rt, testSpec(canon))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	session.finish(cancelled)
	out, status := rt.store.get(session.id)
	if status != scratchStatusCaptured {
		t.Fatalf("capture failure must still publish an outcome, status=%v", status)
	}
	if !out.truncated || out.captureErr == "" {
		t.Fatalf("failed capture must be truncated with a recorded error: %+v", out)
	}
	for _, p := range []string{filepath.Dir(session.reference), filepath.Dir(session.work)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("capture failure must not block cleanup of %s", p)
		}
	}
}

func TestScratchSessionCleanupGuardAbandonsImpostor(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	session, _, err := beginScratchSession(context.Background(), rt, testSpec(canon))
	if err != nil {
		t.Fatal(err)
	}
	// Replace the execution parent with an impostor directory.
	execParent := filepath.Dir(session.work)
	if err := os.RemoveAll(execParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(execParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(execParent, "innocent.txt"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	session.finish(context.Background())
	if _, err := os.Lstat(filepath.Join(execParent, "innocent.txt")); err != nil {
		t.Fatalf("guarded cleanup must abandon a replaced root, impostor content lost: %v", err)
	}
	out, status := rt.store.get(session.id)
	if status != scratchStatusCaptured || out.cleanupErr == "" {
		t.Fatalf("abandoned cleanup must be queryable on the outcome: status=%v out=%+v", status, out)
	}
}

func TestScratchSessionDoubleFinishAndDiscard(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true, MaxConcurrentSessions: 1})
	session, _, err := beginScratchSession(context.Background(), rt, testSpec(canon))
	if err != nil {
		t.Fatal(err)
	}
	session.finish(context.Background())
	session.finish(context.Background()) // idempotent
	if _, status := rt.store.get(session.id); status != scratchStatusCaptured {
		t.Fatal("outcome must survive a duplicate finish")
	}
	// discard after finish deletes the now-unreachable outcome (abandoned
	// background start), and is itself idempotent.
	session.discard()
	if _, status := rt.store.get(session.id); status != scratchStatusUnknown {
		t.Fatal("discard after finish must delete the unreachable outcome")
	}
	session.discard()

	// discard before finish tears down without publishing.
	s2, _, err := beginScratchSession(context.Background(), rt, testSpec(canon))
	if err != nil {
		t.Fatal(err)
	}
	s2.discard()
	if _, status := rt.store.get(s2.id); status != scratchStatusUnknown {
		t.Fatal("discarded session must publish nothing")
	}
	for _, p := range []string{filepath.Dir(s2.reference), filepath.Dir(s2.work)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discard must remove %s", p)
		}
	}
	s2.finish(context.Background()) // no-op after discard
	if _, status := rt.store.get(s2.id); status != scratchStatusUnknown {
		t.Fatal("finish after discard must not resurrect an outcome")
	}
}
