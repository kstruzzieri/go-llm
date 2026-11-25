//go:build linux

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

// postRunGitWorkspace starts with a real outer repository, no Git templates
// or user configuration, and one ordinary file whose contents must survive.
func postRunGitWorkspace(t *testing.T) string {
	t.Helper()
	requireBwrapCapability(t)
	requireBwrapTool(t, "/usr/bin/git")
	root := canonGitTempDir(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	runIn(t, root, "git init -q --template= && echo hi > a.txt")
	return root
}

// postRunGitCommand uses an approved run_command and returns the same tool
// message the scripted model received. Separate Golem notes from stdout:
// a command's own text cannot satisfy a hold assertion.
func postRunGitCommand(t *testing.T, root string, argv ...string) (notes []string, output string) {
	t.Helper()
	tools, err := NewSandboxedExecTools(root, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(map[string][]string{"argv": argv})
	if err != nil {
		t.Fatal(err)
	}
	var operatorNotes []string
	ctx := WithSandboxNotice(context.Background(), func(note string) { operatorNotes = append(operatorNotes, note) })
	caller := &proposalRunCaller{responses: callScript("run_command", string(args))}
	approval := &freshYes{}
	run, err := agent.New(caller, agent.ContextManager{}).Run(ctx, agent.Request{
		Goal: "run it", Tools: tools, Approver: approval, StrictApproval: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.ToolCalls) != 1 || !run.ToolCalls[0].Invoked || run.ToolCalls[0].IsError || len(approval.previews) != 1 {
		t.Fatalf("command %q: calls %+v, approvals %d", argv, run.ToolCalls, len(approval.previews))
	}
	var result string
	for _, msg := range run.Messages {
		if msg.Role == "tool" {
			result = msg.Content
		}
	}
	header, output, ok := strings.Cut(result, "--- stdout ---\n")
	if !ok || !strings.Contains(header, "exit code: 0\n") {
		t.Fatalf("command %q did not complete successfully: %s", argv, result)
	}
	for line := range strings.SplitSeq(header, "\n") {
		if note, found := strings.CutPrefix(line, "note: "); found {
			notes = append(notes, note)
		}
	}
	if !slices.Equal(operatorNotes, notes) {
		t.Fatalf("operator notes %q differ from the notes in the model result %q", operatorNotes, notes)
	}
	return notes, output
}

// requireGitHolds checks both the rendered recovery instructions and the
// host's filesystem. For a nested hold, the outermost entry moves last.
func requireGitHolds(t *testing.T, root string, notes, paths []string) string {
	t.Helper()
	if len(notes) != len(paths) {
		t.Fatalf("notes = %q, want holds for %q", notes, paths)
	}
	id := heldRunID(t, notes[0])
	for i, rel := range paths {
		if want := heldNote(rel, rel+".golem-held-"+id); notes[i] != want {
			t.Fatalf("note %d = %q, want %q", i, notes[i], want)
		}
	}
	last := filepath.Join(root, paths[len(paths)-1])
	if _, err := os.Lstat(last); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("original %s: %v, want absent", last, err)
	}
	if _, err := os.Lstat(last + ".golem-held-" + id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBwrapPostRunGitAcceptance(t *testing.T) {
	// containment: GITMETA-POSTRUN-WIRING
	root := postRunGitWorkspace(t)
	outer := entryIDOf(t, filepath.Join(root, ".git"))
	outerHEAD := entryIDOf(t, filepath.Join(root, ".git/HEAD"))

	notes, _ := postRunGitCommand(t, root, "/usr/bin/git", "init", "--bare", "-q", "sub")
	id := requireGitHolds(t, root, notes, []string{"sub/HEAD"})
	if got := gitDirAt(t, root, "sub"); got != ".git" {
		t.Fatalf("host git inside held bare repo finds %q, want outer .git", got)
	}

	notes, _ = postRunGitCommand(t, root, "/usr/bin/git", "init", "-q", "inner")
	nestedID := requireGitHolds(t, root, notes, []string{"inner/.git/HEAD", "inner/.git"})
	if _, err := os.Stat(filepath.Join(root, "inner/.git.golem-held-"+nestedID, "HEAD.golem-held-"+nestedID)); err != nil {
		t.Fatal(err)
	}
	notes, listing := postRunGitCommand(t, root, "/usr/bin/ls")
	if len(notes) != 0 || !slices.Contains(strings.Fields(listing), "a.txt") {
		t.Fatalf("ls: notes %q, output %q", notes, listing)
	}

	head := filepath.Join(root, "sub/HEAD")
	if err := os.Rename(head+".golem-held-"+id, head); err != nil {
		t.Fatal(err)
	}
	runIn(t, root, "cd sub && test \"$(git rev-parse --git-dir)\" = .")

	notes, status := postRunGitCommand(t, root, "/usr/bin/git", "status", "--short")
	if len(notes) != 0 || !strings.Contains(status, "?? a.txt") {
		t.Fatalf("git status: notes %q, output %q", notes, status)
	}
	if entryIDOf(t, filepath.Join(root, ".git")) != outer || entryIDOf(t, filepath.Join(root, ".git/HEAD")) != outerHEAD {
		t.Fatal("outer repository identity changed")
	}
	if b, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(b) != "hi\n" {
		t.Fatalf("ordinary file contents %q, err %v", b, err)
	}
}

// TestBwrapPostRunGitMetadataNames covers the #802 creation rows with empty
// files and empty directories. Probe the fixture filesystem independently:
// Git names that the filesystem distinguishes must remain ordinary names.
func TestBwrapPostRunGitMetadataNames(t *testing.T) {
	// containment: GITMETA-POSTRUN-WIRING
	for _, tc := range []struct {
		name, spelling, gitName string
		directory               bool
	}{
		{"commondir-create", "commondir", "commondir", false},
		{"commondir-case", "COMMONDIR", "commondir", false},
		{"configworktree-create", "config.worktree", "config.worktree", false},
		{"configworktree-case", "CONFIG.WORKTREE", "config.worktree", false},
		{"configworktree-ligature", "conﬁg.worktree", "config.worktree", false},
		{"hooks-absent-create", "hooks", "hooks", true},
		{"hooks-absent-case", "HOOKS", "hooks", true},
		{"hooks-absent-kelvin", "hooKs", "hooks", true},
		{"info-absent-create", "info", "info", true},
		{"info-absent-case", "INFO", "info", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := postRunGitWorkspace(t)
			// The probe and .git are siblings inheriting the same mount and
			// directory casefold mode from root.
			if err := os.Mkdir(filepath.Join(root, "probe"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "probe", tc.spelling), nil, 0600); err != nil {
				t.Fatal(err)
			}
			_, probeErr := os.Stat(filepath.Join(root, "probe", tc.gitName))
			if probeErr != nil && !errors.Is(probeErr, fs.ErrNotExist) {
				t.Fatal(probeErr)
			}
			command := "/usr/bin/touch"
			if tc.directory {
				command = "/usr/bin/mkdir"
			}
			rel := ".git/" + tc.spelling
			notes, _ := postRunGitCommand(t, root, command, rel)
			if probeErr == nil {
				requireGitHolds(t, root, notes, []string{".git/" + tc.gitName})
			} else {
				if len(notes) != 0 {
					t.Fatalf("distinct name %q generated notes: %q", rel, notes)
				}
				if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
					t.Fatal(err)
				}
			}
			if got := gitDirAt(t, root, "."); got != ".git" {
				t.Fatalf("host git finds %q, want baseline .git", got)
			}
		})
	}
}

func TestBwrapPostRunMovedCheckout(t *testing.T) {
	// containment: GITMETA-POSTRUN-WIRING
	root := postRunGitWorkspace(t)
	runIn(t, root, "git init -q --template= nested")
	before := entryIDOf(t, filepath.Join(root, "nested/.git"))
	notes, _ := postRunGitCommand(t, root, "/bin/sh", "-c", "mv nested saved && git init -q --template= nested")
	requireGitHolds(t, root, notes, []string{"nested/.git/HEAD", "nested/.git"})
	if entryIDOf(t, filepath.Join(root, "saved/.git")) != before || gitDirAt(t, root, "saved") != "saved/.git" {
		t.Fatal("relocated baseline repository was not preserved")
	}
	if got := gitDirAt(t, root, "nested"); got != ".git" {
		t.Fatalf("host git in replacement checkout finds %q, want outer .git", got)
	}
}
