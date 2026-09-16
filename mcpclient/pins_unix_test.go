//go:build unix

package mcpclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestPinUnixPermissions(t *testing.T) {
	for _, kind := range []string{"pin", "lock", "directory", "unwritable"} {
		t.Run(kind, func(t *testing.T) {
			s := pinStoreForTest(t)
			ctx := context.Background()
			a := pinCatalog(t, "fs", "A")
			if _, _, err := s.admit(ctx, "fs", a, false); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{s.dir, filepath.Join(s.dir, fsKey+".json"), filepath.Join(s.dir, fsKey+".lock")} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0600)
				if info.IsDir() {
					want = 0700
				}
				if info.Mode().Perm() != want {
					t.Fatalf("mode %s: %o", path, info.Mode().Perm())
				}
			}
			path, mode := filepath.Join(s.dir, fsKey+".json"), os.FileMode(0644)
			switch kind {
			case "lock":
				path = filepath.Join(s.dir, fsKey+".lock")
			case "directory":
				path = s.dir
				mode = 0755
			case "unwritable":
				if os.Geteuid() == 0 {
					t.Skip("root bypasses permission bits")
				}
				path = s.dir
				mode = 0500
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(s.dir, 0700) })
			var err error
			if kind == "unwritable" {
				_, rev, e := s.capturePin(ctx, "fs")
				if e != nil {
					t.Fatal(e)
				}
				err = s.replacePin(ctx, "fs", rev, pinCatalog(t, "fs", "B"))
			} else {
				_, _, err = s.capturePin(ctx, "fs")
			}
			if err == nil {
				t.Fatalf("accepted %s", kind)
			}
		})
	}
}

type pinInfoWithOwner struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (info pinInfoWithOwner) Sys() any { return &info.stat }
func TestPinUnixForeignOwnership(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("missing native stat")
	}
	foreign := *stat
	foreign.Uid++
	if pinOwnerModeOK(pinInfoWithOwner{FileInfo: info, stat: foreign}) {
		t.Fatal("foreign ownership accepted")
	}
}

func TestPinUnixFIFOSwaps(t *testing.T) {
	for _, lock := range []bool{false, true} {
		t.Run(map[bool]string{false: "pin", true: "lock"}[lock], func(t *testing.T) {
			s := pinStoreForTest(t)
			ctx := context.Background()
			a := pinCatalog(t, "fs", "A")
			if _, _, err := s.admit(ctx, "fs", a, false); err != nil {
				t.Fatal(err)
			}
			target := fsKey + ".json"
			if lock {
				target = fsKey + ".lock"
			}
			path := filepath.Join(s.dir, target)
			s.ops.afterLstat = func(name string) {
				if name != target {
					return
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.capturePin(ctx, "fs"); err == nil {
				t.Fatal("FIFO swap accepted")
			}
		})
	}
}

func TestPinUnixLeaseClose(t *testing.T) {
	var nilLease *pinLease
	if err := nilLease.Close(); err != nil {
		t.Fatal(err)
	}
	s := pinStoreForTest(t)
	root, err := s.openRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, err := s.acquireLease(root, fsKey+".lock")
	if err != nil {
		t.Fatal(err)
	}
	if err = lease.file.Close(); err != nil {
		t.Fatal(err)
	}
	first := lease.Close()
	if first == nil || !errors.Is(first, os.ErrClosed) {
		t.Fatalf("missing unlock/close failure: %v", first)
	}
	if second := lease.Close(); first != second {
		t.Fatal("Close was not idempotent")
	}
}

func TestPinUnixDirectoryModeSwap(t *testing.T) {
	s := pinStoreForTest(t)
	s.ops.afterLstat = func(path string) {
		if filepath.Base(path) == filepath.Base(s.dir) {
			if err := os.Chmod(s.dir, 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, _, err := s.capturePin(context.Background(), "fs"); err == nil {
		t.Fatal("directory mode changed during open was accepted")
	}
}
