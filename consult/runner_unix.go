//go:build unix

package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// envelope is the per-run private directory set. Everything the child is told
// about the filesystem, apart from the real HOME its subscription credentials
// live in, points inside this root, and the root is removed after the run.
type envelope struct {
	root, cwd, tmp, config, cache, state string
}

// cwds returns the private cwd as passed to the child and, when different, its
// symlink-resolved spelling (macOS temp roots are symlinks and a child
// reporting getcwd() sees the physical path).
func (e *envelope) cwds() []string {
	out := []string{e.cwd}
	if real, err := filepath.EvalSymlinks(e.cwd); err == nil && real != e.cwd {
		out = append(out, real)
	}
	return out
}

func newEnvelope() (*envelope, error) {
	root, err := os.MkdirTemp("", "golem-consult-")
	if err != nil {
		return nil, fmt.Errorf("consult: envelope root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("consult: envelope chmod: %w", err)
	}
	e := &envelope{root: root}
	for name, dst := range map[string]*string{
		"cwd": &e.cwd, "tmp": &e.tmp, "config": &e.config, "cache": &e.cache, "state": &e.state,
	} {
		p := filepath.Join(root, name)
		if err := os.Mkdir(p, 0o700); err != nil {
			return nil, fmt.Errorf("consult: envelope %s: %w", name, err)
		}
		*dst = p
	}
	return e, nil
}

// buildEnv constructs the exact eleven-name child environment from scratch.
// Nothing is inherited from the parent process, so no API key, proxy or
// telemetry variable in the caller's environment can reach the consultant.
func buildEnv(e *envelope) ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("consult: home directory: %w", err)
	}
	// The Claude CLI uses USER as its macOS Keychain account lookup key.
	// Resolve it from the uid; never inherit a caller-selected identity.
	identity, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || identity.Username == "" {
		return nil, errors.New("consult: OS username lookup failed")
	}
	return []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"LC_ALL=C",
		"HOME=" + home,
		"TMPDIR=" + e.tmp,
		"USER=" + identity.Username,
		"XDG_CONFIG_HOME=" + e.config,
		"XDG_CACHE_HOME=" + e.cache,
		"XDG_STATE_HOME=" + e.state,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"ENABLE_CLAUDEAI_MCP_SERVERS=false",
	}, nil
}

// cappedWriter counts every byte, retains at most cap of them when retain is
// set, drains the rest without blocking, and fires onExceed exactly once when
// the cap is crossed.
type cappedWriter struct {
	mu       sync.Mutex
	buf      []byte
	retain   bool
	n        int
	cap      int
	exceeded bool
	onExceed func()
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if keep := w.cap - w.n; w.retain && keep > 0 {
		if len(p) < keep {
			keep = len(p)
		}
		w.buf = append(w.buf, p[:keep]...)
	}
	w.n += len(p)
	fire := !w.exceeded && w.n > w.cap
	if fire {
		w.exceeded = true
	}
	w.mu.Unlock()
	if fire && w.onExceed != nil {
		w.onExceed()
	}
	return len(p), nil
}

