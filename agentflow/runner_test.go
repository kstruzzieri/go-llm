package agentflow

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerArgv_BinaryMode(t *testing.T) {
	r := NewExecRunner("/ws")
	bin, argv, env := r.commandFor([]string{"status", "--root", "/ws", "--json"})
	if bin != "agentflow" {
		t.Fatalf("bin = %q, want agentflow", bin)
	}
	if want := []string{"status", "--root", "/ws", "--json"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if len(env) != 0 {
		t.Fatalf("env = %v, want none", env)
	}
}

func TestExecRunnerDisablePythonBytecodeWritesIsChildOnly(t *testing.T) {
	t.Setenv("PYTHONDONTWRITEBYTECODE", "")
	r := &ExecRunner{bin: "sh", dir: t.TempDir()}
	r.DisablePythonBytecodeWrites()
	out, errOut, exit, err := r.Run(context.Background(), []string{"-c", `printf %s "$PYTHONDONTWRITEBYTECODE"`}, nil)
	if err != nil || exit != 0 || string(out) != "1" || len(errOut) != 0 {
		t.Fatalf("child env: stdout=%q stderr=%q exit=%d err=%v", out, errOut, exit, err)
	}
	if got := os.Getenv("PYTHONDONTWRITEBYTECODE"); got != "" {
		t.Fatalf("parent PYTHONDONTWRITEBYTECODE = %q, want inherited empty value", got)
	}
}

func TestExecRunnerArgv_SrcMode(t *testing.T) {
	checkout := writeSourceCheckoutFixture(t)
	r := NewSrcExecRunner("/ws", checkout)
	bin, argv, env := r.commandFor([]string{"status"})
	if bin != "python3" {
		t.Fatalf("bin = %q, want python3", bin)
	}
	if want := []string{"-P", "-m", "agentflow", "status"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if want := []string{"PYTHONPATH=" + filepath.Join(checkout, "src")}; !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want %v", env, want)
	}
}

func TestNewSrcExecRunnerUsesCanonicalCheckout(t *testing.T) {
	checkout := writeSourceCheckoutFixture(t)
	link := filepath.Join(t.TempDir(), "checkout-link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Fatal(err)
	}
	_, _, env := NewSrcExecRunner("/ws", link).commandFor(nil)
	if want := []string{"PYTHONPATH=" + filepath.Join(checkout, "src")}; !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %v, want canonical %v", env, want)
	}
}

