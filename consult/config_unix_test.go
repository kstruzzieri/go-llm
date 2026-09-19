//go:build unix

package consult

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLoadDoesNotBlockOnAFIFO pins the startup-liveness property: a FIFO at the
// config path has no writer, so a blocking open would hang the whole program
// before it prints anything. The deadline is the assertion; an error is the
// only acceptable outcome.
func TestLoadDoesNotBlockOnAFIFO(t *testing.T) {
	path := filepath.Join(realTempDir(t), "consultants.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Load(path)
		done <- err
	}()
	select {
	case err := <-done:
		// Two independent properties, and a bare "some error" would conflate
		// them: the deadline proves the open did not block, and the message
		// proves the FIFO was refused for what it is rather than incidentally,
		// because a nonblocking read of a writer-less FIFO also yields EOF and
		// would fail the JSON decode on its own.
		if err == nil || !strings.Contains(err.Error(), "must be a regular file") {
			t.Fatalf("a FIFO config must be refused as non-regular, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Load blocked on a FIFO: startup would hang forever")
	}
}
