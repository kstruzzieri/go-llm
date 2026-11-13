package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// darwinDirent builds one readdir_r record holding name.
func darwinDirent(name string, ino uint64) unix.Dirent {
	var d unix.Dirent
	d.Ino = ino
	d.Type = unix.DT_REG
	d.Namlen = uint16(len(name))
	for i := 0; i < len(name); i++ {
		d.Name[i] = int8(name[i])
	}
	return d
}

// fakeDir is a dirSys whose every call is a stub: no libSystem call is made.
type fakeDir struct {
	recs     []unix.Dirent
	readErrs map[int]syscall.Errno // readdir call number (1-based) -> its return
	dupErr   error
	openErrs []error // successive fdopendir failures before it succeeds

	dupCalls, openCalls, readCalls, closedirCalls, closeFDCalls int
	openFDs, closedFDs                                          []int
	closedHandles                                               []uintptr
	next                                                        int
}

const (
	fakeFD     = 9001
	fakeHandle = uintptr(0xD1D1)
)

func (fk *fakeDir) sys() dirSys {
	return dirSys{
		dup: func(int) (int, error) {
			fk.dupCalls++
			if fk.dupErr != nil {
				return -1, fk.dupErr
			}
			return fakeFD, nil
		},
		fdopendir: func(fd int) (uintptr, error) {
			fk.openCalls++
			fk.openFDs = append(fk.openFDs, fd)
			if fk.openCalls <= len(fk.openErrs) {
				return 0, fk.openErrs[fk.openCalls-1]
			}
			return fakeHandle, nil
		},
		readdir: func(_ uintptr, entry *unix.Dirent, result **unix.Dirent) syscall.Errno {
			fk.readCalls++
			if e, ok := fk.readErrs[fk.readCalls]; ok {
				return e
			}
			if fk.next == len(fk.recs) {
				*result = nil
				return 0
			}
			*entry = fk.recs[fk.next]
			fk.next++
			*result = entry
			return 0
		},
		closedir: func(dir uintptr) {
			fk.closedirCalls++
			fk.closedHandles = append(fk.closedHandles, dir)
		},
		closeFD: func(fd int) error {
			fk.closeFDCalls++
			fk.closedFDs = append(fk.closedFDs, fd)
			return nil
		},
	}
}

