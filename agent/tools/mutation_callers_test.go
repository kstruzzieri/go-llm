//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// Replacing IfMatch with unconditional write must corrupt a canary and commit.
func TestMutationCallerPreparedGap(t *testing.T) {
	for _, op := range []string{"create", "overwrite", "edit"} {
		for _, victim := range []string{"outside", "denied"} {
			t.Run(op+"/"+victim, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "root")
				allowed := filepath.Join(root, "allowed")
				outside := filepath.Join(base, "outside")
				denied := filepath.Join(root, "denied")
				for _, dir := range []string{allowed, outside, denied} {
					mutationMust(t, os.MkdirAll(dir, 0700))
				}
				mutationMust(t, os.WriteFile(filepath.Join(outside, "file"), []byte("OUTSIDE\n"), 0600))
				mutationMust(t, os.WriteFile(filepath.Join(denied, "file"), []byte("DENIED\n"), 0600))
				if op != "create" {
					mutationMust(t, os.WriteFile(filepath.Join(allowed, "file"), []byte("ORIGINAL\n"), 0600))
				}
				ws, err := NewWorkspace(root)
				mutationMust(t, err)
				ws.SetScopeGuard(func(rel string, _ bool) error {
					if rel == "denied" || strings.HasPrefix(rel, "denied/") {
						return fs.ErrPermission
					}
					return nil
				})
				abortErr := errors.New("journal abort storage failure")
				journal := &prepJournal{abortErr: abortErr}
				reached := false
				journal.onPrepare = func(MutationRecord) {
					reached = true
					mutationMust(t, os.Rename(allowed, allowed+"-old"))
					if victim == "outside" {
						mutationMust(t, os.Rename(outside, allowed))
						outside = allowed
					} else {
						mutationMust(t, os.Rename(denied, allowed))
						denied = allowed
					}
				}
				var tool interface {
					agent.Tool
					agent.PlanningTool
				}
				raw := json.RawMessage(`{"path":"allowed/file","content":"APPROVED\n"}`)
				if op == "edit" {
					tool = NewEditFile(ws, journal)
					raw = json.RawMessage(`{"path":"allowed/file","old_string":"ORIGINAL","new_string":"APPROVED"}`)
				} else {
					tool = NewWriteFile(ws, journal)
				}
				_, err = tool.Plan(context.Background(), raw)
				mutationMust(t, err)
				res, err := tool.Invoke(context.Background(), raw)
				mutationMust(t, err)
				if !reached || !res.IsError || !strings.Contains(res.Content, "changed since preview") || !strings.Contains(res.Content, abortErr.Error()) {
					t.Errorf("reached=%v result=%+v", reached, res)
				}
				if !slices.Equal(journal.events, []string{"prepare", "abort"}) || len(journal.recs) != 0 {
					t.Errorf("journal falsely succeeded: %v %v", journal.events, journal.recs)
				}
				mutationBytes(t, filepath.Join(outside, "file"), "OUTSIDE\n")
				mutationBytes(t, filepath.Join(denied, "file"), "DENIED\n")
				mutationNoTemps(t, allowed)
				mutationNoTemps(t, allowed+"-old")
			})
		}
	}
}

func TestMutationErrorPresentation(t *testing.T) {
	cleanup := errors.New("temporary cleanup failed")
	abort := errors.New("journal abort failed")
	visible := toolVisibleError(errors.Join(ErrPreconditionMismatch, cleanup, abort))
	if !strings.Contains(visible.Error(), "changed since preview") || !errors.Is(visible, ErrPreconditionMismatch) || !errors.Is(visible, cleanup) || !errors.Is(visible, abort) {
		t.Fatalf("lost mismatch or secondary failure: %v", visible)
	}
	host := errors.New("private policy detail")
	visible = toolVisibleError(errors.Join(scopeDeniedError{cause: host}, cleanup))
	if strings.Contains(visible.Error(), host.Error()) || !errors.Is(visible, cleanup) || !errors.Is(visible, errScopeDenied) {
		t.Fatalf("unsafe/lossy denial: %v", visible)
	}
}

func TestMutationCallerMovedCapabilityCommits(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	moved := filepath.Join(t.TempDir(), "admitted")
	mutationMust(t, os.Mkdir(parent, 0700))
	mutationMust(t, os.WriteFile(filepath.Join(parent, "file"), []byte("ORIGINAL\n"), 0600))
	ws, err := NewWorkspace(root)
	mutationMust(t, err)
	journal := &prepJournal{}
	tool := NewWriteFile(ws, journal)
	raw := json.RawMessage(`{"path":"parent/file","content":"APPROVED\n"}`)
	_, err = tool.Plan(context.Background(), raw)
	mutationMust(t, err)
	reached := false
	ws.beforeMutation = func(at mutationPhase, _ string) error {
		if at == mutationAfterCheck {
			reached = true
			mutationMust(t, os.Rename(parent, moved))
		}
		return nil
	}
	result, err := tool.Invoke(context.Background(), raw)
	mutationMust(t, err)
	if !reached || result.IsError || !slices.Equal(journal.events, []string{"prepare", "commit"}) {
		t.Fatalf("landed write was treated as failure: reached=%v result=%+v journal=%v", reached, result, journal.events)
	}
	mutationBytes(t, filepath.Join(moved, "file"), "APPROVED\n")
	mutationNoTemps(t, moved)
}
