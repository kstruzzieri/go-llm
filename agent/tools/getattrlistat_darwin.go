//go:build darwin

package tools

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getattrlistat(2) through libSystem, the route x/sys uses for every darwin
// wrapper (it ships none for getattrlistat as of v0.43.0). syscall.syscall6 is
// marked for linkname use (go.dev/issue/67401) and keeps uintptr args alive.
//
//go:cgo_import_dynamic libc_getattrlistat getattrlistat "/usr/lib/libSystem.B.dylib"
var libc_getattrlistat_trampoline_addr uintptr

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

func getattrlistat(dirfd int, name *byte, al *unix.Attrlist, buf *byte, n int, opts uint32) syscall.Errno {
	_, _, e := syscall_syscall6(libc_getattrlistat_trampoline_addr, uintptr(dirfd), uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(al)), uintptr(unsafe.Pointer(buf)), uintptr(n), uintptr(opts))
	return e
}
