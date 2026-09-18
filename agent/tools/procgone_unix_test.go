//go:build unix

package tools

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"
)

// processGone reports whether pid no longer runs. A zombie counts as gone: the
// process is dead and only awaits reaping by its parent. The orphans these
// tests leave are reparented to PID 1, and inside a container whose PID 1 is
// not an init (for example `sh -c '...; go test ...'`, where the shell execs
// the final command) nothing ever reaps them, so kill(pid, 0) alone would
// report a dead process as alive for the rest of the run.
func processGone(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false // no procfs (Darwin): launchd reaps orphans promptly
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
