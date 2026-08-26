package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// TestCheckpointCrashHelper is the subprocess body for the process-kill
// window tests. Selected by GO_LLM_CHECKPOINT_CRASH_MODE, it drives the real
// store/journal protocol up to the exact window, writes READY on stdout, and
// blocks until the parent kills it — so the on-disk SQLite/WAL and workspace
// state are genuinely mid-operation, with no deferred cleanup.
func TestCheckpointCrashHelper(t *testing.T) {
	mode := os.Getenv("GO_LLM_CHECKPOINT_CRASH_MODE")
	if mode == "" {
		return
	}
	root := os.Getenv("GO_LLM_CHECKPOINT_CRASH_ROOT")
	dataDir := os.Getenv("GO_LLM_CHECKPOINT_CRASH_DATA")
	ctx := context.Background()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("helper workspace: %v", err)
	}
	s, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("helper open: %v", err)
	}
	j := newCheckpointJournal(ws, s)

	ready := func() {
		_, _ = os.Stdout.WriteString("READY\n")
		select {} // block until killed
	}

	if mode == "undo" {
		j.crashAfterRestore = func(string) { ready() }
		var out strings.Builder
		j.undo(ctx, &out, 1)
		t.Fatalf("undo returned without reaching the crash window; output: %s", out.String())
	}

	// Forward windows drive one mutation of a.txt (A0 -> A1) through the
	// real prepared protocol, stopping at the selected point.
	if err := j.beginTurn(ctx, "crash turn", func() {}); err != nil {
		t.Fatalf("helper beginTurn: %v", err)
	}
	prior, err := os.ReadFile(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatalf("helper prior: %v", err)
	}
	after := []byte("A1\n")
	rec := agenttools.MutationRecord{
		Path: "a.txt", PriorContent: prior, Existed: true,
		AfterHash: agenttools.ContentHash(after), Summary: "write a.txt", At: time.Now(),
	}
	p, err := j.Prepare(rec)
	if err != nil {
		t.Fatalf("helper prepare: %v", err)
	}
	if mode == "prepared" {
		ready()
	}
	if err := ws.WriteFileAtomic("a.txt", after); err != nil {
		t.Fatalf("helper write: %v", err)
	}
	if mode == "renamed" {
		ready()
	}
	if err := p.Commit(); err != nil {
		t.Fatalf("helper commit: %v", err)
	}
	if mode == "committed" {
		ready()
	}
	t.Fatalf("unknown crash mode %q", mode)
}

// spawnCrashHelper starts the helper in mode, waits for its READY handshake
// (bounded by a context deadline, never a sleep), kills it, and reaps it.
func spawnCrashHelper(t *testing.T, mode, root, dataDir string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCheckpointCrashHelper$")
	cmd.Env = append(os.Environ(),
		"GO_LLM_CHECKPOINT_CRASH_MODE="+mode,
		"GO_LLM_CHECKPOINT_CRASH_ROOT="+root,
		"GO_LLM_CHECKPOINT_CRASH_DATA="+dataDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	readyCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == "READY" {
				readyCh <- nil
				return
			}
		}
		readyCh <- sc.Err()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case err := <-readyCh:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("helper stdout ended before READY: %v", err)
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("helper never reached its crash window")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_ = cmd.Wait() // killed: a nonzero exit is the point
}

