//go:build unix

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestGolemModels_InterruptDuringCheck pins that SIGINT during a check stops
// golem models: no second request, one cancelled line, the context error
// returned. The test registers for SIGINT itself, so a golem models that did
// not would leave the process running and fail on the second request instead.
// The run has its own deadline, so a broken interrupt path cannot hang it.
func TestGolemModels_InterruptDuringCheck(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	srv, e := cancelFixture(t, func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Errorf("signal: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stdout, stderr := &mcWriter{srv: srv}, &mcWriter{srv: srv}
	err := e.run(ctx, stdout, stderr, "-probe-all")
	wantCancelled(t, srv, err, stderr.String())
}
