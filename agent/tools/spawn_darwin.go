//go:build darwin

package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

// posix_spawnattr flags (sys/spawn.h).
const (
	posixSpawnSetPgroup      = 0x0002
	posixSpawnSetSigdef      = 0x0004
	posixSpawnSetSigmask     = 0x0008
	posixSpawnCloexecDefault = 0x4000
)

// nativeCmd starts a command with posix_spawn and no launchd connection: the
// bootstrap special port is null, every exception port in excMaskAllPorts is
// null, and the registered-port table it would inherit is emptied first
// (registeredPortsGuard). Every descriptor except stdio is closed in the
// child (POSIX_SPAWN_CLOEXEC_DEFAULT). The child leads a new process group
// with an empty signal mask, and every signal this process does not ignore is
// reset to its default.
//
// Otherwise it behaves as the *exec.Cmd the unix runners use: stdin is
// /dev/null, a nil Stdout or Stderr is /dev/null, an equal pair shares one
// pipe, Start errors have os/exec's shape, Wait bounds the output copies by
// WaitDelay from process exit and then returns exec.ErrWaitDelay, and a
// non-success exit is an *exec.ExitError.
type nativeCmd struct {
	Path           string
	Args           []string // argv, including argv[0]
	Dir            string
	Env            []string // nil means os.Environ()
	Stdout, Stderr io.Writer
	WaitDelay      time.Duration
	// Cancel runs once when ctx is done before the process exits.
	// ponytail: unlike exec.CommandContext there is no default kill; the
	// caller always sets it.
	Cancel func() error

	ctx     context.Context
	process *os.Process
	state   *os.ProcessState
	readers []*os.File // parent ends of the output pipes
	copies  chan error // one result per reader's copy goroutine
	exited  chan struct{}
}

// newNativeCmd returns a nativeCmd for path with argv args. A nil ctx, or one
// that is never done (context.Background), means no context.
func newNativeCmd(ctx context.Context, path string, args []string) *nativeCmd {
	return &nativeCmd{Path: path, Args: args, ctx: ctx}
}

// Pid is the child's process ID, or 0 before a successful Start.
func (c *nativeCmd) Pid() int {
	if c.process == nil {
		return 0
	}
	return c.process.Pid
}

// State is the exited child's state, or nil before Wait.
func (c *nativeCmd) State() *os.ProcessState { return c.state }

