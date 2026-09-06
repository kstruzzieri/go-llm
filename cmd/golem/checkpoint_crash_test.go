package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/signing"
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
	signer, verifier, _, err := loadMutationSigning(ctx, testGetenv(dataDir), root, s)
	if err != nil {
		t.Fatal(err)
	}
	j := newCheckpointJournal(ws, s, signer, verifier)

	ready := func() {
		_, _ = os.Stdout.WriteString("READY\n")
		select {} // block until killed
	}

	if mode == "undo-prepared" {
		groups, err := s.newestCompleted(ctx, 1)
		if err != nil || len(groups) != 1 {
			t.Fatal("missing undo group")
		}
		if err := s.markUndoing(ctx, []int64{groups[0].id}); err != nil {
			t.Fatal(err)
		}
		f := groups[0].files[0]
		entry, err := s.loadReceipt(ctx, f.forwardMutationID.String)
		if err != nil {
			t.Fatal(err)
		}
		forward, err := authenticateCheckpointReceipt(ctx, verifier, entry)
		if err != nil {
			t.Fatal(err)
		}
		intent, err := agenttools.SignMutationReceipt(ctx, signer, storeInverse(forward.Body, rand.Text()))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signing.MarshalCanonical(intent)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.prepareInverseIntent(ctx, f.id, raw); err != nil {
			t.Fatal(err)
		}
		ready()
	}
	if mode == "undo-committed" {
		checkpointSQL(t, s.db, `CREATE TRIGGER pause_checkpoint_delete BEFORE DELETE ON checkpoints BEGIN SELECT RAISE(ABORT,'test pause after inverse commit'); END`)
		var out strings.Builder
		j.undo(ctx, &out, 1)
		groups, err := s.undoingGroups(ctx)
		if err != nil || len(groups) != 1 || !groups[0].files[0].restored {
			t.Fatalf("not committed: %s", out.String())
		}
		ready()
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
	if mode == "same-byte" {
		after = prior
	}
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
	if mode == "renamed" || mode == "same-byte" {
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
		mode           string
		wantContent    string
		wantApplied    bool
		wantCheckpoint bool // recovery keeps the row for undo
	}{
		{"prepared", "A0\n", false, false},
		{"same-byte", "A0\n", false, false},
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
			signer, verifier, _, err := loadMutationSigning(ctx, testGetenv(dataDir), root, s)
			if err != nil {
				t.Fatal(err)
			}
			j := newCheckpointJournal(ws, s, signer, verifier)
			notice, err := j.recoverStartup(ctx)
			if err != nil {
				t.Fatalf("recoverStartup: %v", err)
			}
			if notice == "" || strings.Contains(notice, "unconfirmed") == c.wantApplied {
				t.Fatalf("recovery evidence notice = %q", notice)
			}
			entries, err := s.scanReceipts(ctx, 0, 100)
			if err != nil || len(entries) != 1 || (entries[0].appliedJSON != nil) != c.wantApplied {
				t.Fatalf("crash evidence = %d,%v", len(entries), err)
			}
			rawIntent, rawApplied := append([]byte(nil), entries[0].intentJSON...), append([]byte(nil), entries[0].appliedJSON...)
			if err := agenttools.VerifyMutationReceipt(ctx, verifier, mustDecodeCrashReceipt(t, rawIntent)); err != nil {
				t.Fatal(err)
			}
			j.signer = journalTestSigner{Signer: j.signer, sign: func(context.Context, agenttools.MutationReceiptBody) error {
				t.Fatal("recovery signed historical success")
				return nil
			}}
			if next, err := j.recoverStartup(ctx); next != "" || err != nil {
				t.Fatalf("repeated recovery = %q,%v", next, err)
			}
			entries, err = s.scanReceipts(ctx, 0, 100)
			if err != nil || len(entries) != 1 || !bytes.Equal(entries[0].intentJSON, rawIntent) || !bytes.Equal(entries[0].appliedJSON, rawApplied) {
				t.Fatal("recovery changed/duplicated evidence")
			}
			infos, err := s.list(ctx)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if !c.wantCheckpoint {
				if len(infos) != 0 {
					t.Fatalf("infos = %+v, want the never-landed intent dropped", infos)
				}
				return
			}
			if len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 1 {
				t.Fatalf("infos = %+v, want one completed recovered checkpoint", infos)
			}
			j.signer = signer
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
	signer, verifier, _, err := loadMutationSigning(ctx, testGetenv(dataDir), root, s)
	if err != nil {
		t.Fatal(err)
	}
	j := newCheckpointJournal(ws, s, signer, verifier)
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

	signer2, verifier2, _, err := loadMutationSigning(ctx, testGetenv(dataDir), root, s2)
	if err != nil {
		t.Fatal(err)
	}
	j2 := newCheckpointJournal(ws, s2, signer2, verifier2)
	var out strings.Builder
	j2.undo(ctx, &out, 1)
	if !strings.Contains(out.String(), "resumed interrupted undo") {
		t.Fatalf("resume output = %q", out.String())
	}
	if !strings.Contains(out.String(), "undo target reached; interrupted attempt has no applied receipt") || !strings.Contains(out.String(), "[unconfirmed]") {
		t.Fatalf("crash evidence gap hidden: %s", out.String())
	}
	entries, err := s2.scanReceipts(ctx, 0, 100)
	if err != nil || len(entries) != 4 || entries[2].appliedJSON != nil || entries[3].appliedJSON == nil {
		t.Fatalf("resume fabricated or lost inverse: %d,%v", len(entries), err)
	}
	for _, entry := range entries {
		if _, err := authenticateCheckpointReceipt(ctx, verifier2, entry); err != nil {
			t.Fatal(err)
		}
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

func mustDecodeCrashReceipt(t *testing.T, raw []byte) agenttools.MutationReceipt {
	t.Helper()
	receipt, err := agenttools.DecodeMutationReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestCheckpointCrashRecoveryConservativeStates(t *testing.T) {
	for _, state := range []string{"prior", "expected", "divergent", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			root, data := t.TempDir(), t.TempDir()
			target := filepath.Join(root, "a.txt")
			if err := os.WriteFile(target, []byte("A0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			spawnCrashHelper(t, "prepared", root, data)
			switch state {
			case "expected":
				if err := os.WriteFile(target, []byte("A1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "divergent":
				if err := os.WriteFile(target, []byte("external"), 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			s, err := openCheckpointStore(ctx, testGetenv(data), root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, j, _, err := buildWriteTools(root, s, testGetenv(data))
			if err != nil {
				t.Fatal(err)
			}
			j.signer = journalTestSigner{Signer: j.signer, sign: func(context.Context, agenttools.MutationReceiptBody) error {
				t.Fatal("recovery signed historical success")
				return nil
			}}
			before, err := s.scanReceipts(ctx, 0, 10)
			if err != nil || len(before) != 1 {
				t.Fatal("missing crash intent")
			}
			for range 2 {
				notice, err := j.recoverStartup(ctx)
				if (err != nil) != (state == "unreadable") {
					t.Fatalf("classify %s: %q,%v", state, notice, err)
				}
			}
			after, err := s.scanReceipts(ctx, 0, 10)
			if err != nil || len(after) != 1 || after[0].appliedJSON != nil || !bytes.Equal(before[0].intentJSON, after[0].intentJSON) {
				t.Fatal("recovery fabricated/replaced evidence")
			}
			infos, err := s.list(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if state == "prior" {
				if len(infos) != 0 {
					t.Fatal("prior checkpoint retained")
				}
				return
			}
			want := checkpointCompleted
			if state == "unreadable" {
				want = checkpointOpen
			}
			if len(infos) != 1 || infos[0].state != want || infos[0].files != 1 && state != "unreadable" {
				t.Fatalf("classification = %+v", infos)
			}
		})
	}
}

func TestCheckpointUndoCrashBeforeMutationAndAfterCommit(t *testing.T) {
	for _, mode := range []string{"undo-prepared", "undo-committed"} {
		t.Run(mode, func(t *testing.T) {
			root, data := t.TempDir(), t.TempDir()
			ctx := context.Background()
			s, err := openCheckpointStore(ctx, testGetenv(data), root)
			if err != nil {
				t.Fatal(err)
			}
			mutators, j, _, err := buildWriteTools(root, s, testGetenv(data))
			if err != nil {
				t.Fatal(err)
			}
			beginTestTurn(t, j, "crash inverse")
			applyTool(t, mutators, "write_file", map[string]any{"path": "a.txt", "content": "x"})
			mustSealTurn(t, j)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			spawnCrashHelper(t, mode, root, data)
			s, err = openCheckpointStore(ctx, testGetenv(data), root)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, j, _, err = buildWriteTools(root, s, testGetenv(data))
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.scanReceipts(ctx, 0, 100)
			if err != nil || len(before) != 2 {
				t.Fatalf("crash ledger = %d,%v", len(before), err)
			}
			if mode == "undo-committed" {
				if before[1].appliedJSON == nil {
					t.Fatal("commit missing applied evidence")
				}
				checkpointSQL(t, s.db, `DROP TRIGGER pause_checkpoint_delete`)
				if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("user"), 0600); err != nil {
					t.Fatal(err)
				}
				j.signer = journalTestSigner{Signer: j.signer, sign: func(context.Context, agenttools.MutationReceiptBody) error {
					t.Fatal("committed inverse signed again")
					return nil
				}}
			} else {
				if before[1].appliedJSON != nil {
					t.Fatal("prepared inverse claims applied")
				}
				got, _ := readWorkspace(t, root, "a.txt")
				if string(got) != "x" {
					t.Fatal("prepared attempt changed file")
				}
			}
			out := runUndo(t, j, 1)
			after, err := s.scanReceipts(ctx, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before[1].intentJSON, after[1].intentJSON) || !bytes.Equal(before[1].appliedJSON, after[1].appliedJSON) {
				t.Fatal("restart changed original attempt")
			}
			if mode == "undo-prepared" {
				if len(after) != 3 || after[2].appliedJSON == nil || after[2].mutationID == before[1].mutationID || !strings.Contains(out, "[unconfirmed]") {
					t.Fatalf("restart reused identity or hid uncertainty: %s", out)
				}
				if _, ok := readWorkspace(t, root, "a.txt"); ok {
					t.Fatal("retry did not delete")
				}
			} else {
				got, _ := readWorkspace(t, root, "a.txt")
				if string(got) != "user" || len(after) != 2 {
					t.Fatal("committed resume changed user edit or evidence")
				}
			}
			if len(listIDs(t, s)) != 0 {
				t.Fatalf("resume incomplete: %s", out)
			}
		})
	}
}
