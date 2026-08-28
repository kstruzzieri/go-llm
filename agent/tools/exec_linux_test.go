//go:build linux

package tools

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// recordingProbe fails the test if the probe runs when construction must have
// already failed, and records the argv it receives otherwise.
type recordingProbe struct {
	t      *testing.T
	argv   []string
	called int
	err    error
}

func (p *recordingProbe) probe(_ context.Context, argv []string) error {
	p.called++
	p.argv = append([]string(nil), argv...)
	return p.err
}

func writeFakeBinary(t *testing.T, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-bin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBwrapConstructorRejectsUnsupportedConfigBeforeProbe(t *testing.T) {
	probe := &recordingProbe{t: t}
	bwrap := writeFakeBinary(t, 0o755)
	for name, cfg := range map[string]SandboxConfig{
		"cpu limit":       {Runtime: SandboxRuntimeBwrap, CPULimit: 0.5},
		"overflowing cap": {Runtime: SandboxRuntimeBwrap, MemoryCapMB: 1 << 45},
	} {
		if _, err := newBwrapExecBackendAt(bwrap, bwrap, probe.probe, cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if probe.called != 0 {
		t.Fatalf("probe ran %d times for unsupported configs", probe.called)
	}
}

func TestBwrapConstructorChecksBinariesBeforeProbe(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	missing := filepath.Join(t.TempDir(), "missing")
	nonExec := writeFakeBinary(t, 0o644)
	setuid := writeFakeBinary(t, 0o755|os.ModeSetuid)
	worldWritable := writeFakeBinary(t, 0o777)
	symlinked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, symlinked); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":        missing,
		"non-executable": nonExec,
		"setuid":         setuid,
		"world-writable": worldWritable,
		"symlink":        symlinked,
	}
	for name, path := range cases {
		t.Run("bwrap "+name, func(t *testing.T) {
			probe := &recordingProbe{t: t}
			_, err := newBwrapExecBackendAt(path, good, probe.probe, SandboxConfig{Runtime: SandboxRuntimeBwrap})
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name %s bwrap path %q: %v", name, path, err)
			}
			if probe.called != 0 {
				t.Fatalf("probe ran despite unsafe bwrap binary")
			}
		})
		t.Run("prlimit "+name, func(t *testing.T) {
			probe := &recordingProbe{t: t}
			_, err := newBwrapExecBackendAt(good, path, probe.probe,
				SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 256})
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("error must name %s prlimit path %q: %v", name, path, err)
			}
			if probe.called != 0 {
				t.Fatalf("probe ran despite unsafe prlimit binary")
			}
		})
	}
}

func TestBwrapConstructorSkipsPrlimitWhenUncapped(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	missing := filepath.Join(t.TempDir(), "missing-prlimit")
	probe := &recordingProbe{t: t}
	backend, err := newBwrapExecBackendAt(good, missing, probe.probe, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatalf("uncapped config must not inspect prlimit: %v", err)
	}
	if probe.called != 1 {
		t.Fatalf("probe calls = %d, want 1", probe.called)
	}
	if backend == nil {
		t.Fatal("backend missing")
	}
}

func TestBwrapProbeArgvUncappedDeniedNet(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	probe := &recordingProbe{t: t}
	if _, err := newBwrapExecBackendAt(good, good, probe.probe, SandboxConfig{Runtime: SandboxRuntimeBwrap}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		good,
		"--unshare-user", "--disable-userns", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts", "--unshare-net",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--clearenv",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
		"--tmpfs", "/dev/shm",
		"--tmpfs", "/tmp",
		"--remount-ro", "/",
		"/bin/true",
	}
	if !slices.Equal(probe.argv, want) {
		t.Fatalf("probe argv mismatch:\n got %q\nwant %q", probe.argv, want)
	}
}

