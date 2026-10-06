//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchJob catches Ctrl-Z (SIGTSTP) for -watch and returns its stop. Once Go
// has caught SIGTSTP, stopping the relay leaves it ignored rather than
// restoring its default action, so suspend stops the process group with
// SIGSTOP, as the uncaught Ctrl-Z would have. A stop signal sent to the
// caller's own group takes effect before kill returns, so suspend returns
// only once the job is continued.
func watchJob() (opsJob, func()) {
	stops := make(chan os.Signal, 1)
	signal.Notify(stops, syscall.SIGTSTP)
	suspend := func() { _ = syscall.Kill(0, syscall.SIGSTOP) }
	return opsJob{stops: stops, suspend: suspend}, func() { signal.Stop(stops) }
}
