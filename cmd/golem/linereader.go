package main

import (
	"bufio"
	"context"
	"io"
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
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