// TestDirReadSeams drives the Darwin read through stubs (simulated rows) and
// through wrapped real calls (real rows).
func TestDirReadSeams(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	abc := []unix.Dirent{darwinDirent("a", 1), darwinDirent("b", 2), darwinDirent("c", 3), darwinDirent("d", 4), darwinDirent("e", 5)}
	badNamlen := darwinDirent("x", 1)
	badNamlen.Namlen = 1024
	noNul := darwinDirent("x", 1)
	noNul.Name[1] = 1
	embeddedNul := []unix.Dirent{darwinDirent("plain\x00zz", 1), darwinDirent("after", 2)}
	emptyName := []unix.Dirent{darwinDirent("", 1), darwinDirent("after", 2)}
	ghost := []unix.Dirent{darwinDirent("a", 1), darwinDirent("b", 0), darwinDirent("c", 3)}

	f, err := os.Open(t.TempDir())
	if err != nil {
		failNoPath(t, "seams", err)
	}
	defer func() { _ = f.Close() }()

	type counts struct{ dup, open, closedir, closeFD int }
	sim := []struct {
		label string
		fk    *fakeDir
		names []string // checked when err is nil
		err   error
		want  counts
	}{
		{"complete", &fakeDir{recs: abc}, []string{"a", "b", "c", "d", "e"}, nil, counts{1, 1, 1, 0}},
		{"readdir error", &fakeDir{recs: abc, readErrs: map[int]syscall.Errno{3: syscall.EIO}}, nil, syscall.EIO, counts{1, 1, 1, 0}},
		{"readdir eintr", &fakeDir{recs: abc, readErrs: map[int]syscall.Errno{2: syscall.EINTR}}, []string{"a", "b", "c", "d", "e"}, nil, counts{1, 1, 1, 0}},
		{"fdopendir eintr", &fakeDir{recs: abc, openErrs: []error{syscall.EINTR}}, []string{"a", "b", "c", "d", "e"}, nil, counts{1, 2, 1, 0}},
		{"fdopendir fails", &fakeDir{recs: abc, openErrs: []error{syscall.ENOMEM}}, nil, syscall.ENOMEM, counts{1, 1, 0, 1}},
		{"dup fails", &fakeDir{recs: abc, dupErr: syscall.EMFILE}, nil, syscall.EMFILE, counts{1, 0, 0, 0}},
		{"bad namlen", &fakeDir{recs: []unix.Dirent{badNamlen}}, nil, errBadDirent, counts{1, 1, 1, 0}},
		{"no nul", &fakeDir{recs: []unix.Dirent{noNul}}, nil, errBadDirent, counts{1, 1, 1, 0}},
		{"embedded nul", &fakeDir{recs: embeddedNul}, nil, errBadDirent, counts{1, 1, 1, 0}},
		{"empty name", &fakeDir{recs: emptyName}, nil, errBadDirent, counts{1, 1, 1, 0}},
		{"inode zero", &fakeDir{recs: ghost}, []string{"a", "c"}, nil, counts{1, 1, 1, 0}},
	}
	for _, r := range sim {
		t.Run(r.label, func(t *testing.T) {
			var got []dirEntry
			err := r.fk.sys().read(f, collect(&got))
			if !errors.Is(err, r.err) {
				t.Fatalf("%s: err = %v, want %v", r.label, err, r.err)
			}
			if errors.Is(r.err, errBadDirent) && len(got) != 0 {
				t.Fatalf("%s: %d names emitted before the bad record was refused, want 0", r.label, len(got))
			}
			if r.err == nil && !slices.Equal(names(got), r.names) {
				t.Fatalf("%s: got %d names, want %d (or one differs)", r.label, len(got), len(r.names))
			}
			c := counts{r.fk.dupCalls, r.fk.openCalls, r.fk.closedirCalls, r.fk.closeFDCalls}
			if c != r.want {
				t.Fatalf("%s: calls dup/fdopendir/closedir/closeFD = %+v, want %+v", r.label, c, r.want)
			}
			for _, fd := range slices.Concat(r.fk.openFDs, r.fk.closedFDs) {
				if fd != fakeFD {
					t.Fatalf("%s: a call was given descriptor %d, not the duplicate", r.label, fd)
				}
			}
			for _, h := range r.fk.closedHandles {
				if h != fakeHandle {
					t.Fatalf("%s: closedir was given another handle", r.label)
				}
			}
		})
	}

	t.Run("cloexec", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a"), nil, 0o600); err != nil {
			failNoPath(t, "cloexec", err)
		}
		var flags, seen int
		var flagErr error
		s := sysDir
		s.fdopendir = func(fd int) (uintptr, error) {
			seen = fd
			flags, flagErr = unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			return sysDir.fdopendir(fd)
		}
		readOnce := func() {
			t.Helper()
			h, err := os.Open(dir)
			if err != nil {
				failNoPath(t, "cloexec", err)
			}
			defer func() { _ = h.Close() }()
			var got []dirEntry
			if err := s.read(h, collect(&got)); err != nil {
				t.Fatalf("cloexec: read: %v", err)
			}
			if flagErr != nil || flags&unix.FD_CLOEXEC == 0 {
				t.Fatalf("cloexec: duplicate flags = %#x, %v; want FD_CLOEXEC", flags, flagErr)
			}
			if seen == int(h.Fd()) {
				t.Fatal("cloexec: fdopendir was given the caller's own descriptor")
			}
			if !slices.Equal(names(got), []string{"a"}) {
				t.Fatalf("cloexec: got %d names, want 1", len(got))
			}
		}
		noLeak(t, "cloexec", readOnce)
	})

	t.Run("real readdir error", func(t *testing.T) {
		dir := t.TempDir()
		for i := range 5000 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("file-with-a-fairly-long-name-%05d.txt", i)), nil, 0o600); err != nil {
				failNoPath(t, "real readdir error", err)
			}
		}
		reg, err := os.Open(filepath.Join(dir, "file-with-a-fairly-long-name-00000.txt"))
		if err != nil {
			failNoPath(t, "real readdir error", err)
		}
		defer func() { _ = reg.Close() }()
		var dupErr error
		s := sysDir
		s.fdopendir = func(fd int) (uintptr, error) {
			d, err := sysDir.fdopendir(fd)
			if err == nil {
				dupErr = unix.Dup2(int(reg.Fd()), fd)
			}
			return d, err
		}
		readOnce := func() {
			t.Helper()
			h, err := os.Open(dir)
			if err != nil {
				failNoPath(t, "real readdir error", err)
			}
			defer func() { _ = h.Close() }()
			dupErr = errors.New("fdopendir wrapper not called")
			err = s.read(h, func(string, uint8) {})
			if dupErr != nil {
				t.Fatalf("real readdir error: dup2: %v", dupErr)
			}
			if !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("real readdir error: err = %v, want EINVAL", err)
			}
		}
		noLeak(t, "real readdir error", readOnce)
	})
}
