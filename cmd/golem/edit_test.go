package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// editRunCall records one invocation of the injected runner.
type editRunCall struct {
	ctx        context.Context
	name       string
	args       []string
	in, out    *os.File
	errw       *os.File
	seed       string
	fileMode   fs.FileMode
	parentMode fs.FileMode
}

// newEditFixture builds a ttyGoalEditor over fake ops and an injected runner.
// write, when non-nil, replaces the temp file's content as the "editor" would;
// runErr is returned from the runner after any write.
func newEditFixture(t *testing.T, getenv func(string) string, write *string, runErr error) (*ttyGoalEditor, *[]editRunCall) {
	t.Helper()
	stdin, stdout := tempDescriptors(t)
	dataDir := filepath.Join(t.TempDir(), "golem")
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	calls := &[]editRunCall{}
	e := &ttyGoalEditor{
		stdin: stdin, stdout: stdout, stderr: stdout,
		getenv: getenv,
		ops:    &fakeTermOps{ttys: map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true}},
		run: func(ctx context.Context, name string, args []string, in, out, errw *os.File) error {
			t.Helper()
			path := args[len(args)-1]
			seed, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("runner cannot read the seeded file: %v", err)
			}
			var fileMode, parentMode fs.FileMode
			if st, err := os.Stat(path); err == nil {
				fileMode = st.Mode().Perm()
			}
			if st, err := os.Stat(filepath.Dir(path)); err == nil {
				parentMode = st.Mode().Perm()
			}
			*calls = append(*calls, editRunCall{
				ctx: ctx, name: name, args: args, in: in, out: out, errw: errw,
				seed: string(seed), fileMode: fileMode, parentMode: parentMode,
			})
			if write != nil {
				if werr := os.WriteFile(path, []byte(*write), 0o600); werr != nil {
					t.Fatalf("runner rewrite: %v", werr)
				}
			}
			return runErr
		},
		dataDir: dataDir,
	}
	return e, calls
}

func TestTTYGoalEditorAvailability(t *testing.T) {
	stdin, stdout := tempDescriptors(t)
	for _, tc := range []struct {
		name string
		ttys map[int]bool
		want bool
	}{
		{"both terminals", map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true}, true},
		{"stdin only", map[int]bool{int(stdin.Fd()): true}, false},
		{"stdout only", map[int]bool{int(stdout.Fd()): true}, false},
		{"neither", map[int]bool{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ttyGoalEditor{stdin: stdin, stdout: stdout, ops: &fakeTermOps{ttys: tc.ttys}}
			if got := e.Available(); got != tc.want {
				t.Fatalf("Available = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolveEditor(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		name     string
		env      map[string]string
		wantName string
		wantArgs []string
	}{
		{"VISUAL wins", map[string]string{"VISUAL": "vim -u NONE", "EDITOR": "nano"}, "vim", []string{"-u", "NONE"}},
		{"EDITOR with args", map[string]string{"EDITOR": "code -w"}, "code", []string{"-w"}},
		{"whitespace VISUAL skipped", map[string]string{"VISUAL": "   ", "EDITOR": "nano"}, "nano", nil},
		{"whitespace both skipped", map[string]string{"VISUAL": " \t ", "EDITOR": "  "}, defaultEditorCommand, nil},
		{"empty environment", nil, defaultEditorCommand, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, args := resolveEditor(env(tc.env))
			if name != tc.wantName {
				t.Fatalf("name = %q, want %q", name, tc.wantName)
			}
			if len(args) != len(tc.wantArgs) {
				t.Fatalf("args = %q, want %q", args, tc.wantArgs)
			}
			for i := range args {
				if args[i] != tc.wantArgs[i] {
					t.Fatalf("args = %q, want %q", args, tc.wantArgs)
				}
			}
		})
	}
}

func TestTTYGoalEditorComposeSeedsRunsAndReturns(t *testing.T) {
	edited := "the edited goal\nwith a second line\n"
	getenv := func(k string) string {
		if k == "VISUAL" {
			return "fakeed --wait"
		}
		return ""
	}
	e, calls := newEditFixture(t, getenv, &edited, nil)
	got, err := e.Compose(context.Background(), "seed text")
	if err != nil || got != edited {
		t.Fatalf("Compose = %q err=%v, want the edited content", got, err)
	}
	if len(*calls) != 1 {
		t.Fatalf("runner invoked %d times, want 1", len(*calls))
	}
	c := (*calls)[0]
	if c.name != "fakeed" || len(c.args) != 2 || c.args[0] != "--wait" {
		t.Fatalf("runner argv = %q %q, want fakeed [--wait <path>]", c.name, c.args)
	}
	if c.seed != "seed text" {
		t.Fatalf("seeded content = %q, want %q", c.seed, "seed text")
	}
	if c.in != e.stdin || c.out != e.stdout || c.errw != e.stderr {
		t.Fatal("the runner must inherit the editor's real stdin/stdout/stderr")
	}
	if c.fileMode != 0o600 {
		t.Fatalf("temp file mode = %o, want 0600", c.fileMode)
	}
	if c.parentMode != 0o700 {
		t.Fatalf("temp parent mode = %o, want 0700", c.parentMode)
	}
	if _, err := os.Stat(c.args[len(c.args)-1]); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed after Compose (stat err = %v)", err)
	}
}

func TestTTYGoalEditorComposeRunnerErrorRemovesTheFile(t *testing.T) {
	boom := errors.New("exit status 3")
	e, calls := newEditFixture(t, nil, nil, boom)
	got, err := e.Compose(context.Background(), "")
	if !errors.Is(err, boom) || got != "" {
		t.Fatalf("Compose = %q err=%v, want the runner error and no goal", got, err)
	}
	path := (*calls)[0].args[len((*calls)[0].args)-1]
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed after a runner error (stat err = %v)", err)
	}
}

func TestTTYGoalEditorComposeEmptyContent(t *testing.T) {
	empty := ""
	e, _ := newEditFixture(t, nil, &empty, nil)
	got, err := e.Compose(context.Background(), "seed")
	if err != nil || got != "" {
		t.Fatalf("Compose = %q err=%v, want no goal for empty content", got, err)
	}
}

func TestTTYGoalEditorComposeCeiling(t *testing.T) {
	t.Run("exactly the ceiling", func(t *testing.T) {
		content := strings.Repeat("a", maxGoalBytes)
		e, _ := newEditFixture(t, nil, &content, nil)
		got, err := e.Compose(context.Background(), "")
		if err != nil || len(got) != maxGoalBytes {
			t.Fatalf("Compose len=%d err=%v, want exactly maxGoalBytes accepted", len(got), err)
		}
	})
	t.Run("one byte over", func(t *testing.T) {
		content := strings.Repeat("a", maxGoalBytes+1)
		e, _ := newEditFixture(t, nil, &content, nil)
		got, err := e.Compose(context.Background(), "")
		if !errors.Is(err, errEditTooLarge) || got != "" {
			t.Fatalf("Compose len=%d err=%v, want errEditTooLarge and no goal", len(got), err)
		}
	})
}

func TestTTYGoalEditorComposePassesContext(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	e, calls := newEditFixture(t, nil, nil, nil)
	if _, err := e.Compose(ctx, ""); err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if got := (*calls)[0].ctx.Value(ctxKey{}); got != "marker" {
		t.Fatal("the caller's context must reach the runner")
	}
}
