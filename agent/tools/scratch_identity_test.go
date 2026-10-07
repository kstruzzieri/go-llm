//go:build linux || darwin

package tools

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// The scratch source checks (#553) bind the approved object at the last
// check before the command runs in the clone. They do not bind bytes, do not
// make launch atomic, and are not a point-in-time coherence proof; same-UID
// mutation after the checks is the accepted residual (#484). These fixtures
// substitute an object after recheckExecPlan approved it and restore the host
// before validation, so only a check against the snapshot's source manifest
// can see the substitution.

// approvedScript is executable A, the object the swap fixtures approve.
const approvedScript = "#!/bin/sh\n# approved A\nexit 0\n"

const (
	wantRootMismatch = "does not match approved workspace root"
	wantDirMismatch  = "does not match approved working directory"
	wantExeMismatch  = "does not match approved executable"
)

// scratchSeamReader fires fn once on the first entropy read. beginScratchSession
// draws the session id right after admission and before creating either
// snapshot root. fn runs under the store mutex and must not call store methods.
type scratchSeamReader struct {
	io.Reader
	once sync.Once
	fn   func()
}

func (r *scratchSeamReader) Read(p []byte) (int, error) {
	r.once.Do(r.fn)
	return r.Reader.Read(p)
}

// sourceRunner and sourceStarter wrap the platform capture delegates and
// record the bytes at the rewritten executable while the session exists.
type sourceRunner struct {
	captureRunner
	marker []byte
}

func (r *sourceRunner) Run(ctx context.Context, s execSpec) (execResult, error) {
	r.marker, _ = os.ReadFile(s.Path)
	return r.captureRunner.Run(ctx, s)
}

type sourceStarter struct {
	captureStarter
	marker []byte
}

func (s *sourceStarter) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	s.marker, _ = os.ReadFile(spec.Path)
	return s.captureStarter.Start(spec, stdout, stderr)
}

// scratchSourceHarness drives one scratch tool through the production
// Plan -> Invoke path with a capture delegate in place of the host process.
type scratchSourceHarness struct {
	rt     *scratchRuntime
	plan   func(raw string)
	invoke func(raw string) agent.ToolResult
	calls  func() int
	marker func() []byte
}

