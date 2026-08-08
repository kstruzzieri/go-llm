package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedReader makes every Read observable and blocking, so a test can prove
// whether a scan is in flight at a moment when golem is not asking for a line.
// Each Read announces itself on entered and then waits for the test to hand it
// bytes, so nothing about the timing is left to the scheduler.
type gatedReader struct {
	entered chan struct{}
	chunks  chan []byte

	mu            sync.Mutex
	live, maxLive int
}

func newGatedReader() *gatedReader {
	return &gatedReader{entered: make(chan struct{}), chunks: make(chan []byte)}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.mu.Lock()
	g.live++
	if g.live > g.maxLive {
		g.maxLive = g.live
	}
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.live--
		g.mu.Unlock()
	}()

	g.entered <- struct{}{}
	b, ok := <-g.chunks
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (g *gatedReader) concurrentPeak() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxLive
}

// requireNoScanInFlight fails if a Read is entered while nobody asked for a
// line. That is the /edit hazard: a scan sitting in read(2) on the terminal
// steals the keystrokes meant for the external editor, and golem then runs them
// as agent goals with whatever tool grants are active.
func requireNoScanInFlight(t *testing.T, g *gatedReader) {
	t.Helper()
	select {
	case <-g.entered:
		t.Fatal("a scan entered Read while no ReadLine was outstanding; the child editor would be competing for stdin")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLineReaderDoesNotScanBetweenReads(t *testing.T) {
	g := newGatedReader()
	lr := newLineReader(g)

	type result struct {
		line string
		ok   bool
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, ok, err := lr.ReadLine(context.Background())
		got <- result{line, ok, err}
	}()

	<-g.entered // the scan this ReadLine asked for
	g.chunks <- []byte("/edit\n")
	if r := <-got; !r.ok || r.err != nil || r.line != "/edit" {
		t.Fatalf("ReadLine = %q ok=%v err=%v", r.line, r.ok, r.err)
	}

	// This is the assertion the whole change exists for.
	requireNoScanInFlight(t, g)

	// ...and the reader is not simply dead: the next request does scan.
	go func() {
		line, ok, err := lr.ReadLine(context.Background())
		got <- result{line, ok, err}
	}()
	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the next ReadLine never started a scan")
	}
	g.chunks <- []byte("next goal\n")
	if r := <-got; !r.ok || r.line != "next goal" {
		t.Fatalf("second ReadLine = %q ok=%v err=%v", r.line, r.ok, r.err)
	}
	if peak := g.concurrentPeak(); peak != 1 {
		t.Fatalf("peak concurrent Read entries = %d, want 1", peak)
	}
}

func TestLineReaderKeepsACancelledLinePending(t *testing.T) {
	// Ctrl-C at an approval prompt cancels the read, but read(2) cannot be
	// cancelled. Whatever the user typed must survive to the next request
	// instead of being consumed by a goroutine nobody is listening to.
	g := newGatedReader()
	lr := newLineReader(g)

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	go func() {
		<-g.entered
		close(entered)
	}()

	done := make(chan error, 1)
	go func() {
		_, _, err := lr.ReadLine(ctx)
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled read returned a nil error")
	}

	// The line lands after the reader gave up; it must not be lost.
	g.chunks <- []byte("typed anyway\n")

	line, ok, err := lr.ReadLine(context.Background())
	if !ok || err != nil || line != "typed anyway" {
		t.Fatalf("next ReadLine = %q ok=%v err=%v, want the pending line", line, ok, err)
	}
	if peak := g.concurrentPeak(); peak != 1 {
		t.Fatalf("peak concurrent Read entries = %d, want 1: a cancelled read must not leave a second scanner behind", peak)
	}
}

func TestLineReaderStartsNoSecondScanWhileOneIsPending(t *testing.T) {
	// bufio.Scanner is not safe for concurrent use, and two scans would also
	// split the user's typing between them.
	g := newGatedReader()
	lr := newLineReader(g)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-g.entered; cancel() }()
	if _, _, err := lr.ReadLine(ctx); err == nil {
		t.Fatal("cancelled read returned a nil error")
	}

	// A second request while the first scan is still blocked must attach to it
	// rather than open another.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if line, ok, _ := lr.ReadLine(context.Background()); !ok || line != "one line" {
			t.Errorf("ReadLine = %q ok=%v, want the in-flight scan's result", line, ok)
		}
	}()
	requireNoScanInFlight(t, g) // no *new* Read while the old one is unfinished
	g.chunks <- []byte("one line\n")
	<-done

	if peak := g.concurrentPeak(); peak != 1 {
		t.Fatalf("peak concurrent Read entries = %d, want 1", peak)
	}
}

func TestLineReaderSequential(t *testing.T) {
	lr := newLineReader(strings.NewReader("one\ntwo\n"))
	ctx := context.Background()
	if l, ok, err := lr.ReadLine(ctx); !ok || err != nil || l != "one" {
		t.Fatalf("first: %q ok=%v err=%v", l, ok, err)
	}
	if l, ok, err := lr.ReadLine(ctx); !ok || err != nil || l != "two" {
		t.Fatalf("second: %q ok=%v err=%v", l, ok, err)
	}
	if _, ok, _ := lr.ReadLine(ctx); ok {
		t.Fatal("third read should report EOF (ok=false)")
	}
}

func TestLineReaderContextCancel(t *testing.T) {
	pr, pw := io.Pipe() // never written -> ReadLine blocks until ctx cancels
	defer func() { _ = pw.Close() }()
	lr := newLineReader(pr)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, ok, err := lr.ReadLine(ctx)
	if ok || err == nil {
		t.Fatalf("cancel should return ok=false and ctx err, got ok=%v err=%v", ok, err)
	}
}

func TestLineReaderSurfacesScannerError(t *testing.T) {
	huge := strings.Repeat("x", 2*1024*1024) // exceeds the 1 MiB scanner buffer
	lr := newLineReader(strings.NewReader(huge))
	_, ok, err := lr.ReadLine(context.Background())
	if ok {
		t.Fatal("oversized line should not yield ok=true")
	}
	if err == nil {
		t.Fatal("scanner error (token too long) must surface, not be swallowed as clean EOF")
	}
}