func TestNewSrcExecRunnerRejectsInvalidCheckout(t *testing.T) {
	sep := string(os.PathListSeparator)
	sepTarget := filepath.Join(t.TempDir(), "with"+sep+"separator")
	if err := os.MkdirAll(filepath.Join(sepTarget, "src", "agentflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sepTarget, "src", "agentflow", "__init__.py"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sepTarget, link); err != nil {
		t.Fatal(err)
	}
	initDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(initDir, "src", "agentflow", "__init__.py"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, checkout, wantErr string }{
		{"empty", "", "agentflow source checkout is empty"},
		{"relative", filepath.Join("tools", "agentflow"), "agentflow source checkout must be an absolute path"},
		{"separator in path", filepath.Join(t.TempDir(), "a"+sep+"b"), "agentflow source checkout contains a path-list separator"},
		{"missing", filepath.Join(t.TempDir(), "missing"), "agentflow source checkout: "},
		{"separator after symlink resolution", link, "agentflow source checkout contains a path-list separator"},
		{"no package", t.TempDir(), "agentflow source checkout has no src/agentflow package"},
		{"package init is a directory", initDir, "agentflow source checkout has no src/agentflow package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewSrcExecRunner(t.TempDir(), tc.checkout)
			r.bin = "must-not-launch"
			out, errOut, exit, err := r.Run(t.Context(), []string{"status"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || out != nil || errOut != nil || exit != 0 {
				t.Fatalf("stdout=%q stderr=%q exit=%d err=%v, want %q before launch", out, errOut, exit, err, tc.wantErr)
			}
		})
	}
}

func TestExecRunnerSrcModePreservesContextWithSafePath(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable; launch arguments are covered by TestExecRunnerArgv_SrcMode")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(t.TempDir(), "trusted checkout")
	pkg := filepath.Join(checkout, "src", "agentflow")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "__init__.py"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Inspect the child's search path using only a benign, explicitly selected package.
	probe := `import os
import sys
assert sys.flags.safe_path
assert sys.argv[1:] == ["--root", os.getcwd(), "--literal", "value with spaces"]
assert all(p and os.path.realpath(p) != os.getcwd() for p in sys.path)
assert sys.stdin.buffer.read() == b"input"
assert sys.dont_write_bytecode
sys.stdout.write("ok")
`
	if err := os.WriteFile(filepath.Join(pkg, "__main__.py"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PYTHONSAFEPATH", "")
	t.Setenv("PYTHONOPTIMIZE", "")
	t.Setenv("PYTHONDONTWRITEBYTECODE", "")
	t.Setenv("PYTHONPATH", root)
	r := NewSrcExecRunner(root, checkout)
	r.DisablePythonBytecodeWrites()
	out, errOut, exit, err := r.Run(t.Context(), []string{"--root", root, "--literal", "value with spaces"}, []byte("input"))
	if err != nil || exit != 0 || string(out) != "ok" || len(errOut) != 0 {
		t.Fatalf("stdout=%q stderr=%q exit=%d err=%v, want safe-path probe success", out, errOut, exit, err)
	}
}

// TestExecRunner_ExitCodeMapping drives a real ExecRunner (via sh) to prove a
// nonzero exit is reported as exit!=0 with a nil Go error, and that the
// stdout/stderr/exit split is correct.
func TestExecRunner_ExitCodeMapping(t *testing.T) {
	r := &ExecRunner{bin: "sh", dir: t.TempDir()}

	_, _, exit, err := r.Run(context.Background(), []string{"-c", "exit 3"}, nil)
	if err != nil || exit != 3 {
		t.Fatalf("exit=%d err=%v, want exit=3 err=nil", exit, err)
	}

	out, errOut, exit, err := r.Run(context.Background(), []string{"-c", "printf out; printf err 1>&2; exit 0"}, nil)
	if err != nil || exit != 0 || string(out) != "out" || string(errOut) != "err" {
		t.Fatalf("out=%q err=%q exit=%d goErr=%v", out, errOut, exit, err)
	}
}

// TestExecRunner_CancelKillsProcess proves that cancelling the context returns
// from Run promptly instead of blocking for the full child runtime. The child
// runs in its own process group (see runner.go); Cancel SIGKILLs the whole
// group so gate grandchildren are reaped too, which is hard to assert without
// flakiness, so this only checks the prompt-return guarantee.
func TestExecRunner_CancelKillsProcess(t *testing.T) {
	r := &ExecRunner{bin: "sh", dir: t.TempDir()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, _, _, _ = r.Run(ctx, []string{"-c", "sleep 30"}, nil)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run blocked for %v after cancel; process group not killed", elapsed)
	}
}

// fakeRunner is the shared test double for client/driver tests.
func TestFakeRunner_RecordsAndReplies(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{"status": {stdout: []byte("ok"), exit: 0}}}
	out, _, exit, err := f.Run(context.Background(), []string{"status", "--json"}, nil)
	if err != nil || exit != 0 || string(out) != "ok" {
		t.Fatalf("got out=%q exit=%d err=%v", out, exit, err)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "status" {
		t.Fatalf("calls = %v", f.calls)
	}
}

// fakeReply and fakeRunner are the shared test doubles used across the package.
type fakeReply struct {
	stdout []byte
	stderr []byte
	exit   int
	err    error
}

// fakeRunner matches a reply by the subcommand (args[0]) and records every call's
// full argv. A missing reply yields exit 0 with empty output.
type fakeRunner struct {
	replies map[string]fakeReply
	calls   [][]string
	inputs  [][]byte
}

func (f *fakeRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	f.inputs = append(f.inputs, append([]byte(nil), stdin...))
	if len(args) == 0 {
		return nil, nil, 0, nil
	}
	rep := f.replies[args[0]]
	return rep.stdout, rep.stderr, rep.exit, rep.err
}

// writeSourceCheckoutFixture returns a canonical checkout path holding the
// minimal src/agentflow package that source-mode validation requires.
func writeSourceCheckoutFixture(t *testing.T) string {
	t.Helper()
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(checkout, "src", "agentflow")
	if err := os.MkdirAll(pkg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkg, "__init__.py"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return checkout
}