// newScratchSourceFixture builds a workspace at <private parent>/ws holding a
// root-level regular file (so reference->work copies reach the clone seam),
// dir/, and the executable tool.sh, then wires the foreground or background
// scratch tool around a capture delegate. The returned root is canonical; its
// parent is private to the test, so parked fixtures stay under t.TempDir.
func newScratchSourceFixture(t *testing.T, background bool) (*scratchSourceHarness, string) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(filepath.Join(ws, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, data := range map[string]string{"keep.txt": "keep", "dir/inner.txt": "inner"} {
		if err := os.WriteFile(filepath.Join(ws, rel), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(ws, "tool.sh"), approvedScript)

	h := &scratchSourceHarness{}
	if !background {
		rc, rt, _ := scratchExecTools(t, ws, ScratchConfig{Enabled: true})
		runner := &sourceRunner{}
		rc.runner = runner
		h.rt = rt
		h.plan = func(raw string) {
			t.Helper()
			if _, err := rc.Plan(context.Background(), json.RawMessage(raw)); err != nil {
				t.Fatalf("plan: %v", err)
			}
		}
		h.invoke = func(raw string) agent.ToolResult {
			t.Helper()
			res, err := rc.Invoke(context.Background(), json.RawMessage(raw))
			if err != nil {
				t.Fatalf("invoke: %v", err)
			}
			return res
		}
		h.calls = func() int { return runner.called }
		h.marker = func() []byte { return runner.marker }
		return h, rt.root
	}
	starter := &sourceStarter{captureStarter: captureStarter{proc: new(fakeProcess)}}
	manager := newBackgroundManager(starter, cryptoRandReader(t))
	sc, rt := scratchBackgroundTools(t, ws, ScratchConfig{Enabled: true}, manager)
	// Registered after the temp roots, so Shutdown runs before their removal.
	t.Cleanup(manager.Shutdown)
	h.rt = rt
	h.plan = func(raw string) {
		t.Helper()
		if _, err := sc.Plan(context.Background(), json.RawMessage(raw)); err != nil {
			t.Fatalf("plan: %v", err)
		}
	}
	h.invoke = func(raw string) agent.ToolResult {
		t.Helper()
		res, err := sc.Invoke(context.Background(), json.RawMessage(raw))
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		return res
	}
	h.calls = func() int { return starter.called }
	h.marker = func() []byte { return starter.marker }
	return h, rt.root
}

// assertScratchSourceRejected requires the named source check's error, no
// delegate call, and nothing left behind: no temp roots, no store entries,
// and the admission slot released.
func assertScratchSourceRejected(t *testing.T, h *scratchSourceHarness, res agent.ToolResult, want string) {
	t.Helper()
	if n := h.calls(); n != 0 {
		marker := h.marker()
		marker = marker[:min(len(marker), 64)]
		t.Fatalf("delegate reached %d time(s) with rewritten executable bytes %q; want rejection %q, result: %s", n, marker, want, res.Content)
	}
	if !res.IsError || !strings.Contains(res.Content, want) {
		t.Fatalf("want rejection %q, got IsError=%v: %s", want, res.IsError, res.Content)
	}
	assertScratchNoLeak(t, h.rt)
}

func assertScratchNoLeak(t *testing.T, rt *scratchRuntime) {
	t.Helper()
	entries, err := os.ReadDir(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected session leaked scratch roots: %v", entries)
	}
	rt.store.mu.Lock()
	pending, completed := len(rt.store.pending), len(rt.store.completed)
	rt.store.mu.Unlock()
	if pending != 0 || completed != 0 {
		t.Fatalf("rejected session leaked store entries: pending=%d completed=%d", pending, completed)
	}
	if n := len(rt.slots); n != 0 {
		t.Fatalf("rejected session kept %d admission slot(s)", n)
	}
}

// assertScratchFreshRun proves the restored fixture is healthy: the same
// command runs once more, through the delegate.
func assertScratchFreshRun(t *testing.T, h *scratchSourceHarness, raw string) {
	t.Helper()
	h.plan(raw)
	res := h.invoke(raw)
	if res.IsError || h.calls() != 1 {
		t.Fatalf("fresh session after restore: calls=%d IsError=%v: %s", h.calls(), res.IsError, res.Content)
	}
}

// forEachScratchMode runs fn as a foreground and a background subtest.
func forEachScratchMode(t *testing.T, fn func(t *testing.T, background bool)) {
	t.Helper()
	for _, background := range []bool{false, true} {
		name := "foreground"
		if background {
			name = "background"
		}
		t.Run(name, func(t *testing.T) { fn(t, background) })
	}
}

// externalExecutable writes a unique executable outside every workspace, in
// a symlink-resolved private temp dir, and returns its path.
func externalExecutable(t *testing.T, name string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	writeExecutable(t, p, "#!/bin/sh\n# "+name+"\n")
	return p
}

func mustRename(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// isWorkRootCopy reports whether a clone-seam destination is a root-level
// file of the work tree (reference->work copy); "tree" is the reference.
func isWorkRootCopy(dst string) bool { return filepath.Base(filepath.Dir(dst)) == "workspace" }

func TestScratchSourceRootReplaced(t *testing.T) {
	forEachScratchMode(t, func(t *testing.T, background bool) {
		h, root := newScratchSourceFixture(t, background)
		parent := filepath.Dir(root)
		saved, repl := filepath.Join(parent, "saved"), filepath.Join(parent, "repl")
		// Replacement root B: a root-level regular file, and no dir/ so
		// the approved cwd can be moved in (identity unchanged).
		if err := os.Mkdir(repl, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repl, "keep.txt"), []byte("B"), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := `{"argv":["/bin/sh","-c","true"],"dir":"dir"}`
		h.plan(raw)
		approvedRoot := mustLstat(t, root)
		approvedDir := mustLstat(t, filepath.Join(root, "dir"))

		fired, restored := 0, false
		t.Cleanup(func() {
			if fired > 0 && !restored {
				_ = os.Rename(filepath.Join(root, "dir"), filepath.Join(saved, "dir"))
				_ = os.Rename(root, repl)
				_ = os.Rename(saved, root)
			}
		})
		h.rt.store.random = &scratchSeamReader{Reader: cryptorand.Reader, fn: func() {
			fired++
			if len(h.rt.slots) != 1 {
				t.Errorf("seam fired with %d admission slots owned, want 1", len(h.rt.slots))
			}
			mustRename(t, root, saved)
			mustRename(t, repl, root)
			mustRename(t, filepath.Join(saved, "dir"), filepath.Join(root, "dir"))
			if !os.SameFile(mustLstat(t, filepath.Join(root, "dir")), approvedDir) {
				t.Fatal("moved cwd must keep the approved identity")
			}
		}}
		h.rt.clone = func(f *os.File, dst string) error {
			if fired > 0 && !restored && isWorkRootCopy(dst) {
				restored = true
				mustRename(t, filepath.Join(root, "dir"), filepath.Join(saved, "dir"))
				mustRename(t, root, repl)
				mustRename(t, saved, root)
			}
			return cloneFile(f, dst)
		}

		res := h.invoke(raw)
		if fired != 1 || !restored {
			t.Fatalf("schedule did not run: seam fired %d, restored %v", fired, restored)
		}
		if !os.SameFile(mustLstat(t, root), approvedRoot) || !os.SameFile(mustLstat(t, filepath.Join(root, "dir")), approvedDir) {
			t.Fatal("host root and cwd must be restored to the approved objects")
		}
		assertScratchSourceRejected(t, h, res, wantRootMismatch)
		assertScratchFreshRun(t, h, raw)
	})
}

func TestScratchSourceCwdReplaced(t *testing.T) {
	forEachScratchMode(t, func(t *testing.T, background bool) {
		h, root := newScratchSourceFixture(t, background)
		parent := filepath.Dir(root)
		cwd := filepath.Join(root, "dir")
		saved, repl := filepath.Join(parent, "saved-dir"), filepath.Join(parent, "repl-dir")
		if err := os.Mkdir(repl, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repl, "inner.txt"), []byte("B"), 0o644); err != nil {
			t.Fatal(err)
		}
		raw := `{"argv":["/bin/sh","-c","true"],"dir":"dir"}`
		h.plan(raw)
		approvedRoot := mustLstat(t, root)
		approvedDir := mustLstat(t, cwd)

		fired, restored := 0, false
		t.Cleanup(func() {
			if fired > 0 && !restored {
				_ = os.Rename(cwd, repl)
				_ = os.Rename(saved, cwd)
			}
		})
		h.rt.store.random = &scratchSeamReader{Reader: cryptorand.Reader, fn: func() {
			fired++
			if len(h.rt.slots) != 1 {
				t.Errorf("seam fired with %d admission slots owned, want 1", len(h.rt.slots))
			}
			mustRename(t, cwd, saved)
			mustRename(t, repl, cwd)
		}}
		h.rt.clone = func(f *os.File, dst string) error {
			if fired > 0 && !restored && isWorkRootCopy(dst) {
				restored = true
				mustRename(t, cwd, repl)
				mustRename(t, saved, cwd)
			}
			return cloneFile(f, dst)
		}

		res := h.invoke(raw)
		if fired != 1 || !restored {
			t.Fatalf("schedule did not run: seam fired %d, restored %v", fired, restored)
		}
		if !os.SameFile(mustLstat(t, root), approvedRoot) {
			t.Fatal("root identity must be unchanged by the cwd swap")
		}
		if !os.SameFile(mustLstat(t, cwd), approvedDir) {
			t.Fatal("host cwd must be restored to the approved object")
		}
		assertScratchSourceRejected(t, h, res, wantDirMismatch)
		assertScratchFreshRun(t, h, raw)
	})
}

func TestScratchSourceExecutableReplaced(t *testing.T) {
	const markerB = "#!/bin/sh\n# substitute B\nexit 0\n"
	forEachScratchMode(t, func(t *testing.T, background bool) {
		h, root := newScratchSourceFixture(t, background)
		parent := filepath.Dir(root)
		exe := filepath.Join(root, "tool.sh")
		saved, repl := filepath.Join(parent, "saved-tool.sh"), filepath.Join(parent, "repl-tool.sh")
		writeExecutable(t, repl, markerB)
		raw := `{"argv":["./tool.sh"]}`
		h.plan(raw)
		approvedExe := execIdentityOf(t, exe)

		canonicalCopies, restored := 0, false
		t.Cleanup(func() {
			if canonicalCopies > 0 && !restored {
				_ = os.Rename(exe, repl)
				_ = os.Rename(saved, exe)
			}
		})
		h.rt.clone = func(f *os.File, dst string) error {
			if filepath.Base(dst) != "tool.sh" {
				return cloneFile(f, dst)
			}
			switch filepath.Base(filepath.Dir(dst)) {
			case "tree": // canonical -> reference
				canonicalCopies++
				fi, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if err := copyFromHandle(context.Background(), f, dst, fi.Size()); err != nil {
					t.Fatal(err)
				}
				if canonicalCopies == 1 {
					// A is fully copied; substitute B so the source drift
					// check retries and the accepted pass copies B.
					mustRename(t, exe, saved)
					mustRename(t, repl, exe)
				}
				return nil
			case "workspace": // reference -> work
				if canonicalCopies > 0 && !restored {
					restored = true
					mustRename(t, exe, repl)
					mustRename(t, saved, exe)
				}
			}
			return cloneFile(f, dst)
		}

		res := h.invoke(raw)
		if canonicalCopies != 2 || !restored {
			t.Fatalf("schedule did not run: %d canonical copies of tool.sh (want 2), restored %v", canonicalCopies, restored)
		}
		if !os.SameFile(execIdentityOf(t, exe), approvedExe) {
			t.Fatal("host executable must be restored to the approved object")
		}
		assertScratchSourceRejected(t, h, res, wantExeMismatch)
		assertScratchFreshRun(t, h, raw)
		if got := string(h.marker()); got != approvedScript {
			t.Fatalf("fresh session ran %q, want the approved executable A", got)
		}
	})
}

// TestScratchSourceCwdAliasRunsCommand: Workspace.resolveDir keeps the
// caller's spelling, so a case alias of a real directory is an approved cwd
// on a case-insensitive filesystem and must still run under scratch.
func TestScratchSourceCwdAliasRunsCommand(t *testing.T) {
	h, root := newScratchSourceFixture(t, false)
	if fi, err := os.Stat(filepath.Join(root, "DIR")); err != nil || !os.SameFile(fi, execIdentityOf(t, filepath.Join(root, "dir"))) {
		t.Skip("filesystem does not alias DIR to dir")
	}
	raw := `{"argv":["/bin/sh","-c","true"],"dir":"DIR"}`
	h.plan(raw)
	res := h.invoke(raw)
	if res.IsError || h.calls() != 1 {
		t.Fatalf("aliased cwd must run once: calls=%d IsError=%v: %s", h.calls(), res.IsError, res.Content)
	}
}

// TestScratchSourceBeginControls drives beginScratchSession directly with
// approved specs: legitimate spellings pass, substitutions and excluded or
// unprovable sources fail the named check.
func TestScratchSourceBeginControls(t *testing.T) {
	// alias returns spelling when the filesystem resolves it to the same
	// object as target; otherwise only this subtest is skipped.
	alias := func(t *testing.T, target, spelling string) string {
		t.Helper()
		fi, err := os.Stat(spelling)
		if err != nil || !os.SameFile(fi, execIdentityOf(t, target)) {
			t.Skipf("filesystem does not alias %q to %q", spelling, target)
		}
		return spelling
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, canon string, spec *execSpec)
		want  string // "" = passes source validation
	}{
		{"unchanged workspace script", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "scripts/run.sh")
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, ""},
		{"absolute workspace symlink to internal script", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "link.sh")
			if err := os.Symlink(filepath.Join(canon, "scripts/run.sh"), spec.Path); err != nil {
				t.Fatal(err)
			}
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, ""},
		{"hard-link spelling sorted after its twin", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "scripts/run2.sh")
			if err := os.Link(filepath.Join(canon, "scripts/run.sh"), spec.Path); err != nil {
				t.Fatal(err)
			}
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, ""},
		{"case alias of internal script", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = alias(t, filepath.Join(canon, "scripts/run.sh"), filepath.Join(canon, "SCRIPTS/RUN.SH"))
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, ""},
		{"normalization alias of internal script", func(t *testing.T, canon string, spec *execSpec) {
			writeExecutable(t, filepath.Join(canon, "scripts/café.sh"), "#!/bin/sh\n")
			spec.Path = alias(t, filepath.Join(canon, "scripts/café.sh"), filepath.Join(canon, "scripts/café.sh"))
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, ""},
		{"workspace symlink to unique external executable", func(t *testing.T, canon string, spec *execSpec) {
			target := externalExecutable(t, "ext.sh")
			spec.Path = filepath.Join(canon, "ext-link")
			if err := os.Symlink(target, spec.Path); err != nil {
				t.Fatal(err)
			}
			spec.ExeIdentity = execIdentityOf(t, target)
		}, ""},
		{"workspace symlink retargeted to another external executable", func(t *testing.T, canon string, spec *execSpec) {
			first, second := externalExecutable(t, "first.sh"), externalExecutable(t, "second.sh")
			spec.Path = filepath.Join(canon, "ext-link")
			if err := os.Symlink(first, spec.Path); err != nil {
				t.Fatal(err)
			}
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
			if err := os.Symlink(second, spec.Path+".new"); err != nil {
				t.Fatal(err)
			}
			mustRename(t, spec.Path+".new", spec.Path)
		}, wantExeMismatch},
		{"lexically divergent internal link", func(t *testing.T, canon string, spec *execSpec) {
			// On the host a/l -> s/../../x resolves through a/s -> deep/er
			// to a/x (A). rewriteSymlinkTarget cleans the target lexically
			// to root x (B), a pre-existing snapshot inaccuracy tracked in
			// #660; the validator must fail closed on it.
			a := filepath.Join(canon, "a")
			if err := os.MkdirAll(filepath.Join(a, "deep/er"), 0o755); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(a, "x"), "#!/bin/sh\n# A\n")
			writeExecutable(t, filepath.Join(canon, "x"), "#!/bin/sh\n# B\n")
			if err := os.Symlink("deep/er", filepath.Join(a, "s")); err != nil {
				t.Fatal(err)
			}
			spec.Path = filepath.Join(a, "l")
			if err := os.Symlink("s/../../x", spec.Path); err != nil {
				t.Fatal(err)
			}
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
			if !os.SameFile(spec.ExeIdentity, execIdentityOf(t, filepath.Join(a, "x"))) {
				t.Fatal("fixture: the host must resolve a/l to a/x")
			}
		}, wantExeMismatch},
		{"approved executable moved within the workspace", func(t *testing.T, canon string, spec *execSpec) {
			// The approved inode is still in the snapshot, but under
			// moved.sh; the approved spelling now names a different file.
			spec.Path = filepath.Join(canon, "scripts/run.sh")
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
			mustRename(t, spec.Path, filepath.Join(canon, "scripts/moved.sh"))
			writeExecutable(t, spec.Path+".new", "#!/bin/sh\n# B\n")
			mustRename(t, spec.Path+".new", spec.Path)
		}, wantExeMismatch},
		{"external PATH executable", func(t *testing.T, canon string, spec *execSpec) {}, ""},
		{"missing root identity", func(t *testing.T, canon string, spec *execSpec) {
			spec.RootIdentity = nil
		}, wantRootMismatch},
		{"root identity of another directory", func(t *testing.T, canon string, spec *execSpec) {
			spec.RootIdentity = execIdentityOf(t, filepath.Join(canon, "dir"))
		}, wantRootMismatch},
		{"missing cwd identity", func(t *testing.T, canon string, spec *execSpec) {
			spec.Dir = filepath.Join(canon, "dir")
			spec.DirIdentity = nil
		}, wantDirMismatch},
		{"case alias cwd", func(t *testing.T, canon string, spec *execSpec) {
			spec.Dir = alias(t, filepath.Join(canon, "dir"), filepath.Join(canon, "DIR"))
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
		}, ""},
		{"normalization alias cwd", func(t *testing.T, canon string, spec *execSpec) {
			if err := os.Mkdir(filepath.Join(canon, "café"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec.Dir = alias(t, filepath.Join(canon, "café"), filepath.Join(canon, "café"))
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
		}, ""},
		{"approved cwd moved within the workspace", func(t *testing.T, canon string, spec *execSpec) {
			// The approved inode is still in the snapshot, but under dir2;
			// the approved spelling now names a different directory.
			spec.Dir = filepath.Join(canon, "dir")
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
			mustRename(t, spec.Dir, filepath.Join(canon, "dir2"))
			if err := os.Mkdir(spec.Dir+".new", 0o755); err != nil {
				t.Fatal(err)
			}
			mustRename(t, spec.Dir+".new", spec.Dir)
		}, wantDirMismatch},
		{"approved cwd moved, symlink at the spelling", func(t *testing.T, canon string, spec *execSpec) {
			// The spelling still locates the copy of the approved directory
			// (through the link), as symlinked executable spellings do.
			spec.Dir = filepath.Join(canon, "dir")
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
			mustRename(t, spec.Dir, filepath.Join(canon, "dir2"))
			if err := os.Symlink("dir2", spec.Dir+".new"); err != nil {
				t.Fatal(err)
			}
			mustRename(t, spec.Dir+".new", spec.Dir)
		}, ""},
		{"cwd outside the workspace root", func(t *testing.T, canon string, spec *execSpec) {
			spec.Dir = t.TempDir()
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
		}, "escapes workspace root"},
		{"missing executable identity", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "scripts/run.sh")
			spec.ExeIdentity = nil
		}, wantExeMismatch},
		{"cwd under .git", func(t *testing.T, canon string, spec *execSpec) {
			spec.Dir = filepath.Join(canon, ".git/hooks")
			if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
				t.Fatal(err)
			}
			spec.DirIdentity = execIdentityOf(t, spec.Dir)
		}, wantDirMismatch},
		{"workspace executable under .git", func(t *testing.T, canon string, spec *execSpec) {
			if err := os.Mkdir(filepath.Join(canon, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			spec.Path = filepath.Join(canon, ".git/tool.sh")
			writeExecutable(t, spec.Path, "#!/bin/sh\n")
			spec.ExeIdentity = execIdentityOf(t, spec.Path)
		}, wantExeMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
			spec := testSpec(t, canon)
			tc.setup(t, canon, &spec)
			session, _, err := beginScratchSession(context.Background(), rt, spec)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("approved source rejected: %v", err)
				}
				session.discard()
				return
			}
			if err == nil {
				session.discard()
				t.Fatalf("want rejection %q, session began", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection %q, got %v", tc.want, err)
			}
			assertScratchNoLeak(t, rt)
			fresh, _, err := beginScratchSession(context.Background(), rt, testSpec(t, canon))
			if err != nil {
				t.Fatalf("fresh session after rejection: %v", err)
			}
			fresh.discard()
		})
	}
}

