//go:build linux

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// The other #433 tests set renderer.terminal by hand or wrap a buffer, and the
// /dev/tty probe skips wherever there is no controlling terminal (Docker,
// GitHub CI). This drives the production -p path with a real PTY as both
// stdout and stderr, so *os.File terminal detection is proven where CI runs:
// the stream on stderr and the answer reprinted on stdout must both arrive
// quoted.
func TestOneShotRealPTYQuotesModelControls(t *testing.T) {
	p := openPTY(t)
	seen := drainMaster(p)
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "pty\x1b]52;c;YQ==\adone"}}}}
	sess := newTestSession(t, caller, t.TempDir())
	if err := runOneShot(context.Background(), p.slave, p.slave, nil, sess, "go"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	const quoted = `pty\x1b]52;c;YQ==\adone`
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(seen.String(), quoted) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := seen.String()
	if n := strings.Count(got, quoted); n != 2 {
		t.Fatalf("quoted answer seen %d times, want 2 (stderr stream and stdout answer):\n%q", n, got)
	}
	if strings.Contains(got, "\x1b]52") {
		t.Fatalf("raw OSC 52 reached the terminal:\n%q", got)
	}
}