func TestBwrapProbeArgvCappedAllowedNet(t *testing.T) {
	bwrap := writeFakeBinary(t, 0o755)
	prlimit := writeFakeBinary(t, 0o755)
	probe := &recordingProbe{t: t}
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 256, AllowNetwork: true}
	if _, err := newBwrapExecBackendAt(bwrap, prlimit, probe.probe, cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{
		prlimit, "--as=268435456",
		bwrap,
		"--unshare-user", "--disable-userns", "--unshare-pid",
		"--unshare-ipc", "--unshare-uts",
		"--die-with-parent", "--new-session", "--cap-drop", "ALL",
		"--clearenv",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev",
		"--size", "268435456", "--tmpfs", "/dev/shm",
		"--size", "268435456", "--tmpfs", "/tmp",
		"--remount-ro", "/",
		"/bin/true",
	}
	if !slices.Equal(probe.argv, want) {
		t.Fatalf("probe argv mismatch:\n got %q\nwant %q", probe.argv, want)
	}
}

func TestBwrapConstructorPropagatesProbeFailure(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	sentinel := errors.New("userns blocked by host policy")
	probe := &recordingProbe{t: t, err: sentinel}
	_, err := newBwrapExecBackendAt(good, good, probe.probe, SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if !errors.Is(err, sentinel) {
		t.Fatalf("probe failure not propagated: %v", err)
	}
}

func TestBwrapConstructorBuildsUnixSeams(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	probe := &recordingProbe{t: t}
	backend, err := newBwrapExecBackendAt(good, good, probe.probe,
		SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 128})
	if err != nil {
		t.Fatal(err)
	}
	b, ok := backend.(*bwrapBackend)
	if !ok {
		t.Fatalf("backend = %T, want *bwrapBackend", backend)
	}
	if _, ok := b.runner.(unixRunner); !ok {
		t.Fatalf("runner = %T, want unixRunner", b.runner)
	}
	if _, ok := b.starter.(unixStarter); !ok {
		t.Fatalf("starter = %T, want unixStarter", b.starter)
	}
	if b.capBytes != 134217728 {
		t.Fatalf("capBytes = %d, want 134217728", b.capBytes)
	}
	if b.collect == nil {
		t.Fatal("collector missing")
	}
}

func TestProbeBwrapPassesEmptyEnv(t *testing.T) {
	// The shell self-sets PWD/SHLVL, so "env is empty" cannot be asserted
	// directly; a parent sentinel is the discriminating signal. With a nil
	// (inherited) cmd.Env the sentinel would be visible and the script exits
	// non-zero — exactly the mutation this test exists to catch.
	t.Setenv("GO_LLM_PROBE_SENTINEL", "leaked")
	dir := t.TempDir()
	script := filepath.Join(dir, "envcheck")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ -z \"$GO_LLM_PROBE_SENTINEL\" ]\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := probeBwrap(context.Background(), []string{script}); err != nil {
		t.Fatalf("probe leaked parent environment into the TCB chain: %v", err)
	}
}

func TestProbeBwrapCapturesFailureDetail(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "failer")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'no permissions to create namespace' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := probeBwrap(context.Background(), []string{script})
	if err == nil || !strings.Contains(err.Error(), "no permissions to create namespace") {
		t.Fatalf("probe error must carry captured detail: %v", err)
	}
}

