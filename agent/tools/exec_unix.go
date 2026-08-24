//go:build unix

package tools

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// unixRunner executes commands in their own process group so a timeout/cancel can kill
// the whole group (reaping ordinary children), and bounds stdout/stderr to the tool's
// self-caps while always consuming pipe writes (no deadlock).
type unixRunner struct{}

func newPlatformRunner() commandRunner { return unixRunner{} }

func (unixRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	// CommandContext (NOT exec.Command) is REQUIRED: cmd.Cancel and cmd.WaitDelay are
	// only consulted when the command was created with a context. With exec.Command the
	// custom group-kill Cancel would never fire on timeout.
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

	// ProcessState can be nil when Wait fails at the syscall level before it is
	// populated (ECHILD-class race: the child was reaped elsewhere). Calling
	// ExitCode() on it would panic. -1 mirrors (*os.ProcessState).ExitCode()'s
	// own value for a process that has not exited, so callers see one sentinel.
	// Not deterministically testable — the race needs an external reaper — so
	// the guard is documented here instead.
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	res := execResult{
		Stdout:          stdout.buf,
		Stderr:          stderr.buf,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		ExitCode:        exitCode,
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
