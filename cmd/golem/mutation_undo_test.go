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

func TestMutationUndoAdmissionGap(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, existed := range []bool{false, true} {
			for _, victim := range []string{"outside", "denied"} {
				name := "ram/"
				if persistent {
					name = "checkpoint/"
				}
				if existed {
					name += "restore/"
				} else {
					name += "delete/"
				}
				t.Run(name+victim, func(t *testing.T) {
					must := func(err error) {
						t.Helper()
						if err != nil {
							t.Fatal(err)
						}
					}
					base := t.TempDir()
					root := filepath.Join(base, "root")
					allowed := filepath.Join(root, "allowed")
					denied := filepath.Join(root, "denied")
					outside := filepath.Join(base, "outside")
					for _, dir := range []string{allowed, denied, outside} {
						must(os.MkdirAll(dir, 0700))
					}
					write := func(dir, body string) { must(os.WriteFile(filepath.Join(dir, "file"), []byte(body), 0600)) }
					write(outside, "OUTSIDE\n")
					write(denied, "DENIED\n")
					ws, err := agenttools.NewWorkspace(root)
					must(err)
					var undo func(*strings.Builder)
					var checkState func()
					if persistent {
						store := openTestStore(t, root)
						journal := newTestCheckpointJournal(t, ws, store)
						if existed {
							write(allowed, "BEFORE\n")
						}
						_, _ = beginTestTurn(t, journal, "#552 precondition gap")
						result := applyTool(t, agenttools.NewMutatingTools(ws, journal), "write_file", map[string]any{"path": "allowed/file", "content": "AFTER\n"})
						if result.IsError {
							t.Fatal(result.Content)
						}
						mustSealTurn(t, journal)
						undo = func(out *strings.Builder) { journal.undo(context.Background(), out, 1) }
						checkState = func() {
							if !errors.Is(journal.fatal, agenttools.ErrPreconditionMismatch) {
								t.Errorf("late mismatch not latched: %v", journal.fatal)
							}
							groups, err := store.list(context.Background())
							must(err)
							if len(groups) != 1 {
								t.Fatalf("lost undo group: %v", groups)
							}
							files, err := store.loadFiles(context.Background(), groups[0].id, false)
							must(err)
							if len(files) != 1 || files[0].restored || !files[0].inverseMutationID.Valid {
								t.Fatalf("lost inverse intent/progress: %+v", files)
							}
						}
					} else {
						write(allowed, "AFTER\n")
						journal := newMutationJournal(ws)
						journal.Record(agenttools.MutationRecord{Path: "allowed/file", PriorContent: []byte("BEFORE\n"), Existed: existed, AfterHash: hashFor("AFTER\n")})
						undo = func(out *strings.Builder) { journal.undo(out) }
						checkState = func() {
							if len(journal.recs) != 1 {
								t.Error("refusal popped RAM history")
							}
						}
					}
					swapped := false
					reads := 0
					wanted := 1
					if persistent {
						wanted = 3
					}
					ws.SetScopeGuard(func(rel string, writing bool) error {
						if rel == "denied" || strings.HasPrefix(rel, "denied/") {
							return errors.New("denied canary")
						}
						if !writing {
							reads++
						}
						if !writing && reads == wanted && !swapped {
							must(os.Rename(allowed, allowed+"-old"))
							if victim == "outside" {
								must(os.Rename(outside, allowed))
								outside = allowed
							} else {
								must(os.Rename(denied, allowed))
								denied = allowed
							}
							swapped = true
						}
						return nil
					})
					var output strings.Builder
					undo(&output)
					if !swapped {
						t.Fatal("precondition/action gap was not reached")
					}
					read := func(dir string) string {
						b, err := os.ReadFile(filepath.Join(dir, "file"))
						if os.IsNotExist(err) {
							return "<absent>"
						}
						must(err)
						return string(b)
					}
					checkState()
					if strings.Contains(output.String(), "undid ") || !strings.Contains(output.String(), "cannot undo") {
						t.Errorf("false undo result: %s", output.String())
					}
					if read(allowed+"-old") != "AFTER\n" {
						t.Error("refused undo modified original")
					}
					outBody, deniedBody := read(outside), read(denied)
					t.Logf("undo=%q outside=%q denied=%q original=%q", output.String(), outBody, deniedBody, read(allowed+"-old"))
					if outBody != "OUTSIDE\n" || deniedBody != "DENIED\n" {
						t.Error("SECURITY: undo mutated a directory whose content was not checked")
					}
				})
			}
		}
	}
}
