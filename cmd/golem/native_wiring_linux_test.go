package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A pathname Unix socket in the shared workspace is reachable from inside
// Bubblewrap: an honest, documented limit of IP-only network denial, not a
// denial this test claims. Linux only, so the darwin run has no skip.
func TestNativeWiringBwrapUnixSocketIsReachable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		if os.Getenv("GO_LLM_REQUIRE_BWRAP") == "1" {
			t.Fatalf("GO_LLM_REQUIRE_BWRAP=1 but python3, which dials the socket, is missing: %v", err)
		}
		t.Skipf("python3 dials the socket: %v", err)
	}
	config, root := golemWorkspace(t)
	policy := nativePolicyForTest(t, root)
	withGolem(t, config, root, policy, []string{"-allow-exec"}, func(sess *replSession) {
		sock := filepath.Join(sess.root, "s.sock")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		accepted := make(chan struct{}, 1)
		go func() {
			if conn, err := ln.Accept(); err == nil {
				_ = conn.Close()
				accepted <- struct{}{}
			}
		}()
		res := invokeTool(context.Background(), t, sess.tools, "run_command", map[string]any{"argv": []string{
			"python3", "-c", "import socket, sys; socket.socket(socket.AF_UNIX).connect(sys.argv[1])", sock}})
		select {
		case <-accepted:
		case <-time.After(5 * time.Second):
			t.Fatalf("the workspace socket was not reached; the documented limit changed:\n%s", res.Content)
		}
	})
}