// TestScratchSourceStampRejectsNonRegularClone: source validation reads the
// reference, so a work clone that is not a regular file (a clone fault, or a
// same-UID change to the work tree) must fail the stamp, not be presented to
// the backend as the approved executable.
func TestScratchSourceStampRejectsNonRegularClone(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	spec := testSpec(t, canon)
	spec.Path = filepath.Join(canon, "scripts/run.sh")
	spec.ExeIdentity = execIdentityOf(t, spec.Path)
	faulted := false
	rt.clone = func(f *os.File, dst string) error {
		if filepath.Base(dst) == "run.sh" && filepath.Base(filepath.Dir(filepath.Dir(dst))) == "workspace" {
			faulted = true
			return os.Mkdir(dst, 0o700)
		}
		return cloneFile(f, dst)
	}
	session, _, err := beginScratchSession(context.Background(), rt, spec)
	if !faulted {
		t.Fatal("clone fault never reached the work copy of scripts/run.sh")
	}
	if err == nil {
		session.discard()
		t.Fatal("a directory in the work clone was stamped as the approved executable")
	}
	if !strings.Contains(err.Error(), wantExeMismatch) {
		t.Fatalf("want rejection %q, got %v", wantExeMismatch, err)
	}
	assertScratchNoLeak(t, rt)
}

