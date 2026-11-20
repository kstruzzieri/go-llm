//go:build !unix

package tools

import "testing"

// harnessMkfifo skips: FIFOs are a Unix fixture (#784).
func harnessMkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("FIFO fixture: Unix only")
}
