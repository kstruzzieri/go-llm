package main

import (
	"sync"
	"testing"
)

func TestApprovalGrantsLifecycle(t *testing.T) {
	g := newApprovalGrants()
	if g.granted(grantScopeExec, "exec:abc") {
		t.Fatal("fresh store must grant nothing")
	}
	g.grant(grantScopeExec, "exec:abc")
	if !g.granted(grantScopeExec, "exec:abc") {
		t.Fatal("granted (scope, key) must report granted")
	}
	if g.granted(grantScopeExec, "exec:abd") {
		t.Fatal("a one-character key difference must not match")
	}
	if g.granted(grantScopeFiles, "exec:abc") {
		t.Fatal("the same key under a different scope must not match (D12)")
	}
	g.revoke(grantScopeExec, "exec:abc")
	if g.granted(grantScopeExec, "exec:abc") {
		t.Fatal("revoked key must not report granted")
	}
	g.grant(grantScopeExec, "a")
	g.grant(grantScopeFiles, "b")
	if g.count() != 2 {
		t.Fatalf("count = %d, want 2", g.count())
	}
	g.clear()
	if g.granted(grantScopeExec, "a") || g.granted(grantScopeFiles, "b") || g.count() != 0 {
		t.Fatal("clear must drop every grant")
	}
}

func TestApprovalGrantsEmptyScopeOrKeyNeverGrantable(t *testing.T) {
	g := newApprovalGrants()
	g.grant("", "exec:abc")
	g.grant(grantScopeExec, "")
	if g.granted("", "exec:abc") || g.granted(grantScopeExec, "") || g.count() != 0 {
		t.Fatal("empty scope or key must never become granted")
	}
}

func TestApprovalGrantsNilReceiverSafe(t *testing.T) {
	// nil store = grants disabled (the Agentflow author's approver). Every
	// method must be a safe no-op, not a panic (D9).
	var g *approvalGrants
	g.grant(grantScopeExec, "exec:abc")
	if g.granted(grantScopeExec, "exec:abc") {
		t.Fatal("nil store must grant nothing")
	}
	g.revoke(grantScopeExec, "exec:abc")
	g.clear()
	if g.count() != 0 {
		t.Fatalf("nil store count = %d, want 0", g.count())
	}
}

func TestGrantScopeAllowlist(t *testing.T) {
	// The allowlist IS the authorization boundary (D12): pin it exactly.
	cases := map[string]string{
		"run_command":   grantScopeExec,
		"write_file":    grantScopeFiles,
		"edit_file":     grantScopeFiles,
		"mcp__fs__read": "",
		"submit_plan":   "",
		"dispatch":      "",
		"evil_tool":     "",
		"":              "",
	}
	for name, want := range cases {
		if got := grantScope(name); got != want {
			t.Errorf("grantScope(%q) = %q, want %q", name, got, want)
		}
	}
}

// The store is the #346 background-exec contract: concurrent consult/update
// must be race-free (run under -race).
func TestApprovalGrantsConcurrentAccess(t *testing.T) {
	g := newApprovalGrants()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				g.grant(grantScopeExec, "k")
				_ = g.granted(grantScopeExec, "k")
				_ = g.count()
				g.revoke(grantScopeExec, "k")
				g.clear()
			}
		}()
	}
	wg.Wait()
}
