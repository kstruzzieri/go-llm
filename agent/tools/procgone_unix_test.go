//go:build unix

package tools

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// processGone reports whether pid no longer runs. A zombie counts as gone: the
// process is dead and only awaits reaping by its parent. The reaping tests
// assert that a killed group's descendants terminated; who buries them is
// PID 1's job. The compose service runs tini for that, but an invocation
// without an init (`sh -c '...; go test ...'` execs its final command, so go
// becomes PID 1) never reaps reparented orphans, and kill(pid, 0) alone would
// report every dead orphan as alive for the rest of the run. A /proc read
// denied by hidepid leaves the process reported alive: unproven is not gone.
func processGone(pid int) bool {
	if pid <= 0 {
		return false // never probe a process group by accident
	}
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	if runtime.GOOS != "linux" {
		return false // no procfs; launchd reaps orphans promptly on Darwin
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return true // reaped between the signal probe and the read
	}
	if err != nil {
		return false
	}
	// The state field follows the parenthesised comm, which may contain ')'.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && len(stat) > i+2 && stat[i+2] == 'Z'
}

// waitProcessGone polls processGone for up to five seconds.
func waitProcessGone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return processGone(pid)
}
