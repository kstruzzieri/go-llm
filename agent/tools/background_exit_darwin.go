//go:build darwin

package tools

import (
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
		return nil, err
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
