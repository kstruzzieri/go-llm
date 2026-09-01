//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

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
