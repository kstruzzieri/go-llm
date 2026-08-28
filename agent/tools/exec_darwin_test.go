//go:build darwin

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// writeFakeSandboxExec writes an executable shell script standing in for
// /usr/bin/sandbox-exec in probe tests.
func writeFakeSandboxExec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sandbox-exec")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProbeSeatbeltFileChecks(t *testing.T) {
	dir := t.TempDir()
	nonexec := filepath.Join(dir, "sandbox-exec")
	if err := os.WriteFile(nonexec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"missing":        filepath.Join(dir, "missing"),
		"directory":      dir,
		"non-executable": nonexec,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			err := probeSeatbelt(context.Background(), path)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("probe must fail naming the path: %v", err)
			}
		})
	}
}

// TestProbeSeatbeltNestedSandboxShape drives the real probe through a stand-in
// binary reproducing the observed nested-sandbox failure: exists, executable,
// but cannot apply a profile. A stat-only check would pass here; the active
// probe must fail and carry the diagnostic.
func TestProbeSeatbeltNestedSandboxShape(t *testing.T) {
	fake := writeFakeSandboxExec(t,
		"echo 'sandbox_apply: Operation not permitted' >&2\nexit 1\n")
	err := probeSeatbelt(context.Background(), fake)
	if err == nil || !strings.Contains(err.Error(), "Operation not permitted") {
		t.Fatalf("probe must surface the sandbox_apply diagnostic: %v", err)
	}
}

func TestProbeSeatbeltTimeout(t *testing.T) {
	fake := writeFakeSandboxExec(t, "sleep 5\n")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := probeSeatbelt(ctx, fake); err == nil {
		t.Fatal("hung probe must fail, not block construction forever")
	}
}

func TestProbeSeatbeltSucceedsOnFakeApplier(t *testing.T) {
	fake := writeFakeSandboxExec(t, "exit 0\n")
	if err := probeSeatbelt(context.Background(), fake); err != nil {
		t.Fatalf("probe of a working applier failed: %v", err)
	}
}

func TestSeatbeltConstructorRejectsUnenforceableBeforeProbe(t *testing.T) {
	probeCalled := false
	_, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { probeCalled = true; return nil },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt, MemoryCapMB: 512})
	if err == nil || !strings.Contains(err.Error(), "MemoryCapMB") {
		t.Fatalf("unenforceable field accepted: %v", err)
	}
	if probeCalled {
		t.Fatal("probe must not run for a rejected config")
	}
}

func TestSeatbeltConstructorFailsClosedOnProbeError(t *testing.T) {
	probeErr := errors.New("sandbox_apply: Operation not permitted")
	backend, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { return probeErr },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	if backend != nil || !errors.Is(err, probeErr) {
		t.Fatalf("probe failure must fail construction: backend=%T err=%v", backend, err)
	}
}

func TestSeatbeltConstructorSucceedsOnProbeSuccess(t *testing.T) {
	backend, err := newSeatbeltExecBackendAt("/usr/bin/sandbox-exec",
		func(context.Context, string) error { return nil },
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	sb, ok := backend.(*seatbeltBackend)
	if !ok {
		t.Fatalf("backend = %T, want *seatbeltBackend", backend)
	}
	if !sb.allowNetwork || sb.execPath != "/usr/bin/sandbox-exec" {
		t.Fatalf("backend misconfigured: %+v", sb)
	}
	if sb.runner == nil || sb.starter == nil {
		t.Fatal("backend must carry both host delegates")
	}
}

// captureRunner records the spec it was invoked with and returns a canned
// result; the optional during hook runs mid-delegation to model races.
type captureRunner struct {
	spec   execSpec
	res    execResult
	err    error
	called int
	during func(execSpec)
}

func (c *captureRunner) Run(_ context.Context, s execSpec) (execResult, error) {
	c.called++
	c.spec = s
	if c.during != nil {
		c.during(s)
	}
	return c.res, c.err
}

// testSeatbeltBackend builds a backend with fake delegates, an isolated temp
// base, and a fixed system-root set. No real sandbox-exec is involved.
func testSeatbeltBackend(t *testing.T, runner commandRunner) (*seatbeltBackend, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &seatbeltBackend{
		execPath: seatbeltExecPath,
		runner:   runner,
		starter:  unixStarter{},
		tempBase: base,
		systemRoots: func(string) ([]string, error) {
			return []string{"/usr/lib", "/bin"}, nil
		},
	}, base
}

func canonTempDirT(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func seatbeltSpec(t *testing.T, ws string) execSpec {
	t.Helper()
	return execSpec{
		Path:          "/bin/echo",
		Argv:          []string{"echo", "hi"},
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin", "HOME=/Users/nobody"},
		WorkspaceRoot: ws,
	}
}

func tmpdirOf(t *testing.T, env []string) string {
	t.Helper()
	var vals []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "TMPDIR="); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) != 1 {
		t.Fatalf("child env must carry exactly one TMPDIR, got %q", vals)
	}
	return vals[0]
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("temp base not reaped: %q", names)
	}
}

func TestSeatbeltRunRejectsBadWorkspaceRoot(t *testing.T) {
	for name, root := range map[string]string{
		"empty":       "",
		"volume root": "/",
		"unclean":     "/private/tmp/ws/..",
		"relative":    "private/tmp/ws",
	} {
		t.Run(name, func(t *testing.T) {
			fake := &captureRunner{}
			b, base := testSeatbeltBackend(t, fake)
			spec := seatbeltSpec(t, canonTempDirT(t))
			spec.WorkspaceRoot = root
			if _, err := b.Run(context.Background(), spec); err == nil {
				t.Fatal("bad workspace root accepted")
			}
			if fake.called != 0 {
				t.Fatal("delegate ran despite rejected root")
			}
			assertEmptyDir(t, base)
		})
	}
}

