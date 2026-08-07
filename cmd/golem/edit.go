package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	dataDir string // parent for the temp file, created 0700
}

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
	f, err := os.CreateTemp(e.dataDir, "edit-*.txt")
	if err != nil {
		return "", fmt.Errorf("golem: edit file: %w", err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()

	if _, err := f.WriteString(seed); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("golem: seed edit file: %w", err)
	}
	// Closed before the editor spawns: Windows cannot open a file this
	// process still holds, and every editor rewrite must land on disk, not in
	// a stale descriptor.
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("golem: seed edit file: %w", err)
	}

	name, args := resolveEditor(e.getenv)
	if err := e.run(ctx, name, append(args, path), e.stdin, e.stdout, e.stderr); err != nil {
		return "", err
	}

	edited, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("golem: read edited goal: %w", err)
	}
	defer func() { _ = edited.Close() }()
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
