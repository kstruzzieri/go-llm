package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// buildCloneFixture creates a canonical-like tree exercising every clone
// policy branch and returns its root.
func buildCloneFixture(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mk := func(rel string, data string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	mk("top.txt", "top", 0o644)
	mk("bin/tool.sh", "#!/bin/sh\n", 0o755)
	mk("wide.txt", "wide", 0o777)
	mk("nested/deep/leaf.txt", "leaf", 0o600)
	// .git at top level and nested must both be excluded.
	mk(".git/config", "canonical git", 0o644)
	mk("sub/.git/HEAD", "nested git", 0o644)
	mk("sub/kept.txt", "kept", 0o644)
	// Symlink matrix.
	if err := os.Symlink("top.txt", filepath.Join(src, "link-internal-rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "bin/tool.sh"), filepath.Join(src, "link-internal-abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("no-such-file", filepath.Join(src, "link-dangling-inside")); err != nil {
		t.Fatal(err)
	}
	if scratchIdentitySupported {
		if err := os.Symlink("../outside-nowhere", filepath.Join(src, "link-dangling-outside")); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func cloneFixtureConfig() ScratchConfig {
	cfg, err := normalizeScratchConfig(ScratchConfig{Enabled: true})
	if err != nil {
		panic(err)
	}
	return cfg
}

func TestCloneTreeFidelity(t *testing.T) {
	src := buildCloneFixture(t)
	var outside string
	if scratchIdentitySupported {
		// External resolvable relative link: target lives outside the root.
		outside = filepath.Join(filepath.Dir(src), "outside-target.txt")
		if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(outside) })
		if err := os.Symlink("../"+filepath.Base(outside), filepath.Join(src, "link-external-rel")); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatalf("snapshotCanonical: %v", err)
	}

	// Regular files: bytes and permission bits preserved.
	for rel, want := range map[string]struct {
		data string
		mode os.FileMode
	}{
		"top.txt":              {"top", 0o644},
		"bin/tool.sh":          {"#!/bin/sh\n", 0o755},
		"wide.txt":             {"wide", 0o777},
		"nested/deep/leaf.txt": {"leaf", 0o600},
		"sub/kept.txt":         {"kept", 0o644},
	} {
		p := filepath.Join(dst, rel)
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != want.data {
			t.Fatalf("%s content = %q, want %q", rel, data, want.data)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want.mode {
			t.Fatalf("%s mode = %v, want %v", rel, fi.Mode().Perm(), want.mode)
		}
	}

	// .git excluded at every depth.
	for _, rel := range []string{".git", "sub/.git"} {
		if _, err := os.Lstat(filepath.Join(dst, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s must be excluded from the clone, lstat err=%v", rel, err)
		}
	}

	// Internal relative link: verbatim.
	if got, err := os.Readlink(filepath.Join(dst, "link-internal-rel")); err != nil || got != "top.txt" {
		t.Fatalf("internal relative link = %q err=%v, want top.txt", got, err)
	}
	// Internal absolute link: rewritten to a relative target inside the clone.
	got, err := os.Readlink(filepath.Join(dst, "link-internal-abs"))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(got) || strings.Contains(got, src) {
		t.Fatalf("internal absolute link must be rewritten relative, got %q", got)
	}
	if resolved, err := os.Stat(filepath.Join(dst, "link-internal-abs")); err != nil || !resolved.Mode().IsRegular() {
		t.Fatalf("rewritten internal absolute link must resolve inside the clone: %v", err)
	}
	// External resolvable relative link: absolutized to the original resolved
	// target (EvalSymlinks-normalized, e.g. /var -> /private/var on macOS).
	if scratchIdentitySupported {
		wantExternal, err := filepath.EvalSymlinks(outside)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := os.Readlink(filepath.Join(dst, "link-external-rel")); err != nil || got != wantExternal {
			t.Fatalf("external relative link = %q err=%v, want %q", got, err, wantExternal)
		}
	}
	// Dangling links (A1): inside stays relative, outside is absolutized with
	// existing ancestor aliases resolved; neither fails the snapshot.
	if got, err := os.Readlink(filepath.Join(dst, "link-dangling-inside")); err != nil || got != "no-such-file" {
		t.Fatalf("dangling inside link = %q err=%v, want verbatim", got, err)
	}
	if scratchIdentitySupported {
		wantParent, err := filepath.EvalSymlinks(filepath.Dir(src))
		if err != nil {
			t.Fatal(err)
		}
		wantOutside := filepath.Join(wantParent, "outside-nowhere")
		if got, err := os.Readlink(filepath.Join(dst, "link-dangling-outside")); err != nil || got != wantOutside {
			t.Fatalf("dangling outside link = %q err=%v, want %q", got, err, wantOutside)
		}
	}
}

func TestCloneTreeRejectsExternalDirectorySymlink(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	external := filepath.Join(parent, "external")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(external, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "live.txt"), []byte("canonical"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(src, "live.txt"), filepath.Join(external, "back")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(src, "through")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err == nil {
		t.Fatal("external directory target with an unsafe descendant must fail closed")
	}
}

func TestCloneTreeReadOnlyDirectory(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ro/f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(src, "ro"), 0o755) })

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatalf("snapshotCanonical with read-only dir: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "ro"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o555 {
		t.Fatalf("read-only dir mode = %v, want 0555", fi.Mode().Perm())
	}
	if data, err := os.ReadFile(filepath.Join(dst, "ro/f.txt")); err != nil || string(data) != "x" {
		t.Fatalf("file under read-only dir: %q err=%v", data, err)
	}
	// Restore so TempDir cleanup works everywhere.
	_ = os.Chmod(filepath.Join(dst, "ro"), 0o755)
}

func TestCloneTreeFallbackCopy(t *testing.T) {
	src := buildCloneFixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	unsupported := func(f *os.File, target string) error { return errScratchCloneUnsupported }
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), unsupported); err != nil {
		t.Fatalf("snapshotCanonical with copy fallback: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "nested/deep/leaf.txt")); err != nil || string(data) != "leaf" {
		t.Fatalf("fallback copy content: %q err=%v", data, err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "bin/tool.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("fallback copy mode = %v, want 0755 despite umask", fi.Mode().Perm())
	}
}

func TestCloneTreeRejectsExternalSymlinkLoop(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(parent, "loop-a")
	b := filepath.Join(parent, "loop-b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, filepath.Join(src, "external-loop")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err == nil {
		t.Fatal("unresolvable external loop must fail closed")
	}
}

func TestCopyFromHandleRespectsCanceledContext(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 1<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	target := filepath.Join(t.TempDir(), "copy")
	if err := copyFromHandle(ctx, f, target, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled fallback copy error = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled fallback copy left a partial target: %v", err)
	}
}

func TestCopyFromHandleRejectsGrowthBeyondExpectedSize(t *testing.T) {
	src := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(src, []byte(strings.Repeat("x", 2<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	target := filepath.Join(t.TempDir(), "copy")
	if err := copyFromHandle(context.Background(), f, target, 1<<20); !errors.Is(err, errSnapshotDrift) {
		t.Fatalf("grown fallback source error = %v, want errSnapshotDrift", err)
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("grown fallback source left a partial target: %v", err)
	}
}

func TestCloneTreeArbitraryCloneErrorIsFatal(t *testing.T) {
	src := buildCloneFixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	denied := func(f *os.File, target string) error { return syscall.EACCES }
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), denied); err == nil {
		t.Fatal("arbitrary clone error must be fatal, not a silent copy fallback")
	}
}

func TestCloneTreeEntryLimit(t *testing.T) {
	src := buildCloneFixture(t)
	cfg := cloneFixtureConfig()
	cfg.MaxWorkspaceFiles = 3
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cfg, cloneFile); err == nil {
		t.Fatal("entry limit not enforced")
	}
}

func TestCloneTreeEntryLimitAllowsExactBound(t *testing.T) {
	src := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(src, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := cloneFixtureConfig()
	cfg.MaxWorkspaceFiles = 3 // root plus two files
	dst := filepath.Join(t.TempDir(), "clone")
	man, err := snapshotCanonical(context.Background(), src, dst, cfg, cloneFile)
	if err != nil {
		t.Fatalf("exact entry bound rejected: %v", err)
	}
	if len(man.entries) != 3 {
		t.Fatalf("manifest entries = %d, want 3", len(man.entries))
	}
}

func TestReadScratchDirNamesRejectsWideDirectoryAtLimit(t *testing.T) {
	src := t.TempDir()
	for i := 0; i < 256; i++ {
		if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("entry-%03d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readScratchDirNames(context.Background(), root, ".", fi, 1, 2); err == nil {
		t.Fatal("wide directory bypassed the entry limit")
	}
}

func TestCloneTreeByteLimit(t *testing.T) {
	src := buildCloneFixture(t)
	cfg := cloneFixtureConfig()
	cfg.MaxWorkspaceBytes = 4
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cfg, cloneFile); err == nil {
		t.Fatal("workspace byte limit not enforced")
	}
}

func TestCloneTreeCancellation(t *testing.T) {
	src := buildCloneFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(ctx, src, dst, cloneFixtureConfig(), cloneFile); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot must fail with context.Canceled, got %v", err)
	}
}

func TestCloneTreeTimeout(t *testing.T) {
	src := buildCloneFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(ctx, src, dst, cloneFixtureConfig(), cloneFile); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out snapshot must fail with DeadlineExceeded, got %v", err)
	}
}

func TestSnapshotDirectoryModesStopOnCancellation(t *testing.T) {
	src := t.TempDir()
	dir := filepath.Join(src, "readonly")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("readonly/file", filepath.Join(src, "aa-link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	ctx, cancel := context.WithCancel(context.Background())
	cloneThenCancel := func(f *os.File, target string) error {
		err := cloneFile(f, target)
		cancel()
		return err
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotOnce(ctx, src, dst, cloneFixtureConfig(), cloneThenCancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot error = %v, want context.Canceled", err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "readonly"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode work continued after cancellation: %v", fi.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(dst, "aa-link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deferred symlink materialized after cancellation: %v", err)
	}
}

// TestSnapshotDriftRetriesOnceThenFails proves the pre/post manifest check:
// a source that changes during every pass fails closed after one retry,
// and a source that changes during only the first pass succeeds on retry.
func TestSnapshotDriftRetriesOnceThenFails(t *testing.T) {
	src := buildCloneFixture(t)
	victim := filepath.Join(src, "top.txt")

	passes := 0
	mutating := func(f *os.File, target string) error {
		if filepath.Base(target) == "wide.txt" {
			// Grow the victim mid-pass so the post-pass manifest rewalk sees
			// size drift on every pass.
			if err := os.WriteFile(victim, []byte(strings.Repeat("x", 10+passes)), 0o644); err != nil {
				t.Fatal(err)
			}
			passes++
		}
		return cloneFile(f, target)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), mutating); err == nil {
		t.Fatal("persistent source drift must fail the snapshot")
	}
	if passes != 2 {
		t.Fatalf("snapshot must retry the whole pass exactly once, observed %d passes", passes)
	}

	// Drift during the first pass only: retry succeeds.
	firstOnly := true
	oneShot := func(f *os.File, target string) error {
		if firstOnly && filepath.Base(target) == "wide.txt" {
			firstOnly = false
			if err := os.WriteFile(victim, []byte("stable-now"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return cloneFile(f, target)
	}
	dst2 := filepath.Join(t.TempDir(), "clone2")
	if _, err := snapshotCanonical(context.Background(), src, dst2, cloneFixtureConfig(), oneShot); err != nil {
		t.Fatalf("one-pass drift must succeed on retry: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst2, "top.txt")); err != nil || string(data) != "stable-now" {
		t.Fatalf("retry must re-clone the settled content, got %q err=%v", data, err)
	}
}

func TestSnapshotManifestRecordsEntries(t *testing.T) {
	src := buildCloneFixture(t)
	dst := filepath.Join(t.TempDir(), "clone")
	man, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]snapshotEntry{}
	for _, e := range man.entries {
		byPath[e.path] = e
	}
	if _, ok := byPath[".git/config"]; ok {
		t.Fatal("manifest must not record excluded .git entries")
	}
	e, ok := byPath["top.txt"]
	if !ok {
		t.Fatal("manifest missing top.txt")
	}
	if !e.typ.IsRegular() || e.size != int64(len("top")) {
		t.Fatalf("manifest entry wrong: %+v", e)
	}
	if _, ok := byPath["link-dangling-inside"]; !ok {
		t.Fatal("manifest must record symlinks")
	}
}

// --- review round: confirmed findings ---

// TestCloneTreeReentrantSymlinkStable pins the double-clone idempotence of
// symlink rewriting: a relative target that lexically leaves and re-enters
// the root ("../<rootname>/file") must normalize to the same in-root
// relative form in BOTH passes, resolve inside the work tree, and produce
// zero phantom diff entries.
func TestCloneTreeReentrantSymlinkStable(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "proj")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "top.txt"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../proj/top.txt", filepath.Join(src, "reentrant")); err != nil {
		t.Fatal(err)
	}
	cfg := cloneFixtureConfig()
	ref := filepath.Join(t.TempDir(), "reference")
	man, err := snapshotCanonical(context.Background(), src, ref, cfg, cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "workspace")
	if _, err := snapshotCanonical(context.Background(), ref, work, cfg, cloneFile); err != nil {
		t.Fatal(err)
	}
	refTarget, err := os.Readlink(filepath.Join(ref, "reentrant"))
	if err != nil {
		t.Fatal(err)
	}
	workTarget, err := os.Readlink(filepath.Join(work, "reentrant"))
	if err != nil {
		t.Fatal(err)
	}
	if refTarget != workTarget {
		t.Fatalf("rewrite is not idempotent across passes: ref=%q work=%q", refTarget, workTarget)
	}
	if fi, err := os.Stat(filepath.Join(work, "reentrant")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("re-entrant link must resolve inside the work tree: %v", err)
	}
	out, err := diffTrees(context.Background(), ref, work, man, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.changes) != 0 {
		t.Fatalf("untouched trees must diff empty, got phantom changes: %+v", out.changes)
	}
}

// TestCloneTreeWorkspaceAliasSymlinkStaysIsolated pins alias containment: an
// absolute target spelled through a symlink to the canonical workspace must
// be rewritten back into the clone, never preserved as a link to live files.
func TestCloneTreeWorkspaceAliasSymlinkStaysIsolated(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(src, "live.txt")
	if err := os.WriteFile(live, []byte("canonical"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Skipf("workspace aliases unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(alias, "live.txt"), filepath.Join(src, "through-alias")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "through-alias"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(live); err != nil || string(got) != "canonical" {
		cloneTarget, _ := os.Readlink(filepath.Join(dst, "through-alias"))
		t.Fatalf("clone alias escaped to canonical file: target=%q got %q err=%v", cloneTarget, got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "live.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("clone alias did not resolve inside clone: got %q err=%v", got, err)
	}
}

func TestCloneTreeWorkspaceAliasDanglingLeafStaysIsolated(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	src, err = CanonicalWorkspaceRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Skipf("workspace aliases unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(alias, "created.txt"), filepath.Join(src, "through-alias")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "through-alias"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(src, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling alias created a canonical file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "created.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("dangling alias did not resolve inside clone: got %q err=%v", got, err)
	}
}

func TestRewriteSymlinkTargetUsesResolvedLexicalContainmentWithoutIdentity(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	var err error
	src, err = CanonicalWorkspaceRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Skipf("workspace aliases unsupported: %v", err)
	}
	got, err := rewriteSymlinkTarget(context.Background(), src, "through-alias", filepath.Join(alias, "created.txt"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "created.txt" {
		t.Fatalf("resolved lexical alias rewrite = %q, want created.txt", got)
	}
}

func TestCloneTreeWorkspaceSubdirectoryAliasDanglingLeafStaysIsolated(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	sub := filepath.Join(src, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "sub-alias")
	if err := os.Symlink(sub, alias); err != nil {
		t.Skipf("workspace aliases unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(alias, "created.txt"), filepath.Join(src, "through-alias")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "through-alias"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(sub, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("subdirectory alias created a canonical file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "sub", "created.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("subdirectory alias did not resolve inside clone: got %q err=%v", got, err)
	}
}

func TestCloneTreeWorkspaceDanglingAliasChainStaysIsolated(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	rootAlias := filepath.Join(parent, "project-alias")
	if err := os.Symlink(src, rootAlias); err != nil {
		t.Skipf("workspace aliases unsupported: %v", err)
	}
	leafAlias := filepath.Join(parent, "leaf-alias")
	if err := os.Symlink(filepath.Join(rootAlias, "created.txt"), leafAlias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(leafAlias, filepath.Join(src, "through-alias")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "through-alias"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(src, "created.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling alias chain created a canonical file: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "created.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("dangling alias chain did not resolve inside clone: got %q err=%v", got, err)
	}
}

// TestSnapshotManifestRecordsSpecialBits pins full-mode evidence: a setgid
// directory keeps its special bit in the manifest so promotion's live
// comparison does not false-refuse, and special-bit drift is detectable.
func TestSnapshotManifestRecordsSpecialBits(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "sg"), 0o755|os.ModeSetgid); err != nil {
		t.Skipf("cannot set setgid here: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(src, "sg"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 {
		t.Skip("filesystem dropped setgid")
	}
	dst := filepath.Join(t.TempDir(), "clone")
	man, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man.entries {
		if e.path == "sg" {
			if e.fullMode&os.ModeSetgid == 0 {
				t.Fatalf("manifest must record special bits, got fullMode=%v", e.fullMode)
			}
			return
		}
	}
	t.Fatal("manifest missing sg entry")
}
