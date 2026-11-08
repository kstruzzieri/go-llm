//go:build darwin

package tools

import (
	"runtime"
	"syscall"
	"unsafe"
)

// Mach calls through libSystem without cgo, the pattern golang.org/x/sys/unix
// uses: one cgo_import_dynamic and one asm trampoline (libsystem_darwin_*.s)
// per function, called through the syscall package's libc bridges. The bridges
// return the C result in r1; a kern_return_t is int32(r1), and a Mach failure
// can come back with the bridge's errno at zero, so only r1 is checked. Ports,
// masks, counts, behaviors and flavors are 32-bit. syscall_syscall6 is
// declared in getattrlistat_darwin.go.

//go:cgo_import_dynamic libc_task_self_trap task_self_trap "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_ports_lookup mach_ports_lookup "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_ports_register mach_ports_register "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_vm_deallocate vm_deallocate "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_port_deallocate mach_port_deallocate "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_task_get_special_port task_get_special_port "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_task_set_special_port task_set_special_port "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_task_get_exception_ports task_get_exception_ports "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_task_set_exception_ports task_set_exception_ports "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_port_allocate mach_port_allocate "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mach_port_insert_right mach_port_insert_right "/usr/lib/libSystem.B.dylib"

var (
	libc_task_self_trap_trampoline_addr           uintptr
	libc_mach_ports_lookup_trampoline_addr        uintptr
	libc_mach_ports_register_trampoline_addr      uintptr
	libc_vm_deallocate_trampoline_addr            uintptr
	libc_mach_port_deallocate_trampoline_addr     uintptr
	libc_task_get_special_port_trampoline_addr    uintptr
	libc_task_set_special_port_trampoline_addr    uintptr
	libc_task_get_exception_ports_trampoline_addr uintptr
	libc_task_set_exception_ports_trampoline_addr uintptr
	libc_mach_port_allocate_trampoline_addr       uintptr
	libc_mach_port_insert_right_trampoline_addr   uintptr
)

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall9 syscall.syscall9
func syscall_syscall9(fn, a1, a2, a3, a4, a5, a6, a7, a8, a9 uintptr) (r1, r2 uintptr, err syscall.Errno)

// threadStateNone is THREAD_STATE_NONE, which differs by architecture.
var threadStateNone = map[string]uint32{"arm64": 5, "amd64": 13}[runtime.GOARCH]

// taskSelfTrap returns this task's own port name. Use machTaskSelf.
func taskSelfTrap() uint32 {
	r1, _, _ := syscall_syscall(libc_task_self_trap_trampoline_addr, 0, 0, 0)
	return uint32(r1)
}

// machPortsLookup: *ports receives an out-of-line array of *n port names
// that the caller frees with vmDeallocate.
func machPortsLookup(task uint32, ports **uint32, n *uint32) int32 {
	r1, _, _ := syscall_syscall(libc_mach_ports_lookup_trampoline_addr, uintptr(task), uintptr(unsafe.Pointer(ports)), uintptr(unsafe.Pointer(n)))
	runtime.KeepAlive(ports)
	runtime.KeepAlive(n)
	return int32(r1)
}

// machPortsRegister: a nil ports with n 0 empties the table.
func machPortsRegister(task uint32, ports *uint32, n uint32) int32 {
	r1, _, _ := syscall_syscall(libc_mach_ports_register_trampoline_addr, uintptr(task), uintptr(unsafe.Pointer(ports)), uintptr(n))
	runtime.KeepAlive(ports)
	return int32(r1)
}

func vmDeallocate(task uint32, addr, size uintptr) int32 {
	r1, _, _ := syscall_syscall(libc_vm_deallocate_trampoline_addr, uintptr(task), addr, size)
	return int32(r1)
}

func machPortDeallocate(task, name uint32) int32 {
	r1, _, _ := syscall_syscall(libc_mach_port_deallocate_trampoline_addr, uintptr(task), uintptr(name), 0)
	return int32(r1)
}

func taskGetSpecialPort(task uint32, which int32, port *uint32) int32 {
	r1, _, _ := syscall_syscall(libc_task_get_special_port_trampoline_addr, uintptr(task), uintptr(which), uintptr(unsafe.Pointer(port)))
	runtime.KeepAlive(port)
	return int32(r1)
}

func taskSetSpecialPort(task uint32, which int32, port uint32) int32 {
	r1, _, _ := syscall_syscall(libc_task_set_special_port_trampoline_addr, uintptr(task), uintptr(which), uintptr(port))
	return int32(r1)
}

// taskGetExceptionPorts takes seven arguments, so it goes through syscall9.
// *n is the capacity of masks, handlers, behaviors and flavors on entry and
// the number filled on return.
func taskGetExceptionPorts(task, mask uint32, masks, n, handlers *uint32, behaviors, flavors *int32) int32 {
	r1, _, _ := syscall_syscall9(libc_task_get_exception_ports_trampoline_addr, uintptr(task), uintptr(mask),
		uintptr(unsafe.Pointer(masks)), uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(handlers)),
		uintptr(unsafe.Pointer(behaviors)), uintptr(unsafe.Pointer(flavors)), 0, 0)
	runtime.KeepAlive(masks)
	runtime.KeepAlive(n)
	runtime.KeepAlive(handlers)
	runtime.KeepAlive(behaviors)
	runtime.KeepAlive(flavors)
	return int32(r1)
}

func taskSetExceptionPorts(task, mask, port uint32, behavior int32, flavor uint32) int32 {
	r1, _, _ := syscall_syscall6(libc_task_set_exception_ports_trampoline_addr, uintptr(task), uintptr(mask), uintptr(port), uintptr(behavior), uintptr(flavor), 0)
	return int32(r1)
}

func machPortAllocate(task, right uint32, name *uint32) int32 {
	r1, _, _ := syscall_syscall(libc_mach_port_allocate_trampoline_addr, uintptr(task), uintptr(right), uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	return int32(r1)
}

func machPortInsertRight(task, name, port, disposition uint32) int32 {
	r1, _, _ := syscall_syscall6(libc_mach_port_insert_right_trampoline_addr, uintptr(task), uintptr(name), uintptr(port), uintptr(disposition), 0, 0)
	return int32(r1)
}
