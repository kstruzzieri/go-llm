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

// TestExecRunnerEmptyPolicyNeverInherits runs a runner whose policy selects only the runner-owned PWD: nothing from the canary-only parent may appear.
func TestExecRunnerEmptyPolicyNeverInherits(t *testing.T) {
	out := filepath.Join(t.TempDir(), "probe.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecRunnerEmptyPolicyHelper$", "--", "emptypolicy", out)
	cmd.Env = []string{envCanaryName + "=" + envCanaryValue}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	report := readEnvProbe(t, out)
	if report.Canary || !slices.Equal(report.Names, []string{"PWD"}) || report.PWD != "/" {
		t.Fatalf("empty-policy child names=%v pwd=%q canary=%v, want only the runner-owned PWD=/", report.Names, report.PWD, report.Canary)
	}
}

func TestWorkingDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ goos, dir, want string }{
		{goos: "linux", dir: "/a/b/../c", want: "PWD=/a/c"},
		{goos: "darwin", dir: dir, want: "PWD=" + dir},
		{goos: "linux", dir: "rel", want: "PWD=" + filepath.Join(wd, "rel")},
		{goos: "linux", dir: "", want: "PWD=" + wd},
		{goos: "windows", dir: dir, want: ""},
		{goos: "plan9", dir: dir, want: ""},
	} {
		got, err := workingDirEnv(tc.goos, tc.dir)
		if err != nil || got != tc.want {
			t.Errorf("workingDirEnv(%q, %q) = %q, %v; want %q", tc.goos, tc.dir, got, err, tc.want)
		}
	}
}

// The child's PWD is the runner directory as given: never the parent's PWD,
// never canonicalized, absolute for a relative directory.
func TestExecRunnerSetsPWDFromRunnerDir(t *testing.T) {
	t.Chdir(t.TempDir()) // t.Chdir sets PWD, so the stale value must come after it
	t.Setenv("PWD", "/stale/parent/pwd")
	wd, err := os.Getwd() // the stale PWD fails Getwd's identity check
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir("rel", 0o700); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	checkout := writeSourceCheckoutFixture(t)
	for _, tc := range []struct {
		name   string
		runner *ExecRunner
		want   string
	}{
		{name: "installed, symlinked dir keeps its spelling", runner: NewExecRunner(link), want: link},
		{name: "source", runner: NewSrcExecRunner(realDir, checkout), want: realDir},
		{name: "relative dir", runner: NewExecRunner("rel"), want: filepath.Join(wd, "rel")},
		{name: "a second root", runner: NewExecRunner(other), want: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if report := runEnvProbe(t, tc.runner); report.PWD != tc.want {
				t.Fatalf("child PWD = %q, want %q", report.PWD, tc.want)
			}
		})
	}
}

// Run must add PWD to a copy of the owned entries: owned is r.env itself, and
// appending into its spare capacity would race between concurrent launches.
func TestExecRunnerAppendsPWDToACopyOfOwnedEntries(t *testing.T) {
	r := NewExecRunner(t.TempDir())
	backing := []string{"PYTHONDONTWRITEBYTECODE=1", "sentinel"}
	r.env = backing[:1]
	report := runEnvProbe(t, r)
	if backing[1] != "sentinel" || !slices.Equal(r.env, []string{"PYTHONDONTWRITEBYTECODE=1"}) {
		t.Fatalf("Run wrote into the runner's owned entries: backing[1]=%q env=%q", backing[1], r.env)
	}
	if !slices.Contains(report.Names, "PWD") || !slices.Contains(report.Names, "PYTHONDONTWRITEBYTECODE") {
		t.Fatalf("child names = %v, want PWD and PYTHONDONTWRITEBYTECODE", report.Names)
	}
}

// TestExecRunnerRemovedCwdHelper is not a test. Re-executed as `<test binary>
// -test.run=^TestExecRunnerRemovedCwdHelper$ -- removedcwd <out>`, it removes
// its own working directory and runs the env probe through a runner with an
// empty dir, whose PWD therefore needs the (unresolvable) cwd. Exit 6: Run
// succeeded; exit 7: the probe launched.
func TestExecRunnerRemovedCwdHelper(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || len(os.Args) != i+3 || os.Args[i+1] != "removedcwd" {
		return
	}
	dir, err := os.MkdirTemp("", "removed-cwd-")
	if err != nil {
		os.Exit(2)
	}
	if err := os.Chdir(dir); err != nil {
		os.Exit(2)
	}
	if err := os.Remove(dir); err != nil {
		os.Exit(2)
	}
	r := NewExecRunner("")
	r.bin, r.prefix = os.Args[0], envProbeArgv(os.Args[i+2])
	if _, _, _, err := r.Run(context.Background(), nil, nil); err == nil {
		os.Exit(6)
	}
	if _, err := os.Stat(os.Args[i+2]); err == nil {
		os.Exit(7)
	}
	os.Exit(0)
}

func TestExecRunnerFailsBeforeLaunchWhenWorkingDirIsUnresolvable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("darwin's getcwd still resolves a removed directory; Linux reports ENOENT")
	}
	out := filepath.Join(t.TempDir(), "probe.json")
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecRunnerRemovedCwdHelper$", "--", "removedcwd", out)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")} // no PWD: Getwd must ask the kernel
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
}