func TestCollectBwrapLayout(t *testing.T) {
	build := func(t *testing.T, setup func(root string)) (string, []string) {
		t.Helper()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		setup(root)
		covered, err := filepath.EvalSymlinks(filepath.Join(root, "usr", "bin"))
		if err != nil {
			t.Fatal(err)
		}
		return root, []string{covered}
	}

	t.Run("usr-merged symlink", func(t *testing.T) {
		root, covered := build(t, func(root string) {
			if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
				t.Fatal(err)
			}
		})
		dirs, links, err := collectBwrapLayout(root, covered)
		if err != nil {
			t.Fatal(err)
		}
		if len(dirs) != 0 || len(links) != 1 || links["/bin"] != "usr/bin" {
			t.Fatalf("dirs=%q links=%q", dirs, links)
		}
	})

	t.Run("split-layout directory", func(t *testing.T) {
		root, covered := build(t, func(root string) {
			if err := os.Mkdir(filepath.Join(root, "lib"), 0o755); err != nil {
				t.Fatal(err)
			}
		})
		dirs, links, err := collectBwrapLayout(root, covered)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(dirs, []string{"/lib"}) || len(links) != 0 {
			t.Fatalf("dirs=%q links=%q", dirs, links)
		}
	})

	t.Run("missing entries omitted", func(t *testing.T) {
		root, covered := build(t, func(string) {})
		dirs, links, err := collectBwrapLayout(root, covered)
		if err != nil || len(dirs) != 0 || len(links) != 0 {
			t.Fatalf("dirs=%q links=%q err=%v", dirs, links, err)
		}
	})

	t.Run("absolute link target fails closed", func(t *testing.T) {
		root, covered := build(t, func(root string) {
			if err := os.Symlink("/home", filepath.Join(root, "sbin")); err != nil {
				t.Fatal(err)
			}
		})
		if _, _, err := collectBwrapLayout(root, covered); err == nil ||
			!strings.Contains(err.Error(), "non-relative") {
			t.Fatalf("hostile absolute link accepted: %v", err)
		}
	})

	t.Run("out-of-policy link target fails closed", func(t *testing.T) {
		root, covered := build(t, func(root string) {
			if err := os.Mkdir(filepath.Join(root, "usr", "evil"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("usr/evil", filepath.Join(root, "bin")); err != nil {
				t.Fatal(err)
			}
		})
		if _, _, err := collectBwrapLayout(root, covered); err == nil ||
			!strings.Contains(err.Error(), "outside the reviewed") {
			t.Fatalf("out-of-policy link accepted: %v", err)
		}
	})

	t.Run("regular file fails closed", func(t *testing.T) {
		root, covered := build(t, func(root string) {
			if err := os.WriteFile(filepath.Join(root, "lib64"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if _, _, err := collectBwrapLayout(root, covered); err == nil ||
			!strings.Contains(err.Error(), "neither a directory nor a symlink") {
			t.Fatalf("regular-file layout entry accepted: %v", err)
		}
	})
}

// TestNewExecBackendBwrapNeverHost pins the dispatch contract on Linux: the
// bwrap runtime either constructs a real bwrap backend or fails — it never
// silently returns host execution (e.g. inside Docker where the probe fails).
func TestNewExecBackendBwrapNeverHost(t *testing.T) {
	got, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		if got.execBackend != nil {
			t.Fatalf("failed construction still returned backend %T", got.execBackend)
		}
		return
	}
	if _, ok := got.execBackend.(*bwrapBackend); !ok {
		t.Fatalf("backend = %T, want *bwrapBackend", got.execBackend)
	}
}

// --- Task 5: prepare/Run/Start wiring ---

type captureRunner struct {
	called int
	spec   execSpec
	res    execResult
	err    error
}

func (c *captureRunner) Run(_ context.Context, spec execSpec) (execResult, error) {
	c.called++
	c.spec = spec
	return c.res, c.err
}

type captureStarter struct {
	called int
	spec   execSpec
	proc   backgroundProcess
	err    error
}

func (c *captureStarter) Start(spec execSpec, _, _ io.Writer) (backgroundProcess, error) {
	c.called++
	c.spec = spec
	if c.err != nil {
		return nil, c.err
	}
	return c.proc, nil
}

type fakeProcess struct{}

func (fakeProcess) PID() int                 { return 1 }
func (fakeProcess) Wait() (int, bool, error) { return 0, false, nil }
func (fakeProcess) Kill() error              { return nil }

// testBwrapBackend builds a backend around capture seams and a fixed fake
// policy collection, bypassing the constructor probe.
func testBwrapBackend(runner commandRunner, starter backgroundStarter, cfg SandboxConfig, capBytes int64) *bwrapBackend {
	return &bwrapBackend{
		bwrapPath:   "/fake/bwrap",
		prlimitPath: "/fake/prlimit",
		cfg:         cfg,
		capBytes:    capBytes,
		runner:      runner,
		starter:     starter,
		collect: func(string) ([]string, []string, map[string]string, error) {
			return []string{"/usr/bin", "/usr/lib"}, []string{"/lib"},
				map[string]string{"/bin": "usr/bin"}, nil
		},
	}
}

func bwrapTestSpec(t *testing.T, ws string) execSpec {
	t.Helper()
	return execSpec{
		Path:          "/usr/bin/true",
		Argv:          []string{"true", "arg1"},
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/ambient"},
		WorkspaceRoot: ws,
	}
}

// wsTempDir returns a canonical workspace under /tmp (EvalSymlinks so the
// posixCleanAbs checks compare canonical spellings).
func wsTempDir(t *testing.T) string {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestBwrapPrepareWrapsRunSpec(t *testing.T) {
	ws := wsTempDir(t)
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
		t.Fatal(err)
	}
	if runner.called != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.called)
	}
	got := runner.spec
	if got.Path != "/fake/bwrap" {
		t.Fatalf("Path = %q, want the bwrap binary", got.Path)
	}
	if len(got.Argv) < 3 || got.Argv[0] != "bwrap" {
		t.Fatalf("argv[0] = %q, want bwrap", got.Argv)
	}
	// The payload command is the exact suffix; interior policy ordering is
	// pinned literally by the Task 3 builder tests.
	if !slices.Equal(got.Argv[len(got.Argv)-2:], []string{"/usr/bin/true", "arg1"}) {
		t.Fatalf("payload suffix = %q", got.Argv[len(got.Argv)-2:])
	}
	joined := " " + strings.Join(got.Argv, " ") + " "
	for _, must := range []string{
		" --bind " + ws + " " + ws + " ",
		" --chdir " + ws + " ",
		" --setenv TMPDIR /tmp ",
		" --ro-bind /usr/bin/true /usr/bin/true ",
		" --remount-ro / /usr/bin/true ",
	} {
		if !strings.Contains(joined, must) {
			t.Fatalf("wrapped argv missing %q:\n%q", must, got.Argv)
		}
	}
	if strings.Contains(joined, "/ambient") {
		t.Fatalf("ambient TMPDIR leaked into policy: %q", got.Argv)
	}
	if got.Env == nil || len(got.Env) != 0 {
		t.Fatalf("outer env = %#v, want non-nil empty slice", got.Env)
	}
	if got.Dir != ws {
		t.Fatalf("Dir = %q, want %q", got.Dir, ws)
	}
}

func TestBwrapPrepareCapUsesPrlimitChain(t *testing.T) {
	ws := wsTempDir(t)
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}},
		SandboxConfig{Runtime: SandboxRuntimeBwrap, MemoryCapMB: 256}, 268435456)
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
		t.Fatal(err)
	}
	got := runner.spec
	if got.Path != "/fake/prlimit" {
		t.Fatalf("Path = %q, want the prlimit binary", got.Path)
	}
	if !slices.Equal(got.Argv[:3], []string{"prlimit", "--as=268435456", "/fake/bwrap"}) {
		t.Fatalf("chain prefix = %q", got.Argv[:3])
	}
	joined := " " + strings.Join(got.Argv, " ") + " "
	if c := strings.Count(joined, " --size 268435456 --tmpfs "); c != 2 {
		t.Fatalf("tmpfs quota count = %d, want 2 (%q)", c, got.Argv)
	}
}

