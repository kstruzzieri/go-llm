//go:build darwin

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
