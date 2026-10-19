//go:build unix

package golem

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
)

// inheritableAtInit is what init left inheritable: none, unless the
// sanitizer is broken.
var inheritableAtInit []int

// init makes this test process a clean native-sandbox parent, mirroring
// agent/tools' platformTestSetup and cmd/golem's test init. CI runners
// (observed: GitHub Actions on Linux and macOS) leak an inheritable
// descriptor into test processes, and both native backends' fail-closed
// descriptor audit would then refuse the real backend
// TestNativeBackendScratchFrozenAcrossReplacement builds. Marking an
// inherited descriptor close-on-exec only changes what FUTURE children of
// this process inherit; the owner's use of its own copy is untouched.
// Test-only: production keeps the strict audit.
func init() {
	for _, fd := range descriptorsAboveStdio() {
		syscall.CloseOnExec(fd)
	}
	inheritableAtInit = inheritableFDs()
}

// descriptorsAboveStdio lists this process's descriptors above stdio, read
// from /proc/self/fd on Linux and /dev/fd elsewhere.
func descriptorsAboveStdio() []int {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	var fds []int
	for _, name := range names {
		if fd, err := strconv.Atoi(name); err == nil && fd > 2 {
			fds = append(fds, fd)
		}
	}
	return fds
}

// inheritableFDs lists the descriptors above stdio without FD_CLOEXEC, read
// with F_GETFD.
func inheritableFDs() []int {
	var fds []int
	for _, fd := range descriptorsAboveStdio() {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			fds = append(fds, fd)
		}
	}
	return fds
}

// The init above ran before any test, so no descriptor the launcher leaked
// can reach a native child and trip the backend's audit. It reads the set
// init recorded: a later hostname lookup may leave libSystem's own netsrc
// socket, which the audit exempts.
func TestInheritedDescriptorsAreCloseOnExec(t *testing.T) {
	if len(inheritableAtInit) != 0 {
		t.Fatalf("descriptors %v are inheritable; the native fd audit would refuse every spawn", inheritableAtInit)
	}
}
