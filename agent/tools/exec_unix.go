//go:build unix

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// unixRunner executes commands in their own process group so a timeout/cancel can kill
// the whole group (reaping ordinary children), and bounds stdout/stderr to the tool's
// self-caps while always consuming pipe writes (no deadlock).
type unixRunner struct{}

func newPlatformRunner() commandRunner { return unixRunner{} }

func (unixRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	// CommandContext (NOT exec.Command) is REQUIRED: cmd.Cancel is only consulted
	// when the command was created with a context — with exec.Command the custom
	// group-kill Cancel would never fire on timeout. (cmd.WaitDelay, by contrast,
	// works with plain exec.Command too; see background_unix.go.)
	cmd := exec.CommandContext(ctx, spec.Path, spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout := &cappedBuffer{cap: execStdoutCap}
	stderr := &cappedBuffer{cap: execStderrCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Context-driven cancel: kill the whole process GROUP (negative pid). WaitDelay
	// bounds how long a child that ignores the kill can keep pipes open.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = execWaitDelay

	if err := cmd.Start(); err != nil {
		return execResult{}, err
	}
	waitErr := cmd.Wait()

	res := execResult{
		Stdout:          stdout.buf,
		Stderr:          stderr.buf,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		ExitCode:        cmd.ProcessState.ExitCode(),
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
		}
		return res, ctx.Err()
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			// normal non-zero exit: ExitCode already set, NOT an infra error
			return res, nil
		}
		return res, waitErr
	}
	return res, nil
}

// rejectInheritableFDs lists this process's descriptor directory (dir:
// /proc/self/fd on Linux, /dev/fd on macOS) immediately before a native spawn
// and fails closed on any descriptor above stdio without FD_CLOEXEC. Both
// native backends pass inherited descriptors through to the payload, and
// neither re-checks writes or connects through an already-open descriptor
// against its policy. exempt, when non-nil, may excuse one such descriptor;
// it must refuse on any doubt. An entry that is gone by inspection (EBADF:
// closed during the scan, or the listing's own descriptor) is ignored. The
// audit never mutates descriptors it does not own. The names are read
// without stat: fdescfs cannot stat some descriptors, a kqueue among them.
// The scan and os/exec are not one kernel-atomic operation; that residual
// interval is the documented D10 limitation.
func rejectInheritableFDs(backend, dir string, exempt func(fd int) bool) error {
	f, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("tools: %s audit inherited fds: %w", backend, err)
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("tools: %s audit inherited fds: %w", backend, err)
	}
	for _, name := range names {
		fd, convErr := strconv.Atoi(name)
		if convErr != nil {
			return fmt.Errorf("tools: %s audit inherited fds: unexpected entry %q", backend, name)
		}
		if fd <= 2 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			return fmt.Errorf("tools: %s audit fd %d: %w", backend, fd, err)
		}
		if flags&unix.FD_CLOEXEC != 0 || (exempt != nil && exempt(fd)) {
			continue
		}
		return fmt.Errorf("tools: %s refuses to spawn with inheritable host fd %d (missing FD_CLOEXEC); "+
			"it was inherited from the process that launched this agent, which must set FD_CLOEXEC "+
			"on its own descriptors before spawning", backend, fd)
	}
	return nil
}
