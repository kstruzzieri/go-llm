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

func TestMutationUndoLateModeAndCapability(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, moved := range []bool{false, true} {
			t.Run(map[bool]string{false: "ram", true: "checkpoint"}[persistent]+map[bool]string{false: "/mode", true: "/admitted-move"}[moved], func(t *testing.T) {
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
					if !reached && (moved && write || !moved && !write && reads == wanted) {
						reached = true
						must(os.Rename(parent, parent+"-old"))
						must(os.Rename(outside, parent))
						outside = parent
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
				if moved {
					if _, err := os.Lstat(filepath.Join(parent+"-old", "file")); !os.IsNotExist(err) {
						t.Fatalf("admitted target not deleted: %v", err)
					}
				} else {
					read(filepath.Join(parent+"-old", "file"), "AFTER\n")
				}
				if persistent {
					if checkpoint.fatal == nil || !moved && !errors.Is(checkpoint.fatal, agenttools.ErrPreconditionMismatch) ||
						moved && checkpoint.fatal.Error() != "golem: inverse after-state mismatch" {
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
				} else if moved {
					if len(ram.recs) != 0 || !strings.Contains(out.String(), "undid ") {
						t.Fatalf("successful capability undo not recorded: %s", out.String())
					}
				} else if len(ram.recs) != 1 || out.String() != "cannot undo parent/file: file changed since golem wrote it\nundo failed for parent/file: file precondition mismatch\n" {
					t.Fatalf("mode refusal lost: %q", out.String())
				}
			})
		}
	}
}