func TestBwrapPrepareRejectsInvalidSpecs(t *testing.T) {
	ws := wsTempDir(t)
	cases := map[string]func(*execSpec){
		"empty argv":      func(s *execSpec) { s.Argv = nil },
		"empty path":      func(s *execSpec) { s.Path = "" },
		"empty dir":       func(s *execSpec) { s.Dir = "" },
		"empty workspace": func(s *execSpec) { s.WorkspaceRoot = "" },
		"non-canonical workspace": func(s *execSpec) {
			s.WorkspaceRoot = ws + "/./x"
			s.Dir = s.WorkspaceRoot
		},
		"broad workspace":       func(s *execSpec) { s.WorkspaceRoot = "/tmp"; s.Dir = "/tmp" },
		"dir outside workspace": func(s *execSpec) { s.Dir = "/somewhere/else" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &captureRunner{}
			starter := &captureStarter{proc: fakeProcess{}}
			b := testBwrapBackend(runner, starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			spec := bwrapTestSpec(t, ws)
			mutate(&spec)
			if _, err := b.Run(context.Background(), spec); err == nil {
				t.Fatal("Run accepted invalid spec")
			}
			if proc, err := b.Start(spec, io.Discard, io.Discard); err == nil {
				if proc != nil {
					_, _, _ = proc.Wait()
				}
				t.Fatal("Start accepted invalid spec")
			}
			if runner.called+starter.called != 0 {
				t.Fatalf("delegate calls = %d, want 0", runner.called+starter.called)
			}
		})
	}
}

