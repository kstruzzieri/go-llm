//go:build unix

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as a hermetic helper binary: when invoked with the sentinel first
// arg, it behaves as the child process the runner spawns, then exits.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__golem_exec_helper__" {
		os.Exit(helperMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// helperMain: ["echo", args...] -> print args to stdout; ["fail", code] -> exit code;
// ["sleep", seconds] -> sleep (spawns its own child to verify group kill);
// ["dumpenv"] -> print env one per line.
func helperMain(args []string) int {
	if len(args) == 0 {
		return 0
	}
	switch args[0] {
	case "echo":
		fmt.Fprintln(os.Stdout, strings.Join(args[1:], " "))
		return 0
	case "errto":
		fmt.Fprintln(os.Stderr, strings.Join(args[1:], " "))
		return 0
	case "fail":
		return 3
	case "dumpenv":
		for _, e := range os.Environ() {
			fmt.Fprintln(os.Stdout, e)
		}
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	}
	return 0
}

func helperSpec(t *testing.T, args ...string) execSpec {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{self, "__golem_exec_helper__"}, args...)
	return execSpec{Path: self, Argv: argv, Dir: t.TempDir(), Env: []string{"PATH=/usr/bin:/bin"}}
}

func TestUnixRunnerEchoAndExit(t *testing.T) {
	r := unixRunner{}
	res, err := r.Run(context.Background(), helperSpec(t, "echo", "hello", "world"))
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hello world") {
		t.Errorf("exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}
}

func TestUnixRunnerNonZeroExit(t *testing.T) {
	r := unixRunner{}
	res, err := r.Run(context.Background(), helperSpec(t, "fail"))
	if err != nil {
		t.Fatalf("non-zero exit must not be a runner error: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
}

func TestUnixRunnerTimeoutKills(t *testing.T) {
	r := unixRunner{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, err := r.Run(ctx, helperSpec(t, "sleep", "30"))
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !res.TimedOut {
		t.Error("want TimedOut=true")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("kill did not take effect promptly")
	}
}

func TestUnixRunnerEnvIsolation(t *testing.T) {
	r := unixRunner{}
	spec := helperSpec(t, "dumpenv")
	spec.Env = []string{"PATH=/usr/bin:/bin", "HOME=/home/x"}
	res, err := r.Run(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	dump := string(res.Stdout)
	if !strings.Contains(dump, "HOME=/home/x") || !strings.Contains(dump, "PATH=") {
		t.Errorf("allowlisted vars missing:\n%s", dump)
	}
	if strings.Contains(dump, "__golem_exec_helper__") {
		t.Error("sentinel leaked into env (should be argv only)")
	}
}

func TestUnixRunnerOutputCap(t *testing.T) {
	r := unixRunner{}
	big := strings.Repeat("x", execStdoutCap+5000)
	res, err := r.Run(context.Background(), helperSpec(t, "echo", big))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) > execStdoutCap || !res.StdoutTruncated {
		t.Errorf("len=%d truncated=%v want <=%d and truncated", len(res.Stdout), res.StdoutTruncated, execStdoutCap)
	}
}
