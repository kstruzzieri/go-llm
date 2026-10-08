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

// errFileChanged is agent/tools' unexported identity sentinel, reached through
// the exported ErrRootReplaced that wraps it.
var errFileChanged = errors.Unwrap(agenttools.ErrRootReplaced)

// TestMutationUndoAdmissionGap: an undo whose target changes between the
// precondition read and the mutation must refuse, keep its record or intent,
// and never touch content it did not check (#552). "outside" and "denied" swap
// the admitted directory inside the precondition read's own guard decision;
// #613's post-guard reachability check refuses that read (spec §4.11), so the
// refusal is early. "edited" rewrites the file in place inside the write
// decision, after the read verified it, so only the late IfMatch check can
// refuse it.
func TestMutationUndoAdmissionGap(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		for _, existed := range []bool{false, true} {
			for _, victim := range []string{"outside", "denied", "edited"} {
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
							want := errFileChanged
							if victim == "edited" {
								want = agenttools.ErrPreconditionMismatch
							}
							if !errors.Is(journal.fatal, want) {
								t.Errorf("refusal not latched as %v: %v", want, journal.fatal)
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
						if victim == "edited" && writing && !swapped {
							// Same inode in the same admitted directory: the
							// precondition read already passed.
							write(allowed, "EDITED\n")
							swapped = true
						}
						if victim != "edited" && !writing && reads == wanted && !swapped {
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
					// Exact output names the refusing check. A swap is refused by
					// the precondition read itself: no IfMatch cause line. Only
					// "edited" reaches the late IfMatch check and its cause line.
					want := "cannot undo allowed/file: file changed since golem wrote it\n"
					if persistent {
						want = "undo failed for allowed/file: file identity changed between stat and open\n"
					}
					if victim == "edited" {
						want = "cannot undo allowed/file: file changed since golem wrote it\nundo failed for allowed/file: file precondition mismatch\n"
					}
					if persistent {
						want += "undo interrupted; run /undo to resume\n"
					}
					if output.String() != want {
						t.Errorf("undo result %q, want %q", output.String(), want)
					}
					original, body := allowed+"-old", "AFTER\n"
					if victim == "edited" {
						original, body = allowed, "EDITED\n"
					}
					if read(original) != body {
						t.Error("refused undo modified original")
					}
					outBody, deniedBody := read(outside), read(denied)
					t.Logf("undo=%q outside=%q denied=%q original=%q", output.String(), outBody, deniedBody, read(original))
					if outBody != "OUTSIDE\n" || deniedBody != "DENIED\n" {
						t.Error("SECURITY: undo mutated a directory whose content was not checked")
					}
				})
			}
		}
	}
}
