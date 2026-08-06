package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

// lineReader serializes line reads from a single underlying reader across the REPL
// prompt loop and the approval prompt. One goroutine scans lines into an unbuffered
// channel; ReadLine selects on the channel or ctx, so a read can be canceled
// (Ctrl-C during an approval) without a second reader stealing buffered input.
type lineReader struct {
	lines chan string
	err   error // final scanner error, set before lines is closed
}

// newLineReader starts the single scanning goroutine over r. The caller must
// ensure r eventually reaches EOF or is closed so the goroutine can exit; until
// then it lives for the process. For the REPL this is os.Stdin (process lifetime).
func newLineReader(r io.Reader) *lineReader {
	lr := &lineReader{lines: make(chan string)}
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxGoalBytes)
		for sc.Scan() {
			lr.lines <- sc.Text()
		}
		// Set err BEFORE closing: this write is sequenced before close(lr.lines),
		// which happens-before any receive that observes the closed channel, so
		// ReadLine reads lr.err race-free (write → close → receive → read).
		lr.err = sc.Err()
		close(lr.lines)
	}()
	return lr
}

// ReadLine returns the next line. ok=false means no line: with a nil error it is a
// clean EOF; with a non-nil error it is either a scanner failure (channel closed
// after an error) or ctx cancellation.
func (lr *lineReader) ReadLine(ctx context.Context) (string, bool, error) {
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case line, ok := <-lr.lines:
		if !ok {
			return "", false, lr.err // nil on clean EOF, non-nil on scanner error
		}
		return line, true, nil
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