// TestScratchSourceRejectsExcludedGitExecutable checks the validator itself
// over a real snapshot, which omits every .git entry: an internal executable
// spelling under .git has no reference copy. Through beginScratchSession the
// clone stamp would also reject it, so only this direct call discriminates
// the validator.
func TestScratchSourceRejectsExcludedGitExecutable(t *testing.T) {
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(canon, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(canon, ".git/tool.sh"), "#!/bin/sh\n")
	writeExecutable(t, filepath.Join(canon, "tool.sh"), "#!/bin/sh\n")
	reference := filepath.Join(t.TempDir(), "tree")
	man, err := snapshotCanonical(context.Background(), canon, reference, cloneFixtureConfig(), cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, rel, want string }{
		{"included executable", "tool.sh", ""},
		{"executable under .git", ".git/tool.sh", wantExeMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := execSpec{
				Path: filepath.Join(canon, tc.rel), Dir: canon, WorkspaceRoot: canon,
				ExeIdentity:  execIdentityOf(t, filepath.Join(canon, tc.rel)),
				DirIdentity:  execIdentityOf(t, canon),
				RootIdentity: execIdentityOf(t, canon),
			}
			err := validateScratchSource(man, spec, canon, reference)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("snapshotted executable rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection %q, got %v", tc.want, err)
			}
		})
	}
}

