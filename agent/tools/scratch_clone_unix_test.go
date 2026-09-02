//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCloneTreeSkipsFIFO(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatalf("snapshot with FIFO present: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "pipe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FIFO must be skipped, lstat err=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Fatalf("regular sibling of FIFO must survive: %v", err)
	}
}

func TestCloneTreeFreshSingleLinkInodes(t *testing.T) {
	src := t.TempDir()
	orig := filepath.Join(src, "a.txt")
	if err := os.WriteFile(orig, []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Hard-link the source: source nlink is 2, clones must still be nlink 1.
	if err := os.Link(orig, filepath.Join(src, "b.txt")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	man, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile)
	if err != nil {
		t.Fatal(err)
	}
	srcInfo, err := os.Lstat(orig)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"a.txt", "b.txt"} {
		fi, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatal("no Stat_t")
		}
		if st.Nlink != 1 {
			t.Fatalf("clone %s nlink = %d, want 1", rel, st.Nlink)
		}
		if os.SameFile(srcInfo, fi) {
			t.Fatalf("clone %s shares the source inode", rel)
		}
	}
	// The manifest records the source's link count for the diff layer.
	for _, e := range man.entries {
		if e.path == "a.txt" && e.nlink != 2 {
			t.Fatalf("manifest nlink for hard-linked source = %d, want 2", e.nlink)
		}
	}
}

func TestCloneTreeDarwinDataVolumeAliasStaysIsolated(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin firmlink behavior")
	}
	src, err := CanonicalWorkspaceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(src, "live.txt")
	if err := os.WriteFile(live, []byte("canonical"), 0o644); err != nil {
		t.Fatal(err)
	}
	dataAlias := filepath.Join("/System/Volumes/Data", strings.TrimPrefix(live, string(filepath.Separator)))
	liveInfo, liveErr := os.Stat(live)
	aliasInfo, aliasErr := os.Stat(dataAlias)
	if liveErr != nil || aliasErr != nil || !os.SameFile(liveInfo, aliasInfo) {
		t.Skipf("no equivalent Data-volume spelling: live=%v alias=%v", liveErr, aliasErr)
	}
	if err := os.Symlink(dataAlias, filepath.Join(src, "through-alias")); err != nil {
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
		t.Fatalf("Data-volume alias escaped to canonical file: got %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "live.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("Data-volume alias did not resolve inside clone: got %q err=%v", got, err)
	}
}

func TestScratchRuntimeRejectsDarwinDataVolumeTempAlias(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin firmlink behavior")
	}
	alias := "/System/Volumes/Data/private/tmp"
	baseInfo, baseErr := os.Stat(scratchTempBase())
	aliasInfo, aliasErr := os.Stat(alias)
	if baseErr != nil || aliasErr != nil || !os.SameFile(baseInfo, aliasInfo) {
		t.Skipf("no equivalent Data-volume temp spelling: base=%v alias=%v", baseErr, aliasErr)
	}
	if _, err := newScratchRuntime(alias, ScratchConfig{Enabled: true}, nil); err == nil {
		t.Fatal("scratch runtime accepted a filesystem alias of its temp base")
	}
}

func TestCloneTreeRewritesExternalHardLinkToCanonicalFile(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(src, "live.txt")
	if err := os.WriteFile(live, []byte("canonical"), 0o644); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(parent, "hard-link")
	if err := os.Link(live, hard); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := os.Symlink(hard, filepath.Join(src, "through-hard-link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "through-hard-link"), []byte("scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(live); err != nil || string(got) != "canonical" {
		t.Fatalf("external hard-link alias escaped to canonical file: got %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "live.txt")); err != nil || string(got) != "scratch" {
		t.Fatalf("external hard-link alias did not resolve inside clone: got %q err=%v", got, err)
	}
}

func TestCloneTreeRejectsUnprovenExternalHardLink(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(parent, "external")
	if err := os.WriteFile(external, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(external, filepath.Join(parent, "external-hard-link")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := os.Symlink(external, filepath.Join(src, "external-link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err == nil {
		t.Fatal("external hard link absent from the manifest must fail closed")
	}
}

func TestCloneTreeRejectsHardLinkAliasToExcludedGitMetadata(t *testing.T) {
	parent := t.TempDir()
	src := filepath.Join(parent, "project")
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(src, ".git", "config")
	if err := os.WriteFile(config, []byte("canonical git"), 0o644); err != nil {
		t.Fatal(err)
	}
	hard := filepath.Join(parent, "git-config-hard-link")
	if err := os.Link(config, hard); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := os.Symlink(hard, filepath.Join(src, "through-git-hard-link")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err == nil {
		t.Fatal("hard-link alias to excluded .git metadata must fail closed")
	}
}

func TestCloneRegularRejectsFIFOSwapWithoutBlocking(t *testing.T) {
	src := t.TempDir()
	path := filepath.Join(src, "entry")
	if err := os.WriteFile(path, []byte("regular"), 0o644); err != nil {
		t.Fatal(err)
	}
	walkInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		f, _ := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if f != nil {
			_ = f.Close()
		}
	}()
	started := time.Now()
	err = cloneRegular(context.Background(), src, "entry", walkInfo, filepath.Join(t.TempDir(), "copy"), cloneFile)
	if err == nil || !errors.Is(err, errSnapshotDrift) {
		t.Fatalf("FIFO swap error = %v, want errSnapshotDrift", err)
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("FIFO swap blocked for %v", elapsed)
	}
}

func TestCloneTreeStripsSpecialBits(t *testing.T) {
	src := t.TempDir()
	p := filepath.Join(src, "sticky.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Skipf("cannot set special bits here: %v", err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 {
		t.Skip("filesystem silently dropped special bits")
	}
	dst := filepath.Join(t.TempDir(), "clone")
	if _, err := snapshotCanonical(context.Background(), src, dst, cloneFixtureConfig(), cloneFile); err != nil {
		t.Fatal(err)
	}
	got, err := os.Lstat(filepath.Join(dst, "sticky.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Fatalf("special bits must be stripped, got mode %v", got.Mode())
	}
	if got.Mode().Perm() != 0o755 {
		t.Fatalf("permission bits must survive stripping, got %v", got.Mode().Perm())
	}
}

func TestOpenManifestRegularRejectsFIFOSwap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry")
	if err := os.WriteFile(path, []byte("regular"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := walkSource(context.Background(), dir, cloneFixtureConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var entry snapshotEntry
	for _, candidate := range man.entries {
		if candidate.path == "entry" {
			entry = candidate
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if f, err := openManifestRegular(root, entry); err == nil {
		_ = f.Close()
		t.Fatal("capture opened a path swapped from a regular file to a FIFO")
	}
}

func TestPromoteArtifactIgnoresRestrictiveUmask(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("restrictive umask broke promotion: %s", res.Content)
	}
	fi, err := os.Lstat(filepath.Join(f.root, "dir", "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("promoted mode under restrictive umask = %04o, want 0640", fi.Mode().Perm())
	}
}

func TestScratchSessionIgnoresRestrictiveUmask(t *testing.T) {
	rt, canon := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	old := syscall.Umask(0o777)
	defer syscall.Umask(old)
	session, _, err := beginScratchSession(context.Background(), rt, testSpec(canon))
	syscall.Umask(old)
	if err != nil {
		t.Fatalf("restrictive umask broke scratch setup: %v", err)
	}
	defer session.discard()
	for _, p := range []string{session.refParent, session.execParent, filepath.Join(session.execParent, "tmp")} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("scratch private directory %q mode = %04o, want 0700", p, fi.Mode().Perm())
		}
	}
}
