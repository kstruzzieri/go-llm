//go:build linux

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/kstruzzieri/go-llm/internal/secretscan/secrettest"
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
	configs := map[string]SandboxConfig{
		"cpu limit": {Runtime: SandboxRuntimeBwrap, CPULimit: 0.5},
	}
	if strconv.IntSize == 64 {
		overflowingCap := int64(1) << 45
		configs["overflowing cap"] = SandboxConfig{
			Runtime: SandboxRuntimeBwrap, MemoryCapMB: int(overflowingCap),
		}
	}
	for name, cfg := range configs {
		if _, err := newBwrapExecBackendAt(bwrap, bwrap, probe.probe, cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if probe.called != 0 {
		t.Fatalf("probe ran %d times for unsupported configs", probe.called)
	}
}

// The audit also runs at construction, before the probe (bwrap's first
// spawn), so ValidateSandbox fails where every later spawn would (#576 I2).
// Not parallel: FD flags are process-global state.
func TestBwrapConstructorRefusesInheritableFD(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	cfg := SandboxConfig{Runtime: SandboxRuntimeBwrap}
	fd, err := syscall.Open("/dev/null", syscall.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0); errno != 0 || flags&syscall.FD_CLOEXEC != 0 {
		t.Fatalf("precondition: fd %d flags=%d errno=%v, want it inheritable", fd, flags, errno)
	}
	probe := &recordingProbe{t: t}
	_, err = newBwrapExecBackendAt(good, good, probe.probe, cfg)
	want := fmt.Sprintf("tools: bwrap refuses to spawn with inheritable host fd %d (missing FD_CLOEXEC)", fd)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("constructor with inheritable fd %d = %v, want %q", fd, err, want)
	}
	if probe.called != 0 {
		t.Fatalf("probe ran %d times with an inheritable fd, want 0", probe.called)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		t.Fatal(errno)
	}
	if _, err := newBwrapExecBackendAt(good, good, probe.probe, cfg); err != nil || probe.called != 1 {
		t.Fatalf("control constructor = %v with %d probe(s), want nil and 1", err, probe.called)
	}
}

func TestBwrapConstructorChecksBinariesBeforeProbe(t *testing.T) {
	good := writeFakeBinary(t, 0o755)
	missing := filepath.Join(t.TempDir(), "missing")
	nonExec := writeFakeBinary(t, 0o644)
	setuid := writeFakeBinary(t, 0o755|os.ModeSetuid)
	setgid := writeFakeBinary(t, 0o755|os.ModeSetgid)
	groupWritable := writeFakeBinary(t, 0o775)
	otherWritable := writeFakeBinary(t, 0o757)
	worldWritable := writeFakeBinary(t, 0o777)
	symlinked := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, symlinked); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":        missing,
		"non-executable": nonExec,
		"setuid":         setuid,
		"setgid":         setgid,
		"group-writable": groupWritable,
		"other-writable": otherWritable,
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
		"--remount-ro", "/dev",
		"--remount-ro", "/",
		"--argv0", "/bin/true", "/bin/true",
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
		"--remount-ro", "/dev",
		"--remount-ro", "/",
		"--argv0", "/bin/true", "/bin/true",
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

func TestCollectSystemRootMountsRetainsAliasDestination(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", alias); err != nil {
		t.Fatal(err)
	}
	canonTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}

	roots, aliases, err := collectSystemRootMounts(
		[]string{alias}, "/workspace", "/home/user", bwrapBroadRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(roots, []string{canonTarget}) {
		t.Fatalf("collectSystemRootMounts(alias) roots = %q, want %q", roots, []string{canonTarget})
	}
	if len(aliases) != 1 || aliases[alias] != canonTarget {
		t.Fatalf("collectSystemRootMounts(alias) aliases = %q, want {%q: %q}", aliases, alias, canonTarget)
	}
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
	during func()
	called int
	spec   execSpec
	proc   backgroundProcess
	err    error
}

// Start records the prepared command and returns the configured process or error.
func (c *captureStarter) Start(spec execSpec, _, _ io.Writer) (backgroundProcess, error) {
	c.called++
	c.spec = spec
	if c.during != nil {
		c.during()
	}
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
		collect: func(string) ([]string, map[string]string, []string, map[string]string, error) {
			return []string{"/usr/bin", "/usr/lib"}, nil, []string{"/lib"},
				map[string]string{"/bin": "usr/bin"}, nil
		},
	}
}