func TestCheckpointForwardCrashWindows(t *testing.T) {
	cases := []struct {
		mode        string
		wantContent string
		wantApplied bool
		wantNotice  bool // recovery keeps the row and reports a checkpoint
	}{
		{"prepared", "A0\n", false, false},
		{"renamed", "A1\n", false, true},
		{"committed", "A1\n", true, true},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			root := t.TempDir()
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			spawnCrashHelper(t, c.mode, root, dataDir)

			// The kernel released the killed child's flock: reopen succeeds.
			ctx := context.Background()
			s, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
			if err != nil {
				t.Fatalf("reopen after kill: %v", err)
			}
			defer func() { _ = s.Close() }()

			got, err := os.ReadFile(filepath.Join(root, "a.txt"))
			if err != nil || string(got) != c.wantContent {
				t.Fatalf("a.txt = %q,%v, want %q at the %s window", got, err, c.wantContent, c.mode)
			}
			groups, err := s.loadGroups(ctx, checkpointOpen, 0, false)
			if err != nil || len(groups) != 1 || len(groups[0].files) != 1 {
				t.Fatalf("open groups = %+v, %v — want the crashed turn's row", groups, err)
			}
			if got := groups[0].files[0].applied; got != c.wantApplied {
				t.Fatalf("applied = %v, want %v at the %s window", got, c.wantApplied, c.mode)
			}

			ws, err := agenttools.NewWorkspace(root)
			if err != nil {
				t.Fatalf("workspace: %v", err)
			}
			j := newCheckpointJournal(ws, s)
			notice, err := j.recoverStartup(ctx)
			if err != nil {
				t.Fatalf("recoverStartup: %v", err)
			}
			if (notice != "") != c.wantNotice {
				t.Fatalf("notice = %q, wantNotice=%v", notice, c.wantNotice)
			}
			infos, err := s.list(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if !c.wantNotice {
				if len(infos) != 0 {
					t.Fatalf("infos = %+v, want the never-landed intent dropped", infos)
				}
				return
			}
			if len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 1 {
				t.Fatalf("infos = %+v, want one completed recovered checkpoint", infos)
			}
			// The recovered checkpoint is undoable: revert A1 back to A0.
			var undoOut strings.Builder
			j.undo(ctx, &undoOut, 1)
			if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "A0\n" {
				t.Fatalf("post-undo a.txt = %q,%v; undo output: %s", got, err, undoOut.String())
			}
		})
	}
}

func TestCheckpointUndoCrashResume(t *testing.T) {
	root := t.TempDir()
	dataDir := t.TempDir()
	ctx := context.Background()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	// Stage one sealed turn: a.txt modified (A0 -> A1) then b.txt created,
	// through the real prepared protocol.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("A0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	j := newCheckpointJournal(ws, s)
	if err := j.beginTurn(ctx, "crash turn", func() {}); err != nil {
		t.Fatalf("beginTurn: %v", err)
	}
	apply := func(path string, prior []byte, existed bool, content string) {
		t.Helper()
		rec := agenttools.MutationRecord{
			Path: path, PriorContent: prior, Existed: existed,
			AfterHash: agenttools.ContentHash([]byte(content)), Summary: "write " + path, At: time.Now(),
		}
		p, err := j.Prepare(rec)
		if err != nil {
			t.Fatalf("prepare %s: %v", path, err)
		}
		if err := ws.WriteFileAtomic(path, []byte(content)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := p.Commit(); err != nil {
			t.Fatalf("commit %s: %v", path, err)
		}
	}
	apply("a.txt", []byte("A0\n"), true, "A1\n")
	apply("b.txt", nil, false, "B1\n")
	if err := j.sealTurn(ctx); err != nil {
		t.Fatalf("sealTurn: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The child undoes and is killed after restoring the first reverse-order
	// file (b.txt: content removed) but BEFORE its progress flag persists.
	spawnCrashHelper(t, "undo", root, dataDir)

	s2, err := openCheckpointStore(ctx, testGetenv(dataDir), root)
	if err != nil {
		t.Fatalf("reopen after kill: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt: %v, want removed before the crash", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "A1\n" {
		t.Fatalf("a.txt = %q,%v, want untouched A1 at the crash", got, err)
	}
	groups, err := s2.undoingGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("undoingGroups = %+v, %v", groups, err)
	}
	// The crash window is BEFORE the progress flag: every row still reads
	// restored = 0 even though b.txt's content restore already landed.
	for _, f := range groups[0].files {
		if f.restored {
			t.Fatalf("%s flagged restored at the crash, want the pre-flag window", f.path)
		}
	}

	j2 := newCheckpointJournal(ws, s2)
	var out strings.Builder
	j2.undo(ctx, &out, 1)
	if !strings.Contains(out.String(), "resumed interrupted undo") {
		t.Fatalf("resume output = %q", out.String())
	}
	if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "A0\n" {
		t.Fatalf("a.txt = %q,%v, want A0 after resume", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt: %v, want absent after resume", err)
	}
	infos, err := s2.list(ctx)
	if err != nil || len(infos) != 0 {
		t.Fatalf("infos = %+v, %v — checkpoint deleted only after full restore", infos, err)
	}
}
