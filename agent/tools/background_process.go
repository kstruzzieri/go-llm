package tools

import "io"

// backgroundStarter is the detached-process seam (#346). It stays separate
// from commandRunner and deliberately minimal; execBackend composes both
// lifetimes behind the #440 runtime-selection boundary. newPlatformStarter
// returns the host implementation for the current platform.
type backgroundStarter interface {
	Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error)
}

// backgroundProcess is a started detached process — on unix, the leader of its
// own process group. The manager owns its lifetime after publication.
type backgroundProcess interface {
	PID() int
	// Wait blocks until the process completes; single-shot. Callers code
	// against this taxonomy, not the unix implementation:
	//   - nil err: normal completion — both a natural exit (real exit code)
	//     and a signal-kill (exitCode -1).
	//   - exec.ErrWaitDelay: the process HAS exited and exitCode is valid; a
	//     descendant abandoned the stdio pipes. Classify as a normal exit
	//     with an output caveat, never as infra failure.
	//   - any other non-nil err (exitCode -1): infra failure.
	// Residual same-group cleanup (the managed-process-group containment
	// policy) happens before the leader is reaped. managerKilled reports that
	// a manager SIGKILL attempt coincided with a signal-killed leader.
	Wait() (exitCode int, managerKilled bool, err error)
	Kill() error // SIGKILL the whole managed group; idempotent; never blocks on Wait
}
