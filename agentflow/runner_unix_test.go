//go:build unix

package agentflow

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const execRunnerHelperEnv = "GO_LLM_AGENTFLOW_RUNNER_HELPER"

func TestExecRunnerGroupKillHelper(t *testing.T) {
	if os.Getenv(execRunnerHelperEnv) != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		os.Exit(2)
	}
	switch os.Args[separator+1] {
	case "group":
		if separator+2 >= len(os.Args) {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestExecRunnerGroupKillHelper$", "--", "sleep")
		child.Env = os.Environ()
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Args[separator+2], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(2)
		}
		time.Sleep(30 * time.Second)
	case "sleep":
		time.Sleep(30 * time.Second)
	default:
		os.Exit(2)
	}
}

func TestExecRunner_CancelKillsProcessGroup(t *testing.T) {
	pidFile := t.TempDir() + "/grandchild.pid"
	r := &ExecRunner{
		bin: os.Args[0],
		prefix: []string{
			"-test.run=^TestExecRunnerGroupKillHelper$", "--", "group", pidFile,
		},
		dir: t.TempDir(),
		env: []string{execRunnerHelperEnv + "=1"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _, _ = r.Run(ctx, nil, nil)
	}()

	grandchildPID := waitForRunnerPIDFile(t, pidFile)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	waitForProcessExit(t, grandchildPID)
}

func TestProcessStatIsZombie(t *testing.T) {
	tests := []struct {
		name string
		stat string
		want bool
	}{
		{name: "zombie", stat: "123 (go test) Z 1 2 3", want: true},
		{name: "running", stat: "123 (go test) R 1 2 3", want: false},
		{name: "closing parenthesis in name", stat: "123 (go) test) Z 1 2 3", want: true},
		{name: "malformed", stat: "123 go-test Z 1 2 3", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processStatIsZombie([]byte(tt.stat)); got != tt.want {
				t.Fatalf("processStatIsZombie(%q) = %v, want %v", tt.stat, got, tt.want)
			}
		})
	}
}

func waitForRunnerPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for grandchild pid")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processExited(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d still exists after process-group cancellation", pid)
}

func processExited(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	if runtime.GOOS != "linux" {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && processStatIsZombie(stat)
}

func processStatIsZombie(stat []byte) bool {
	closeParen := bytes.LastIndexByte(stat, ')')
	return closeParen >= 0 && len(stat) > closeParen+2 && stat[closeParen+1] == ' ' && stat[closeParen+2] == 'Z'
}

// A stat failure other than a missing file keeps its cause: an unreadable
// package directory must not be reported as a missing package.
func TestNewSrcExecRunnerKeepsUnexpectedStatError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	checkout := writeSourceCheckoutFixture(t)
	pkg := filepath.Join(checkout, "src", "agentflow")
	if err := os.Chmod(pkg, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pkg, 0o700) })
	_, _, _, err := NewSrcExecRunner(t.TempDir(), checkout).Run(t.Context(), nil, nil)
	if !errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "no src/agentflow package") {
		t.Fatalf("err = %v, want the permission error, not a missing package", err)
	}
}

// TestExecRunnerEmptyPolicyHelper is not a test. Re-executed as `<test
// binary> -test.run=^TestExecRunnerEmptyPolicyHelper$ -- emptypolicy <out>`
// with an environment that holds only the canary, it runs the env probe
// through an installed-mode runner whose policy selects nothing.
func TestExecRunnerEmptyPolicyHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) != i+3 || os.Args[i+1] != "emptypolicy" {
		return
	}
	r := NewExecRunner("/")
	r.bin, r.prefix = os.Args[0], envProbeArgv(os.Args[i+2])
	if _, _, exit, err := r.Run(context.Background(), nil, nil); err != nil || exit != 0 {
		os.Exit(5)
	}
	os.Exit(0)
}

// TestExecRunnerEmptyPolicyNeverInherits catches a regression to
// `if len(env) > 0 { cmd.Env = env }`: with no baseline variable set, the
// policy is empty, and an empty environment must not fall back to inheritance.
func TestExecRunnerEmptyPolicyNeverInherits(t *testing.T) {
	out := filepath.Join(t.TempDir(), "probe.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecRunnerEmptyPolicyHelper$", "--", "emptypolicy", out)
	cmd.Env = []string{envCanaryName + "=" + envCanaryValue}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	report := readEnvProbe(t, out)
	if report.Canary || len(report.Names) != 0 {
		t.Fatalf("empty-policy child names=%v canary=%v, want none", report.Names, report.Canary)
	}
}
