//go:build linux || darwin

package main

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// typeAhead writes s to the master as if typed and waits for the terminal's
// echo of want, which the line discipline sends only once it has queued the
// bytes: without the barrier a discard could run before they arrive.
func typeAhead(t *testing.T, p *ptyPair, seen *lockedBuffer, s, want string) {
	t.Helper()
	if _, err := p.master.WriteString(s); err != nil {
		t.Fatalf("write %q to the pty master: %v", s, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(seen.String(), want) {
		if time.Now().After(deadline) {
			p.close()
			t.Fatalf("no echo of %q; master saw %q", want, seen.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// checkQueueEmpty proves the slave's input queue is empty: it types a marker
// line and reads one line, which any leftover would prefix. FIONREAD alone
// would not do, since in canonical mode Linux counts only completed lines.
// The slave must be cooked, so a read returns at most one line.
func checkQueueEmpty(t *testing.T, p *ptyPair) {
	t.Helper()
	const marker = "queue-empty-marker\n"
	if _, err := p.master.WriteString(marker); err != nil {
		t.Fatalf("write the marker: %v", err)
	}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := p.slave.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case line := <-got:
		if line != marker {
			t.Fatalf("line read after the discard = %q, want only the marker %q", line, marker)
		}
	case <-time.After(10 * time.Second):
		p.close()
		t.Fatal("the marker line never reached the terminal")
	}
}

// TestDiscardTypedAheadEmptiesTheQueue queues benign text on a real terminal,
// a complete line and the unterminated tail of a split paste opener, and
// proves the discard leaves nothing for the shell.
func TestDiscardTypedAheadEmptiesTheQueue(t *testing.T) {
	// echo is the cooked terminal's echo of the queued text's last bytes
	// (ONLCR turns "\n" into "\r\n", ECHOCTL shows ESC as "^[").
	for _, tc := range []struct{ queued, echo string }{
		{"echo benign\n", "benign\r\n"},
		{"0~echo benign\x1b[201~", "^[[201~"},
	} {
		t.Run(strconv.Quote(tc.queued), func(t *testing.T) {
			p := openPTY(t)
			seen := drainMaster(p)
			typeAhead(t, p, seen, tc.queued, tc.echo)
			if err := discardTypedAhead(p.slave); err != nil {
				t.Fatalf("discardTypedAhead: %v", err)
			}
			checkQueueEmpty(t, p)
		})
	}
}

// TestSplitPasteOpenerTailDiscarded is the split-opener exit on a real
// terminal. The editor's key filter consumed "\x1b[20", the start of a paste
// opener, then the answer window refused to read on because the echo of "a"
// failed, so no paste began and nothing drained it. The rest of the paste,
// typed afterwards, waits in the terminal until the discard removes it.
func TestSplitPasteOpenerTailDiscarded(t *testing.T) {
	p := openPTY(t)
	seen := drainMaster(p)
	// Typed while the terminal is still cooked, so the raw window's first read
	// takes all four bytes at once. The echo shows ESC as ^[.
	typeAhead(t, p, seen, "a\x1b[20", "a^[[20")

	src := newInput(inputConfig{
		Stdin: p.slave, Stdout: p.slave, Stderr: io.Discard,
		Out:    &promptThenFail{prompt: "allow? "},
		Getenv: func(string) string { return "" },
	})
	e, ok := src.(*editorSource)
	if !ok {
		t.Fatalf("newInput selected %T over a real PTY, want the editor", src)
	}
	if _, _, err := e.ReadAnswer(context.Background(), "allow? "); !errors.Is(err, errConsentNotShown) {
		t.Fatalf("ReadAnswer = %v, want errConsentNotShown", err)
	}
	if f := e.filter; !f.stalled || f.inPaste || string(f.work) != "\x1b[20" {
		t.Fatalf("filter stalled=%v inPaste=%v holding %q; want the opener's first bytes held outside a paste", f.stalled, f.inPaste, f.work)
	}

	// The window restored the cooked terminal: ICRNL ends the line.
	typeAhead(t, p, seen, "0~echo benign\x1b[201~\r", "^[[201~\r\n")
	if err := src.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := discardTypedAhead(p.slave); err != nil {
		t.Fatalf("discardTypedAhead: %v", err)
	}
	checkQueueEmpty(t, p)
}