func bwrapTestSpec(t *testing.T, ws string) execSpec {
	t.Helper()
	return execSpec{
		Path:          "/bin/sh",
		Argv:          []string{"sh", "arg1"},
		Dir:           ws,
		Env:           []string{"PATH=/usr/bin", "HOME=/home/u", "TMPDIR=/ambient"},
		WorkspaceRoot: ws,
		ExeIdentity:   execIdentityOf(t, "/bin/sh"),
		Screen:        testScreen(),
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
	spec := bwrapTestSpec(t, ws)
	wantExe, err := filepath.EvalSymlinks(spec.Path)
	if err != nil {
		t.Fatal(err)
	}
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Run(context.Background(), spec); err != nil {
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
	// The canonical payload command is the exact suffix; interior policy ordering is
	// pinned literally by the Task 3 builder tests.
	if !slices.Equal(got.Argv[len(got.Argv)-2:], []string{wantExe, "arg1"}) {
		t.Fatalf("payload suffix = %q", got.Argv[len(got.Argv)-2:])
	}
	joined := " " + strings.Join(got.Argv, " ") + " "
	for _, must := range []string{
		" --bind " + ws + " " + ws + " ",
		" --chdir " + ws + " ",
		" --setenv TMPDIR /tmp ",
		" --remount-ro / --argv0 " + spec.Path + " " + wantExe + " ",
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
	// /bin/sh canonicalizes under a covered read-only region, so no redundant
	// executable literal may appear (it would fail mountpoint creation inside
	// the RO bind).
	if strings.Contains(joined, " --ro-bind /bin/sh ") {
		t.Fatalf("covered executable still literal-bound: %q", got.Argv)
	}
}

// TestBwrapPrepareMountsGitLayout: prepare hands every class of the
// discovered git layout to the argv (a .git dir and a submodule git dir
// writable; a container, an intermediate, metadata, a modules-tree hooks dir
// and a gitfile read-only), so a dropped gitWritable or gitReadOnly entry
// fails without bwrap.
func TestBwrapPrepareMountsGitLayout(t *testing.T) {
	ws := wsTempDir(t)
	writeGitFixture(t, ws)
	applyGitMetaVariant(t, ws, "stripped") // .git/modules/s/hooks: moduleReadOnly
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
		t.Fatal(err)
	}
	// containment: GITMETA-BWRAP-PREPARE
	joined := " " + strings.Join(runner.spec.Argv, " ") + " "
	for _, m := range [][2]string{
		{"--bind", "/.git"}, {"--bind", "/.git/modules/m"},
		{"--ro-bind", "/.git/modules"}, {"--ro-bind", "/.git/modules/libs"},
		{"--ro-bind", "/.git/hooks"}, {"--ro-bind", "/.git/modules/s/hooks"}, {"--ro-bind", "/m/.git"},
	} {
		if must := " " + m[0] + " " + ws + m[1] + " " + ws + m[1] + " "; !strings.Contains(joined, must) {
			t.Errorf("wrapped argv missing %q", must)
		}
	}
}

// TestBwrapPrepareWorkspaceMountsPinned pins, byte for byte, the workspace
// segment of prepare's argv (the workspace bind through --chdir) for the git
// fixture plus a .git symlink, a bare repository and a HEAD-and-commondir
// layout. The walk records those three for the post-run check; none of them
// may add, drop or reorder a mount.
func TestBwrapPrepareWorkspaceMountsPinned(t *testing.T) {
	ws := wsTempDir(t)
	writeGitFixture(t, ws)
	applyGitMetaVariant(t, ws, "stripped")
	for _, d := range []string{"link", "bare.git/objects", "bare.git/refs/heads", "wtadmin"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../nested/.git", filepath.Join(ws, "link", ".git")); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"bare.git/HEAD":     "ref: refs/heads/main\n",
		"wtadmin/HEAD":      "0123456789012345678901234567890123456789\n",
		"wtadmin/commondir": "../.git\n",
	} {
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
		t.Fatal(err)
	}
	argv := runner.spec.Argv
	start := -1
	for i := 0; i+2 < len(argv) && start < 0; i++ {
		if argv[i] == "--bind" && argv[i+1] == ws && argv[i+2] == ws {
			start = i
		}
	}
	end := slices.Index(argv, "--chdir")
	if start < 0 || end < start {
		t.Fatalf("no workspace bind before --chdir in %q", argv)
	}
	want := []string{"--bind", "$WS", "$WS"}
	for _, m := range []string{
		"--bind .git", "--ro-bind .git/config", "--ro-bind .git/hooks", "--ro-bind .git/info",
		"--ro-bind .git/modules", "--ro-bind .git/modules/libs", "--bind .git/modules/libs/a",
		"--ro-bind .git/modules/libs/a/config", "--ro-bind .git/modules/libs/a/hooks",
		"--bind .git/modules/m", "--ro-bind .git/modules/m/config", "--ro-bind .git/modules/m/hooks",
		"--ro-bind .git/modules/m/info", "--ro-bind .git/modules/m/modules", "--bind .git/modules/m/modules/inner",
		"--ro-bind .git/modules/m/modules/inner/config", "--ro-bind .git/modules/m/modules/inner/hooks",
		"--ro-bind .git/modules/s", "--ro-bind .git/modules/s/hooks", "--ro-bind .git/modules/s/info",
		"--ro-bind .git/modules/s/objects", "--ro-bind .git/modules/s/refs",
		"--ro-bind .git/worktrees", "--bind .git/worktrees/wt",
		"--ro-bind .git/worktrees/wt/commondir", "--ro-bind .git/worktrees/wt/gitdir",
		"--ro-bind libs/a/.git", "--ro-bind m/.git",
		"--bind nested/.git", "--ro-bind nested/.git/config", "--ro-bind nested/.git/hooks",
		"--ro-bind wt/.git",
	} {
		flag, rel, _ := strings.Cut(m, " ")
		want = append(want, flag, "$WS/"+rel, "$WS/"+rel)
	}
	got := make([]string, 0, end-start)
	for _, a := range argv[start:end] {
		got = append(got, strings.ReplaceAll(a, ws, "$WS"))
	}
	if !slices.Equal(got, want) {
		t.Errorf("workspace argv segment changed:\n%s", unifiedDiff("argv",
			[]byte(strings.Join(want, "\n")+"\n"), []byte(strings.Join(got, "\n")+"\n"), true))
	}
	t.Run("rebase-merge", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(ws, ".git", "rebase-merge"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Run(t.Context(), bwrapTestSpec(t, ws)); err != nil {
			t.Fatal(err)
		}
		if got := runner.spec.Argv; !slices.Equal(got, argv) {
			t.Errorf("bwrap argv with rebase-merge:\n%s", unifiedDiff("argv",
				[]byte(strings.Join(argv, "\n")+"\n"), []byte(strings.Join(got, "\n")+"\n"), true))
		}
	})
}

// TestBwrapPrepareUnchangedByLookups: with git's names resolving as on a
// casefold directory, a repository spelled .g<U+200D>it and a bare layout
// with H<U+200D>EAD from before the run join only the post-run check's
// baseline: the workspace argv is the one prepare builds without the lookups,
// and the odd repository's symlinked config (which a mounted git dir's would
// refuse) refuses nothing.
func TestBwrapPrepareUnchangedByLookups(t *testing.T) {
	// containment: GITMETA-POSTRUN-HOLD
	ws := wsTempDir(t)
	writeGitFixture(t, ws)
	runIn(t, ws, `mkdir -p "src/.g`+zwj+`it/objects" "src/.g`+zwj+`it/refs" "src/.g`+zwj+`it/hooks" `+
		`"X/ob`+zwj+`jects" X/refs && `+validHEADBody+` > "src/.g`+zwj+`it/HEAD" && echo x > src/cfg && `+
		`ln -s ../cfg "src/.g`+zwj+`it/config" && `+validHEADBody+` > "X/H`+zwj+`EAD"`)
	segment := func() []string {
		t.Helper()
		runner := &captureRunner{}
		b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
		if _, err := b.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
			t.Fatalf("prepare refused: %v", err)
		}
		argv := runner.spec.Argv
		start := slices.Index(argv, ws) - 1
		end := slices.Index(argv, "--chdir")
		if start < 0 || end < start {
			t.Fatalf("no workspace bind before --chdir in %q", argv)
		}
		return slices.Clone(argv[start:end])
	}
	want := segment()
	lookup, probe := gitLookupPath, openDirProbe
	gitLookupPath = casefoldLookup
	openDirProbe = (&fakeFS{typ: unix.EXT4_SUPER_MAGIC, flags: fsCasefoldFL}).open
	t.Cleanup(func() { gitLookupPath, openDirProbe = lookup, probe })
	if got := segment(); !slices.Equal(got, want) {
		t.Errorf("the lookups changed the workspace argv:\n%s", unifiedDiff("argv",
			[]byte(strings.Join(want, "\n")+"\n"), []byte(strings.Join(got, "\n")+"\n"), true))
	}
}

// TestBwrapPrepareRefusesSymlinkedGitMetadata: a symlinked metadata entry
// fails prepare (R-13) before anything runs, whether its target exists or
// not.
func TestBwrapPrepareRefusesSymlinkedGitMetadata(t *testing.T) {
	for _, c := range []struct{ name, rel, target string }{
		{"in-tree hooks", ".git/hooks", "../scripts/hooks"},
		{"dangling submodule config", ".git/modules/m/config", "../../../nowhere"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := wsTempDir(t)
			writeGitFixture(t, ws)
			p := filepath.Join(ws, c.rel)
			if err := os.MkdirAll(filepath.Join(ws, "scripts"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(p, filepath.Join(ws, "scripts", filepath.Base(p))); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(c.target, p); err != nil {
				t.Fatal(err)
			}
			runner := &captureRunner{}
			b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			_, err := b.Run(context.Background(), bwrapTestSpec(t, ws))
			// containment: GITMETA-BWRAP-PREPARE
			want := `tools: bwrap cannot protect git metadata "` + c.rel +
				`": it is a symbolic link; replace the link with the file or directory itself (for hooks, core.hooksPath keeps them in the working tree)`
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if runner.called != 0 {
				t.Fatalf("runner called %d times after a refused prepare", runner.called)
			}
		})
	}
}

func TestBwrapPrepareBindsWorkspaceResidentExecutable(t *testing.T) {
	ws := wsTempDir(t)
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	spec := bwrapTestSpec(t, ws)
	exe := filepath.Join(ws, "tool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.Path = exe
	spec.ExeIdentity = execIdentityOf(t, exe)
	if _, err := b.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(runner.spec.Argv, " ") + " "
	if !strings.Contains(joined, " --ro-bind "+exe+" "+exe+" ") {
		t.Fatalf("workspace-resident executable lost its read-only literal: %q", runner.spec.Argv)
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
	const (
		emptyField   = "requires a non-empty argv, executable path, and working directory"
		badWorkspace = "must be a canonical non-root path"
	)
	// Each case pins its own check: "dir outside workspace" is caught by the
	// argv builder, after the #553 identity check, so a bare err != nil would
	// pass on an identity refusal instead.
	cases := map[string]struct {
		mutate func(*execSpec)
		want   string
	}{
		"empty argv":      {func(s *execSpec) { s.Argv = nil }, emptyField},
		"empty path":      {func(s *execSpec) { s.Path = "" }, emptyField},
		"empty dir":       {func(s *execSpec) { s.Dir = "" }, emptyField},
		"empty workspace": {func(s *execSpec) { s.WorkspaceRoot = "" }, "requires a workspace root"},
		"non-canonical workspace": {func(s *execSpec) {
			s.WorkspaceRoot = ws + "/./x"
			s.Dir = s.WorkspaceRoot
		}, badWorkspace},
		"broad workspace":       {func(s *execSpec) { s.WorkspaceRoot = "/tmp"; s.Dir = "/tmp" }, badWorkspace},
		"dir outside workspace": {func(s *execSpec) { s.Dir = "/somewhere/else" }, "must be a canonical path inside workspace"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &captureRunner{}
			starter := &captureStarter{proc: fakeProcess{}}
			b := testBwrapBackend(runner, starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			spec := bwrapTestSpec(t, ws)
			tc.mutate(&spec)
			if _, err := b.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run = %v; want %q", err, tc.want)
			}
			if proc, err := b.Start(spec, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
				if proc != nil {
					_, _, _ = proc.Wait()
				}
				t.Fatalf("Start = %v; want %q", err, tc.want)
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

func TestSafeEtcPolicyPaths(t *testing.T) {
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	home := filepath.Join(dir, "home")
	for _, d := range []string{ws, home, filepath.Join(dir, "sub")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The systemd shape: /etc/resolv.conf -> /run/systemd/resolve/... . A
	// canonical target outside /etc is legitimate and must stay accepted.
	benignLink := filepath.Join(dir, "benign-link")
	if err := os.Symlink(file, benignLink); err != nil {
		t.Fatal(err)
	}
	toRoot := filepath.Join(dir, "to-root")
	if err := os.Symlink("/", toRoot); err != nil {
		t.Fatal(err)
	}
	toWorkspace := filepath.Join(dir, "to-ws")
	if err := os.Symlink(ws, toWorkspace); err != nil {
		t.Fatal(err)
	}
	toWorkspaceParent := filepath.Join(dir, "to-ws-parent")
	if err := os.Symlink(dir, toWorkspaceParent); err != nil {
		t.Fatal(err)
	}
	toHome := filepath.Join(dir, "to-home")
	if err := os.Symlink(home, toHome); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}

	kept, err := safeEtcPolicyPaths(
		[]string{file, filepath.Join(dir, "sub"), benignLink, filepath.Join(dir, "missing"), dangling}, ws, home)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(kept, []string{file, filepath.Join(dir, "sub"), benignLink}) {
		t.Fatalf("kept = %q", kept)
	}

	rejects := map[string]struct {
		path string
		frag string
	}{
		"symlink to volume root":  {toRoot, "broad root"},
		"symlink to workspace":    {toWorkspace, "covers the workspace"},
		"symlink above workspace": {toWorkspaceParent, "covers the workspace"},
		"symlink to home":         {toHome, "covers the workspace or home"},
	}
	for name, tc := range rejects {
		t.Run(name, func(t *testing.T) {
			if _, err := safeEtcPolicyPaths([]string{tc.path}, ws, home); err == nil {
				t.Fatalf("accepted aliased policy path %q", tc.path)
			} else if !strings.Contains(err.Error(), tc.frag) || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("error %q must name the path and mention %q", err, tc.frag)
			}
		})
	}

	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err == nil {
		if _, err := safeEtcPolicyPaths([]string{fifo}, ws, home); err == nil {
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
		if _, err := safeEtcPolicyPaths([]string{inside}, ws, home); err == nil ||
			!strings.Contains(err.Error(), inside) {
			t.Fatalf("permission failure must fail closed with the path: %v", err)
		}
	}
}

// TestDefaultBwrapCollect covers the PRODUCTION collector directly. The
// constructor test can only assert it is non-nil, so dropping a guard here
// (e.g. passing an empty home) is otherwise invisible to the whole suite.
func TestDefaultBwrapCollect(t *testing.T) {
	ws := wsTempDir(t)
	roots, aliases, layoutDirs, links, err := defaultBwrapCollect(ws)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) == 0 {
		t.Fatal("no reviewed system roots collected on this host")
	}
	home := canonicalHome()
	for _, r := range roots {
		if !posixCleanAbs(r) {
			t.Errorf("root %q is not canonical", r)
		}
		if pathCovers(r, ws) {
			t.Errorf("root %q covers the workspace %q", r, ws)
		}
		if home != "" && pathCovers(r, home) {
			t.Errorf("root %q covers home %q", r, home)
		}
	}
	for dest, source := range aliases {
		if !slices.Contains(bwrapDefaultSystemRoots, dest) {
			t.Errorf("system root alias %q outside the reviewed set", dest)
		}
		if !slices.Contains(roots, source) {
			t.Errorf("system root alias %q references uncollected source %q", dest, source)
		}
	}
	for _, d := range layoutDirs {
		if !slices.Contains(bwrapLayoutDirs, d) {
			t.Errorf("layout dir %q outside the fixed set", d)
		}
	}
	for dest, target := range links {
		if !slices.Contains(bwrapLayoutDirs, dest) {
			t.Errorf("layout link %q outside the fixed set", dest)
		}
		if strings.HasPrefix(target, "/") {
			t.Errorf("layout link %q has absolute target %q", dest, target)
		}
	}
}

// TestDefaultBwrapCollectRejectsRootAliasedIntoHome pins the home guard that a
// mutation proved invisible: with the guard dropped, a system root aliased
// into $HOME silently enters the read-only policy.
func TestDefaultBwrapCollectRejectsRootAliasedIntoHome(t *testing.T) {
	home := canonicalHome()
	if home == "" {
		t.Skip("no resolvable home on this host")
	}
	// collectSystemRoots is the unit the guard lives in; drive it with a root
	// that canonicalizes above home, which is exactly the aliasing shape.
	if _, err := collectSystemRoots([]string{home}, "/w", home, bwrapBroadRoot); err == nil {
		t.Fatal("a system root equal to home was accepted")
	}
	parent := filepath.Dir(home)
	if parent == home || parent == "/" {
		return
	}
	if _, err := collectSystemRoots([]string{parent}, "/w", home, bwrapBroadRoot); err == nil {
		t.Fatalf("a system root above home (%q) was accepted", parent)
	}
}

func TestCollectBwrapLayoutOmitsDanglingLink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Debian/Ubuntu ship /lib32 -> usr/lib32 from base-files while the target
	// belongs to separate 32-bit packages; purging those leaves this shape.
	if err := os.Symlink("usr/lib32", filepath.Join(root, "lib32")); err != nil {
		t.Fatal(err)
	}
	covered, err := filepath.EvalSymlinks(filepath.Join(root, "usr", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	dirs, links, err := collectBwrapLayout(root, []string{covered})
	if err != nil {
		t.Fatalf("dangling compatibility link must be omitted, not fatal: %v", err)
	}
	if len(dirs) != 0 || len(links) != 0 {
		t.Fatalf("dangling link entered the policy: dirs=%q links=%q", dirs, links)
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

// TestBwrapPrepareBindsAndExecutesCanonicalExecutableTarget is the
// discriminator for executable canonicalization. The checked object must be
// both the bind source and the executed path; resolving an approved spelling
// again after bwrap changes namespaces or cwd can select a different object.
func TestBwrapPrepareBindsAndExecutesCanonicalExecutableTarget(t *testing.T) {
	ws := wsTempDir(t)
	outside := filepath.Join(t.TempDir(), "real-tool")
	if err := os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	canonOutside, err := filepath.EvalSymlinks(outside)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	spec := bwrapTestSpec(t, ws)
	spec.Path = link
	spec.ExeIdentity = execIdentityOf(t, link)
	if _, err := b.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	joined := " " + strings.Join(runner.spec.Argv, " ") + " "
	if !strings.Contains(joined, " --ro-bind "+canonOutside+" "+canonOutside+" ") {
		t.Fatalf("executable bind source is not the canonical target: %q", runner.spec.Argv)
	}
	if strings.Contains(joined, " --ro-bind "+link+" "+link+" ") {
		t.Fatalf("executable bound by its symlink spelling: %q", runner.spec.Argv)
	}
	if !strings.Contains(joined, " --argv0 "+link+" "+canonOutside+" ") {
		t.Fatalf("canonical command does not preserve approved argv[0]: %q", runner.spec.Argv)
	}
	if runner.spec.Argv[len(runner.spec.Argv)-2] != canonOutside {
		t.Fatalf("payload path is not the canonical target: %q", runner.spec.Argv)
	}
}

// TestBwrapPrepareRejectsNonRegularExecutable pins the companion guard: the
// canonical target must be a regular file, so a symlink retargeted at a
// directory cannot become a recursive read-only mount of that subtree.
func TestBwrapPrepareRejectsNonRegularExecutable(t *testing.T) {
	ws := wsTempDir(t)
	dir := t.TempDir()
	link := filepath.Join(ws, "dirlink")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	runner := &captureRunner{}
	b := testBwrapBackend(runner, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	spec := bwrapTestSpec(t, ws)
	spec.Path = link
	if _, err := b.Run(context.Background(), spec); err == nil ||
		!strings.Contains(err.Error(), "non-regular file") {
		t.Fatalf("directory-targeted executable accepted: %v", err)
	}
	if runner.called != 0 {
		t.Fatal("runner ran despite a non-regular executable target")
	}
}

// --- Executable identity binding (#553) ---

var bwrapLifetimes = []string{"foreground", "background"}

// bwrapLaunch drives one lifetime of b with fresh capture delegates and
// returns the wrapped spec the selected delegate received, the total delegate
// calls, and the launch error. A Start that returns a process is waited.
func bwrapLaunch(t *testing.T, b *bwrapBackend, lifetime string, spec execSpec) (execSpec, int, error) {
	t.Helper()
	runner := &captureRunner{}
	starter := &captureStarter{proc: fakeProcess{}}
	b.runner, b.starter = runner, starter
	if lifetime == "foreground" {
		_, err := b.Run(context.Background(), spec)
		return runner.spec, runner.called + starter.called, err
	}
	proc, err := b.Start(spec, io.Discard, io.Discard)
	if proc != nil {
		if _, _, werr := proc.Wait(); werr != nil {
			t.Errorf("Wait = %v; want nil", werr)
		}
	}
	return starter.spec, runner.called + starter.called, err
}

// bwrap must refuse when the executable target resolved at prepare time is
// not the approved object. Regular-over-regular so the existing non-regular
// guard cannot mask the identity check.
func TestBwrapPrepareRejectsExecutableIdentityChange(t *testing.T) {
	for _, lifetime := range bwrapLifetimes {
		t.Run(lifetime, func(t *testing.T) {
			b := testBwrapBackend(nil, nil, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			ws := wsTempDir(t)
			approved := filepath.Join(ws, "tool")
			if err := os.WriteFile(approved, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec := bwrapTestSpec(t, ws)
			spec.Path = approved
			spec.Argv = []string{"tool"}
			spec.ExeIdentity = execIdentityOf(t, approved)

			newer := filepath.Join(ws, "tool.new")
			if err := os.WriteFile(newer, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(newer, approved); err != nil {
				t.Fatal(err)
			}
			_, calls, err := bwrapLaunch(t, b, lifetime, spec)
			if err == nil || !strings.Contains(err.Error(), "executable changed since approval") {
				t.Fatalf("%s launch = %v; want the executable-changed error", lifetime, err)
			}
			if calls != 0 {
				t.Fatalf("delegate called %d times; want 0 (prepare must refuse before launch)", calls)
			}
		})
	}
}

// A spec with no approved identity (never produced by recheckExecPlan) is
// refused, so a future caller cannot silently skip the check.
func TestBwrapPrepareRejectsMissingIdentity(t *testing.T) {
	for _, lifetime := range bwrapLifetimes {
		t.Run(lifetime, func(t *testing.T) {
			b := testBwrapBackend(nil, nil, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			ws := wsTempDir(t)
			exe := filepath.Join(ws, "tool")
			if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec := bwrapTestSpec(t, ws)
			spec.Path = exe
			spec.Argv = []string{"tool"}
			spec.ExeIdentity = nil // deliberately missing after helper initialization
			_, calls, err := bwrapLaunch(t, b, lifetime, spec)
			if err == nil || !strings.Contains(err.Error(), "executable changed since approval") {
				t.Fatalf("%s launch = %v; want refusal on missing identity", lifetime, err)
			}
			if calls != 0 {
				t.Fatalf("delegate called %d times; want 0", calls)
			}
		})
	}
}

// TestBwrapAcceptsApprovedExecutable is the capture-delegate compatibility
// control for the identity check: an unchanged #! script and a Homebrew-style
// absolute symlink both reach the delegate in both lifetimes, with the
// canonical target bound and launched and the approved spelling kept as
// --argv0.
func TestBwrapAcceptsApprovedExecutable(t *testing.T) {
	fixtures := map[string]func(t *testing.T, ws string) execSpec{
		"script": func(t *testing.T, ws string) execSpec {
			p := filepath.Join(ws, "tool.sh")
			if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec := bwrapTestSpec(t, ws)
			spec.Path, spec.Argv = p, []string{"tool.sh", "arg1"}
			spec.ExeIdentity = execIdentityOf(t, p)
			return spec
		},
		"homebrew symlink": func(t *testing.T, ws string) execSpec {
			prefix := wsTempDir(t)
			real := filepath.Join(prefix, "Cellar", "tool", "1.0", "bin", "tool")
			link := filepath.Join(prefix, "bin", "tool")
			for _, d := range []string{filepath.Dir(real), filepath.Dir(link)} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			spec := bwrapTestSpec(t, ws)
			spec.Path, spec.Argv = link, []string{"tool", "arg1"}
			spec.ExeIdentity = execIdentityOf(t, link)
			return spec
		},
	}
	for name, fixture := range fixtures {
		for _, lifetime := range bwrapLifetimes {
			t.Run(name+"/"+lifetime, func(t *testing.T) {
				b := testBwrapBackend(nil, nil, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
				spec := fixture(t, wsTempDir(t))
				canonExe, err := filepath.EvalSymlinks(spec.Path)
				if err != nil {
					t.Fatal(err)
				}
				wrapped, calls, err := bwrapLaunch(t, b, lifetime, spec)
				if err != nil {
					t.Fatalf("%s launch = %v; want the approved executable accepted", lifetime, err)
				}
				if calls != 1 {
					t.Fatalf("delegate called %d times; want 1", calls)
				}
				assertBwrapLaunchesCanonical(t, wrapped.Argv, spec.Path, canonExe)
			})
		}
	}
}

// assertBwrapLaunchesCanonical pins the unchanged launch shape: canonExe is
// the read-only bind source and the executed path, spelling only --argv0.
// It assumes exactly one trailing payload argument after the executed path.
func assertBwrapLaunchesCanonical(t *testing.T, argv []string, spelling, canonExe string) {
	t.Helper()
	joined := " " + strings.Join(argv, " ") + " "
	if !strings.Contains(joined, " --ro-bind "+canonExe+" "+canonExe+" ") {
		t.Fatalf("canonical target %q is not the bind source: %q", canonExe, argv)
	}
	if !strings.Contains(joined, " --argv0 "+spelling+" "+canonExe+" ") {
		t.Fatalf("want --argv0 %q launching %q: %q", spelling, canonExe, argv)
	}
	if argv[len(argv)-2] != canonExe {
		t.Fatalf("payload path = %q, want the canonical target %q", argv[len(argv)-2], canonExe)
	}
}

// TestBwrapAcceptsScratchStampedExecutable composes scratch with the bwrap
// check: scratch rewrites a workspace-local executable to its work clone and
// restamps ExeIdentity, so prepare sees the clone as the approved object and
// binds and launches its canonical target in both lifetimes.
func TestBwrapAcceptsScratchStampedExecutable(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	// bwrap requires a canonical workspace root, and the rewritten root is a
	// descendant of the scratch temp base.
	tempBase, err := filepath.EvalSymlinks(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	rt.tempBase = tempBase
	script := filepath.Join(canon, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spec := testSpec(t, canon)
	spec.Path, spec.Argv = script, []string{"./tool.sh", "arg1"}
	spec.ExeIdentity = execIdentityOf(t, script)
	session, rewritten, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	defer session.discard()

	clone := filepath.Join(session.work, "tool.sh")
	if rewritten.Path != clone {
		t.Fatalf("rewritten Path = %q, want the work clone %q", rewritten.Path, clone)
	}
	// Non-fatal so the bwrap legs below still show what prepare does with an
	// unstamped identity.
	if !os.SameFile(rewritten.ExeIdentity, execIdentityOf(t, clone)) {
		t.Errorf("rewritten ExeIdentity is not the work clone")
	}
	if os.SameFile(rewritten.ExeIdentity, spec.ExeIdentity) {
		t.Errorf("rewritten ExeIdentity still names the source executable")
	}
	canonClone, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	for _, lifetime := range bwrapLifetimes {
		t.Run(lifetime, func(t *testing.T) {
			b := testBwrapBackend(nil, nil, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			wrapped, calls, err := bwrapLaunch(t, b, lifetime, rewritten)
			if err != nil {
				t.Fatalf("%s launch = %v; want the scratch-stamped clone accepted", lifetime, err)
			}
			if calls != 1 {
				t.Fatalf("delegate called %d times; want 1", calls)
			}
			assertBwrapLaunchesCanonical(t, wrapped.Argv, rewritten.Path, canonClone)
		})
	}
}

// TestNewExecBackendBwrapToolchainSentinel pins that the declared toolchain
// fields (#722) refused by bwrap reach the public path wrapping
// ErrSandboxPolicy, from the bwrap refusal itself rather than normalization.
func TestNewExecBackendBwrapToolchainSentinel(t *testing.T) {
	for _, tc := range []struct {
		field string
		cfg   SandboxConfig
	}{
		{"ReadOnlyRoots", SandboxConfig{Runtime: SandboxRuntimeBwrap, ReadOnlyRoots: []string{t.TempDir()}}},
		{"DeveloperDirectory", SandboxConfig{Runtime: SandboxRuntimeBwrap, DeveloperDirectory: t.TempDir()}},
		{"Env", SandboxConfig{Runtime: SandboxRuntimeBwrap, Env: []SandboxEnv{{Name: "GOPROXY", Value: "off"}}}},
		{"TempEnv", SandboxConfig{Runtime: SandboxRuntimeBwrap, TempEnv: []string{"GOCACHE"}}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			err := ValidateSandbox(tc.cfg, t.TempDir())
			if !errors.Is(err, ErrSandboxPolicy) {
				t.Fatalf("ValidateSandbox err = %v, want ErrSandboxPolicy", err)
			}
			if !strings.Contains(err.Error(), "bwrap cannot enforce "+tc.field) {
				t.Fatalf("ValidateSandbox err = %v, want the bwrap refusal naming %s", err, tc.field)
			}
		})
	}
}

// --- the post-run check for new git machinery (#802) ---

// heldRunIDPattern finds the run id in a hold note's new name.
var heldRunIDPattern = regexp.MustCompile(`\.golem-held-([^ ]*) \(`)

// heldRunID returns the run id note names, failing the test unless it is
// eight lowercase hex digits.
func heldRunID(t *testing.T, note string) string {
	t.Helper()
	m := heldRunIDPattern.FindStringSubmatch(note)
	if m == nil || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(m[1]) {
		t.Fatalf("no hex run id in %q", note)
	}
	return m[1]
}

// heldInitNotes is the hold's notes for a new repository at rel made by git
// init: its HEAD first, then its .git.
func heldInitNotes(rel, id string) []string {
	return []string{heldNote(rel+"/.git/HEAD", rel+"/.git/HEAD.golem-held-"+id),
		heldNote(rel+"/.git", rel+"/.git.golem-held-"+id)}
}

// requireBwrapGit skips without bwrap or a host git; real git, kept inside
// the workspace by GIT_CEILING_DIRECTORIES, is the oracle for "held".
func requireBwrapGit(t *testing.T, ws string) {
	t.Helper()
	requireBwrapCapability(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", ws)
}

// bwrapShSpec is a spec for /bin/sh -c script rooted at ws.
func bwrapShSpec(t *testing.T, ws, script string) execSpec {
	t.Helper()
	return execSpec{
		Path: "/bin/sh", Argv: []string{"/bin/sh", "-c", script}, Dir: ws,
		Env: []string{"PATH=/usr/bin:/bin", "HOME=" + bwrapTestHome}, WorkspaceRoot: ws,
		ExeIdentity: execIdentityOf(t, "/bin/sh"), Screen: testScreen(),
	}
}

// runBwrapJob starts script as a background job on m and returns its handle.
func runBwrapJob(t *testing.T, m *BackgroundManager, ws, script string) string {
	t.Helper()
	st, err := m.start(context.Background(), bwrapShSpec(t, ws, script), ws)
	if err != nil {
		t.Fatal(err)
	}
	return st.Handle
}

// reapedBwrapJob waits until the job is reaped, checks it exited 0, and
// returns its status.
func reapedBwrapJob(t *testing.T, m *BackgroundManager, handle string) JobStatus {
	t.Helper()
	select {
	case <-jobDoneChan(t, m, handle):
	case <-time.After(90 * time.Second):
		t.Fatalf("job %s never finished", handle)
	}
	st, ok := m.status(handle)
	if !ok {
		t.Fatalf("job %s evicted", handle)
	}
	if !st.ExitKnown || st.ExitCode != 0 {
		t.Fatalf("job %s: exit %d (known %v)", handle, st.ExitCode, st.ExitKnown)
	}
	return st
}

// TestBwrapRunHoldsNewGitMachinery: a foreground command's new repository is
// set aside once it returns, and the notes say so.
func TestBwrapRunHoldsNewGitMachinery(t *testing.T) {
	ws := wsTempDir(t)
	requireBwrapGit(t, ws)
	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws, []string{"/bin/sh", "-c", "git init -q sub"})
	if res.ExitCode != 0 {
		t.Fatalf("git init: exit %d: %s", res.ExitCode, res.Stderr)
	}
	if len(res.Notes) == 0 {
		t.Fatal("no notes")
	}
	if want := heldInitNotes("sub", heldRunID(t, res.Notes[0])); !slices.Equal(res.Notes, want) {
		t.Fatalf("notes:\n got %q\nwant %q", res.Notes, want)
	}
	if got := gitDirAt(t, ws, "sub"); got != "" {
		t.Fatalf("host git in sub finds %q", got)
	}
}

// TestBwrapRunLeavesOrdinaryChanges: files and folders, one of them named
// HEAD, are not git machinery.
func TestBwrapRunLeavesOrdinaryChanges(t *testing.T) {
	requireBwrapCapability(t)
	ws := wsTempDir(t)
	res := realBwrapRun(t, SandboxConfig{Runtime: SandboxRuntimeBwrap}, ws,
		[]string{"/bin/sh", "-c", "mkdir -p d/e && echo x > d/e/f && echo y > top && mkdir HEAD"})
	if res.ExitCode != 0 || len(res.Notes) != 0 {
		t.Fatalf("exit %d, notes %q", res.ExitCode, res.Notes)
	}
	for _, p := range []string{"d/e/f", "top", "HEAD"} {
		if _, err := os.Lstat(filepath.Join(ws, p)); err != nil {
			t.Error(err)
		}
	}
}

// plantingRunner stands for a command that plants an empty .git at plant
// and then times out.
type plantingRunner struct{ plant string }

// Run creates the fixture and returns a timeout.
func (r plantingRunner) Run(context.Context, execSpec) (execResult, error) {
	if err := os.MkdirAll(r.plant, 0o755); err != nil {
		return execResult{}, err
	}
	return execResult{TimedOut: true, Notes: []string{"earlier"}}, context.DeadlineExceeded
}

// TestBwrapRunWorkspaceReplaced: a workspace moved away during the run, with
// another checkout put at its path, is not compared with the first one's
// baseline: the check is skipped with a warning and the other checkout's .git
// is left alone.
func TestBwrapRunWorkspaceReplaced(t *testing.T) {
	ws := filepath.Join(wsTempDir(t), "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	runIn(t, ws, `mkdir src && echo x > src/x.go`)
	b := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) {
		runIn(t, filepath.Dir(ws), `mv ws ws.moved && mkdir -p ws/.git/hooks && echo P > ws/.git/hooks/pre-commit`)
		return execResult{}, nil
	}), &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	res, err := b.Run(context.Background(), bwrapTestSpec(t, ws))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"WARNING: the workspace was moved or replaced during the run; the post-run git check was skipped (" +
		noteName(ws) + ")"}
	if !slices.Equal(res.Notes, want) {
		t.Fatalf("notes:\n got %q\nwant %q", res.Notes, want)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git", "hooks", "pre-commit")); err != nil {
		t.Fatalf("the other checkout's .git was touched: %v", err)
	}
}

// syntheticSecret is a credential-shaped value from the synthetic fixtures
// (secrettest), never a live one.
func syntheticSecret(t *testing.T) string {
	t.Helper()
	cases, err := secrettest.Cases()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.ID != "github-ghp-env-min-body" {
			continue
		}
		params := map[string]string{"seed": c.ID}
		for k, v := range c.Params {
			params[k] = v
		}
		v, err := secrettest.Generate(c.Generator, params)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	t.Fatal("no github-ghp-env-min-body case")
	return ""
}

// TestBwrapHeldNotesScreenPaths: a held-git note never shows a path that
// holds a value the command's credential screen screens for, whether the
// check runs after a foreground run, at a background job's exit or when a
// start fails: the note becomes a WARNING that names no path, and other
// paths are shown as usual.
func TestBwrapHeldNotesScreenPaths(t *testing.T) {
	secret := syntheticSecret(t)
	if strings.ContainsAny(secret, "/\x00") {
		t.Fatalf("the synthetic value cannot be a file name")
	}
	plant := func(ws string) {
		for _, p := range []string{secret, "plain"} {
			if err := os.MkdirAll(filepath.Join(ws, p, ".git", "hooks"), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, mode := range []string{"foreground", "background", "start fails"} {
		for _, incomplete := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/incomplete=%v", mode, incomplete), func(t *testing.T) {
				ws := wsTempDir(t)
				runner := funcRunner(func(context.Context, execSpec) (execResult, error) {
					plant(ws)
					return execResult{}, nil
				})
				starter := &captureStarter{proc: fakeProcess{}}
				if mode == "start fails" {
					starter = &captureStarter{err: errors.New("start failed"), during: func() { plant(ws) }}
				}
				b := testBwrapBackend(runner, starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
				spec := bwrapTestSpec(t, ws)
				spec.Screen = newOutputScreen([][]byte{[]byte(secret)})
				if incomplete {
					spec.Screen.withhold = "incomplete read"
				}
				var notes []string
				switch mode {
				case "foreground":
					res, err := b.Run(context.Background(), spec)
					if err != nil {
						t.Fatal(err)
					}
					notes = res.Notes
				case "background":
					p, err := b.Start(spec, io.Discard, io.Discard)
					if err != nil {
						t.Fatal(err)
					}
					plant(ws)
					_, _, _ = p.Wait()
					notes = strings.Split(cleanupNoteOf(p), "\n")
				case "start fails":
					_, err := b.Start(spec, io.Discard, io.Discard)
					if err == nil {
						t.Fatal("start did not fail")
					}
					notes = notesOf(err)
					if len(notes) == 1 {
						notes = strings.Split(notes[0], "\n")
					}
				}
				if joined := strings.Join(notes, "\n"); strings.Contains(joined, secret) {
					t.Fatalf("a note shows the screened value:\n%s", joined)
				}
				if len(notes) != 2 {
					t.Fatalf("notes %q", notes)
				}
				entries, err := os.ReadDir(filepath.Join(ws, "plain"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("held names %v, error %v", entries, err)
				}
				id := strings.TrimPrefix(entries[0].Name(), ".git.golem-held-")
				want := []string{
					"WARNING: held back new git metadata at a path withheld by the credential screen (its name now ends in .golem-held-" +
						id + "; rename it back to restore)",
					heldNote("plain/.git", "plain/.git.golem-held-"+id),
				}
				if incomplete {
					want[1] = want[0]
				}
				if !slices.Equal(notes, want) {
					t.Fatalf("notes:\n got %q\nwant %q", notes, want)
				}
				if _, err := os.Lstat(filepath.Join(ws, secret, ".git.golem-held-"+id)); err != nil {
					t.Fatalf("the screened path was not set aside: %v", err)
				}
			})
		}
	}
}

// TestBwrapQueuedStartCancelled: a start waiting behind another job's
// post-run check ends with its admission context's error once that context
// ends, without the check finishing, and never spawns.
func TestBwrapQueuedStartCancelled(t *testing.T) {
	ws := wsTempDir(t)
	starter := &captureStarter{proc: fakeProcess{}}
	b := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) { return execResult{}, nil }),
		starter, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	if err := b.lockHeld(context.Background()); err != nil { // a check in progress
		t.Fatal(err)
	}
	spec := bwrapTestSpec(t, ws)
	m := newBackgroundManagerWithBackend(bindExecBackend(b, b.cfg), strings.NewReader(strings.Repeat("a", 16)))
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	b.heldContended = func() { close(waiting) }
	done := make(chan error, 1)
	go func() {
		_, err := m.start(ctx, spec, ".")
		done <- err
	}()
	select {
	case <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("start did not reach lock")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the queued start did not give up while the check held the lock")
	}
	b.unlockHeld()
	if !errors.Is(err, context.Canceled) || starter.called != 0 {
		t.Fatalf("err %v, starter called %d times", err, starter.called)
	}
}

// TestBwrapWorkspacePinReleased: the descriptor pinning the workspace for the
// post-run check is closed on every path: a foreground run, a background job
// once it is waited for, a start that fails, and a prepare that refuses.
func TestBwrapWorkspacePinReleased(t *testing.T) {
	openFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	ws := wsTempDir(t)
	ok := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) { return execResult{}, nil }),
		&captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	failing := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) { return execResult{}, nil }),
		&captureStarter{err: errors.New("start failed")}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	for _, c := range []struct {
		name string
		run  func()
	}{
		{"foreground run", func() {
			if _, err := ok.Run(context.Background(), bwrapTestSpec(t, ws)); err != nil {
				t.Fatal(err)
			}
		}},
		{"background job waited for", func() {
			p, err := ok.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _ = p.Wait()
		}},
		{"start fails", func() {
			if _, err := failing.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard); err == nil {
				t.Fatal("start did not fail")
			}
		}},
		{"prepare refuses", func() {
			spec := bwrapTestSpec(t, ws)
			spec.ExeIdentity = nil
			if _, err := ok.Run(context.Background(), spec); err == nil {
				t.Fatal("prepare did not refuse")
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			was := openFDs()
			c.run()
			if now := openFDs(); now != was {
				t.Errorf("%d descriptors open after, %d before", now, was)
			}
		})
	}
}

// TestBwrapRunErrorKeepsHeldNotes: a run that ends in an error is checked
// too, and its notes ride on the result exec renders for that error.
func TestBwrapRunErrorKeepsHeldNotes(t *testing.T) {
	ws := wsTempDir(t)
	b := testBwrapBackend(plantingRunner{plant: filepath.Join(ws, "sub", ".git")}, &captureStarter{},
		SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	res, err := b.Run(context.Background(), bwrapTestSpec(t, ws))
	if !errors.Is(err, context.DeadlineExceeded) || !res.TimedOut {
		t.Fatalf("err %v, timed out %v", err, res.TimedOut)
	}
	if len(res.Notes) != 2 {
		t.Fatalf("notes %q", res.Notes)
	}
	id := heldRunID(t, res.Notes[1])
	if want := []string{"earlier", heldNote("sub/.git", "sub/.git.golem-held-"+id)}; !slices.Equal(res.Notes, want) {
		t.Fatalf("notes:\n got %q\nwant %q", res.Notes, want)
	}
}

// TestBwrapStartHoldsAtExit: a background job's new repository is set aside
// when the job exits, and the job's notes say so.
func TestBwrapStartHoldsAtExit(t *testing.T) {
	ws := wsTempDir(t)
	requireBwrapGit(t, ws)
	m, err := NewSandboxedBackgroundManager(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	st := reapedBwrapJob(t, m, runBwrapJob(t, m, ws, "git init -q sub"))
	if len(st.Notes) != 1 {
		t.Fatalf("notes %q", st.Notes)
	}
	if want := strings.Join(heldInitNotes("sub", heldRunID(t, st.Notes[0])), "\n"); st.Notes[0] != want {
		t.Fatalf("note:\n got %q\nwant %q", st.Notes[0], want)
	}
	if got := gitDirAt(t, ws, "sub"); got != "" {
		t.Fatalf("host git in sub finds %q", got)
	}
}

// plantThenFailStarter starts the command, lets it run to the end, and then
// fails, as unixStarter.Start does when it cannot observe a child's exit:
// that child was started, then killed and reaped inside Start.
type plantThenFailStarter struct{}

// Start waits for the child before reporting the injected failure.
func (plantThenFailStarter) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	p, err := unixStarter{}.Start(spec, stdout, stderr)
	if err != nil {
		return nil, err
	}
	_, _, _ = p.Wait()
	return nil, errors.New("observe background process exit: injected")
}

// TestBwrapStartErrorStillChecks: when the starter fails after the child
// ran, what it planted is still set aside, and the notes ride on the error.
func TestBwrapStartErrorStillChecks(t *testing.T) {
	ws := wsTempDir(t)
	requireBwrapGit(t, ws)
	backend, err := newExecBackend(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	b := backend.execBackend.(*bwrapBackend)
	b.starter = plantThenFailStarter{}
	_, err = b.Start(bwrapShSpec(t, ws, "git init -q sub"), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "observe background process exit: injected") {
		t.Fatalf("err %v", err)
	}
	notes := notesOf(err)
	if len(notes) != 1 {
		t.Fatalf("no notes on %v", err)
	}
	if want := strings.Join(heldInitNotes("sub", heldRunID(t, notes[0])), "\n"); notes[0] != want {
		t.Fatalf("note:\n got %q\nwant %q", notes[0], want)
	}
	if got := gitDirAt(t, ws, "sub"); got != "" {
		t.Fatalf("host git in sub finds %q", got)
	}
}

// TestBwrapOverlappingJobs: job b plants a repository while job a runs, a
// moves it, and both exit. The first check to run holds it, and host git then
// finds nothing at the original, moved or held places.
func TestBwrapOverlappingJobs(t *testing.T) {
	ws := wsTempDir(t)
	requireBwrapGit(t, ws)
	m, err := NewSandboxedBackgroundManager(SandboxConfig{Runtime: SandboxRuntimeBwrap})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Shutdown)
	const wait = `i=0; until [ -e %s ]; do sleep 0.05; i=$((i+1)); [ $i -lt 1200 ] || exit 9; done`
	a := runBwrapJob(t, m, ws, fmt.Sprintf(wait, "planted")+"; mv sub moved")
	b := runBwrapJob(t, m, ws, "git init -q sub && touch planted && "+fmt.Sprintf(wait, "moved"))
	var notes []string
	for _, h := range []string{a, b} {
		for _, n := range reapedBwrapJob(t, m, h).Notes {
			notes = append(notes, strings.Split(n, "\n")...)
		}
	}
	if len(notes) == 0 {
		t.Fatal("nothing held")
	}
	id := heldRunID(t, notes[0])
	if want := heldInitNotes("moved", id); !slices.Equal(notes, want) {
		t.Fatalf("notes:\n got %q\nwant %q", notes, want)
	}
	if _, err := os.Lstat(filepath.Join(ws, "sub")); err == nil {
		t.Fatal("sub is still there")
	}
	for _, at := range []string{".", "moved", "moved/.git.golem-held-" + id} {
		if got := gitDirAt(t, ws, at); got != "" {
			t.Errorf("host git in %q finds %q", at, got)
		}
	}
}

// TestBwrapConcurrentChecksRestoreExactMode: two background jobs' checks run
// at once over a directory the command locked. The second starts once the
// first has opened the directory up to read it; were they not serialized,
// the second would record that temporary mode as the original, to rename in
// it, and put it back after the first restored the real one. The directory
// ends with its exact mode.
func TestBwrapConcurrentChecksRestoreExactMode(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ws := wsTempDir(t)
	restoreModes(t, ws)
	b := testBwrapBackend(&captureRunner{}, &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	p1, err := b.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := b.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	runIn(t, ws, `git init -q a && chmod 0 a`)

	waiting := make(chan struct{})
	b.heldContended = func() { close(waiting) }
	note2 := make(chan string, 1)
	var once sync.Once
	ownerAccessChmod = func(p string, mode uint32) error {
		err := unix.Chmod(p, mode)
		once.Do(func() {
			go func() {
				_, _, _ = p2.Wait()
				note2 <- cleanupNoteOf(p2)
			}()
			select {
			case <-waiting:
			case <-time.After(10 * time.Second):
				t.Error("second check did not contend for the lock")
			}
		})
		return err
	}
	t.Cleanup(func() { ownerAccessChmod = unix.Chmod })
	_, _, _ = p1.Wait()
	note1 := cleanupNoteOf(p1)
	if got := <-note2; got != "" {
		t.Errorf("second note: got %q, want none", got)
	}
	if want := strings.Join(heldInitNotes("a", heldRunID(t, note1)), "\n"); note1 != want {
		t.Errorf("first note:\n got %q\nwant %q", note1, want)
	}
	fi, err := os.Lstat(filepath.Join(ws, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky); got != 0 {
		t.Errorf("mode of a is %v, want 0", got)
	}
}

// funcRunner is a commandRunner from a function.
type funcRunner func(context.Context, execSpec) (execResult, error)

// Run invokes the supplied test function.
func (f funcRunner) Run(ctx context.Context, spec execSpec) (execResult, error) { return f(ctx, spec) }

// heldSummaryPattern finds the run id in capHeldNotes's summary line.
var heldSummaryPattern = regexp.MustCompile(`^… and \d+ more notes \(\d+ reporting renames, \d+ with warnings, run ([0-9a-f]{8})\)`)

// TestBwrapHeldNotesCapped: a run that plants more than the cap gets the cap's
// lines, every WARNING first, and one summary line with the run id and how
// many dropped lines were WARNINGs; a path longer than the cap's bytes is cut.
// The locked directories belong to "another user" (the euid seam), so each
// is a WARNING the check cannot open up.
func TestBwrapHeldNotesCapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	long := strings.Repeat("a", 220)
	multi := strings.Repeat("a", 199) + "\u00e9bbbb" // é spans bytes 199 and 200
	for _, c := range []struct {
		name           string
		locked, plants int
		plant          string // one more planted .git, at this path
		warns          int    // WARNING lines kept
		summary        string // with %[1]s for the run id; "" for none
		want           []string
	}{
		{name: "exactly the cap", plants: heldNotesMax},
		{name: "one past the cap", plants: heldNotesMax + 1,
			summary: "… and 2 more notes (2 reporting renames, 0 with warnings, run %[1]s); look for *.golem-held-%[1]s* to find renamed entries"},
		{name: "renames past the cap", locked: 3, plants: 60, warns: 3,
			summary: "… and 14 more notes (14 reporting renames, 0 with warnings, run %[1]s); look for *.golem-held-%[1]s* to find renamed entries"},
		{name: "WARNINGs past the cap", locked: 55, plants: 5, warns: heldNotesMax - 1,
			summary: "… and 11 more notes (5 reporting renames, 6 with warnings, run %[1]s); look for *.golem-held-%[1]s* to find renamed entries"},
		{name: "a long path is cut", plant: long + "/.git",
			want: []string{heldNote(long[:200]+"…[cut]", long[:200]+"…[cut]")}},
		{name: "a path is cut at a rune boundary", plant: multi + "/.git",
			want: []string{heldNote(multi[:199]+"…[cut]", multi[:199]+"…[cut]")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ws := wsTempDir(t)
			restoreModes(t, ws)
			b := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) {
				for i := range c.locked {
					runIn(t, ws, fmt.Sprintf("mkdir l%02d && chmod 0 l%02d", i, i))
				}
				for i := range c.plants {
					runIn(t, ws, fmt.Sprintf("mkdir -p p%02d/.git", i))
				}
				if c.plant != "" {
					runIn(t, ws, "mkdir -p '"+c.plant+"'")
				}
				return execResult{}, nil
			}), &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			ownerAccessEuid = func() int { return os.Geteuid() + 1 }
			t.Cleanup(func() { ownerAccessEuid = os.Geteuid })
			res, err := b.Run(context.Background(), bwrapTestSpec(t, ws))
			if err != nil {
				t.Fatal(err)
			}
			if c.want != nil {
				if !slices.Equal(res.Notes, c.want) {
					t.Fatalf("notes:\n got %q\nwant %q", res.Notes, c.want)
				}
				return
			}
			details := res.Notes
			id := ""
			if c.summary != "" {
				if len(res.Notes) != heldNotesMax {
					t.Fatalf("%d notes, want %d with the summary: %q", len(res.Notes), heldNotesMax, res.Notes)
				}
				last := res.Notes[heldNotesMax-1]
				m := heldSummaryPattern.FindStringSubmatch(last)
				if m == nil || last != fmt.Sprintf(c.summary, m[1]) {
					t.Fatalf("summary %q, want %q", last, c.summary)
				}
				id, details = m[1], res.Notes[:heldNotesMax-1]
			} else {
				if len(res.Notes) != c.locked+c.plants {
					t.Fatalf("%d notes, want %d: %q", len(res.Notes), c.locked+c.plants, res.Notes)
				}
				id = heldRunID(t, res.Notes[len(res.Notes)-1])
			}
			for i, n := range details {
				want := heldNote(fmt.Sprintf("p%02d/.git", i-c.warns), fmt.Sprintf("p%02d/.git.golem-held-%s", i-c.warns, id))
				if i < c.warns {
					want = checkWarn(fmt.Sprintf("l%02d", i), "owned by another user")
				}
				if n != want {
					t.Errorf("note %d: got %q, want %q", i, n, want)
				}
			}
			// The cap bounds the notes, not the check: every plant is set aside.
			for i := range c.plants {
				if _, err := os.Lstat(filepath.Join(ws, fmt.Sprintf("p%02d/.git.golem-held-%s", i, id))); err != nil {
					t.Errorf("p%02d/.git was not set aside: %v", i, err)
				}
			}
		})
	}
}

// TestHeldGitProcessWaitTwice: the check runs on the first Wait only, and a
// second Wait keeps its notes.
func TestHeldGitProcessWaitTwice(t *testing.T) {
	calls := 0
	p := &heldGitProcess{backgroundProcess: fakeProcess{}, check: func() []string {
		calls++
		if calls > 1 {
			return nil
		}
		return []string{"one", "two"}
	}}
	for range 2 {
		if _, _, err := p.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || cleanupNoteOf(p) != "one\ntwo" {
		t.Fatalf("checks %d, note %q", calls, cleanupNoteOf(p))
	}
	if p.check != nil {
		t.Fatal("the check, and the before-walk snapshot it holds, outlives the job")
	}
}

// TestBwrapPrepareWaitsForCheck: a start's before-walk waits while another
// job's check has a directory opened up, so it never sees that mode.
func TestBwrapPrepareWaitsForCheck(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ws := wsTempDir(t)
	restoreModes(t, ws)
	ran := make(chan struct{}, 1)
	b := testBwrapBackend(funcRunner(func(context.Context, execSpec) (execResult, error) {
		ran <- struct{}{}
		return execResult{}, nil
	}), &captureStarter{proc: fakeProcess{}}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	p, err := b.Start(bwrapTestSpec(t, ws), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	runIn(t, ws, `mkdir a && chmod 0 a`)
	var once sync.Once
	fired := false
	waiting := make(chan struct{})
	b.heldContended = func() { close(waiting) }
	runDone := make(chan struct{})
	ownerAccessChmod = func(p string, mode uint32) error {
		err := unix.Chmod(p, mode)
		once.Do(func() { // the check has a opened up
			fired = true
			go func() {
				_, _ = b.Run(context.Background(), bwrapTestSpec(t, ws))
				close(runDone)
			}()
			select {
			case <-waiting:
			case <-ran:
				t.Error("before-walk overlapped a check")
			case <-time.After(10 * time.Second):
				t.Error("before-walk did not contend for the lock")
			}
		})
		return err
	}
	t.Cleanup(func() { ownerAccessChmod = unix.Chmod })
	_, _, _ = p.Wait()
	if !fired {
		t.Fatal("the check never opened a up")
	}
	<-runDone
}

// TestBwrapPinnedPreparation inspects the pinned workspace across a path replacement.
func TestBwrapPinnedPreparation(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%v", restore), func(t *testing.T) {
			parent := wsTempDir(t)
			ws := filepath.Join(parent, "ws")
			if err := os.MkdirAll(ws+"/original", 0755); err != nil {
				t.Fatal(err)
			}
			runner := &captureRunner{}
			b := testBwrapBackend(runner, &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
			if err := b.lockHeld(context.Background()); err != nil {
				t.Fatal(err)
			}
			blocked := make(chan struct{})
			b.heldContended = func() { close(blocked) }
			spec := bwrapTestSpec(t, ws)
			result := make(chan error, 1)
			seen := false
			probe := openDirProbe
			openDirProbe = func(p string) (dirProbe, error) {
				if filepath.Base(p) == "original" && !seen {
					seen = true
					if restore {
						if err := os.Rename(ws, ws+".other"); err != nil {
							return nil, err
						}
						if err := os.Rename(ws+".pinned", ws); err != nil {
							return nil, err
						}
					}
				}
				return probe(p)
			}
			t.Cleanup(func() { openDirProbe = probe })
			go func() {
				wrapped, before, err := b.prepare(spec)
				closePin(before)
				if err == nil && len(wrapped.Argv) == 0 {
					err = errors.New("missing argv")
				}
				result <- err
			}()
			select {
			case <-blocked:
			case <-time.After(10 * time.Second):
				t.Fatal("prepare did not reach lock")
			}
			if err := os.Rename(ws, ws+".pinned"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(ws, 0755); err != nil {
				t.Fatal(err)
			}
			b.unlockHeld()
			err := <-result
			if !seen || (err == nil) != restore {
				t.Fatalf("observed pinned workspace=%v, error=%v", seen, err)
			}
		})
	}
	ws := wsTempDir(t)
	b := testBwrapBackend(&captureRunner{}, &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	spec := bwrapTestSpec(t, ws)
	spec.WorkspaceRoot = filepath.Join(ws, "link")
	if err := os.Symlink(ws, spec.WorkspaceRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.prepare(spec); err == nil || !strings.Contains(err.Error(), "pin sandbox workspace") {
		t.Fatalf("pin failure: %v", err)
	}
}

// TestReboundWarningsCapped retains the root warning and counts omitted restoration failures.
func TestReboundWarningsCapped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires ordinary owner permissions")
	}
	ws := filepath.Join(wsTempDir(t), "ws")
	if err := os.Mkdir(ws, 0755); err != nil {
		t.Fatal(err)
	}
	b := testBwrapBackend(&captureRunner{}, &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	_, before, err := b.prepare(bwrapTestSpec(t, ws))
	if err != nil {
		t.Fatal(err)
	}
	defer closePin(before)
	for i := range heldNotesMax + 3 {
		if err := os.Mkdir(filepath.Join(ws, strconv.Itoa(i)), 0); err != nil {
			t.Fatal(err)
		}
	}
	restoreModes(t, ws+".moved")
	original := ownerAccessChmod
	t.Cleanup(func() { ownerAccessChmod = original })
	moved := false
	ownerAccessChmod = func(p string, mode uint32) error {
		if mode != 0 {
			return unix.Chmod(p, mode)
		}
		if !moved {
			if err := os.Rename(ws, ws+".moved"); err != nil {
				return err
			}
			moved = true
		}
		return unix.EIO
	}
	got := b.holdNewGit(before, testScreen())
	if len(got) != heldNotesMax || !strings.HasPrefix(got[0], "WARNING: the workspace was moved") || !strings.Contains(got[len(got)-1], "5 more notes (0 reporting renames, 5 with warnings, run ") || strings.Contains(got[len(got)-1], ".golem-held-") {
		t.Fatalf("warnings %q", got)
	}
}

// waitOutcome supplies terminal process status to the cleanup wrapper.
type waitOutcome struct {
	fakeProcess
	code   int
	killed bool
	err    error
}

// Wait returns the configured process outcome.
func (p waitOutcome) Wait() (int, bool, error) { return p.code, p.killed, p.err }

// TestHeldGitWaitOutcomes preserves the process result and cleanup note on every exit.
func TestHeldGitWaitOutcomes(t *testing.T) {
	for _, c := range []waitOutcome{{code: 3}, {code: -1, killed: true}, {code: -1, err: unix.EIO}} {
		p := &heldGitProcess{backgroundProcess: c, check: func() []string { return []string{"held"} }}
		code, killed, err := p.Wait()
		if code != c.code || killed != c.killed || err != c.err || cleanupNoteOf(p) != "held" || p.check != nil {
			t.Errorf("Wait %d %v %v; note %q", code, killed, err, cleanupNoteOf(p))
		}
	}
}

// TestHeldPathScreenIncomplete withholds whole paths whenever screening is incomplete.
func TestHeldPathScreenIncomplete(t *testing.T) {
	secret := syntheticSecret(t)
	for _, c := range []struct {
		name   string
		screen *outputScreen
		path   string
	}{
		{"credential after display limit", newOutputScreen([][]byte{[]byte(secret)}), strings.Repeat("x", 210) + secret},
		{"incomplete", &outputScreen{withhold: "unreadable"}, "plain"},
		{"skipped credentials", &outputScreen{changed: 1}, "plain"},
		{"released", &outputScreen{released: true}, "plain"},
		{"missing", nil, "plain"},
		{"record limit", testScreen(), strings.Repeat("x", screenHoldBound+1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, ok := screenedName(c.screen, c.path); ok || got != withheldPath {
				t.Fatal("path was disclosed")
			}
		})
	}
}

// TestBwrapUnknownBaselineWarning keeps a checkout entry whose baseline lookup failed.
func TestBwrapUnknownBaselineWarning(t *testing.T) {
	ws := wsTempDir(t)
	if err := os.Symlink("elsewhere", filepath.Join(ws, ".git")); err != nil {
		t.Fatal(err)
	}
	link := gitReadlink
	t.Cleanup(func() { gitReadlink = link })
	gitReadlink = func(string) (string, error) { return "", unix.EIO }
	b := testBwrapBackend(&captureRunner{}, &captureStarter{}, SandboxConfig{Runtime: SandboxRuntimeBwrap}, 0)
	_, before, err := b.prepare(bwrapTestSpec(t, ws))
	if err != nil {
		t.Fatal(err)
	}
	defer closePin(before)
	gitReadlink = link
	got := b.holdNewGit(before, testScreen())
	want := "WARNING: could not compare .git with its state before the run (input/output error); do not run git there until you have checked it"
	if !slices.Equal(got, []string{want}) {
		t.Fatalf("notes %q, want %q", got, want)
	}
	if target, err := os.Readlink(filepath.Join(ws, ".git")); err != nil || target != "elsewhere" {
		t.Fatalf("link target %q, error %v", target, err)
	}
}

// TestHeldNoteScreensDisplayedPath checks the final note after quoting and shortening.
func TestHeldNoteScreensDisplayedPath(t *testing.T) {
	for _, tc := range []struct{ name, path, key string }{
		{"escaped bytes", "work/\x1b[32m-file", `\x1b[32m`},
		{"CSI boundary", "work/\x1b[32m-file", "32m-file"},
		{"original beyond display", strings.Repeat("x", noteNameMax) + "\x1b[32m-file", "32m-file"},
		{"shortened bytes", strings.Repeat("x", noteNameMax+8), "xxx…[cut]"},
		{"credential directory", ".ssh/project/.git", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := wsTempDir(t)
			l := sandboxGitLayout{root: root, screen: newOutputScreen([][]byte{[]byte(tc.key)}),
				unlisted: []string{filepath.Join(root, tc.path)}}
			notes := holdGitMachinery(l, nil, "r")
			if !slices.Equal(notes, []string{"WARNING: could not check for new git metadata at " + withheldPath}) {
				t.Errorf("notes %q", notes)
			}
		})
	}
}

// TestHeldNotesCountWarningRenames includes successful renames reported as warnings.
func TestHeldNotesCountWarningRenames(t *testing.T) {
	var notes heldNotes
	for range heldNotesMax {
		notes.add("WARNING: could not check a directory", false)
	}
	notes.add("WARNING: moved a HEAD that was there before the run", true)
	lines := notes.lines("r")
	want := "… and 2 more notes (1 reporting renames, 2 with warnings, run r); look for *.golem-held-r* to find renamed entries"
	if got := lines[len(lines)-1]; got != want {
		t.Errorf("summary %q, want %q", got, want)
	}
}
