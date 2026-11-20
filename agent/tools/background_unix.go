//go:build linux || darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// unixStarter starts detached background processes as leaders of their own
// process group. Unlike unixRunner it spawns with no context
// (context.Background): the manager owns lifetime after publication, so no
// context may kill the child behind its back. spawn nil means osExecSpawn.
type unixStarter struct{ spawn spawnFunc }

func newPlatformStarter() backgroundStarter { return unixStarter{} }

func (s unixStarter) Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error) {
	spawn := s.spawn
	if spawn == nil {
		spawn = osExecSpawn
	}
	cmd := spawn(context.Background(), spec, stdout, stderr)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Pid()
	observer, err := newProcessExitObserver(pid)
	if err != nil {
		// The child is still ours and unreaped, so this group signal cannot
		// target a reused PGID. Fail closed when safe exit observation is not
		// available on a shipped Unix platform.
		_ = signalProcessGroup(pid)
		_ = cmd.Wait()
		return nil, fmt.Errorf("observe background process exit: %w", err)
	}
	// Remember the PGID now (== child PID: the spawner makes it a group leader).
	return &unixProcess{
		cmd:      cmd,
		pgid:     pid,
		observer: observer,
		group:    processGroupGuard{pgid: pid},
	}, nil
}

type unixProcess struct {
	cmd      procCmd
	pgid     int
	observer processExitObserver
	group    processGroupGuard
}

func (p *unixProcess) PID() int { return p.pgid }

// Wait observes the direct child's exit without reaping it, closes the group
// signal gate with one residual SIGKILL while the child still pins the PGID,
// and only then calls cmd.Wait. A concurrent or later Kill therefore either
// linearizes before reap or becomes a no-op; it can never signal a reused ID.
func (p *unixProcess) Wait() (int, bool, error) {
	observeErr := p.observer.Wait()
	_ = p.group.close(false)
	_ = p.observer.Close()
	waitErr := p.cmd.Wait()
	state := p.cmd.State()
	if state == nil {
		// ECHILD-class wait failure: no exit status exists. Not reachable in
		// tests without faking wait(2); the guard is documented instead.
		if observeErr != nil {
			return -1, false, fmt.Errorf("observe background process exit: %w", observeErr)
		}
		return -1, false, waitErr
	}
	code := state.ExitCode() // killed-by-signal leader reports -1
	managerKilled := p.group.wasManagerKill(code)
	if observeErr != nil {
		return code, managerKilled, fmt.Errorf("observe background process exit: %w", observeErr)
	}
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			// Normal process exit (non-zero or signal): real code, nil error.
			return code, managerKilled, nil
		}
		// Preserve other wait errors, incl. exec.ErrWaitDelay — there the
		// process HAS exited and ProcessState is valid, so the real code
		// accompanies the error.
		return code, managerKilled, waitErr
	}
	return code, managerKilled, nil
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
func (p *unixProcess) Kill() error {
	return p.group.close(true)
}

type processGroupGuard struct {
	mu              sync.Mutex
	pgid            int
	closed          bool
	managerSignaled bool
}

func (g *processGroupGuard) close(manager bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	err := signalProcessGroup(g.pgid)
	if err == nil {
		g.closed = true
		g.managerSignaled = manager
	}
	return err
}

func (g *processGroupGuard) wasManagerKill(exitCode int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.managerSignaled && exitCode == -1
}

func signalProcessGroup(pgid int) error {
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}

type processExitObserver interface {
	Wait() error
	Close() error
}
