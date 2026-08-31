//go:build linux

package tools

import "golang.org/x/sys/unix"

type linuxProcessExitObserver struct{ pid int }

func newProcessExitObserver(pid int) (processExitObserver, error) {
	o := &linuxProcessExitObserver{pid: pid}
	// Probe support without blocking or reaping. Some compatibility kernels
	// expose waitid but return ENOSYS; background execution must fail closed.
	if err := o.wait(unix.WNOHANG); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *linuxProcessExitObserver) Wait() error { return o.wait(0) }
func (*linuxProcessExitObserver) Close() error  { return nil }

func (o *linuxProcessExitObserver) wait(extra int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, o.pid, &info, unix.WEXITED|unix.WNOWAIT|extra, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
