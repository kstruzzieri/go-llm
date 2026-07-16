package agentflow

import (
	"context"
	"reflect"
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

func TestExecRunnerArgv_SrcMode(t *testing.T) {
	r := NewSrcExecRunner("/ws", "/checkout")
	bin, argv, env := r.commandFor([]string{"status"})
	if bin != "python3" {
		t.Fatalf("bin = %q, want python3", bin)
	}
	if want := []string{"-m", "agentflow", "status"}; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	if want := "PYTHONPATH=/checkout/src"; len(env) != 1 || env[0] != want {
		t.Fatalf("env = %v, want [%s]", env, want)
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