// noIdentityInfo is a FileInfo whose platform identity is unavailable, so
// statIdentity reports all-zero dev/ino.
type noIdentityInfo struct{ os.FileInfo }

func (noIdentityInfo) Sys() any { return nil }

// TestScratchSourceManifestRows checks validateScratchSource against
// synthetic source manifests: a missing entry, a wrong-type entry with the
// approved identity, or an all-zero identity fails the corresponding check.
func TestScratchSourceManifestRows(t *testing.T) {
	canon, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reference := t.TempDir() // a separate copy, as the real snapshot makes
	for _, base := range []string{canon, reference} {
		if err := os.Mkdir(filepath.Join(base, "dir"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(base, "tool.sh"), "#!/bin/sh\n")
	}
	rootFI := execIdentityOf(t, canon)
	dirFI := execIdentityOf(t, filepath.Join(canon, "dir"))
	exeFI := execIdentityOf(t, filepath.Join(canon, "tool.sh"))
	entry := func(path string, typ fs.FileMode, fi os.FileInfo) snapshotEntry {
		dev, ino, _, _ := statIdentity(fi)
		return snapshotEntry{path: path, typ: typ, dev: dev, ino: ino}
	}
	zero := func(path string, typ fs.FileMode) snapshotEntry { return snapshotEntry{path: path, typ: typ} }
	root, dir, exe := entry(".", fs.ModeDir, rootFI), entry("dir", fs.ModeDir, dirFI), entry("tool.sh", 0, exeFI)
	tests := []struct {
		name    string
		entries []snapshotEntry
		edit    func(*execSpec)
		want    string
	}{
		{"complete manifest", []snapshotEntry{root, dir, exe}, nil, ""},
		{"root entry missing", []snapshotEntry{dir, exe}, nil, wantRootMismatch},
		{"root entry wrong type", []snapshotEntry{entry(".", 0, rootFI), dir, exe}, nil, wantRootMismatch},
		{"root identity all zero", []snapshotEntry{zero(".", fs.ModeDir), dir, exe},
			func(s *execSpec) { s.RootIdentity = noIdentityInfo{rootFI} }, wantRootMismatch},
		{"cwd entry missing", []snapshotEntry{root, exe}, nil, wantDirMismatch},
		{"cwd entry wrong type", []snapshotEntry{root, entry("dir", 0, dirFI), exe}, nil, wantDirMismatch},
		{"cwd identity all zero", []snapshotEntry{root, zero("dir", fs.ModeDir), exe},
			func(s *execSpec) { s.DirIdentity = noIdentityInfo{dirFI} }, wantDirMismatch},
		{"executable entry missing", []snapshotEntry{root, dir}, nil, wantExeMismatch},
		{"executable entry is a directory", []snapshotEntry{root, dir, entry("tool.sh", fs.ModeDir, exeFI)}, nil, wantExeMismatch},
		{"executable entry is a symlink", []snapshotEntry{root, dir, entry("tool.sh", fs.ModeSymlink, exeFI)}, nil, wantExeMismatch},
		{"executable identity all zero", []snapshotEntry{root, dir, zero("tool.sh", 0)},
			func(s *execSpec) { s.ExeIdentity = noIdentityInfo{exeFI} }, wantExeMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := execSpec{
				Path: filepath.Join(canon, "tool.sh"), Dir: filepath.Join(canon, "dir"), WorkspaceRoot: canon,
				ExeIdentity: exeFI, DirIdentity: dirFI, RootIdentity: rootFI,
			}
			if tc.edit != nil {
				tc.edit(&spec)
			}
			err := validateScratchSource(snapshotManifest{entries: tc.entries}, spec, canon, reference)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("complete manifest rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection %q, got %v", tc.want, err)
			}
		})
	}
}

