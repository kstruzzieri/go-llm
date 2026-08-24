package tools

import "io"

// backgroundStarter is the detached-process seam (#346). Deliberately separate
// from commandRunner (run-to-completion) and deliberately minimal: sandbox work
// (#440) wraps it later — do not widen it. newPlatformStarter (defined per
// platform in background_unix.go / background_other.go) returns the host
// implementation.
type backgroundStarter interface {
	Start(spec execSpec, stdout, stderr io.Writer) (backgroundProcess, error)
}

// backgroundProcess is a started detached process — on unix, the leader of its
// own process group. The manager (Task 4) owns its lifetime after publication.
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
	// C1 residual-group cleanup happens inside, before return.
	Wait() (exitCode int, err error)
	Kill() error // SIGKILL the whole managed group; idempotent; never blocks on Wait
}
