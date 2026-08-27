package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// verifyWorkspace returns a canonical workspace over a fresh temp dir.
func verifyWorkspace(t *testing.T) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// helperArgv runs this test binary's hermetic exec helper as the verify
// command, so no external tool is needed.
func helperArgv(t *testing.T, args ...string) []string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{self, "__golem_exec_helper__"}, args...)
}

// stubRunner returns a scripted execResult so the mapping from execResult to
// VerifyResult can be pinned without a real process.
type stubRunner struct {
	res execResult
	err error
	got execSpec
	n   int
}

func (s *stubRunner) Run(_ context.Context, spec execSpec) (execResult, error) {
	s.n++
	s.got = spec
	return s.res, s.err
}

func newTestVerifyCommand(t *testing.T, ws *Workspace, r commandRunner, argv []string, dir string, d time.Duration) *VerifyCommand {
	t.Helper()
	v, err := newVerifyCommand(ws, r, argv, dir, d)
	if err != nil {
		t.Fatalf("newVerifyCommand: %v", err)
	}
	return v
}

func TestNewVerifyCommandRejectsBadInput(t *testing.T) {
	ws := verifyWorkspace(t)
	ok := helperArgv(t, "echo", "hi")
	cases := []struct {
		name    string
		argv    []string
		dir     string
		timeout time.Duration
		want    string
	}{
		{name: "empty argv", argv: nil, timeout: time.Second, want: "argv"},
		{name: "blank argv0", argv: []string{"  "}, timeout: time.Second, want: "argv[0]"},
		{name: "NUL in argv", argv: []string{ok[0], "a\x00b"}, timeout: time.Second, want: "NUL"},
		{name: "missing executable", argv: []string{"definitely-not-on-path-347"}, timeout: time.Second, want: "resolve"},
		{name: "dir escapes workspace", argv: ok, dir: "../..", timeout: time.Second, want: "dir"},
		{name: "zero timeout", argv: ok, timeout: 0, want: "timeout"},
		{name: "negative timeout", argv: ok, timeout: -time.Second, want: "timeout"},
		{name: "timeout above the ceiling", argv: ok, timeout: execMaxTimeout + time.Second, want: "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newVerifyCommand(ws, &stubRunner{}, tc.argv, tc.dir, tc.timeout)
			if err == nil {
				t.Fatalf("expected a construction error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q must mention %q", err, tc.want)
			}
		})
	}
}

func TestVerifyCommandApprovalKeyIdentity(t *testing.T) {
	ws := verifyWorkspace(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := helperArgv(t, "echo", "hi")
	base := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 30*time.Second)

	if !strings.HasPrefix(base.ApprovalKey(), verifyApprovalKeyPrefix) {
		t.Fatalf("key %q must carry the verify namespace %q", base.ApprovalKey(), verifyApprovalKeyPrefix)
	}
	if strings.HasPrefix(base.ApprovalKey(), execApprovalKeyPrefix) {
		t.Fatalf("verify key must not share the exec namespace: %q", base.ApprovalKey())
	}
	same := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 30*time.Second)
	if base.ApprovalKey() != same.ApprovalKey() {
		t.Fatalf("identical inputs must yield one key: %q vs %q", base.ApprovalKey(), same.ApprovalKey())
	}

	otherArgs := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "bye"), "", 30*time.Second)
	otherDir := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "sub", 30*time.Second)
	otherTimeout := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", 31*time.Second)
	for name, v := range map[string]*VerifyCommand{
		"argv":    otherArgs,
		"dir":     otherDir,
		"timeout": otherTimeout,
	} {
		if v.ApprovalKey() == base.ApprovalKey() {
			t.Fatalf("a changed %s must change the approval key", name)
		}
	}
}

func TestVerifyCommandPreview(t *testing.T) {
	ws := verifyWorkspace(t)
	if err := os.Mkdir(filepath.Join(ws.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "hi"), "sub", 45*time.Second)
	preview := v.Preview()
	for _, want := range []string{"__golem_exec_helper__", "sub", "45s", "may change the code this command runs"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestVerifyCommandMapsRunnerResult(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	r := &stubRunner{res: execResult{
		ExitCode: 7,
		Stdout:   []byte("out"), Stderr: []byte("err"),
		StdoutTruncated: true,
	}}
	v := newTestVerifyCommand(t, ws, r, argv, "", time.Minute)

	got, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.ExitCode != 7 || string(got.Stdout) != "out" || string(got.Stderr) != "err" {
		t.Fatalf("result not mapped through: %+v", got)
	}
	if !got.Truncated {
		t.Fatalf("truncation must pass through: %+v", got)
	}
	if got.TimedOut {
		t.Fatalf("TimedOut must be false for a completed run: %+v", got)
	}
	if r.got.Dir != ws.root {
		t.Fatalf("spec Dir = %q, want workspace root %q", r.got.Dir, ws.root)
	}
}

func TestVerifyCommandTruncationOnEitherStream(t *testing.T) {
	ws := verifyWorkspace(t)
	argv := helperArgv(t, "echo", "hi")
	for name, res := range map[string]execResult{
		"stdout only": {StdoutTruncated: true},
		"stderr only": {StderrTruncated: true},
		"both":        {StdoutTruncated: true, StderrTruncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			v := newTestVerifyCommand(t, ws, &stubRunner{res: res}, argv, "", time.Minute)
			got, err := v.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !got.Truncated {
				t.Fatalf("%s must report Truncated: %+v", name, got)
			}
		})
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, argv, "", time.Minute)
	got, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Truncated {
		t.Fatalf("an uncapped run must not report Truncated: %+v", got)
	}
}

func TestVerifyCommandInfraFailureIsAnError(t *testing.T) {
	ws := verifyWorkspace(t)
	boom := errors.New("spawn refused")
	v := newTestVerifyCommand(t, ws, &stubRunner{err: boom}, helperArgv(t, "echo", "hi"), "", time.Minute)
	if _, err := v.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("infra failure must surface, got %v", err)
	}
}

func TestVerifyCommandRunsRealCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"success", []string{"echo", "ok"}, 0},
		{"non-zero exit", []string{"fail"}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := NewVerifyCommand(ws, helperArgv(t, tc.args...), "", 30*time.Second)
			if err != nil {
				t.Fatalf("NewVerifyCommand: %v", err)
			}
			got, err := v.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.ExitCode != tc.want {
				t.Fatalf("ExitCode = %d, want %d", got.ExitCode, tc.want)
			}
			if got.TimedOut {
				t.Fatalf("TimedOut must be false: %+v", got)
			}
		})
	}
}

// TestVerifyCommandAppliesItsOwnTimeout is the pin that matters most for a
// non-tool runner: nothing wraps Run the way the Orchestrator's invokeCall
// wraps a dispatched tool, so the deadline must come from VerifyCommand.
func TestVerifyCommandAppliesItsOwnTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	v, err := NewVerifyCommand(ws, helperArgv(t, "sleep"), "", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	start := time.Now()
	got, runErr := v.Run(context.Background())
	if runErr != nil {
		t.Fatalf("a timeout is an outcome, not an error: %v", runErr)
	}
	if !got.TimedOut {
		t.Fatalf("want TimedOut, got %+v", got)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the helper sleeps 30s; the deadline was not applied (took %s)", elapsed)
	}
}

func TestVerifyCommandParentCancellationPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix helper semantics")
	}
	ws := verifyWorkspace(t)
	v, err := NewVerifyCommand(ws, helperArgv(t, "sleep"), "", 30*time.Second)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	got, runErr := v.Run(ctx)
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("parent cancellation must surface as an error, got %v (%+v)", runErr, got)
	}
	if got.TimedOut {
		t.Fatalf("a cancelled run is not a timeout: %+v", got)
	}
}

func TestVerifyCommandRechecksExecutableIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix exec-bit semantics")
	}
	ws := verifyWorkspace(t)
	script := filepath.Join(ws.root, "check.sh")
	writeExecutable(t, script, "#!/bin/sh\nexit 0\n")

	v := newTestVerifyCommand(t, ws, &stubRunner{}, []string{"./check.sh"}, "", 30*time.Second)

	// Swap the binary by write-sibling + rename, NOT remove + recreate: inode
	// reuse would mask the identity change on APFS.
	sibling := filepath.Join(ws.root, "check.sh.new")
	writeExecutable(t, sibling, "#!/bin/sh\nexit 1\n")
	if err := os.Rename(sibling, script); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "executable changed") {
		t.Fatalf("a swapped executable must fail closed, got %v", err)
	}
}

func TestVerifyCommandRechecksWorkingDirectoryIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix path semantics")
	}
	ws := verifyWorkspace(t)
	dir := filepath.Join(ws.root, "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	v := newTestVerifyCommand(t, ws, &stubRunner{}, helperArgv(t, "echo", "hi"), "work", 30*time.Second)

	// Create the replacement FIRST, so its inode is allocated before the
	// original is unlinked and cannot be the reused one. (Directory rename
	// onto an existing directory is not portable, so the unlink is required;
	// creating first keeps the identity change guaranteed, which a plain
	// remove-then-recreate would not.)
	replacement := filepath.Join(ws.root, "work.new")
	if err := os.Mkdir(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Run(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "working directory changed") {
		t.Fatalf("a swapped cwd must fail closed, got %v", err)
	}
}

func TestVerifyCommandIsNotAModelTool(t *testing.T) {
	// The model must be unable to call the workspace's verifier: VerifyCommand
	// must not satisfy agent.Tool, and no tool set may carry it.
	if _, ok := any(&VerifyCommand{}).(agent.Tool); ok {
		t.Fatal("VerifyCommand must not satisfy agent.Tool")
	}
	for _, tool := range NewMutatingTools(nil, nil) {
		if strings.Contains(tool.Spec().Name, "verify") {
			t.Fatalf("verify must not be a model-visible tool, found %q", tool.Spec().Name)
		}
	}
}
