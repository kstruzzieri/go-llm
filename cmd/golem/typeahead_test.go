package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// TestDiscardIfConsentNotShown is run's exit step: queued terminal input is
// discarded for an errConsentNotShown error on a terminal stdin, and for
// nothing else. A discard that fails is joined into the error, which main
// still prints as one line.
func TestDiscardIfConsentNotShown(t *testing.T) {
	stdin, _, _ := runTestFiles(t)
	unshown := questionNotShown(errors.New("write /dev/stdout: no space left on device"))
	discardErr := errors.New("inappropriate ioctl for device")
	for _, tc := range []struct {
		name       string
		err        error
		stdin      *os.File
		terminal   bool
		discardErr error
		want       bool // discard runs
	}{
		{name: "not shown, terminal", err: unshown, stdin: stdin, terminal: true, want: true},
		{name: "not shown joined with a cancellation", err: errors.Join(unshown, context.Canceled), stdin: stdin, terminal: true, want: true},
		{name: "not shown, stdin not a terminal", err: unshown, stdin: stdin},
		{name: "not shown, no stdin", err: unshown, terminal: true},
		{name: "other error, terminal", err: errors.New("model unreachable"), stdin: stdin, terminal: true},
		{name: "cancelled, terminal", err: context.Canceled, stdin: stdin, terminal: true},
		{name: "no error, terminal", stdin: stdin, terminal: true},
		{name: "discard fails", err: unshown, stdin: stdin, terminal: true, discardErr: discardErr, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var discarded []*os.File
			isTerminal := func(fd int) bool {
				if fd != int(stdin.Fd()) {
					t.Errorf("terminal check on fd %d, want stdin's %d", fd, stdin.Fd())
				}
				return tc.terminal
			}
			got := discardIfConsentNotShown(tc.err, tc.stdin, isTerminal, func(f *os.File) error {
				discarded = append(discarded, f)
				return tc.discardErr
			})
			if tc.want != (len(discarded) == 1) || len(discarded) > 1 || (len(discarded) == 1 && discarded[0] != stdin) {
				t.Fatalf("discarded %v, want stdin discarded: %v", discarded, tc.want)
			}
			if tc.discardErr == nil {
				if got != tc.err {
					t.Fatalf("error = %v, want %v unchanged", got, tc.err)
				}
				return
			}
			if !errors.Is(got, errConsentNotShown) || !errors.Is(got, tc.discardErr) {
				t.Fatalf("error = %v, want the refusal joined with the discard failure", got)
			}
			line := cliFailureLine(got)
			if strings.Contains(line, "\n") || !strings.Contains(line, "typed-ahead input could not be discarded and may reach the shell") {
				t.Errorf("failure line = %q, want one line saying the input may reach the shell", line)
			}
		})
	}
}
