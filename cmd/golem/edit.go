package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// goalEditor composes a goal in an external editor. It is a capability seam
// independent of editorSource: -no-editor on a real TTY selects the scanner
// for line input but must leave /edit available, because the two answer
// different questions (inline editing vs external composition).
type goalEditor interface {
	// Available reports whether an interactive editor can run at all. A piped
	// script must not be able to spawn one.
	Available() bool

	// Compose seeds a temp file, runs the editor over the real terminal, and
	// returns the edited content. errEditTooLarge reports content past the
	// goal ceiling; any other error is a launch or filesystem failure.
	Compose(ctx context.Context, seed string) (string, error)
}

// errEditTooLarge rejects an edited goal larger than maxGoalBytes, detected
// through a bounded read that never allocates the whole file.
var errEditTooLarge = errors.New("golem: edited goal exceeds 1 MiB")

// ttyGoalEditor is the production goalEditor: a temp file under the golem
// data dir, an editor resolved from the environment, and a process inheriting
// the real terminal in cooked mode.
type ttyGoalEditor struct {
	stdin, stdout, stderr *os.File
	getenv                func(string) string
	ops                   termOps
	// run is injected so tests never spawn a process.
	run func(ctx context.Context, name string, args []string,
		in, out, errw *os.File) error
	dataDir string // parent for the per-edit directory, created 0700
	root    string // workspace root the draft must stay outside of
}

// errEditNotRegular rejects a draft that is no longer a regular file when it is
// read back. The editor ran in between, so the name is not trusted afterwards.
var errEditNotRegular = errors.New("golem: edited goal is not a regular file")

// Available requires both descriptors to be terminals: the editor draws on
// stdout and reads keys from stdin, and a redirected half would hang or draw
// into a file.
func (e *ttyGoalEditor) Available() bool {
	return e.ops.IsTerminal(int(e.stdin.Fd())) && e.ops.IsTerminal(int(e.stdout.Fd()))
}

// Compose runs one edit cycle: seed a 0600 temp file under the 0700 data dir,
// close it before spawning so Windows can open it, run the editor over the
// real terminal, then read the result through a bounded reader. The temp file
// is removed by one defer covering every failure path.
func (e *ttyGoalEditor) Compose(ctx context.Context, seed string) (string, error) {
	if err := os.MkdirAll(e.dataDir, 0o700); err != nil {
		return "", fmt.Errorf("golem: edit dir: %w", err)
	}
	// One directory per invocation, removed whole. Editors write more than the
	// file they were given -- emacs leaves goal.txt~, vim a .swp -- and removing
	// only the primary file left those behind holding goal text indefinitely.
	// It also gives the reopen below something to confine to.
	dir, err := os.MkdirTemp(e.dataDir, "edit-")
	if err != nil {
		return "", fmt.Errorf("golem: edit dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Every other data-dir writer applies this; /edit skipped it. With
	// XDG_DATA_HOME pointed inside the repo the draft would land in the
	// workspace, where it is a stray artifact carrying whatever the user typed.
	if err := validatePathOutsideWorkspace(dir, e.root); err != nil {
		return "", fmt.Errorf("golem: edit dir: %w", err)
	}

	const draftName = "goal.txt"
	path := filepath.Join(dir, draftName)
	// Written and closed before the editor spawns: Windows cannot open a file
	// this process still holds, and every editor rewrite must land on disk
	// rather than in a stale descriptor.
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		return "", fmt.Errorf("golem: seed edit file: %w", err)
	}

	name, args := resolveEditor(e.getenv)
	if err := e.run(ctx, name, append(args, path), e.stdin, e.stdout, e.stderr); err != nil {
		return "", err
	}

	// Reopened through a confined root, not by bare name. An arbitrary process
	// has run since the write, so a symlink planted at the path would otherwise
	// be followed wherever it points. Deliberately NOT an os.SameFile check:
	// atomic-save editors legitimately replace the inode, and rejecting that
	// would break saving in vim, emacs and every editor that writes-and-renames.
	confined, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("golem: read edited goal: %w", err)
	}
	defer func() { _ = confined.Close() }()
	edited, err := confined.Open(draftName)
	if err != nil {
		return "", fmt.Errorf("golem: read edited goal: %w", err)
	}
	defer func() { _ = edited.Close() }()
	if fi, serr := edited.Stat(); serr != nil {
		return "", fmt.Errorf("golem: read edited goal: %w", serr)
	} else if !fi.Mode().IsRegular() {
		return "", errEditNotRegular
	}
	// Bounded: one byte past the ceiling proves oversize without ever
	// allocating the whole file.
	content, err := io.ReadAll(io.LimitReader(edited, maxGoalBytes+1))
	if err != nil {
		return "", fmt.Errorf("golem: read edited goal: %w", err)
	}
	if len(content) > maxGoalBytes {
		return "", errEditTooLarge
	}
	return string(content), nil
}

// resolveEditor picks the editor command: non-empty $VISUAL, else non-empty
// $EDITOR, else the build's default. Values are split on whitespace into argv
// with no shell interpretation; quoting and shell syntax are unsupported.
//
// The command MUST block until the edit is finished. An editor that forks and
// returns immediately -- `code` or `subl` without a wait flag -- hands back the
// unmodified seed, which then runs as a goal before the window has painted.
// Configure `code --wait` or `subl -w`. golem cannot detect this for you:
// "returned quickly with the content unchanged" is also what a legitimate
// no-op edit looks like, so rejecting it would reject deliberate ones.
func resolveEditor(getenv func(string) string) (string, []string) {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if fields := strings.Fields(getenv(key)); len(fields) > 0 {
			return fields[0], fields[1:]
		}
	}
	return defaultEditorCommand, nil
}

// runEditorProcess is the production runner: the editor inherits the real
// descriptors so it owns the terminal for its lifetime.
func runEditorProcess(ctx context.Context, name string, args []string,
	in, out, errw *os.File) error {

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errw
	return cmd.Run()
}
