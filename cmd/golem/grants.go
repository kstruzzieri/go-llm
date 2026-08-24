package main

import "sync"

// Grant scopes (#341, D12). The grantScope allowlist below is the ONLY source
// of a scope: authorization requires BOTH a listed tool name AND a matching
// key, so a future tool that emits (or reuses) another tool's key cannot
// inherit its grant. #346's background exec consults grantScopeExec.
const (
	grantScopeExec  = "exec"
	grantScopeFiles = "files"
)

// grantScope maps a tool name to its grant scope. Everything not listed is
// ungrantable (""). start_command (#346) shares the exec scope safely: its
// exec-bg:v1: key prefix partitions the grant space from run_command's
// exec:v2:, so a foreground grant can never authorize a background start.
// stop_command stays OFF the allowlist by frozen contract (every stop
// prompts); command_status/command_tail never prompt, so they need no scope.
func grantScope(toolName string) string {
	switch toolName {
	case "run_command", "start_command":
		return grantScopeExec
	case "write_file", "edit_file":
		return grantScopeFiles
	}
	return ""
}

// grantID identifies one grant: the domain modeled directly as a comparable
// struct map key — no string encoding, no separator assumptions.
type grantID struct {
	scope, key string
}

// approvalGrants is the session-scoped approval-grant store (#341): the
// (scope, opaque structural key) pairs the user has approved for the rest of
// the active session. In-memory only, by contract: grants die with the
// session (cleared unconditionally on /new and /clear, on successful /resume,
// and via /grants clear) and with the process; they are never written to
// SQLite, history, or config.
//
// This store plus agent.KeyedApprover IS the grant contract #346 (background
// exec) must reuse: consult granted(scope, key) before prompting, add with
// grant(scope, key), and treat an empty scope or key as permanently
// ungrantable. The mutex exists for #346's concurrent consumers; today's
// approval path is serial (dispatch prepare phase), so contention is zero.
//
// Every method is nil-receiver safe (D9): a nil store means "grants disabled"
// — the mode the Agentflow author's plan-lock approver runs in — so callers
// never need nil guards. (The /auto-edits and /grants commands lazily
// initialize the store instead, so a toggle never reports state it did not
// save.)
//
// #346 pointer discipline: capture sess.grants ONCE when spawning a background
// consumer and hold that pointer. The slash-command lazy init reassigns
// sess.grants only when it is nil, which the main.go construction makes
// unreachable in production — but a captured pointer is immune either way.
type approvalGrants struct {
	mu   sync.Mutex
	keys map[grantID]struct{}
}

func newApprovalGrants() *approvalGrants {
	return &approvalGrants{keys: make(map[grantID]struct{})}
}

// granted reports whether (scope, key) holds a session grant. An empty scope
// or key is never granted.
func (g *approvalGrants) granted(scope, key string) bool {
	if g == nil || scope == "" || key == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.keys[grantID{scope, key}]
	return ok
}

// grant records a session grant for (scope, key). Empty scope or key is a
// no-op: structurally ungrantable.
func (g *approvalGrants) grant(scope, key string) {
	if g == nil || scope == "" || key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys[grantID{scope, key}] = struct{}{}
}

// revoke removes one grant (used by /auto-edits off).
func (g *approvalGrants) revoke(scope, key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.keys, grantID{scope, key})
}

// clear drops every grant (session switch/reset, /grants clear).
func (g *approvalGrants) clear() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.keys = make(map[grantID]struct{})
}

// count reports the number of active grants — /grants visibility only. Keys
// stay opaque: they are counted, never listed or parsed.
func (g *approvalGrants) count() int {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.keys)
}