// TestSeatbeltRunRejectsEmptyArgv guards the argv[1:] slice: a hand-built
// spec with no argv (or no path) must fail closed, never panic, and never
// reach the delegate.
func TestSeatbeltRunRejectsEmptyArgv(t *testing.T) {
	for name, mutate := range map[string]func(*execSpec){
		"nil argv":   func(s *execSpec) { s.Argv = nil },
		"empty argv": func(s *execSpec) { s.Argv = []string{} },
		"empty path": func(s *execSpec) { s.Path = "" },
	} {
		t.Run(name, func(t *testing.T) {
			fake := &captureRunner{}
			b, base := testSeatbeltBackend(t, fake)
			spec := seatbeltSpec(t, canonTempDirT(t))
			mutate(&spec)
			if _, err := b.Run(context.Background(), spec); err == nil {
				t.Fatal("empty argv/path accepted")
			}
			if fake.called != 0 {
				t.Fatal("delegate ran despite empty argv/path")
			}
			assertEmptyDir(t, base)
		})
	}
}

func TestSeatbeltRunWrapsSpec(t *testing.T) {
	fake := &captureRunner{res: execResult{ExitCode: 0, Stdout: []byte("hi")}}
	b, base := testSeatbeltBackend(t, fake)
	ws := canonTempDirT(t)
	spec := seatbeltSpec(t, ws)
	res, err := b.Run(context.Background(), spec)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("run: res=%+v err=%v", res, err)
	}
	got := fake.spec
	if got.Path != seatbeltExecPath {
		t.Fatalf("wrapped Path = %q, want %q", got.Path, seatbeltExecPath)
	}
	if len(got.Argv) != 5 || got.Argv[0] != "sandbox-exec" || got.Argv[1] != "-p" ||
		got.Argv[3] != "/bin/echo" || got.Argv[4] != "hi" {
		t.Fatalf("wrapped argv shape wrong: %q", got.Argv)
	}
	if got.Dir != spec.Dir || got.WorkspaceRoot != ws {
		t.Fatalf("Dir/WorkspaceRoot altered: %+v", got)
	}
	childTemp := tmpdirOf(t, got.Env)
	if filepath.Dir(childTemp) != base {
		t.Fatalf("child TMPDIR %q not under injected base %q", childTemp, base)
	}
	profile := got.Argv[2]
	for _, want := range []string{
		"(deny default)",
		`(subpath "` + ws + `")`,
		`(subpath "` + childTemp + `")`,
		`(literal "/bin/echo")`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "network") {
		t.Fatalf("default profile must not mention network:\n%s", profile)
	}
	assertEmptyDir(t, base) // cleaned after completion
}

