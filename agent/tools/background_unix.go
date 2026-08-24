//go:build unix

package tools

import (
	"errors"
	"io"
	"os/exec"
	"syscall"
)

// unixStarter starts detached background processes as leaders of their own
// process group. Unlike unixRunner it uses exec.Command, NOT CommandContext:
// the manager owns lifetime after publication, so no context may kill the
// child behind its back.
type unixStarter struct{}

func newPlatformStarter() backgroundStarter { return unixStarter{} }

func (unixStarter) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	cmd := exec.Command(spec.Path, spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	// cmd.Stdin stays nil: os/exec wires /dev/null, so a child that prompts
	// for input reads immediate EOF instead of hanging forever.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// WaitDelay bounds Wait when a same-group descendant outlives the leader
	// while holding the stdio pipes open.
	cmd.WaitDelay = execWaitDelay
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// Remember the PGID now (== child PID because Setpgid with Pgid 0).
	// cmd.Process is never nil after a successful Start; the real constraint
	// is never re-deriving the group via Getpgid on a pid that may since have
	// died or been reaped.
	return &unixProcess{cmd: cmd, pgid: cmd.Process.Pid}, nil
}

type unixProcess struct {
	cmd  *exec.Cmd
	pgid int
}

func (p *unixProcess) PID() int { return p.pgid }

// Wait blocks for the direct child, then applies the managed-process-group
// containment policy: best-effort SIGKILL of the remembered process group
// reaps residual same-group descendants before completion is reported,
// without rewriting how the leader's own exit is reported.
func (p *unixProcess) Wait() (int, error) {
	waitErr := p.cmd.Wait()
	// Residual-group cleanup, best-effort. An accepted PGID-reuse limitation
	// of that policy: the direct child was just reaped, so if no descendant
	// survives the pgid may already be recycled to an unrelated same-uid
	// group; the window between reap and this kill is microseconds.
	_ = syscall.Kill(-p.pgid, syscall.SIGKILL)
	if p.cmd.ProcessState == nil {
		// ECHILD-class wait failure: no exit status exists. Not reachable in
		// tests without faking wait(2); the guard is documented instead.
		return -1, waitErr
	}
	code := p.cmd.ProcessState.ExitCode() // killed-by-signal leader reports -1
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			// Normal process exit (non-zero or signal): real code, nil error.
			return code, nil
		}
		// Preserve other wait errors, incl. exec.ErrWaitDelay — there the
		// process HAS exited and ProcessState is valid, so the real code
		// accompanies the error.
		return code, waitErr
	}
	return code, nil
}

// Kill SIGKILLs the whole managed group. Idempotent; never waits.
//
// ESRCH (group gone) is success. EPERM is treated as success too: children
// normally run with our real uid, so EPERM on the remembered pgid usually
// means a zombie-only group (darwin returns EPERM, not ESRCH, when the sole
// member is exited-but-unreaped — the routine stop-vs-reap window) or a
// recycled pgid now owned by another uid, both correctly "nothing left for
// us to kill" under the managed-process-group containment policy. Known
// limitation of that policy: a group member whose real uid changed (which
// requires the user to have approved a sudo-style command) is live but
// unsignalable, so EPERM then masks it and the process lingers until it
// exits on its own.
//
// An accepted PGID-reuse limitation of that policy: once the last member is
// reaped the pgid can be recycled; a late Kill could then signal an
// unrelated same-uid group. The manager should prefer not to Kill after
// Wait has returned, which shrinks the window to microseconds.
func (p *unixProcess) Kill() error {
	err := syscall.Kill(-p.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
