//go:build unix

package main

import (
	"os"
	"runtime"
	"strconv"
	"syscall"
)

// clearInheritedDescriptors marks every descriptor above stdio close-on-exec
// (#576). A launcher can leave descriptors inheritable (CI runners do, and so
// does a shell holding a redirection open); a native backend would pass them
// to sandboxed commands, which can write or connect through an open
// descriptor whatever their profile says, so agent/tools refuses to spawn
// while one exists. Golem owns its own process: the flag changes only what
// its future children inherit, never golem's own use of a descriptor. It is
// best effort. If the directory cannot be read, the library's fail-closed
// audit still refuses at the startup probe.
func clearInheritedDescriptors() {
	dir := "/dev/fd"
	if runtime.GOOS == "linux" {
		dir = "/proc/self/fd"
	}
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	names, _ := f.Readdirnames(-1)
	_ = f.Close()
	for _, name := range names {
		if fd, err := strconv.Atoi(name); err == nil && fd > 2 {
			syscall.CloseOnExec(fd)
		}
	}
}
