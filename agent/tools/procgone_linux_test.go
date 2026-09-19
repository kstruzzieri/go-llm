//go:build linux

package tools

import (
	"os/exec"
	"testing"
	"time"
)

// TestProcessGoneCountsZombie proves the /proc branch: a killed child that has
// not been reaped is a zombie, kill(pid, 0) still succeeds on it, and
// processGone must report it gone anyway. Darwin has no procfs and is not
// covered; launchd reaps the orphans the reaping tests leave there.
func TestProcessGoneCountsZombie(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if processGone(pid) {
		t.Fatalf("live child %d reported gone", pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Not reaped: no Wait yet, so the child stays a zombie of this process.
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("zombie child %d still reported alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
