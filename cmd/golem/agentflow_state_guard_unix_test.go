//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A subprocess bounds regressions that block in open even after cancellation.
func TestRunAgentflowAuthor_RejectsFIFOStateWithoutBlocking(t *testing.T) {
	const childEnv = "GOLEM_AGENTFLOW_FIFO_CHILD"
	const rootEnv = "GOLEM_AGENTFLOW_FIFO_ROOT"
	if os.Getenv(childEnv) != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunAgentflowAuthor_RejectsFIFOStateWithoutBlocking$")
		cmd.Env = append(os.Environ(), childEnv+"=1", rootEnv+"="+t.TempDir())
		output, err := cmd.CombinedOutput()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal("retained-state FIFO blocked authoring despite context cancellation")
		}
		if err != nil {
			t.Fatalf("FIFO subprocess: %v\n%s", err, output)
		}
		return
	}

	root := os.Getenv(rootEnv)
	writeAgentFile(t, root, "plan.lock.json", v1Lock)
	if err := syscall.Mkfifo(filepath.Join(root, ".agent", "file-receipts.jsonl"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess := newTestSession(t, &scriptCaller{}, root)
	client := &stubLocker{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runAgentflowAuthorWithClient(ctx, &bytes.Buffer{}, &bytes.Buffer{}, nil, sess,
		flags{goal: "x", goalSet: true}, root, client, nil)
	if err == nil || !strings.Contains(err.Error(), ".agent/file-receipts.jsonl unreadable") {
		t.Fatalf("authoring error = %v, want FIFO state refusal", err)
	}
	if client.probes != 0 || client.inits != 0 {
		t.Fatalf("state refusal reached AgentFlow: probes=%d inits=%d", client.probes, client.inits)
	}
	// Refusal must also release the authoring lock for the next invocation.
	release, err := acquireAuthorLock(ctx, root)
	if err != nil {
		t.Fatalf("reacquire authoring lock: %v", err)
	}
	release()
}