// Start spawns the command. It fails closed: an error from the registered-port
// guard, any attribute or file action, or posix_spawn itself leaves no child
// and no descriptor behind.
func (c *nativeCmd) Start() error {
	if c.process != nil {
		return errors.New("exec: already started")
	}
	if err := registeredPortsGuard(); err != nil {
		return err
	}
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	argv := c.Args
	if len(argv) == 0 {
		argv = []string{c.Path}
	}
	env := c.Env
	if env == nil {
		// ponytail: os/exec also appends PWD here when Dir is set; no caller
		// passes a nil Env.
		env = os.Environ()
	}
	path, argvp, envp, dir, err := spawnStrings(c.Path, argv, env, c.Dir)
	if err != nil {
		return &os.PathError{Op: "fork/exec", Path: c.Path, Err: err}
	}
	devNull, err := syscall.BytePtrFromString(os.DevNull)
	if err != nil {
		return err
	}

	var attr, fa uintptr
	if rc := posixSpawnattrInit(&attr); rc != 0 {
		return spawnErr("posix_spawnattr_init", rc)
	}
	defer posixSpawnattrDestroy(&attr)
	if rc := posixSpawnFileActionsInit(&fa); rc != 0 {
		return spawnErr("posix_spawn_file_actions_init", rc)
	}
	defer posixSpawnFileActionsDestroy(&fa)

	var childEnds, readers []*os.File
	var writers []io.Writer
	started := false
	defer func() {
		closeFiles(childEnds)
		if !started {
			closeFiles(readers)
		}
	}()
	// output wires fd to a fresh pipe feeding w, or to /dev/null for nil w.
	output := func(fd int32, w io.Writer) error {
		if w == nil {
			return spawnErr("posix_spawn_file_actions_addopen", posixSpawnFileActionsAddopen(&fa, fd, devNull, syscall.O_WRONLY, 0))
		}
		r, pw, err := os.Pipe()
		if err != nil {
			return err
		}
		childEnds, readers, writers = append(childEnds, pw), append(readers, r), append(writers, w)
		// Fd puts pw in blocking mode, which the child's stdio needs.
		return spawnErr("posix_spawn_file_actions_adddup2", posixSpawnFileActionsAdddup2(&fa, int32(pw.Fd()), fd))
	}

	noMask, sigdef := uint32(0), sigDefaultSet()
	err = errors.Join(
		spawnErr("posix_spawnattr_setflags", posixSpawnattrSetflags(&attr,
			posixSpawnSetPgroup|posixSpawnSetSigmask|posixSpawnSetSigdef|posixSpawnCloexecDefault)),
		spawnErr("posix_spawnattr_setpgroup", posixSpawnattrSetpgroup(&attr, 0)),
		spawnErr("posix_spawnattr_setsigmask", posixSpawnattrSetsigmask(&attr, &noMask)),
		spawnErr("posix_spawnattr_setsigdefault", posixSpawnattrSetsigdefault(&attr, &sigdef)),
		spawnErr("posix_spawnattr_setspecialport_np", posixSpawnattrSetspecialportNp(&attr, machPortNull, taskBootstrapPort)),
		spawnErr("posix_spawnattr_setexceptionports_np", posixSpawnattrSetexceptionportsNp(&attr,
			excMaskAllPorts, machPortNull, exceptionDefault, threadStateNone)),
		spawnErr("posix_spawn_file_actions_addopen", posixSpawnFileActionsAddopen(&fa, 0, devNull, syscall.O_RDONLY, 0)),
	)
	if err != nil {
		return err
	}
	if err := output(1, c.Stdout); err != nil {
		return err
	}
	if c.Stderr != nil && interfaceEqual(c.Stderr, c.Stdout) {
		err = spawnErr("posix_spawn_file_actions_adddup2", posixSpawnFileActionsAdddup2(&fa, 1, 2))
	} else {
		err = output(2, c.Stderr)
	}
	if err != nil {
		return err
	}
	if dir != nil {
		if err := spawnErr("posix_spawn_file_actions_addchdir_np", posixSpawnFileActionsAddchdirNp(&fa, dir)); err != nil {
			return err
		}
	}

	var pid int32
	if rc := posixSpawn(&pid, path, &fa, &attr, &argvp[0], &envp[0]); rc != 0 {
		return &os.PathError{Op: "fork/exec", Path: c.Path, Err: syscall.Errno(rc)}
	}
	started = true
	// FindProcess never fails on unix.
	c.process, _ = os.FindProcess(int(pid))
	c.readers = readers
	c.copies = make(chan error, len(readers))
	c.exited = make(chan struct{})
	for i, r := range readers {
		go func() {
			_, err := io.Copy(writers[i], r)
			_ = r.Close()
			c.copies <- err
		}()
	}
	if c.ctx != nil && c.ctx.Done() != nil {
		go c.watchCtx()
	}
	return nil
}

// watchCtx calls Cancel once if ctx is done before the process exits. If the
// process has still not exited WaitDelay later, it kills the process (Cancel
// may have missed it) and then closes the output pipes, as os/exec does; if it
// exits first, Wait bounds the pipes.
func (c *nativeCmd) watchCtx() {
	select {
	case <-c.exited:
		return
	case <-c.ctx.Done():
	}
	if c.Cancel != nil {
		_ = c.Cancel()
	}
	if c.WaitDelay <= 0 {
		return
	}
	t := time.NewTimer(c.WaitDelay)
	defer t.Stop()
	select {
	case <-c.exited:
	case <-t.C:
		// Kill before closing, so the child dies of SIGKILL, not SIGPIPE. An
		// os.ErrProcessDone from a racing exit is harmless.
		_ = c.process.Kill()
		closeFiles(c.readers)
	}
}

