//go:build linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
