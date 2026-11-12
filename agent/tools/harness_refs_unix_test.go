//go:build unix

package tools

import (
	"testing"

	"golang.org/x/sys/unix"
)

// harnessMkfifo makes a FIFO at p for the reference-set rows (#784).
func harnessMkfifo(t *testing.T, p string) {
	t.Helper()
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
}
