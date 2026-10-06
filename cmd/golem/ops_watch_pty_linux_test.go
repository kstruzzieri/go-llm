//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

func TestOpsWatchRealPTYRestoresScreen(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	p := openPTY(t)
	seen := drainMaster(p)
	f := opsfixture.NewLlamaSwap(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := runOps(ctx, []string{"-watch", "-config", fixtureConfig(t, f.URL())}, nil, p.slave, p.slave); err != nil {
		t.Fatalf("runOps -watch: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(seen.String(), "\x1b[?1049l") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := seen.String()
	if !strings.Contains(got, "\x1b[?1049h") || !strings.Contains(got, "\x1b[?1049l") || !strings.Contains(got, "gemma4:31b") {
		t.Fatalf("PTY output missing enter/frame/restore:\n%q", got)
	}
}

// TestOpsWatchRealPTYRestoresOnSignal signals the test process while -watch
// draws on a real PTY. The test holds its own registration for the signal,
// so one runOps fails to catch is harmless and trips the deadline below
// instead of killing the test binary.
func TestOpsWatchRealPTYRestoresOnSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Setenv("TERM", "xterm-256color")
			held := make(chan os.Signal, 1)
			signal.Notify(held, sig)
			defer signal.Stop(held)
			p := openPTY(t)
			seen := drainMaster(p)
			f := opsfixture.NewLlamaSwap(t)
			cfg := fixtureConfig(t, f.URL())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- runOps(ctx, []string{"-watch", "-config", cfg}, nil, p.slave, p.slave) }()
			// A drawn frame means runOps registered its handler first.
			waitSeen(seen, func(s string) bool { return strings.Contains(s, "gemma4:31b") })
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("runOps -watch after %v: %v", sig, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%v did not end -watch", sig)
			}
			waitSeen(seen, func(s string) bool { return strings.HasSuffix(s, altScreenOff) })
			if got := seen.String(); !strings.Contains(got, "gemma4:31b") || !strings.HasSuffix(got, altScreenOff) {
				t.Fatalf("after %v the PTY did not end restored (cursor shown, main screen):\n%q", sig, got)
			}
		})
	}
}

// waitSeen polls the PTY output for up to two seconds.
func waitSeen(seen *lockedBuffer, ok func(string) bool) {
	deadline := time.Now().Add(2 * time.Second)
	for !ok(seen.String()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}
