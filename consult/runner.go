package consult

import (
	"errors"
	"time"
)

// maxStdinBytes bounds the prompt handed to a consultant. The Stage 0 evidence
// gate ran under the same 64 KiB ceiling.
const maxStdinBytes = 65536

// Sentinel errors returned by run so callers can classify a failure without
// matching message text. Everything else (envelope creation, exec start) is an
// internal failure of the run itself.
var (
	// errStdinInvalid reports a prompt that is too large or not valid UTF-8.
	errStdinInvalid = errors.New("consult: invalid input")
	// errTargetInvalid reports an exec target that is not a regular file, is a
	// symlink, or could not be read immediately before exec.
	errTargetInvalid = errors.New("consult: invalid exec target")
	// errTargetDrift reports that the exec target's bytes no longer match the
	// configured digest.
	errTargetDrift = errors.New("consult: exec target digest drift")
)

// runSpec describes one bounded child invocation. Nothing here is inherited
// from the parent process: the runner builds the child environment from
// scratch and executes inside a disposable private filesystem envelope.
type runSpec struct {
	// command is the absolute path of the executable; sha256, when set, is the
	// lowercase hex digest its bytes must match immediately before exec.
	command, sha256 string
	args            []string
	stdin           string
	timeout         time.Duration
	waitDelay       time.Duration // grace after cancel before Wait abandons pipe I/O; 0 => 5s
	outputCap       int           // max retained/counted bytes per stream before the run is aborted
}

// runOutcome is the redacted result of one run. Stderr content is counted but
// never retained, so no field can carry it across this boundary.
type runOutcome struct {
	Stdout        []byte // capped at runSpec.outputCap
	StderrBytes   int
	ExitCode      int
	WaitStatus    string // exited(N) | signaled(SIGKILL|SIGTERM|SIGINT|other)
	WaitErrorKind string // none | exit | wait-delay | other
	TimedOut      bool
	// Canceled is set only for context.Canceled on the caller's context. A
	// deadline the caller imposed lands in TimedOut instead, by design: both are
	// "the caller stopped waiting", but only Canceled is an explicit abort.
	Canceled       bool
	CapExceeded    bool
	GroupCleanupOK bool
	CleanupOK      bool
	Duration       time.Duration
	Cwds           []string // private cwd spellings, for the parser's cwd check
}
