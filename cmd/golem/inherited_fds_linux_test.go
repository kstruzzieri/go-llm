package main

import (
	"os"
	"strconv"
	"syscall"
	"testing"
)

// init makes this test process a clean bwrap parent, mirroring agent/tools'
// platformTestSetup (agent/tools/exec_linux_test.go). CI runners (observed:
// GitHub Actions) leak an inheritable descriptor into test processes, and the
// bwrap backend's fail-closed FD audit (rejectInheritableFDs) then refuses
// every native spawn the wiring tests make. Marking an inherited descriptor
// close-on-exec only changes what FUTURE children of this process inherit;
// the owner's use of its own copy is untouched. Test-only: production keeps
// the strict fail-closed audit.
func init() {
	for _, fd := range inheritableFDs() {
		_, _, _ = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC)
	}
}

// inheritableFDs lists the descriptors above stdio without FD_CLOEXEC, read
// from /proc/self/fd with F_GETFD as rejectInheritableFDs reads them.
func inheritableFDs() []int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil
	}
	var fds []int
	for _, e := range entries {
		fd, convErr := strconv.Atoi(e.Name())
		if convErr != nil || fd <= 2 {
			continue
		}
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFD, 0)
		if errno == 0 && flags&syscall.FD_CLOEXEC == 0 {
			fds = append(fds, fd)
		}
	}
	return fds
}

// The init above ran before any test, so no descriptor the launcher leaked
// can reach a bwrap child and trip the backend's audit.
func TestInheritedDescriptorsAreCloseOnExec(t *testing.T) {
	if fds := inheritableFDs(); len(fds) != 0 {
		t.Fatalf("descriptors %v are inheritable; bwrap's fd audit would refuse every spawn", fds)
	}
}
