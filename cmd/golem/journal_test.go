package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

func hashFor(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newJournal(t *testing.T, root string) (*mutationJournal, *agenttools.Workspace) {
	t.Helper()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return newMutationJournal(ws), ws
}

func TestUndoRestoresOverwrite(t *testing.T) {
	root := t.TempDir()
	jr, ws := newJournal(t, root)
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("OLD"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteFileAtomic("a.txt", []byte("NEW"))
	jr.Record(agenttools.MutationRecord{
		Path: "a.txt", PriorContent: []byte("OLD"), Existed: true,
		AfterHash: hashFor("NEW"),
	})
	var out strings.Builder
	jr.undo(&out)
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "OLD" {
		t.Fatalf("undo did not restore prior content: %q", got)
	}
	if !strings.Contains(out.String(), "undid a.txt") {
		t.Fatalf("undo should report success: %q", out.String())
	}
}

func TestUndoDeletesCreatedFile(t *testing.T) {
	root := t.TempDir()
	jr, ws := newJournal(t, root)
	_ = ws.WriteFileAtomic("new.txt", []byte("DATA"))
	jr.Record(agenttools.MutationRecord{
		Path: "new.txt", Existed: false, AfterHash: hashFor("DATA"),
	})
	var out strings.Builder
	jr.undo(&out)
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("undo of a created file must delete it")
	}
}

func TestUndoRefusesWhenChangedSince(t *testing.T) {
	root := t.TempDir()
	jr, ws := newJournal(t, root)
	_ = ws.WriteFileAtomic("a.txt", []byte("NEW"))
	jr.Record(agenttools.MutationRecord{
		Path: "a.txt", PriorContent: []byte("OLD"), Existed: true, AfterHash: hashFor("NEW"),
	})
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("USER-EDIT"), 0o600)
	var out strings.Builder
	jr.undo(&out)
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "USER-EDIT" {
		t.Fatalf("undo must not clobber a changed file: %q", got)
	}
	if !strings.Contains(out.String(), "changed since") {
		t.Fatalf("undo should explain the refusal:\n%s", out.String())
	}
	// Record must remain on the stack: after restoring the expected state, a second
	// undo should now succeed.
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("NEW"), 0o600)
	var out2 strings.Builder
	jr.undo(&out2)
	got, _ = os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "OLD" {
		t.Fatalf("record should have survived the refused undo: %q", got)
	}
}

func TestUndoEmptyStack(t *testing.T) {
	root := t.TempDir()
	jr, _ := newJournal(t, root)
	var out strings.Builder
	jr.undo(&out)
	if !strings.Contains(out.String(), "nothing to undo") {
		t.Fatalf("empty stack message missing:\n%s", out.String())
	}
}

func TestUndoCreatedFileAlreadyAbsent(t *testing.T) {
	root := t.TempDir()
	jr, ws := newJournal(t, root)
	_ = ws.WriteFileAtomic("new.txt", []byte("DATA"))
	jr.Record(agenttools.MutationRecord{Path: "new.txt", Existed: false, AfterHash: hashFor("DATA")})
	// User deletes it before /undo.
	_ = os.Remove(filepath.Join(root, "new.txt"))
	var out strings.Builder
	jr.undo(&out)
	if !strings.Contains(out.String(), "already absent") {
		t.Fatalf("undo of an already-absent created file should report success: %q", out.String())
	}
	// Record must be popped (a second undo says nothing-to-undo).
	var out2 strings.Builder
	jr.undo(&out2)
	if !strings.Contains(out2.String(), "nothing to undo") {
		t.Fatalf("record should have been popped: %q", out2.String())
	}
}

func TestUndoCreatedFileRefusesNonRegularReplacement(t *testing.T) {
	root := t.TempDir()
	jr, ws := newJournal(t, root)
	_ = ws.WriteFileAtomic("new.txt", []byte("DATA"))
	jr.Record(agenttools.MutationRecord{Path: "new.txt", Existed: false, AfterHash: hashFor("DATA")})
	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "new.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	jr.undo(&out)
	if !strings.Contains(out.String(), "changed since") {
		t.Fatalf("undo should refuse non-regular replacement: %q", out.String())
	}
	if fi, err := os.Stat(filepath.Join(root, "new.txt")); err != nil || !fi.IsDir() {
		t.Fatalf("replacement directory should remain: fi=%v err=%v", fi, err)
	}

	if err := os.Remove(filepath.Join(root, "new.txt")); err != nil {
		t.Fatal(err)
	}
	_ = ws.WriteFileAtomic("new.txt", []byte("DATA"))
	var out2 strings.Builder
	jr.undo(&out2)
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("record should remain after refusal and later undo should delete file: %v", err)
	}
}

// --- #443 Task 8: tracked-mode guard on RAM undo ---

func TestRAMUndoTrackedModeGuard(t *testing.T) {
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	j := newMutationJournal(ws)
	p := filepath.Join(root, "promoted.txt")
	if err := os.WriteFile(p, []byte("artifact"), 0o640); err != nil {
		t.Fatal(err)
	}
	j.Record(agenttools.MutationRecord{
		Path:        "promoted.txt",
		Existed:     false,
		AfterHash:   agenttools.ContentHash([]byte("artifact")),
		TrackedMode: true,
		AfterMode:   0o640,
	})

	// Identical bytes, drifted mode: refuse and retain the record.
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	j.undo(&out)
	if !strings.Contains(out.String(), "cannot undo") {
		t.Fatalf("mode drift with identical bytes must refuse: %q", out.String())
	}
	if _, err := os.Lstat(p); err != nil {
		t.Fatal("refused undo must not delete the file")
	}

	// Exact bytes and mode: undo deletes the created file.
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	j.undo(&out)
	if !strings.Contains(out.String(), "undid promoted.txt") {
		t.Fatalf("exact mode must permit undo: %q", out.String())
	}
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("undo must delete the created file, err=%v", err)
	}
}

func TestRAMUndoLegacyRecordIgnoresMode(t *testing.T) {
	root := t.TempDir()
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	j := newMutationJournal(ws)
	p := filepath.Join(root, "legacy.txt")
	if err := os.WriteFile(p, []byte("bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	j.Record(agenttools.MutationRecord{
		Path:      "legacy.txt",
		Existed:   false,
		AfterHash: agenttools.ContentHash([]byte("bytes")),
	})
	// Mode drift on a legacy record stays invisible: byte-hash-only.
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	j.undo(&out)
	if !strings.Contains(out.String(), "undid legacy.txt") {
		t.Fatalf("legacy record must keep byte-hash-only semantics: %q", out.String())
	}
}