// TestScratchSourceStampsCloneExecutable pins the rewritten spec's
// ExeIdentity. A workspace-local spelling is rewritten into the clone and
// restamped with the object that clone path resolves to: the work clone of a
// workspace file (never the source), or the external target of an internal
// link. An external spelling is not rewritten and keeps its approval, even
// when the host object behind it is swapped after approval.
func TestScratchSourceStampsCloneExecutable(t *testing.T) {
	const (
		stampClone    = "work clone"
		stampTarget   = "external link target"
		stampApproved = "approved identity"
	)
	tests := []struct {
		name  string
		setup func(t *testing.T, canon string, spec *execSpec)
		swap  func(t *testing.T, spec execSpec) // after approval, before the session
		stamp string
	}{
		{"unchanged workspace script", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "scripts/run.sh")
		}, nil, stampClone},
		{"internal absolute symlink", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = filepath.Join(canon, "link.sh")
			if err := os.Symlink(filepath.Join(canon, "scripts/run.sh"), spec.Path); err != nil {
				t.Fatal(err)
			}
		}, nil, stampClone},
		{"workspace symlink to external executable", func(t *testing.T, canon string, spec *execSpec) {
			target := externalExecutable(t, "ext.sh")
			spec.Path = filepath.Join(canon, "ext-link")
			if err := os.Symlink(target, spec.Path); err != nil {
				t.Fatal(err)
			}
		}, nil, stampTarget},
		{"external PATH executable", func(t *testing.T, canon string, spec *execSpec) {}, nil, stampApproved},
		{"external executable swapped after approval", func(t *testing.T, canon string, spec *execSpec) {
			spec.Path = externalExecutable(t, "ext.sh")
		}, func(t *testing.T, spec execSpec) {
			writeExecutable(t, spec.Path+".new", "#!/bin/sh\n# B\n")
			mustRename(t, spec.Path+".new", spec.Path)
		}, stampApproved},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
			spec := testSpec(t, canon)
			tc.setup(t, canon, &spec)
			approved := execIdentityOf(t, spec.Path)
			spec.ExeIdentity = approved
			if tc.swap != nil {
				tc.swap(t, spec)
			}
			session, rewritten, err := beginScratchSession(context.Background(), rt, spec)
			if err != nil {
				t.Fatal(err)
			}
			cloneInfo, statErr := os.Stat(rewritten.Path)
			session.discard()
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !os.SameFile(rewritten.DirIdentity, spec.DirIdentity) || !os.SameFile(rewritten.RootIdentity, spec.RootIdentity) {
				t.Fatal("DirIdentity and RootIdentity must stay the approved source identities")
			}
			switch tc.stamp {
			case stampClone:
				if !os.SameFile(rewritten.ExeIdentity, cloneInfo) {
					t.Fatalf("ExeIdentity must be the work clone %q, got %v", rewritten.Path, rewritten.ExeIdentity)
				}
				if os.SameFile(cloneInfo, approved) {
					t.Fatal("the work clone must be a distinct object from the approved source")
				}
			case stampTarget:
				// Rewritten into the clone, where the link still resolves to
				// the approved external object; the restamp records it.
				if rewritten.Path == spec.Path {
					t.Fatalf("internal link spelling %q must be rewritten into the clone", spec.Path)
				}
				if !os.SameFile(rewritten.ExeIdentity, cloneInfo) || !os.SameFile(cloneInfo, approved) {
					t.Fatalf("ExeIdentity must be the external target the clone link resolves to, got %v", rewritten.ExeIdentity)
				}
			case stampApproved:
				if rewritten.Path != spec.Path {
					t.Fatalf("external spelling %q must not be rewritten, got %q", spec.Path, rewritten.Path)
				}
				if !os.SameFile(rewritten.ExeIdentity, approved) {
					t.Fatalf("external executable must keep its approved identity, got %v", rewritten.ExeIdentity)
				}
				if tc.swap == nil {
					return
				}
				if os.SameFile(cloneInfo, approved) {
					t.Fatal("fixture: the swap must install a different host object")
				}
				if os.SameFile(rewritten.ExeIdentity, cloneInfo) {
					t.Fatal("external executable was restamped with the object swapped in after approval")
				}
			}
		})
	}
}
