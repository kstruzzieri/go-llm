//go:build unix

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// procCmd is one child process as the unix runner and starter drive it:
// *exec.Cmd (osExecCmd) or, on darwin, nativeCmd.
type procCmd interface {
	Start() error
	Wait() error
	Pid() int                // 0 before a successful Start
	State() *os.ProcessState // nil before Wait, or after a failed wait
}

// spawnFunc builds an unstarted command for spec that leads its own process
// group, reads stdin from /dev/null, writes to stdout and stderr, and bounds
// Wait by execWaitDelay. A ctx that is never done (context.Background) means
// no context; otherwise ctx being done kills the whole group.
type spawnFunc func(ctx context.Context, spec execSpec, stdout, stderr io.Writer) procCmd

// osExecSpawn is the default spawnFunc, on os/exec.
func osExecSpawn(ctx context.Context, spec execSpec, stdout, stderr io.Writer) procCmd {
	var cmd *exec.Cmd
	if ctx.Done() == nil {
		// The starter's path: the manager owns lifetime after publication, so
		// no context may kill the child behind its back. A runner given a
		// never-done ctx (context.Background) lands here too, and that is
		// equivalent: exec.CommandContext with a ctx that is never done never
		// calls Cancel, so the group-kill below could not fire anyway.
		cmd = exec.Command(spec.Path, spec.Argv[1:]...)
	} else {
		// CommandContext (NOT exec.Command) is REQUIRED: cmd.Cancel is only
		// consulted when the command was created with a context — with
		// exec.Command the custom group-kill Cancel would never fire on timeout.
		cmd = exec.CommandContext(ctx, spec.Path, spec.Argv[1:]...)
		// Context-driven cancel: kill the whole process GROUP (negative pid).
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
	}
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	// cmd.Stdin stays nil: os/exec wires /dev/null, so a child that prompts
	// for input reads immediate EOF instead of hanging forever.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay bounds how long a child that ignores the kill, or a same-group
	// descendant that outlives the leader, can keep the pipes open.
	cmd.WaitDelay = execWaitDelay
	return osExecCmd{cmd}
}

// osExecCmd adapts *exec.Cmd to procCmd.
type osExecCmd struct{ *exec.Cmd }

func (c osExecCmd) Pid() int {
	if c.Process == nil {
		return 0
	}
	return c.Process.Pid
}

func (c osExecCmd) State() *os.ProcessState { return c.ProcessState }

// unixRunner executes commands in their own process group so a timeout/cancel can kill
// the whole group (reaping ordinary children), and bounds stdout/stderr to the tool's
// self-caps while always consuming pipe writes (no deadlock). spawn starts the
// command; nil means osExecSpawn.
type unixRunner struct{ spawn spawnFunc }

func newPlatformRunner() commandRunner { return unixRunner{} }

func (r unixRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	spawn := r.spawn
	if spawn == nil {
		spawn = osExecSpawn
	}
	stdout := &cappedBuffer{cap: execStdoutCap}
	stderr := &cappedBuffer{cap: execStderrCap}
	cmd := spawn(ctx, spec, stdout, stderr)
	if err := cmd.Start(); err != nil {
		return execResult{}, err
	}
	waitErr := cmd.Wait()

	res := execResult{
		Stdout:          stdout.buf,
		Stderr:          stderr.buf,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		ExitCode:        cmd.State().ExitCode(),
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
// and fails closed on any descriptor above stdio without FD_CLOEXEC. The
// bwrap backend passes inherited descriptors through to the payload; a
// Seatbelt command starts with POSIX_SPAWN_CLOEXEC_DEFAULT, so only stdio
// reaches it and the audit is a second line there. Neither backend re-checks
// writes or connects through an already-open descriptor against its policy.
// maySetCloexec, when non-nil, identifies descriptors that may be marked
// close-on-exec instead of refused; it must refuse on any doubt. An entry
// gone by inspection (EBADF: closed during the scan, or the listing's own
// descriptor) is ignored. The audit changes no other descriptor flags and
// never closes an audited descriptor. The names are read without stat:
// fdescfs cannot stat some descriptors, a kqueue among them. Under bwrap the
// scan and the spawn are not one kernel-atomic operation; that residual
// interval is the documented D10 limitation.
func rejectInheritableFDs(backend, dir string, maySetCloexec func(fd int) bool) error {
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
		if flags&unix.FD_CLOEXEC != 0 {
			continue
		}
		if maySetCloexec != nil && maySetCloexec(fd) {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
				return fmt.Errorf("tools: %s mark fd %d close-on-exec: %w", backend, fd, err)
			}
			continue
		}
		return fmt.Errorf("tools: %s refuses to spawn with inheritable host fd %d (missing FD_CLOEXEC); "+
			"it was inherited from the process that launched this agent, which must set FD_CLOEXEC "+
			"on its own descriptors before spawning", backend, fd)
	}
	return nil
}
