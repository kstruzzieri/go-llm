//go:build unix

package consult

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const duplexShutdownGrace = 5 * time.Second
const duplexInterruptWriteGrace = 100 * time.Millisecond

// duplexOutput lets os/exec own and join the stdout copier as well as stderr.
// Its one-slot handoff is independent of the serialized stdin writer. Once an
// exchange fails, stop releases any pending handoff while counting/draining
// continues. True EOF is recorded by ReadFrom, independently of process exit.
type duplexOutput struct {
	*cappedWriter
	chunks chan []byte
	stop   <-chan struct{}
}

func (w *duplexOutput) Write(p []byte) (int, error) {
	n, err := w.cappedWriter.Write(p)
	select {
	case <-w.stop:
	case w.chunks <- bytes.Clone(p):
	}
	return n, err
}

func (w *duplexOutput) ReadFrom(r io.Reader) (int64, error) {
	defer close(w.chunks)
	n, err := io.Copy(struct{ io.Writer }{w}, r)
	w.mu.Lock()
	w.drained = err == nil
	w.mu.Unlock()
	return n, err
}

// runDuplex owns Start/Wait and joins its writer plus both os/exec copiers. The
// caller must apply runOutcome's host-failure precedence before the returned
// exchange error: an otherwise valid terminal cannot excuse a failed drain.
// Callbacks are synchronous, bounded parsing only; they must never perform I/O.
func runDuplex(ctx context.Context, spec runSpec, exchange duplexExchange) (out runOutcome, err error) {
	out.ExitCode = -1
	if len(spec.stdin) > maxStdinBytes || !utf8.ValidString(spec.stdin) {
		return out, errStdinInvalid
	}
	if spec.adapter != codexAdapter || spec.timeout <= 0 || spec.outputCap <= 0 || spec.outputCap > maxOutputBytes || exchange.start == nil || exchange.receive == nil {
		return out, errors.New("consult: invalid duplex invocation")
	}
	if spec.waitDelay <= 0 {
		spec.waitDelay = 5 * time.Second
	}
	env, err := newEnvelope()
	if err != nil {
		return out, err
	}
	defer func() {
		out.CleanupOK = os.RemoveAll(env.root) == nil
		if _, statErr := os.Stat(env.root); !os.IsNotExist(statErr) {
			out.CleanupOK = false
		}
	}()
	out.Cwds = env.cwds()
	childEnv, err := codexEnv(env)
	if err != nil {
		return out, err
	}
	childEnv = append(childEnv, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1")
	runCtx, cancel := context.WithTimeout(ctx, spec.timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, spec.command, spec.args...)
	var capHit atomic.Bool
	onExceed := func() {
		capHit.Store(true)
		cancel()
		// A cap may fire after caller cancellation already started its soft
		// interrupt window. It must still kill immediately in that case.
		_ = killGroup(cmd.Process.Pid)
	}
	stopDispatch := make(chan struct{})
	dispatchStopped := false
	stopReading := func() {
		if !dispatchStopped {
			dispatchStopped = true
			close(stopDispatch)
		}
	}
	defer stopReading()
	stdout := &duplexOutput{cappedWriter: &cappedWriter{cap: spec.outputCap, retain: true, onExceed: onExceed}, chunks: make(chan []byte, 1), stop: stopDispatch}
	stderr := &cappedWriter{cap: spec.outputCap, onExceed: onExceed}
	cmd.Dir, cmd.Env = env.cwd, childEnv
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = spec.waitDelay
	cmd.Cancel = func() error {
		if errors.Is(ctx.Err(), context.Canceled) && !capHit.Load() {
			// Start os/exec's bounded WaitDelay now, while the dispatcher has
			// a chance to send one confirmed-turn interrupt. Unsafe/no-ID
			// cancellation is killed directly by the dispatcher below.
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return out, err
	}
	defer func() { _ = stdin.Close() }()
	initial, initialErr := exchange.start(env.cwd)
	out.TargetSHA256, err = verifyExecTarget(spec.command, spec.sha256)
	if err != nil {
		return out, err
	}
	start := time.Now()
	if startErr := cmd.Start(); startErr != nil {
		out.Duration = time.Since(start)
		return out, fmt.Errorf("consult: start %s: %w", spec.command, startErr)
	}
	forceStop := func() {
		cancel()
		_ = killGroup(cmd.Process.Pid)
	}
	// A canceled parent suppresses runCtx's later deadline notification. Keep
	// the original caller/spec deadline authoritative during the interrupt
	// window as well; this timer never waits for an RPC acknowledgement.
	deadline, _ := runCtx.Deadline()
	hardDeadline := time.NewTimer(time.Until(deadline))
	defer hardDeadline.Stop()

	// Two queued batches accommodate initialized plus model/list when startup
	// notifications arrive together before the initial writer resumes. The
	// in-flight write and queued batches share the 1 MiB total outbound bound.
	// Saturation fails closed instead of blocking the stdout dispatcher.
	writes := make(chan []byte, 2)
	written := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		var writeErr error
		for p := range writes {
			if _, writeErr = stdin.Write(p); writeErr != nil {
				break
			}
		}
		// Wait also closes StdinPipe after exit. A repeated final close is
		// harmless once every required write has completed successfully.
		if closeErr := stdin.Close(); writeErr == nil && !errors.Is(closeErr, os.ErrClosed) {
			writeErr = closeErr
		}
		written <- writeErr
	}()
	waited := make(chan error, 1)
	go func() {
		defer workers.Done()
		waited <- cmd.Wait()
	}()
	var shutdown *time.Timer
	var shutdownC <-chan time.Time
	defer func() {
		if shutdown != nil {
			shutdown.Stop()
		}
	}()
	startShutdown := func(delay time.Duration) {
		if shutdown == nil {
			shutdown = time.NewTimer(delay)
		} else {
			shutdown.Reset(delay)
		}
		shutdownC = shutdown.C
	}
	writesClosed := false
	closeWrites := func() {
		if !writesClosed {
			writesClosed = true
			close(writes)
			// A queued write may prevent stdin from closing. Bound that phase
			// independently from the normal timer at actual stdin close.
			startShutdown(duplexShutdownGrace)
		}
	}
	setError := func(failure error) {
		if err == nil {
			err = failure
		}
	}
	outbound := 0
	exchangeClosed := false
	apply := func(action duplexAction, failure error) {
		setError(failure)
		if action.closeStdin {
			exchangeClosed = true
		}
		if len(action.write) > 0 {
			if writesClosed {
				setError(errDuplexWrite)
			} else if len(action.write) > maxOutputBytes-outbound {
				onExceed()
			} else {
				outbound += len(action.write)
				select {
				case writes <- bytes.Clone(action.write):
				default:
					setError(errDuplexBackpressure)
					forceStop()
				}
			}
		}
		if action.closeStdin || failure != nil {
			closeWrites()
		}
		if err != nil {
			stopReading()
		}
	}
	apply(initial, initialErr)
	chunks := stdout.chunks
	ctxDone := runCtx.Done()
	var pending []byte
	records := 0
	shutdownExpired := false
	deadlineExpired := false
	var interruptDeadline time.Time
	for waited != nil || written != nil || chunks != nil {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				chunks = nil
				// Process exit may have closed the writer queue first. That is
				// not the dispatcher's proof of a completed exchange.
				if len(pending) != 0 || !exchangeClosed {
					setError(errDuplexEOF)
				}
				closeWrites()
				continue
			}
			if dispatchStopped || capHit.Load() {
				continue
			}
			pending = append(pending, chunk...)
			for !dispatchStopped {
				lf := bytes.IndexByte(pending, '\n')
				if lf < 0 {
					break
				}
				records++
				if records > 4096 {
					apply(duplexAction{closeStdin: true}, errDuplexRecords)
					break
				}
				action, failure := exchange.receive(pending[:lf])
				pending = pending[lf+1:]
				apply(action, failure)
			}
		case writeErr := <-written:
			written = nil
			if writeErr != nil {
				setError(errDuplexWrite)
				stopReading()
				forceStop()
			}
			// WaitDelay starts only after cancellation or process exit. This
			// separate timer starts at actual stdin close, even for a leader
			// that ignores EOF and leaves both output streams open forever.
			if err == nil {
				if interruptDeadline.IsZero() {
					startShutdown(duplexShutdownGrace)
				} else {
					startShutdown(time.Until(interruptDeadline))
				}
			}
		case waitErr := <-waited:
			waited = nil
			out.WaitErrorKind = waitErrorKind(waitErr)
			out.WaitStatus = waitStatus(cmd.ProcessState)
			closeWrites()
			_ = stdin.Close() // Release a writer whose pipe holder outlived the leader.
		case <-ctxDone:
			ctxDone = nil
			var interrupt []byte
			if errors.Is(ctx.Err(), context.Canceled) && !capHit.Load() && err == nil && !writesClosed && exchange.interrupt != nil {
				interrupt = exchange.interrupt()
			}
			stopReading()
			if len(interrupt) != 0 {
				interruptDeadline = time.Now().Add(duplexShutdownGrace)
				apply(duplexAction{write: interrupt, closeStdin: true}, nil)
				// Writable peers receive the complete interrupt and stdin EOF.
				// A blocked writer gets only this short attempt; the full
				// cancellation grace is never spent waiting on a pipe write.
				startShutdown(duplexInterruptWriteGrace)
			} else {
				forceStop()
				closeWrites()
				_ = stdin.Close()
			}
		case <-hardDeadline.C:
			deadlineExpired = true
			forceStop()
			_ = stdin.Close()
		case <-shutdownC:
			shutdownC = nil
			if waited != nil {
				shutdownExpired = true
				forceStop()
				_ = stdin.Close()
			}
		}
	}
	workers.Wait()
	if !stdout.complete() || !stderr.complete() || shutdownExpired {
		out.WaitErrorKind = "wait-delay"
	}
	out.GroupCleanupOK = cleanupGroup(cmd.Process.Pid)
	out.Duration = time.Since(start)
	out.Stdout, out.StderrBytes = stdout.snapshot(), stderr.count()
	out.CapExceeded = capHit.Load()
	out.Canceled = errors.Is(ctx.Err(), context.Canceled)
	out.TimedOut = (errors.Is(runCtx.Err(), context.DeadlineExceeded) || deadlineExpired) && !out.CapExceeded
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	return out, err
}
