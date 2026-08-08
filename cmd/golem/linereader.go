package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

// lineReader serializes line reads from a single underlying reader across the
// REPL prompt loop and the approval prompt.
//
// Scanning is demand-driven: a scan goroutine exists only while a ReadLine is
// outstanding or a previous one was abandoned by ctx cancellation. It is never
// running merely because the reader exists.
//
// That property is load-bearing, not tidiness. A scan parked in read(2) on the
// terminal is a second consumer of stdin, and `/edit` hands that same terminal
// to an external editor: every keystroke the scan wins is a line golem then
// runs as an agent goal, with whatever -allow-write/-allow-exec grants are
// active. Draining stdin between prompts is a privilege-escalation shape, not
// just lost input. The scanner path is reachable with -no-editor, TERM=dumb, on
// Windows, and after a mid-session MakeRaw fallback, so this cannot be left to
// the editor path alone.
//
// Not safe for concurrent use. Reads are serialized by construction -- the goal
// prompt reads only when idle, the approval prompt only mid-turn -- the same
// sole-reader contract editorSource relies on. Violating it is a data race the
// detector will report rather than a silent corruption.
type lineReader struct {
	sc *bufio.Scanner

	// inFlight is the delivery channel of the at-most-one live scan. Non-nil
	// means a scan is running: either the caller is waiting on it now, or a
	// previous caller lost the race to ctx cancellation and left it running.
	// Buffered by one so an abandoned scan can park its result and exit instead
	// of leaking, and so the next ReadLine picks that result up rather than
	// discarding what the user actually typed.
	inFlight chan scanResult

	done bool  // the stream ended; err is its final word
	err  error // nil on clean EOF, non-nil on scanner failure
}

// scanResult is one Scan outcome, carried whole so the receiving side needs no
// access to the Scanner.
type scanResult struct {
	line string
	ok   bool
	err  error
}

// newLineReader wraps r. It starts nothing: the first scan begins when a caller
// actually asks for a line.
func newLineReader(r io.Reader) *lineReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxGoalBytes)
	return &lineReader{sc: sc}
}

// ReadLine returns the next line. ok=false means no line: with a nil error it is
// a clean EOF; with a non-nil error it is either a scanner failure or ctx
// cancellation.
//
// Cancellation abandons the wait, never the line. read(2) on a terminal cannot
// be interrupted, so the scan continues and its result waits in inFlight for
// the next caller.
func (lr *lineReader) ReadLine(ctx context.Context) (string, bool, error) {
	if lr.done {
		return "", false, lr.err
	}
	if lr.inFlight == nil {
		ch := make(chan scanResult, 1)
		lr.inFlight = ch
		// Only the goroutine touches lr.sc, and only one exists at a time: a
		// successor is created solely after its predecessor's result has been
		// received, which happens after that goroutine's last Scanner call.
		go func() {
			if lr.sc.Scan() {
				ch <- scanResult{line: lr.sc.Text(), ok: true}
				return
			}
			ch <- scanResult{err: lr.sc.Err()}
		}()
	}
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case res := <-lr.inFlight:
		lr.inFlight = nil
		if !res.ok {
			lr.done = true
			lr.err = res.err
		}
		return res.line, res.ok, res.err
	}
}

// scannerSource is the non-TTY line source: today's bufio.Scanner behavior
// behind the lineSource seam. It stays the path for piped and scripted stdin,
// where a raw-mode editor would be wrong.
//
// It prints its own prompt because the seam requires it, and serializes that
// with asynchronous notices so a notice can neither bury a visible prompt nor
// print a phantom one before the read starts.
type scannerSource struct {
	lr  *lineReader
	out io.Writer

	mu         sync.Mutex
	readActive bool
	prompt     string
}

// Compile-time assertion: scannerSource must satisfy lineSource.
var _ lineSource = (*scannerSource)(nil)

func newScannerSource(in io.Reader, out io.Writer) *scannerSource {
	return &scannerSource{lr: newLineReader(in), out: out}
}

func (s *scannerSource) ReadGoal(ctx context.Context, prompt string) (string, bool, error) {
	return s.read(ctx, prompt)
}

func (s *scannerSource) ReadAnswer(ctx context.Context, prompt string) (string, bool, error) {
	return s.read(ctx, prompt)
}

func (s *scannerSource) read(ctx context.Context, prompt string) (string, bool, error) {
	// Marking the read active and printing the prompt are one critical
	// section: a notice landing between them would print a prompt of its own
	// and then be followed by this one.
	s.mu.Lock()
	s.readActive = true
	s.prompt = prompt
	_, _ = fmt.Fprint(s.out, prompt)
	s.mu.Unlock()

	line, ok, err := s.lr.ReadLine(ctx)

	s.mu.Lock()
	s.readActive = false
	s.mu.Unlock()
	return line, ok, err
}

// RecordGoal is a no-op. History belongs to the interactive editor; piped and
// scripted stdin must not accumulate a per-workspace file.
func (s *scannerSource) RecordGoal(string) {}

// IdleDisplay reprints the prompt only when one is already on screen. Between
// enterPrompt and the read that prints it, there is nothing to restore, so the
// message goes out alone and the upcoming read remains the sole prompt printer.
func (s *scannerSource) IdleDisplay(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readActive {
		_, _ = fmt.Fprintf(s.out, "\n%s\n%s", msg, s.prompt)
		return
	}
	_, _ = fmt.Fprintf(s.out, "%s\n", msg)
}

// Close releases nothing: the scanning goroutine lives for the process because
// os.Stdin has no cancelable read. Present so the seam has one lifecycle.
func (s *scannerSource) Close() error { return nil }
