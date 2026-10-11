//go:build unix

package mcpclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A child that exits during startup must be waited for, not left a zombie:
// kill(pid, 0) succeeds on a zombie and fails with ESRCH once reaped. This
// pins the SDK's own cleanup of that path (no go-llm code to mutate).
func TestConnectStartupExitReapsChild(t *testing.T) {
	s, dir := lifecycleServer(t, "exit")
	m, warns := connectLifecycle(t, context.Background(), s)
	wantAdmission(t, m, warns)
	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper pid %d not reaped (kill 0 = %v)", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
