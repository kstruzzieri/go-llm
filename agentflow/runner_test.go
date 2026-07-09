package agentflow

import (
	"context"
	"reflect"
	"testing"
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
}

func (f *fakeRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, nil, 0, nil
	}
	rep := f.replies[args[0]]
	return rep.stdout, rep.stderr, rep.exit, rep.err
}
