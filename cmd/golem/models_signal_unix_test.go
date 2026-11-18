//go:build unix

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestGolemModels_InterruptDuringCheck pins that SIGINT during a check stops
// golem models: no second request, one cancelled line, the context error
// returned. The test registers for SIGINT itself, so a golem models that did
// not would leave the process running and fail on the second request instead.
func TestGolemModels_InterruptDuringCheck(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	srv, e := cancelFixture(t, func() {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Errorf("signal: %v", err)
		}
	})
	stdout, stderr := &mcWriter{srv: srv}, &mcWriter{srv: srv}
	err := e.run(context.Background(), stdout, stderr, "-probe-all")
	wantCancelled(t, srv, err, stderr.String())
}