func (w *cappedWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func (w *cappedWriter) snapshot() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

// run executes one consultant command inside a disposable envelope with a
// from-scratch environment, a hard deadline, capped output, and process-group
// lifetime control. It returns an error only when the run could not be made at
// all; every bounded termination is reported in the outcome instead.
func run(ctx context.Context, spec runSpec) (out runOutcome, err error) {
	out.ExitCode = -1
	if len(spec.stdin) > maxStdinBytes || !utf8.ValidString(spec.stdin) {
		return out, fmt.Errorf("%w: input exceeds 64 KiB or is not valid UTF-8", errStdinInvalid)
	}
	if spec.timeout <= 0 || spec.outputCap <= 0 {
		return out, errors.New("consult: timeout and output cap must be positive")
	}
	if spec.waitDelay <= 0 {
		spec.waitDelay = 5 * time.Second
	}

	env, envErr := newEnvelope()
	if envErr != nil {
		return out, envErr
	}
	defer func() {
		out.CleanupOK = os.RemoveAll(env.root) == nil
		if _, statErr := os.Stat(env.root); !os.IsNotExist(statErr) {
			out.CleanupOK = false
		}
	}()
	out.Cwds = env.cwds()

	childEnv, envErr := buildEnv(env)
	if envErr != nil {
		return out, envErr
	}

	runCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()

	capHit := &atomic.Bool{}
	onExceed := func() {
		capHit.Store(true)
		cancel()
	}
	outW := &cappedWriter{cap: spec.outputCap, retain: true, onExceed: onExceed}
	// Stderr is counted and drained but never retained: consultant diagnostics
	// must not cross this boundary.
	errW := &cappedWriter{cap: spec.outputCap, onExceed: onExceed}

	cmd := exec.CommandContext(runCtx, spec.command, spec.args...)
	cmd.Dir = env.cwd
	cmd.Env = childEnv
	cmd.Stdin = strings.NewReader(spec.stdin)
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = spec.waitDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		// Negative PID: SIGKILL the whole process group. A descendant that
		// deliberately starts a new session escapes this.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// Verify the resolved regular file immediately before exec, never a
	// launcher symlink the kernel would re-resolve. The residual window is the
	// microseconds between this read and execve; an updater replaces by
	// rename, which this catches.
	if verifyErr := verifyExecTarget(spec.command, spec.sha256); verifyErr != nil {
		return out, verifyErr
	}

	start := time.Now()
	if startErr := cmd.Start(); startErr != nil {
		out.Duration = time.Since(start)
		return out, fmt.Errorf("consult: start %s: %w", spec.command, startErr)
	}
	waitErr := cmd.Wait()
	out.WaitErrorKind = waitErrorKind(waitErr)
	out.WaitStatus = waitStatus(cmd.ProcessState)

	// Same-group descendants can outlive a normally exiting leader; setsid
	// escapes remain outside this trust boundary.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	deadline := time.Now().Add(time.Second)
	for {
		if syscall.Kill(-cmd.Process.Pid, 0) == syscall.ESRCH {
			out.GroupCleanupOK = true
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	out.Duration = time.Since(start)
	out.Stdout = outW.snapshot()
	out.StderrBytes = errW.count()
	out.CapExceeded = capHit.Load()
	// Cancellation is the caller's, never our own deadline or cap abort.
	out.Canceled = errors.Is(ctx.Err(), context.Canceled)
	out.TimedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded) && !out.CapExceeded
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	return out, nil
}

// verifyExecTarget refuses symlinks and non-regular files and, when a digest is
// expected, requires the file bytes to match it.
func verifyExecTarget(path, expected string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: command must be an absolute path", errTargetInvalid)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: lstat failed", errTargetInvalid)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: must be a regular file, not a symlink", errTargetInvalid)
	}
	if expected == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%w: open failed", errTargetInvalid)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("%w: read failed", errTargetInvalid)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expected {
		return fmt.Errorf("%w: digest drift immediately before exec", errTargetDrift)
	}
	return nil
}

// waitStatus renders the leader's wait status as a fixed literal.
func waitStatus(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := "other"
		switch ws.Signal() {
		case syscall.SIGKILL:
			name = "SIGKILL"
		case syscall.SIGTERM:
			name = "SIGTERM"
		case syscall.SIGINT:
			name = "SIGINT"
		}
		return "signaled(" + name + ")"
	}
	return "exited(" + strconv.Itoa(ps.ExitCode()) + ")"
}

// waitErrorKind maps cmd.Wait's error to a fixed literal. Only "none" and
// "exit" mean the pipes were drained to EOF; "wait-delay" means WaitDelay
// abandoned the copy and "other" is any unexpected error. Both fail closed.
func waitErrorKind(err error) string {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, exec.ErrWaitDelay):
		return "wait-delay"
	case errors.As(err, &exitErr):
		return "exit"
	default:
		return "other"
	}
}
