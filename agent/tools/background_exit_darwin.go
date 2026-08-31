//go:build darwin

package tools

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

type darwinProcessExitObserver struct{ kq int }

func newProcessExitObserver(pid int) (processExitObserver, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	var change unix.Kevent_t
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return observerForRegistrationError(err)
	}
	return &darwinProcessExitObserver{kq: kq}, nil
}

func (o *darwinProcessExitObserver) Wait() error {
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(o.kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("kevent returned %d events, want 1", n)
		}
		if events[0].Flags&unix.EV_ERROR != 0 {
			return syscall.Errno(events[0].Data)
		}
		return nil
	}
}

func (o *darwinProcessExitObserver) Close() error { return unix.Close(o.kq) }

// observerForRegistrationError classifies a failed EVFILT_PROC registration.
//
// ESRCH means the kernel no longer reports the pid as a live process: darwin's
// proc lookup skips zombies, so it is also what registration gets for a child
// that exited between fork and this call — the window unixStarter.Start opens
// by design, since it must have a pid before it can watch one. The pid cannot
// have been recycled there: the child is still ours and unreaped. The exit an
// observer would report has therefore already happened, and the status is
// still recoverable through the normal cmd.Wait reap, so an observer whose
// Wait returns immediately is the correct resolution rather than a failure.
//
// Every other errno stays fail-closed: background execution must never publish
// a process whose exit nothing is watching.
func observerForRegistrationError(err error) (processExitObserver, error) {
	if errors.Is(err, unix.ESRCH) {
		return exitedProcessExitObserver{}, nil
	}
	return nil, err
}

// exitedProcessExitObserver resolves a process that had already exited when
// its observer was created. Wait reports an exit in the past, so it returns
// immediately; there is no kqueue to close.
type exitedProcessExitObserver struct{}

func (exitedProcessExitObserver) Wait() error  { return nil }
func (exitedProcessExitObserver) Close() error { return nil }
