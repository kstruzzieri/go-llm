//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// TestMutationUndoLateModeAndCapability pins three schedules for undoing a
// tracked create whose bytes still match:
//   - read-swap: the admitted parent is swapped, inside the precondition read's
//     guard decision, for an outside directory holding the same bytes at another
//     mode. #613's post-guard reachability check refuses that read (spec §4.11),
//     so the refusal is early and no IfMatch runs.
//   - late-mode: the file is chmod'ed in place inside the write decision, after
//     the read verified it, so only the late IfMatch mode check can refuse it.
//   - admitted-move: the parent moves inside the write decision; the admitted
//     directory stays authoritative (#552) and the admitted file is deleted.
func TestMutationUndoLateModeAndCapability(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, variant := range []string{"read-swap", "late-mode", "admitted-move"} {
			t.Run(map[bool]string{false: "ram/", true: "checkpoint/"}[persistent]+variant, func(t *testing.T) {
				must := func(err error) {
					t.Helper()
					if err != nil {
						t.Fatal(err)
					}
				}
				base := t.TempDir()
				root := filepath.Join(base, "root")
				parent := filepath.Join(root, "parent")
				outside := filepath.Join(base, "outside")
				denied := filepath.Join(root, "denied")
				for _, dir := range []string{parent, outside, denied} {
					must(os.MkdirAll(dir, 0700))
				}
				must(os.WriteFile(filepath.Join(outside, "file"), []byte("AFTER\n"), 0644))
				must(os.WriteFile(filepath.Join(denied, "file"), []byte("DENIED\n"), 0600))
				ws, err := agenttools.NewWorkspace(root)
				must(err)
				var undo func(*strings.Builder)
				var ram *mutationJournal
				var checkpoint *checkpointJournal
				if persistent {
					store := openTestStore(t, root)
					checkpoint = newTestCheckpointJournal(t, ws, store)
					beginTestTurn(t, checkpoint, "tracked create")
					prepareTrackedCreate(t, checkpoint, root, "parent/file", "AFTER\n", 0600)
					mustSealTurn(t, checkpoint)
					undo = func(out *strings.Builder) { checkpoint.undo(context.Background(), out, 1) }
				} else {
					must(os.WriteFile(filepath.Join(parent, "file"), []byte("AFTER\n"), 0600))
					ram = newMutationJournal(ws)
					ram.Record(agenttools.MutationRecord{Path: "parent/file", AfterHash: hashFor("AFTER\n"), TrackedMode: true, AfterMode: 0600})
					undo = func(out *strings.Builder) { ram.undo(out) }
				}
				reads := 0
				wanted := 1
				if persistent {
					wanted = 3
				}
				reached := false
				ws.SetScopeGuard(func(rel string, write bool) error {
					if strings.HasPrefix(rel, "denied/") {
						return os.ErrPermission
					}
					if !write {
						reads++
					}
					if !reached && (variant == "read-swap" && !write && reads == wanted || variant != "read-swap" && write) {
						reached = true
						if variant == "late-mode" {
							must(os.Chmod(filepath.Join(parent, "file"), 0644))
						} else {
							must(os.Rename(parent, parent+"-old"))
							must(os.Rename(outside, parent))
							outside = parent
						}
					}
					return nil
				})
				var out strings.Builder
				undo(&out)
				if !reached {
					t.Fatal("late state boundary not reached")
				}
				read := func(path, want string) {
					t.Helper()
					b, err := os.ReadFile(path)
					if err != nil || string(b) != want {
						t.Fatalf("%s: %q %v want %q", path, b, err, want)
					}
				}
				read(filepath.Join(outside, "file"), "AFTER\n")
				read(filepath.Join(denied, "file"), "DENIED\n")
				switch variant {
				case "admitted-move":
					if _, err := os.Lstat(filepath.Join(parent+"-old", "file")); !os.IsNotExist(err) {
						t.Fatalf("admitted target not deleted: %v", err)
					}
				case "read-swap":
					read(filepath.Join(parent+"-old", "file"), "AFTER\n")
				case "late-mode":
					read(filepath.Join(parent, "file"), "AFTER\n")
				}
				// Exact output names the refusing check: only late-mode reaches
				// IfMatch and prints its cause line.
				refusal := "cannot undo parent/file: file changed since golem wrote it\n"
				late := refusal + "undo failed for parent/file: file precondition mismatch\n"
				want := map[string]string{"read-swap": refusal, "late-mode": late}[variant]
				if persistent {
					interrupted := "undo interrupted; run /undo to resume\n"
					want = map[string]string{
						"read-swap": "undo failed for parent/file: file identity changed between stat and open\n" + interrupted,
						"late-mode": late + interrupted,
					}[variant]
					wantFatal := map[string]error{"read-swap": errFileChanged, "late-mode": agenttools.ErrPreconditionMismatch}[variant]
					if checkpoint.fatal == nil || variant != "admitted-move" && !errors.Is(checkpoint.fatal, wantFatal) ||
						variant == "admitted-move" && checkpoint.fatal.Error() != "golem: inverse after-state mismatch" {
						t.Fatalf("failure not latched: %v output=%s", checkpoint.fatal, out.String())
					}
					groups, err := checkpoint.store.list(context.Background())
					must(err)
					if len(groups) != 1 {
						t.Fatalf("lost group: %v", groups)
					}
					files, err := checkpoint.store.loadFiles(context.Background(), groups[0].id, false)
					must(err)
					if len(files) != 1 || files[0].restored || !files[0].inverseMutationID.Valid {
						t.Fatalf("lost pending intent: %+v", files)
					}
					if strings.Contains(out.String(), "undid ") {
						t.Fatalf("false success: %s", out.String())
					}
				} else if variant == "admitted-move" {
					if len(ram.recs) != 0 || !strings.Contains(out.String(), "undid ") {
						t.Fatalf("successful capability undo not recorded: %s", out.String())
					}
				} else if len(ram.recs) != 1 {
					t.Fatalf("refusal popped RAM history: %q", out.String())
				}
				if variant != "admitted-move" && out.String() != want {
					t.Fatalf("undo result %q, want %q", out.String(), want)
				}
			})
		}
	}
}
