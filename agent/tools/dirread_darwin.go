package tools

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// fdopendir, readdir_r and closedir through libSystem, the calls and the
// unsuffixed symbols os.File.Readdirnames uses on both arm64 and amd64, bound
// by the pattern of getattrlistat_darwin.go. syscall_syscall is declared in
// libsystem_darwin.go; syscallPtr is the bridge for a call returning a
// pointer.
//
//go:cgo_import_dynamic libc_fdopendir fdopendir "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_readdir_r readdir_r "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_closedir closedir "/usr/lib/libSystem.B.dylib"
var (
	libc_fdopendir_trampoline_addr uintptr
	libc_readdir_r_trampoline_addr uintptr
	libc_closedir_trampoline_addr  uintptr
)

//go:linkname syscall_syscallPtr syscall.syscallPtr
func syscall_syscallPtr(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

// dirSys is the walk's own directory read (#789): fdopendir on a
// close-on-exec duplicate of the held descriptor, readdir_r until it returns
// no entry, closedir. Each record's name and raw type reach the caller. It is
// not os.File.ReadDir, which on a filesystem that reports no types drops
// entries whose stat fails. The fields are seams for tests.
type dirSys struct {
	dup       func(fd int) (int, error)
	fdopendir func(fd int) (uintptr, error)
	// readdir returns readdir_r's return value: it reports failure there,
	// not in errno, and the end of the directory as 0 with a nil *result.
	readdir  func(dir uintptr, entry *unix.Dirent, result **unix.Dirent) syscall.Errno
	closedir func(dir uintptr)
	closeFD  func(fd int) error
}

// sysDir is the real directory read.
var sysDir = dirSys{
	dup: func(fd int) (int, error) {
		return unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	},
	fdopendir: func(fd int) (uintptr, error) {
		d, _, e := syscall_syscallPtr(libc_fdopendir_trampoline_addr, uintptr(fd), 0, 0)
		if d == 0 {
			return 0, e
		}
		return d, nil
	},
	readdir: func(dir uintptr, entry *unix.Dirent, result **unix.Dirent) syscall.Errno {
		r, _, _ := syscall_syscall(libc_readdir_r_trampoline_addr, dir, uintptr(unsafe.Pointer(entry)), uintptr(unsafe.Pointer(result)))
		return syscall.Errno(r)
	},
	closedir: func(dir uintptr) {
		_, _, _ = syscall_syscall(libc_closedir_trampoline_addr, dir, 0, 0)
	},
	closeFD: unix.Close,
}

// read emits the name and raw d_type of every entry of f except ".", ".."
// and records with inode 0. f must be freshly opened: the duplicate shares
// its read position. A nil return means the whole directory was read; on any
// error the caller discards what was emitted. closedir, which frees the
// handle and closes the duplicate, is called exactly once once fdopendir has
// succeeded; before that, a refused duplicate is closed once.
func (s dirSys) read(f *os.File, emit func(name string, typ uint8)) error {
	defer runtime.KeepAlive(f)
	fd, err := s.dup(int(f.Fd()))
	if err != nil {
		return err
	}
	var dir uintptr
	for {
		dir, err = s.fdopendir(fd)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = s.closeFD(fd)
		return err
	}
	defer s.closedir(dir)
	entry := new(unix.Dirent)
	for {
		*entry = unix.Dirent{}
		var result *unix.Dirent
		if e := s.readdir(dir, entry, &result); e != 0 {
			if e == syscall.EINTR {
				continue
			}
			return e
		}
		if result == nil {
			return nil
		}
		if entry.Ino == 0 {
			continue
		}
		if int(entry.Namlen) >= len(entry.Name) || entry.Name[entry.Namlen] != 0 {
			return errBadDirent
		}
		name := string(unsafe.Slice((*byte)(unsafe.Pointer(&entry.Name[0])), entry.Namlen))
		if name == "." || name == ".." {
			continue
		}
		emit(name, entry.Type)
	}
}
