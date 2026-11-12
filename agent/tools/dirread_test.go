//go:build linux || darwin

package tools

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// readDirAt opens dir on its own handle, reads it with sysDir and closes it.
func readDirAt(t *testing.T, label, dir string) []dirEntry {
	t.Helper()
	h, err := os.Open(dir)
	if err != nil {
		failNoPath(t, label, err)
	}
	defer func() { _ = h.Close() }()
	var got []dirEntry
	if err := sysDir.read(h, collect(&got)); err != nil {
		t.Fatalf("%s: read: %v", label, err)
	}
	return got
}

// failNoPath fails the test with label and the cause of err: the errno it
// wraps, else only its type. Never err's text, which can quote a test path.
func failNoPath(t testing.TB, label string, err error) {
	t.Helper()
	var errno syscall.Errno
	if errors.As(err, &errno) {
		t.Fatalf("%s: %v", label, errno)
	}
	t.Fatalf("%s: failed (%T)", label, err)
}

// noLeak runs read once to warm up, then checks that 64 more leave the
// process's descriptor count unchanged. GC stays off so a finalizer cannot
// close a leaked descriptor and hide it (paths_reachability_test.go).
func noLeak(t *testing.T, label string, read func()) {
	t.Helper()
	read()
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	before := openFDCount(t)
	for range 64 {
		read()
	}
	if after := openFDCount(t); after != before {
		t.Fatalf("%s: descriptors before=%d after=%d", label, before, after)
	}
}

func TestDirReadKinds(t *testing.T) {
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		failNoPath(t, "kinds", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "reg"), nil, 0o600); err != nil {
		failNoPath(t, "kinds", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o700); err != nil {
		failNoPath(t, "kinds", err)
	}
	if err := os.Symlink("reg", filepath.Join(dir, "lnk")); err != nil {
		failNoPath(t, "kinds", err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		failNoPath(t, "kinds", err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "sock"))
	if err != nil {
		failNoPath(t, "kinds", err)
	}
	defer func() { _ = ln.Close() }()

	got := readDirAt(t, "kinds", dir)
	slices.SortFunc(got, func(a, b dirEntry) int { return strings.Compare(a.name, b.name) })
	want := []dirEntry{
		{"dir", unix.DT_DIR},
		{"fifo", unix.DT_FIFO},
		{"lnk", unix.DT_LNK},
		{"reg", unix.DT_REG},
		{"sock", unix.DT_SOCK},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("kinds: got %d entries, want %d (or a name or type differs)", len(got), len(want))
	}
}

func TestDirReadEmpty(t *testing.T) {
	if got := readDirAt(t, "empty", t.TempDir()); len(got) != 0 {
		t.Fatalf("empty: emitted %d entries", len(got))
	}
}

func TestDirReadMany(t *testing.T) {
	dir := t.TempDir()
	for i := range 10000 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), nil, 0o600); err != nil {
			failNoPath(t, "many", err)
		}
	}
	got := names(readDirAt(t, "many", dir))
	h, err := os.Open(dir)
	if err != nil {
		failNoPath(t, "many", err)
	}
	want, err := h.Readdirnames(-1)
	_ = h.Close()
	if err != nil {
		failNoPath(t, "many", err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(want) != 10000 || !slices.Equal(got, want) {
		t.Fatalf("many: got %d names, Readdirnames %d (or one differs)", len(got), len(want))
	}
}

func TestDirReadNames(t *testing.T) {
	rows := []struct{ label, name string }{
		{"space", "a b"},
		{"newline", "a\nb"},
		{"255 bytes", strings.Repeat("n", 255)},
	}
	if runtime.GOOS == "linux" {
		rows = append(rows, struct{ label, name string }{"invalid utf-8", "\xff\xfe"})
	}
	for _, r := range rows {
		t.Run(r.label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, r.name), nil, 0o600); err != nil {
				failNoPath(t, r.label+": create", err)
			}
			got := readDirAt(t, r.label, dir)
			if len(got) != 1 || got[0].name != r.name {
				t.Fatalf("%s: name did not come back unchanged", r.label)
			}
		})
	}
}

func TestDirReadNoDescriptorLeak(t *testing.T) {
	dir := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0o600); err != nil {
			failNoPath(t, "no leak", err)
		}
	}
	noLeak(t, "no leak", func() { readDirAt(t, "no leak", dir) })
}