func TestBwrapPrepareRejectsExternallyLinkedWorkspace(t *testing.T) {
	ws := wsTempDir(t)
	outside := filepath.Join(t.TempDir(), "outside-canary")
	if err := os.WriteFile(outside, []byte("safe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(ws, "pre-linked")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err == nil ||
		!strings.Contains(err.Error(), "linked outside the workspace") {
		t.Fatalf("externally linked workspace accepted: %v", err)
	}
	if runner.called != 0 {
		t.Fatalf("runner ran despite link violation")
	}
}

func TestBwrapStartRoutesThroughSameWrapping(t *testing.T) {
	ws := wsTempDir(t)
	runner := &captureRunner{}
	starter := &captureStarter{proc: fakeProcess{}}
	b := testBwrapBackend(runner, starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	spec := bwrapTestSpec(t, ws)
	if _, err := b.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	proc, err := b.Start(spec, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := proc.Wait(); err != nil {
		t.Fatal(err)
	}
	if starter.called != 1 {
		t.Fatalf("starter calls = %d, want 1", starter.called)
	}
	if starter.spec.Path != runner.spec.Path || !slices.Equal(starter.spec.Argv, runner.spec.Argv) ||
		!slices.Equal(starter.spec.Env, runner.spec.Env) {
		t.Fatalf("Start wrapping diverges from Run:\n run   %q\n start %q", runner.spec.Argv, starter.spec.Argv)
	}
}

func TestBwrapStartPropagatesSpawnError(t *testing.T) {
	ws := wsTempDir(t)
	sentinel := errors.New("spawn refused")
	starter := &captureStarter{err: sentinel}
	b := testBwrapBackend(&captureRunner{}, starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard); !errors.Is(err, sentinel) {
		t.Fatalf("spawn error not propagated: %v", err)
	}
}

func TestExistingPolicyPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(dir, "linked")
	if err := os.Symlink(file, linked); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")

	got, err := existingPolicyPaths([]string{file, sub, linked, missing})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{file, sub, linked}) {
		t.Fatalf("kept = %q", got)
	}

	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err == nil {
		if _, err := existingPolicyPaths([]string{filepath.Join(dir, "fifo")}); err == nil {
			t.Fatal("fifo accepted as policy path")
		}
	}

	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(locked, "x")
	if err := os.WriteFile(inside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if os.Getuid() != 0 {
		if _, err := existingPolicyPaths([]string{inside}); err == nil ||
			!strings.Contains(err.Error(), inside) {
			t.Fatalf("permission failure must fail closed with the path: %v", err)
		}
	}
}

// Not parallel: FD flags are process-global state.
func TestBwrapPrepareRejectsInheritableFDs(t *testing.T) {
	ws := wsTempDir(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)

	// Control: Go pipes are CLOEXEC by default, prepare accepts them.
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
		t.Fatalf("CLOEXEC descriptors rejected: %v", err)
	}

	// Go pipe fds are created CLOEXEC; clear the flag explicitly to simulate
	// an inheritable host descriptor, and restore it afterwards.
	fd := int(w.Fd())
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); errno != 0 {
		t.Fatal(errno)
	}
	defer func() {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC)
	}()
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err == nil ||
		!strings.Contains(err.Error(), "inheritable host fd") {
		t.Fatalf("inheritable fd accepted: %v", err)
	}
	if runner.called != 1 {
		t.Fatalf("runner calls = %d, want 1 (control only)", runner.called)
	}
}
