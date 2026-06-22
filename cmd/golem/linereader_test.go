package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

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
