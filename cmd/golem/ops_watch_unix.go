//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// watchJob catches Ctrl-Z (SIGTSTP) for -watch and returns its stop.
//
// A process group that is its session's own (golem started directly by tmux,
// ssh -t or a terminal emulator) has no job-control shell to continue it: the
// kernel discards an uncaught Ctrl-Z there, while a stop golem raised itself
// would freeze it for good. There Ctrl-Z stays uncaught.
//
// Elsewhere, once Go has caught SIGTSTP, stopping the relay leaves it ignored
// rather than restoring its default action, so suspend stops the process
// group with SIGSTOP, as the uncaught Ctrl-Z would have, and returns on the
// SIGCONT that resumes it: kill(2) can return before the stop lands.
func watchJob() (opsJob, func()) {
	if sid, err := getsid(); err != nil || sid == syscall.Getpgrp() {
		return opsJob{}, func() {}
	}
	stops := make(chan os.Signal, 1)
	signal.Notify(stops, syscall.SIGTSTP)
	suspend := func() {
		cont := make(chan os.Signal, 1)
		signal.Notify(cont, syscall.SIGCONT)
		defer signal.Stop(cont)
		if syscall.Kill(0, syscall.SIGSTOP) == nil {
			<-cont
		}
	}
	return opsJob{stops: stops, suspend: suspend}, func() { signal.Stop(stops) }
}