// TestSeatbeltRunHostileAmbientTMPDIR pins D5: inherited TMPDIR values are
// command input, never policy. TMPDIR=/ must not become (subpath "/") and
// TMPDIR=$HOME must not smuggle home into the profile.
func TestSeatbeltRunHostileAmbientTMPDIR(t *testing.T) {
	for name, hostile := range map[string]string{
		"volume root": "/",
		"home":        "/Users/nobody",
	} {
		t.Run(name, func(t *testing.T) {
			fake := &captureRunner{}
			b, base := testSeatbeltBackend(t, fake)
			ws := canonTempDirT(t)
			spec := seatbeltSpec(t, ws)
			spec.Env = append(spec.Env, "TMPDIR="+hostile)
			if _, err := b.Run(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			childTemp := tmpdirOf(t, fake.spec.Env)
			if childTemp == hostile || filepath.Dir(childTemp) != base {
				t.Fatalf("child TMPDIR %q not the private directory", childTemp)
			}
			profile := fake.spec.Argv[2]
			if strings.Contains(profile, `(subpath "`+hostile+`")`) {
				t.Fatalf("hostile TMPDIR %q entered the profile:\n%s", hostile, profile)
			}
		})
	}
}

// TestSeatbeltRunCanonicalExecutableTarget: the read allowance names the
// canonical target, so a symlinked approved path cannot dangle policy on the
// link spelling the kernel never matches against.
func TestSeatbeltRunCanonicalExecutableTarget(t *testing.T) {
	fake := &captureRunner{}
	b, _ := testSeatbeltBackend(t, fake)
	ws := canonTempDirT(t)
	real := filepath.Join(ws, "real-tool")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link-tool")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	spec := seatbeltSpec(t, ws)
	spec.Path = link
	spec.Argv = []string{"link-tool"}
	if _, err := b.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	profile := fake.spec.Argv[2]
	if !strings.Contains(profile, `(literal "`+real+`")`) {
		t.Fatalf("profile missing canonical executable target %q:\n%s", real, profile)
	}
	if fake.spec.Argv[3] != link {
		t.Fatalf("approved argv path rewritten: %q", fake.spec.Argv[3])
	}
}

func TestSeatbeltRunCleanupAndResultTaxonomy(t *testing.T) {
	t.Run("non-zero exit preserved", func(t *testing.T) {
		fake := &captureRunner{res: execResult{ExitCode: 7, Stderr: []byte("boom")}}
		b, base := testSeatbeltBackend(t, fake)
		res, err := b.Run(context.Background(), seatbeltSpec(t, canonTempDirT(t)))
		if err != nil || res.ExitCode != 7 || string(res.Stderr) != "boom" {
			t.Fatalf("result altered: res=%+v err=%v", res, err)
		}
		assertEmptyDir(t, base)
	})
	t.Run("runner error propagated and temp cleaned", func(t *testing.T) {
		fake := &captureRunner{err: errors.New("spawn failed")}
		b, base := testSeatbeltBackend(t, fake)
		if _, err := b.Run(context.Background(), seatbeltSpec(t, canonTempDirT(t))); err == nil {
			t.Fatal("runner error swallowed")
		}
		assertEmptyDir(t, base)
	})
	t.Run("collector error cleans temp and skips delegate", func(t *testing.T) {
		fake := &captureRunner{}
		b, base := testSeatbeltBackend(t, fake)
		b.systemRoots = func(string) ([]string, error) { return nil, errors.New("collector down") }
		if _, err := b.Run(context.Background(), seatbeltSpec(t, canonTempDirT(t))); err == nil {
			t.Fatal("collector error swallowed")
		}
		if fake.called != 0 {
			t.Fatal("delegate ran despite collector failure")
		}
		assertEmptyDir(t, base)
	})
}

// TestSeatbeltRunReplacementRaceLeavesForeignDir: if the private temp pathname
// is swapped for a foreign directory while the command runs, guarded cleanup
// must neither delete the foreign directory's contents nor claim the original
// was reaped.
func TestSeatbeltRunReplacementRaceLeavesForeignDir(t *testing.T) {
	b, base := testSeatbeltBackend(t, nil)
	var stolen, foreignMarker string
	fake := &captureRunner{during: func(s execSpec) {
		temp := tmpdirOf(t, s.Env)
		stolen = filepath.Join(base, "stolen")
		if err := os.Rename(temp, stolen); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(temp, 0o700); err != nil {
			t.Fatal(err)
		}
		foreignMarker = filepath.Join(temp, "marker")
		if err := os.WriteFile(foreignMarker, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}}
	b.runner = fake
	res, err := b.Run(context.Background(), seatbeltSpec(t, canonTempDirT(t)))
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("cleanup outcome overwrote the command result: res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(foreignMarker); err != nil {
		t.Fatalf("guarded cleanup deleted the foreign directory's contents: %v", err)
	}
	if _, err := os.Stat(stolen); err != nil {
		t.Fatalf("original temp directory unexpectedly gone: %v", err)
	}
}

type captureStarter struct {
	spec   execSpec
	proc   backgroundProcess
	err    error
	called int
	during func(execSpec)
}

func (c *captureStarter) Start(s execSpec, _, _ io.Writer) (backgroundProcess, error) {
	c.called++
	c.spec = s
	if c.during != nil {
		c.during(s)
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.proc, nil
}

type fakeProcess struct {
	pid           int
	code          int
	managerKilled bool
	waitErr       error
	killErr       error
	killCalls     int
}

func (f *fakeProcess) PID() int                 { return f.pid }
func (f *fakeProcess) Wait() (int, bool, error) { return f.code, f.managerKilled, f.waitErr }
func (f *fakeProcess) Kill() error              { f.killCalls++; return f.killErr }

func TestSeatbeltStartWrapsSpecAndCleansAfterWait(t *testing.T) {
	starter := &captureStarter{proc: &fakeProcess{pid: 42, code: 7, managerKilled: true}}
	b, base := testSeatbeltBackend(t, nil)
	b.starter = starter
	ws := canonTempDirT(t)
	proc, err := b.Start(seatbeltSpec(t, ws), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if starter.spec.Path != seatbeltExecPath || starter.spec.Argv[1] != "-p" {
		t.Fatalf("delegate did not receive the wrapped spec: %q", starter.spec.Argv)
	}
	childTemp := tmpdirOf(t, starter.spec.Env)
	if _, err := os.Stat(childTemp); err != nil {
		t.Fatalf("private temp must exist while the process is outstanding: %v", err)
	}
	if !strings.Contains(starter.spec.Argv[2], `(subpath "`+ws+`")`) {
		t.Fatalf("profile missing workspace:\n%s", starter.spec.Argv[2])
	}
	code, managerKilled, err := proc.Wait()
	if code != 7 || !managerKilled || err != nil {
		t.Fatalf("Wait result altered: code=%d managerKilled=%v err=%v", code, managerKilled, err)
	}
	assertEmptyDir(t, base)
}

func TestSeatbeltStartRejectsBadRootBeforeDelegate(t *testing.T) {
	starter := &captureStarter{proc: &fakeProcess{}}
	b, base := testSeatbeltBackend(t, nil)
	b.starter = starter
	spec := seatbeltSpec(t, canonTempDirT(t))
	spec.WorkspaceRoot = ""
	if _, err := b.Start(spec, io.Discard, io.Discard); err == nil {
		t.Fatal("empty workspace root accepted by Start")
	}
	if starter.called != 0 {
		t.Fatal("delegate ran despite rejected root")
	}
	assertEmptyDir(t, base)
}

func TestSeatbeltStartSpawnFailureCleansTemp(t *testing.T) {
	starter := &captureStarter{err: errors.New("spawn failed")}
	b, base := testSeatbeltBackend(t, nil)
	b.starter = starter
	if _, err := b.Start(seatbeltSpec(t, canonTempDirT(t)), io.Discard, io.Discard); err == nil {
		t.Fatal("spawn failure swallowed")
	}
	assertEmptyDir(t, base)
}

func TestSeatbeltProcessDelegatesPIDAndKill(t *testing.T) {
	killErr := errors.New("kill unavailable")
	inner := &fakeProcess{pid: 4242, killErr: killErr}
	proc := &seatbeltProcess{backgroundProcess: inner, cleanup: func() error { return nil }}
	if proc.PID() != 4242 {
		t.Fatalf("PID = %d, want delegate's 4242", proc.PID())
	}
	if err := proc.Kill(); !errors.Is(err, killErr) {
		t.Fatalf("Kill error altered: %v", err)
	}
	if inner.killCalls != 1 {
		t.Fatalf("Kill delegate calls = %d, want 1", inner.killCalls)
	}
}

func TestSeatbeltProcessWaitCleansOnceAndPreservesErrors(t *testing.T) {
	waitErr := errors.New("observer broke")
	cleanups := 0
	proc := &seatbeltProcess{
		backgroundProcess: &fakeProcess{code: -1, waitErr: waitErr},
		cleanup: func() error {
			cleanups++
			return errors.New("cleanup failed")
		},
	}
	code, managerKilled, err := proc.Wait()
	if code != -1 || managerKilled || !errors.Is(err, waitErr) {
		t.Fatalf("Wait result rewritten by cleanup failure: code=%d mk=%v err=%v", code, managerKilled, err)
	}
	if cleanups != 1 {
		t.Fatalf("cleanup calls after Wait = %d, want 1", cleanups)
	}
}

// TestSeatbeltStartReplacementRaceLeavesForeignDir mirrors the foreground
// race regression through the background Wait path.
func TestSeatbeltStartReplacementRaceLeavesForeignDir(t *testing.T) {
	starter := &captureStarter{proc: &fakeProcess{}}
	b, base := testSeatbeltBackend(t, nil)
	b.starter = starter
	proc, err := b.Start(seatbeltSpec(t, canonTempDirT(t)), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	temp := tmpdirOf(t, starter.spec.Env)
	stolen := filepath.Join(base, "stolen")
	if err := os.Rename(temp, stolen); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(temp, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(temp, "marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, err := proc.Wait(); code != 0 || err != nil {
		t.Fatalf("Wait result altered: code=%d err=%v", code, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("guarded cleanup deleted the foreign directory's contents: %v", err)
	}
	if _, err := os.Stat(stolen); err != nil {
		t.Fatalf("original temp directory unexpectedly gone: %v", err)
	}
}

// --- Behavioral acceptance: real processes under real sandbox-exec ---

// requireSeatbeltCapability gates behavioral tests on the real active probe.
// Default mode skips with the probe's reason (e.g. a nested-sandbox host);
// GO_LLM_REQUIRE_SEATBELT=1 turns the skip into a hard failure so the release
// gate cannot silently pass without exercising confinement.
func requireSeatbeltCapability(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	if err := probeSeatbelt(ctx, seatbeltExecPath); err != nil {
		if os.Getenv("GO_LLM_REQUIRE_SEATBELT") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_SEATBELT=1 but Seatbelt is unavailable: %v", err)
		}
		t.Skipf("Seatbelt unavailable on this host: %v", err)
	}
}

// TestSeatbeltHelperProcess is the argv-selected helper entry point that runs
// INSIDE the sandbox. It is not a test: without the sentinel env it skips.
// Results are printed with the HELPER- prefix so assertions never confuse
// them with go-test chatter.
func TestSeatbeltHelperProcess(t *testing.T) {
	if os.Getenv("GO_LLM_SEATBELT_HELPER") != "1" {
		t.Skip("helper process entry point")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		fmt.Println("HELPER-ERR: no mode")
		os.Exit(2)
	}
	mode := args[0]
	fail := func(err error) {
		fmt.Printf("HELPER-ERR: %v\n", err)
		os.Exit(1)
	}
	switch mode {
	case "pid":
		fmt.Printf("HELPER-PID: %d\n", os.Getpid())
	case "tmprw":
		p := filepath.Join(os.TempDir(), "canary.txt")
		if err := os.WriteFile(p, []byte("temp-canary"), 0o600); err != nil {
			fail(err)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-OK: %s\n", data)
	case "read":
		data, err := os.ReadFile(args[1])
		if err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-OK: %s\n", data)
	case "tmpreport":
		fmt.Printf("HELPER-TMP: %s\n", os.TempDir())
	case "spawnchild":
		cmd := exec.Command("/bin/sleep", "300")
		if err := cmd.Start(); err != nil {
			fail(err)
		}
		fmt.Printf("HELPER-CHILD: %d\n", cmd.Process.Pid)
		// A timer sleep, not select{}: an empty select trips Go's deadlock
		// detector and exits the helper before the test can Kill it.
		time.Sleep(300 * time.Second)
	case "tcp", "unix":
		conn, err := net.DialTimeout(mode, args[1], 3*time.Second)
		if err != nil {
			fail(err)
		}
		_, _ = conn.Write([]byte("ping"))
		_ = conn.Close()
		fmt.Println("HELPER-OK: connected")
	case "udp":
		conn, err := net.Dial("udp", args[1])
		if err != nil {
			fail(err)
		}
		if _, err := conn.Write([]byte("ping")); err != nil {
			fail(err)
		}
		_ = conn.Close()
		fmt.Println("HELPER-OK: sent")
	default:
		fmt.Printf("HELPER-ERR: unknown mode %q\n", mode)
		os.Exit(2)
	}
	os.Exit(0)
}

// realSeatbeltRun executes argv under the real Seatbelt backend rooted at ws.
// extraEnv entries are appended to the sanitized base env.
func realSeatbeltRun(t *testing.T, cfg SandboxConfig, ws string, argv []string, extraEnv ...string) execResult {
	t.Helper()
	backend, err := newExecBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	spec := execSpec{
		Path:          argv[0],
		Argv:          argv,
		Dir:           ws,
		Env:           append([]string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME")}, extraEnv...),
		WorkspaceRoot: ws,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := backend.Run(ctx, spec)
	if err != nil {
		t.Fatalf("infra failure running %q: %v (stderr: %s)", argv, err, res.Stderr)
	}
	return res
}

// helperRun executes this test binary's helper entry point inside the sandbox.
func helperRun(t *testing.T, cfg SandboxConfig, ws, mode string, modeArgs ...string) execResult {
	t.Helper()
	if raceInstrumented {
		t.Skip("race-instrumented helper cannot initialize under the sandbox; helper legs run in the non-race pass")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{exe, "-test.run=^TestSeatbeltHelperProcess$", "-test.v", "--", mode}, modeArgs...)
	return realSeatbeltRun(t, cfg, ws, argv, "GO_LLM_SEATBELT_HELPER=1")
}

// outsideCanaryDir creates a unique directory under the real home holding a
// secret canary, asserting first that it is outside every allowance.
func outsideCanaryDir(t *testing.T, ws string) (dir, canary string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir, err = os.MkdirTemp(home, "go-llm-seatbelt-canary-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	assertOutsideAllowances(t, dir, ws)
	canary = filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(canary, []byte("SEATBELT-ESCAPE-CANARY"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, canary
}

// assertOutsideAllowances proves a probe path is not covered by the canonical
// workspace, the private temp base, or any fixed system read root — so a
// denial on it can only come from the deny-default policy.
func assertOutsideAllowances(t *testing.T, p, ws string) {
	t.Helper()
	covered := func(root string) bool {
		return p == root || strings.HasPrefix(p, root+"/")
	}
	if covered(ws) || covered(seatbeltTempBase) || covered("/dev") {
		t.Fatalf("probe path %q is inside an allowance", p)
	}
	for _, root := range seatbeltDefaultSystemRoots {
		if covered(root) {
			t.Fatalf("probe path %q is inside system root %q", p, root)
		}
	}
}

func TestSeatbeltBehavioralSimpleCommand(t *testing.T) {
	requireSeatbeltCapability(t)
	res := realSeatbeltRun(t, seatbeltTestCfg(), canonTempDirT(t), []string{"/bin/echo", "hi"})
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "hi") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestSeatbeltBehavioralReadConfinement(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	_, canary := outsideCanaryDir(t, ws)

	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/cat", canary})
	if res.ExitCode == 0 {
		t.Fatal("cat of a $HOME canary succeeded inside the sandbox")
	}
	if strings.Contains(string(res.Stdout), "SEATBELT-ESCAPE-CANARY") {
		t.Fatal("canary content leaked through the sandbox")
	}

	// Same-binary control: a workspace read must succeed, so the denial above
	// cannot be an unrelated failure of cat.
	inside := filepath.Join(ws, "readable.txt")
	if err := os.WriteFile(inside, []byte("inside-ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctrl := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/cat", inside})
	if ctrl.ExitCode != 0 || !strings.Contains(string(ctrl.Stdout), "inside-ok") {
		t.Fatalf("control read failed: exit=%d stdout=%q stderr=%q", ctrl.ExitCode, ctrl.Stdout, ctrl.Stderr)
	}
}

func TestSeatbeltBehavioralMetadataConfinement(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	_, canary := outsideCanaryDir(t, ws)
	inside := filepath.Join(ws, "stat-me.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctrl := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/stat", inside})
	if ctrl.ExitCode != 0 {
		t.Fatalf("control stat failed: stderr=%q", ctrl.Stderr)
	}
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/stat", canary})
	if res.ExitCode == 0 {
		t.Fatalf("stat of an outside canary succeeded: metadata is globally exposed:\n%s", res.Stdout)
	}
}

func TestSeatbeltBehavioralWriteConfinement(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	dir, _ := outsideCanaryDir(t, ws)
	probe := filepath.Join(dir, "write-probe")

	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/touch", probe})
	if res.ExitCode == 0 {
		t.Fatal("touch outside the workspace succeeded inside the sandbox")
	}
	if _, err := os.Lstat(probe); err == nil {
		t.Fatal("probe file exists: the write escaped the sandbox")
	}

	insideWS := filepath.Join(ws, "probe-ws")
	ctrl := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/touch", insideWS})
	if ctrl.ExitCode != 0 {
		t.Fatalf("control workspace write failed: stderr=%q", ctrl.Stderr)
	}
	if _, err := os.Lstat(insideWS); err != nil {
		t.Fatalf("control workspace file missing: %v", err)
	}
}

func TestSeatbeltBehavioralPrivateTempReadWrite(t *testing.T) {
	requireSeatbeltCapability(t)
	res := helperRun(t, seatbeltTestCfg(), canonTempDirT(t), "tmprw")
	if res.ExitCode != 0 || !strings.Contains(string(res.Stdout), "HELPER-OK: temp-canary") {
		t.Fatalf("private temp read/write failed: exit=%d stdout=%q stderr=%q",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}

func TestSeatbeltBehavioralSymlinkEscapeDenied(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	dir, canary := outsideCanaryDir(t, ws)

	readLink := filepath.Join(ws, "read-link")
	if err := os.Symlink(canary, readLink); err != nil {
		t.Fatal(err)
	}
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/cat", readLink})
	if res.ExitCode == 0 || strings.Contains(string(res.Stdout), "SEATBELT-ESCAPE-CANARY") {
		t.Fatalf("read through a workspace symlink escaped: exit=%d stdout=%q", res.ExitCode, res.Stdout)
	}

	writeTarget := filepath.Join(dir, "symlink-write-probe")
	writeLink := filepath.Join(ws, "write-link")
	if err := os.Symlink(writeTarget, writeLink); err != nil {
		t.Fatal(err)
	}
	res = realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/touch", writeLink})
	if res.ExitCode == 0 {
		t.Fatal("write through a workspace symlink succeeded")
	}
	if _, err := os.Lstat(writeTarget); err == nil {
		t.Fatal("symlink write probe exists outside the workspace")
	}
}

func TestSeatbeltBehavioralHardLinkCreationDenied(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	_, canary := outsideCanaryDir(t, ws)
	linkPath := filepath.Join(ws, "hard-link")
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/ln", canary, linkPath})
	if res.ExitCode == 0 {
		t.Fatal("hard-link creation from an outside canary succeeded")
	}
	if _, err := os.Lstat(linkPath); err == nil {
		t.Fatal("hard link exists inside the workspace")
	}
}

// TestSeatbeltRejectsWorkspaceHardLinkToOutsideInode reproduces the pathname
// escape without requiring a usable nested sandbox: a delegate writing the
// allowed workspace name mutates the outside inode unless prepare rejects the
// workspace before either foreground or background spawn.
func TestSeatbeltRejectsWorkspaceHardLinkToOutsideInode(t *testing.T) {
	for _, lifetime := range []string{"foreground", "background"} {
		t.Run(lifetime, func(t *testing.T) {
			ws := canonTempDirT(t)
			outside := filepath.Join(t.TempDir(), "outside-canary")
			if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
				t.Fatal(err)
			}
			linked := filepath.Join(ws, "pre-linked")
			if err := os.Link(outside, linked); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}

			writeThroughLink := func(execSpec) {
				if err := os.WriteFile(linked, []byte("escaped"), 0o600); err != nil {
					t.Fatalf("simulated workspace write: %v", err)
				}
			}
			runner := &captureRunner{during: writeThroughLink}
			starter := &captureStarter{proc: &fakeProcess{}, during: writeThroughLink}
			b, base := testSeatbeltBackend(t, runner)
			b.starter = starter

			var err error
			if lifetime == "foreground" {
				_, err = b.Run(context.Background(), seatbeltSpec(t, ws))
			} else {
				var proc backgroundProcess
				proc, err = b.Start(seatbeltSpec(t, ws), io.Discard, io.Discard)
				if proc != nil {
					_, _, _ = proc.Wait()
				}
			}
			if err == nil {
				t.Error("Seatbelt launch error = nil, want outside-hard-link rejection")
			}
			if calls := runner.called + starter.called; calls != 0 {
				t.Errorf("delegate calls = %d, want 0", calls)
			}
			got, readErr := os.ReadFile(outside)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(got) != "safe" {
				t.Errorf("outside canary = %q, want %q", got, "safe")
			}
			assertEmptyDir(t, base)
		})
	}
}

func TestSeatbeltAllowsHardLinksWhollyInsideWorkspace(t *testing.T) {
	ws := canonTempDirT(t)
	first := filepath.Join(ws, "first")
	if err := os.WriteFile(first, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, filepath.Join(ws, "second")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	fake := &captureRunner{}
	b, base := testSeatbeltBackend(t, fake)
	if _, err := b.Run(context.Background(), seatbeltSpec(t, ws)); err != nil {
		t.Fatalf("Seatbelt Run with internal hard links: %v", err)
	}
	if fake.called != 1 {
		t.Errorf("delegate calls = %d, want 1", fake.called)
	}
	assertEmptyDir(t, base)
}

// TestSeatbeltBehavioralDataVolumeAliasDenied: /System/Volumes/Data aliases
// user data on current macOS. Reading the alias spelling of the outside
// canary must fail — this catches an accidental broad /System rule.
func TestSeatbeltBehavioralDataVolumeAliasDenied(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	_, canary := outsideCanaryDir(t, ws)
	alias := filepath.Join("/System/Volumes/Data", canary)
	afi, err := os.Stat(alias)
	if err != nil {
		t.Skipf("Data-volume alias not present on this host: %v", err)
	}
	cfi, err := os.Stat(canary)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(afi, cfi) {
		t.Skip("alias names a different file on this host")
	}
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/bin/cat", alias})
	if res.ExitCode == 0 || strings.Contains(string(res.Stdout), "SEATBELT-ESCAPE-CANARY") {
		t.Fatalf("Data-volume alias read escaped: exit=%d", res.ExitCode)
	}
}

// TestSeatbeltBehavioralSocketConfinement observes denial at the LISTENERS:
// with AllowNetwork false no TCP connection, UDP datagram, or Unix-domain
// connection arrives; with true the same helper reaches each one, proving the
// profile (not the environment) causes the denial.
func TestSeatbeltBehavioralSocketConfinement(t *testing.T) {
	requireSeatbeltCapability(t)
	// Short workspace: macOS caps sun_path at 104 bytes, so the Unix socket
	// cannot live under the deep /var/folders test dir.
	ws, err := os.MkdirTemp("/private/tmp", "sbws-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ws) })

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tcpLn.Close() }()
	var tcpAccepts atomic.Int64
	go func() {
		for {
			conn, err := tcpLn.Accept()
			if err != nil {
				return
			}
			tcpAccepts.Add(1)
			_ = conn.Close()
		}
	}()

	udpConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udpConn.Close() }()
	var udpPackets atomic.Int64
	go func() {
		buf := make([]byte, 16)
		for {
			if _, _, err := udpConn.ReadFrom(buf); err != nil {
				return
			}
			udpPackets.Add(1)
		}
	}()

	sockPath := filepath.Join(ws, "test.sock")
	if !strings.HasPrefix(sockPath, ws+"/") {
		t.Fatalf("unix socket %q must live inside the workspace", sockPath)
	}
	unixLn, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unixLn.Close() }()
	var unixAccepts atomic.Int64
	go func() {
		for {
			conn, err := unixLn.Accept()
			if err != nil {
				return
			}
			unixAccepts.Add(1)
			_ = conn.Close()
		}
	}()

	targets := map[string][]string{
		"tcp":  {"tcp", tcpLn.Addr().String()},
		"udp":  {"udp", udpConn.LocalAddr().String()},
		"unix": {"unix", sockPath},
	}
	for name, args := range targets {
		res := helperRun(t, seatbeltTestCfg(), ws, args[0], args[1])
		if res.ExitCode == 0 {
			t.Errorf("%s helper succeeded under the denied profile: stdout=%q", name, res.Stdout)
		}
	}
	if got := tcpAccepts.Load(); got != 0 {
		t.Errorf("TCP listener accepted %d connections from a sandboxed process", got)
	}
	if got := udpPackets.Load(); got != 0 {
		t.Errorf("UDP listener received %d datagrams from a sandboxed process", got)
	}
	if got := unixAccepts.Load(); got != 0 {
		t.Errorf("Unix listener accepted %d connections from a sandboxed process", got)
	}

	allowCfg := SandboxConfig{Runtime: SandboxRuntimeSeatbelt, AllowNetwork: true}
	for name, args := range targets {
		res := helperRun(t, allowCfg, ws, args[0], args[1])
		if res.ExitCode != 0 {
			t.Errorf("%s AllowNetwork control failed: stdout=%q stderr=%q", name, res.Stdout, res.Stderr)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if tcpAccepts.Load() > 0 && udpPackets.Load() > 0 && unixAccepts.Load() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if tcpAccepts.Load() == 0 || udpPackets.Load() == 0 || unixAccepts.Load() == 0 {
		t.Errorf("AllowNetwork controls did not reach the listeners (tcp=%d udp=%d unix=%d)",
			tcpAccepts.Load(), udpPackets.Load(), unixAccepts.Load())
	}
}

func TestSeatbeltBehavioralResultTaxonomy(t *testing.T) {
	requireSeatbeltCapability(t)
	ws := canonTempDirT(t)
	if res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/false"}); res.ExitCode != 1 {
		t.Fatalf("non-zero exit not preserved: %d", res.ExitCode)
	}
	res := realSeatbeltRun(t, seatbeltTestCfg(), ws, []string{"/usr/bin/head", "-c", "100000", "/dev/urandom"})
	if res.ExitCode != 0 || !res.StdoutTruncated || len(res.Stdout) != execStdoutCap {
		t.Fatalf("output caps not preserved: exit=%d truncated=%v len=%d",
			res.ExitCode, res.StdoutTruncated, len(res.Stdout))
	}
}

// TestSeatbeltBehavioralPublicWiring runs the public foreground constructor
// end to end: approval identity in the plan, sandboxed execution in Invoke.
func TestSeatbeltBehavioralPublicWiring(t *testing.T) {
	requireSeatbeltCapability(t)
	toolsList, err := NewSandboxedExecTools(t.TempDir(), seatbeltTestCfg())
	if err != nil {
		t.Fatal(err)
	}
	rc := toolsList[0].(*RunCommand)
	raw := json.RawMessage(`{"argv":["/bin/echo","sandboxed"]}`)
	plan, err := rc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Preview, `runtime="seatbelt"`) ||
		!strings.Contains(plan.Preview, "temp=private") ||
		!strings.Contains(plan.ApprovalKey, "sb:") {
		t.Fatalf("plan missing seatbelt approval identity:\nkey=%q\n%s", plan.ApprovalKey, plan.Preview)
	}
	res, err := rc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "exit code: 0") || !strings.Contains(res.Content, "sandboxed") {
		t.Fatalf("unexpected result:\n%s", res.Content)
	}
}

func seatbeltTestCfg() SandboxConfig { return SandboxConfig{Runtime: SandboxRuntimeSeatbelt} }

// --- Background lifetime acceptance (Task 7) ---

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// helperStart launches the helper entry point as a background process under
// the real Seatbelt backend, returning the process and its live stdout.
func helperStart(t *testing.T, ws, mode string, modeArgs ...string) (backgroundProcess, *lockedBuffer) {
	t.Helper()
	if raceInstrumented {
		t.Skip("race-instrumented helper cannot initialize under the sandbox; helper legs run in the non-race pass")
	}
	backend, err := newExecBackend(seatbeltTestCfg())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := append([]string{exe, "-test.run=^TestSeatbeltHelperProcess$", "-test.v", "--", mode}, modeArgs...)
	spec := execSpec{
		Path:          exe,
		Argv:          argv,
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin:/bin", "GO_LLM_SEATBELT_HELPER=1"},
		WorkspaceRoot: ws,
	}
	out := &lockedBuffer{}
	proc, err := backend.Start(spec, out, io.Discard)
	if err != nil {
		t.Fatalf("start helper %q: %v", mode, err)
	}
	return proc, out
}

// awaitHelperLine polls the live stdout for a "PREFIX: value" line.
func awaitHelperLine(t *testing.T, out *lockedBuffer, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(out.String(), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), prefix+": "); ok {
				return v
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("helper never printed %q; output so far:\n%s", prefix, out.String())
	return ""
}

func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestSeatbeltBehavioralBackgroundPIDIdentity proves sandbox-exec applies the
// profile and execs the target IN PLACE: the sandboxed process's own PID must
// equal the group-leader PID the manager remembers, or group termination
// would target the wrong process.
func TestSeatbeltBehavioralBackgroundPIDIdentity(t *testing.T) {
	requireSeatbeltCapability(t)
	proc, out := helperStart(t, canonTempDirT(t), "pid")
	defer func() { _ = proc.Kill() }()
	reported := awaitHelperLine(t, out, "HELPER-PID")
	code, _, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("helper exit: code=%d err=%v output:\n%s", code, err, out.String())
	}
	if reported != fmt.Sprintf("%d", proc.PID()) {
		t.Fatalf("in-sandbox PID %s != manager leader PID %d", reported, proc.PID())
	}
}

// TestSeatbeltBehavioralBackgroundDescendantKill proves a sandboxed leader's
// same-group child dies on Kill: sandboxing must not break the managed
// process-group containment policy.
func TestSeatbeltBehavioralBackgroundDescendantKill(t *testing.T) {
	requireSeatbeltCapability(t)
	proc, out := helperStart(t, canonTempDirT(t), "spawnchild")
	childStr := awaitHelperLine(t, out, "HELPER-CHILD")
	var childPID int
	if _, err := fmt.Sscanf(childStr, "%d", &childPID); err != nil {
		t.Fatalf("bad child pid %q: %v", childStr, err)
	}
	if !pidAlive(childPID) {
		t.Fatalf("descendant %d not alive before Kill", childPID)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_, managerKilled, _ := proc.Wait()
		if !managerKilled {
			t.Error("manager kill not observed by Wait")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sandboxed background leader did not die after Kill")
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(childPID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(childPID) {
		t.Fatalf("descendant %d survived the group kill", childPID)
	}
}

// TestSeatbeltBehavioralBackgroundTempReaped proves the real private temp
// directory is removed after background reap.
func TestSeatbeltBehavioralBackgroundTempReaped(t *testing.T) {
	requireSeatbeltCapability(t)
	proc, out := helperStart(t, canonTempDirT(t), "tmpreport")
	tempDir := awaitHelperLine(t, out, "HELPER-TMP")
	if !strings.HasPrefix(tempDir, seatbeltTempBase+"/") {
		t.Fatalf("helper temp %q not under %q", tempDir, seatbeltTempBase)
	}
	if code, _, err := proc.Wait(); err != nil || code != 0 {
		t.Fatalf("helper exit: code=%d err=%v", code, err)
	}
	if _, err := os.Lstat(tempDir); err == nil {
		t.Fatalf("private temp %q survived background reap", tempDir)
	}
}

func bgToolsByName(t *testing.T, root string, cfg SandboxConfig) map[string]agent.Tool {
	t.Helper()
	m, err := NewSandboxedBackgroundManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	toolsList, err := NewExecToolsWithBackground(root, m)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]agent.Tool{}
	for _, tool := range toolsList {
		byName[tool.Spec().Name] = tool
	}
	return byName
}

func bgStart(t *testing.T, byName map[string]agent.Tool, argvJSON string) string {
	t.Helper()
	sc := byName["start_command"].(*StartCommand)
	raw := json.RawMessage(argvJSON)
	plan, err := sc.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Preview, `runtime="seatbelt"`) || !strings.Contains(plan.Preview, "temp=private") {
		t.Fatalf("start_command preview missing seatbelt identity:\n%s", plan.Preview)
	}
	res, err := sc.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(res.Content, "\n") {
		if h, ok := strings.CutPrefix(line, "handle: "); ok {
			return h
		}
	}
	t.Fatalf("no handle in start result:\n%s", res.Content)
	return ""
}

func bgAwaitExit(t *testing.T, byName map[string]agent.Tool, handle string) string {
	t.Helper()
	status := byName["command_status"].(*CommandStatus)
	raw := json.RawMessage(fmt.Sprintf(`{"handle":%q}`, handle))
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		res, err := status.Invoke(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(res.Content, "state: running") {
			return res.Content
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background job never exited")
	return ""
}

// TestSeatbeltBehavioralBackgroundPublicConfinement drives the PUBLIC
// background constructors end to end: an outside write is denied (the probe
// file never appears) while a workspace write succeeds — through
// NewSandboxedBackgroundManager + NewExecToolsWithBackground, so no tool-set
// construction path can split runtimes.
func TestSeatbeltBehavioralBackgroundPublicConfinement(t *testing.T) {
	requireSeatbeltCapability(t)
	root := t.TempDir()
	canonRoot, err := CanonicalWorkspaceRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	byName := bgToolsByName(t, root, seatbeltTestCfg())
	dir, _ := outsideCanaryDir(t, canonRoot)
	probe := filepath.Join(dir, "bg-write-probe")

	handle := bgStart(t, byName, fmt.Sprintf(`{"argv":["/usr/bin/touch",%q]}`, probe))
	final := bgAwaitExit(t, byName, handle)
	if strings.Contains(final, "exit_code: 0") {
		t.Fatalf("outside background write reported success:\n%s", final)
	}
	if _, err := os.Lstat(probe); err == nil {
		t.Fatal("probe file exists: the background write escaped the sandbox")
	}

	inside := filepath.Join(canonRoot, "bg-probe-ws")
	handle = bgStart(t, byName, fmt.Sprintf(`{"argv":["/usr/bin/touch",%q]}`, inside))
	final = bgAwaitExit(t, byName, handle)
	if !strings.Contains(final, "exit_code: 0") {
		t.Fatalf("control background workspace write failed:\n%s", final)
	}
	if _, err := os.Lstat(inside); err != nil {
		t.Fatalf("control background file missing: %v", err)
	}
}

// TestSeatbeltPrepareMissingExecutableCleansTemp covers real-prepare spawn
// preconditions: a vanished executable fails after temp creation, so the
// private directory must still be reaped and the delegate never called.
func TestSeatbeltPrepareMissingExecutableCleansTemp(t *testing.T) {
	fake := &captureRunner{}
	b, base := testSeatbeltBackend(t, fake)
	spec := seatbeltSpec(t, canonTempDirT(t))
	spec.Path = "/nonexistent-go-llm-tool"
	if _, err := b.Run(context.Background(), spec); err == nil {
		t.Fatal("missing executable accepted")
	}
	if fake.called != 0 {
		t.Fatal("delegate ran despite missing executable")
	}
	assertEmptyDir(t, base)
}

// TestNewExecBackendSeatbeltMatchesRealCapability is the characterization pin:
// public construction succeeds exactly when the real active probe succeeds on
// this host. On an unsandboxed macOS host both succeed; inside a nested
// sandbox both must fail.
func TestNewExecBackendSeatbeltMatchesRealCapability(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), seatbeltProbeTimeout)
	defer cancel()
	probeErr := probeSeatbelt(ctx, seatbeltExecPath)
	got, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeSeatbelt})
	if (err == nil) != (probeErr == nil) {
		t.Fatalf("constructor outcome (err=%v) disagrees with real capability (probe=%v)", err, probeErr)
	}
	if err != nil {
		return // incapable host: fail-closed is the correct outcome
	}
	if _, ok := got.execBackend.(*seatbeltBackend); !ok {
		t.Fatalf("backend = %T, want *seatbeltBackend", got.execBackend)
	}
	if !strings.HasPrefix(got.approval.keyComponent, "sb:") {
		t.Fatalf("seatbelt must get a sandbox key namespace, got %q", got.approval.keyComponent)
	}
	want := `runtime="seatbelt" network=denied memory_cap=none cpu_limit=none drop_caps=[] temp=private`
	if got.approval.preview != want {
		t.Fatalf("preview = %q, want %q", got.approval.preview, want)
	}
}

// platformTestSetup is the Darwin no-op counterpart of the Linux test-parent
// descriptor hygiene in exec_linux_test.go; TestMain calls it on every unix
// platform.
func platformTestSetup() {}