// Wait waits for the process to exit, then for its output to drain, for at
// most WaitDelay after exit when WaitDelay is set.
func (c *nativeCmd) Wait() error {
	if c.process == nil {
		return errors.New("exec: not started")
	}
	// exited, not state: state stays nil when the wait itself fails.
	select {
	case <-c.exited:
		return errors.New("exec: Wait was already called")
	default:
	}
	state, err := c.process.Wait()
	if err == nil && !state.Success() {
		err = &exec.ExitError{ProcessState: state}
	}
	c.state = state
	close(c.exited)
	if copyErr := c.awaitCopies(); err == nil {
		err = copyErr
	}
	closeFiles(c.readers)
	return err
}

// awaitCopies collects the copy goroutines' results. If WaitDelay expires
// first it closes the pipes (a descendant may still hold the write ends),
// waits for the goroutines to see that, and returns exec.ErrWaitDelay.
func (c *nativeCmd) awaitCopies() error {
	var expired <-chan time.Time
	if c.WaitDelay > 0 {
		t := time.NewTimer(c.WaitDelay)
		defer t.Stop()
		expired = t.C
	}
	var first error
	for n := len(c.readers); n > 0; n-- {
		select {
		case err := <-c.copies:
			if first == nil {
				first = err
			}
		case <-expired:
			closeFiles(c.readers)
			for ; n > 0; n-- {
				<-c.copies
			}
			return exec.ErrWaitDelay
		}
	}
	return first
}

// spawnStrings converts the spawn's strings to C strings, after os/exec's
// last-wins dedup of env. Any NUL byte is EINVAL: a truncated argv is not the
// command that was approved. dir is nil when empty.
func spawnStrings(path string, argv, env []string, dirStr string) (p *byte, argvp, envp []*byte, dir *byte, err error) {
	if slices.ContainsFunc(env, func(kv string) bool { return strings.IndexByte(kv, 0) >= 0 }) {
		return nil, nil, nil, nil, syscall.EINVAL
	}
	if p, err = syscall.BytePtrFromString(path); err != nil {
		return
	}
	if argvp, err = syscall.SlicePtrFromStrings(argv); err != nil {
		return
	}
	if envp, err = syscall.SlicePtrFromStrings(dedupEnv(env)); err != nil {
		return
	}
	if dirStr != "" {
		dir, err = syscall.BytePtrFromString(dirStr)
	}
	return
}

// dedupEnv keeps the last value of each key, in order, as os/exec does.
func dedupEnv(env []string) []string {
	out := make([]string, 0, len(env))
	saw := make(map[string]bool, len(env))
	for _, kv := range slices.Backward(env) {
		i := strings.Index(kv, "=")
		if i == 0 {
			i = strings.Index(kv[1:], "=") + 1
		}
		if i < 0 {
			if kv != "" {
				out = append(out, kv)
			}
			continue
		}
		if saw[kv[:i]] {
			continue
		}
		saw[kv[:i]] = true
		out = append(out, kv)
	}
	slices.Reverse(out)
	return out
}

// sigDefaultSet is every signal 1-31 except SIGKILL and SIGSTOP, which cannot
// be reset, and the signals this process ignores, which an os/exec child keeps
// ignored.
func sigDefaultSet() uint32 {
	var set uint32
	for s := syscall.Signal(1); s <= 31; s++ {
		if s == syscall.SIGKILL || s == syscall.SIGSTOP || signal.Ignored(s) {
			continue
		}
		set |= 1 << (s - 1)
	}
	return set
}

func spawnErr(op string, rc int32) error {
	if rc == 0 {
		return nil
	}
	return fmt.Errorf("tools: %s: %w", op, syscall.Errno(rc))
}

// interfaceEqual is os/exec's: == that reports false instead of panicking on
// an uncomparable dynamic type.
func interfaceEqual(a, b any) bool {
	defer func() { _ = recover() }()
	return a == b
}

func closeFiles(fs []*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}
