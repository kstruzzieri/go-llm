//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// dispatchScope runs one scoped task over ws and returns the result with the
// number of child model calls made.
func dispatchScope(t *testing.T, ws *Workspace, scope string) (agent.ToolResult, int) {
	t.Helper()
	caller := &dispatchCaller{}
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(ws), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"tasks": []any{map[string]string{"task": "inspect", "scope": scope}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	return out, len(caller.requests())
}

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDispatchRefusesProtectedScopes (#627 T10): a child rooted at or below a
// protected directory would judge its paths without that ancestor, so the
// scope is refused before any child runs. The control scope proves the
// fixture dispatches at all.
func TestDispatchRefusesProtectedScopes(t *testing.T) {
	root := t.TempDir()
	scopes := []string{".ssh", "a/.aws", ".git", "x/.git/modules", ".gnupg", ".kube"}
	mkdirs(t, root, append([]string{"home"}, scopes...)...)
	ws := mustWorkspace(t, root)
	for _, scope := range scopes {
		out, calls := dispatchScope(t, ws, scope)
		if !out.IsError || out.Content != "path denied by workspace policy" || calls != 0 {
			t.Errorf("scope %q = %+v with %d child calls, want the policy denial and no child", scope, out, calls)
		}
	}
	if out, calls := dispatchScope(t, ws, "home"); out.IsError || calls != 1 {
		t.Fatalf("control scope = %+v with %d child calls, want one child run", out, calls)
	}
}

// TestCredentialAliasScopesRefused (#627 T10, spec F13): alias spellings of a
// protected directory are refused on every platform. On APFS, .SSH and
// .\u017Fsh name .ssh itself; elsewhere each is its own directory whose
// stored spelling normalizes to .ssh. The darwin-smoke step runs this
// natively on APFS.
func TestCredentialAliasScopesRefused(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, ".ssh")
	ws := mustWorkspace(t, root)
	for _, scope := range []string{".SSH", ".\u017Fsh", ".s\u200Csh"} {
		if err := os.Mkdir(filepath.Join(root, scope), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
		out, calls := dispatchScope(t, ws, scope)
		if !out.IsError || out.Content != "path denied by workspace policy" || calls != 0 {
			t.Errorf("scope %q = %+v with %d child calls, want the policy denial and no child", scope, out, calls)
		}
	}
}

// TestCredentialAliasScopeUsesStoredSpelling (#627, spec D10, kills M8): the
// scope check judges the stored spelling, not the requested one. On a
// normalization-insensitive volume (APFS), the NFD request ".gI\u0307t"
// (I plus combining dot above) opens a directory stored as ".g\u0130t"
// (precomposed dotted capital I). Go lower-cases the stored U+0130 to i, so
// the stored spelling matches the protected .git: the dotted-I over-block
// that spec §5.1 accepts. The requested spelling keeps U+0307 and would
// pass. Filesystems that keep the two spellings apart skip.
func TestCredentialAliasScopeUsesStoredSpelling(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, ".g\u0130t")
	stored, err := os.Stat(filepath.Join(root, ".g\u0130t"))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := os.Stat(filepath.Join(root, ".gI\u0307t"))
	if err != nil || !os.SameFile(stored, requested) {
		t.Skip("this filesystem keeps the precomposed and decomposed dotted-I spellings apart")
	}
	out, calls := dispatchScope(t, mustWorkspace(t, root), ".gI\u0307t")
	if !out.IsError || out.Content != "path denied by workspace policy" || calls != 0 {
		t.Fatalf("scope = %+v with %d child calls, want the policy denial and no child", out, calls)
	}
}

// TestDispatchAllowsCredentialNamedScope (#627 T10, spec §5.4): .env is a
// basename rule, so a virtualenv directory named .env is a valid scope and
// the child reads inside it. The degenerate requests the §5.4 property does
// not cover are pinned here: "." is the wrong type, an escape is a policy
// denial, and a non-escaping ".." reads.
func TestDispatchAllowsCredentialNamedScope(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{".env/bin/activate": "VENV-627 activate\n"})
	child := &credentialChild{calls: []provider.ToolCall{
		childCall("r1", "read_file", `{"path":"bin/activate"}`),
		childCall("r2", "read_file", `{"path":"."}`),
		childCall("r3", "read_file", `{"path":"../x"}`),
		childCall("r4", "read_file", `{"path":"bin/../bin/activate"}`),
	}}
	d, err := NewDispatch(child, agent.ContextManager{}, NewFileToolsForWorkspace(mustWorkspace(t, root)), DispatchLimits{}, defaultInvariants(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":[{"task":"inspect","scope":".env"}]}`))
	if err != nil || out.IsError {
		t.Fatalf("dispatch = %+v, %v", out, err)
	}
	obs := child.observations()
	for id, want := range map[string]string{
		"r1": "VENV-627 activate",
		"r2": "path has the wrong type",
		"r3": "path denied by workspace policy",
		"r4": "VENV-627 activate",
	} {
		if !strings.Contains(obs[id], want) {
			t.Errorf("observation %s = %q, want it to contain %q", id, obs[id], want)
		}
	}
}

// TestDispatchScopedChildSkipsNestedCredentials (#627 T10): inside an allowed
// scope, the child's search skips nested credential files and the default
// invariants block its read of one. The allowed line proves the search ran.
func TestDispatchScopedChildSkipsNestedCredentials(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"home/notes.txt":   "SCOPED-627 allowed\n",
		"home/.env":        "SCOPED-627 env-secret\n",
		"home/.ssh/id_rsa": "SCOPED-627 key-secret\n",
	})
	child := &credentialChild{calls: []provider.ToolCall{
		childCall("s1", "search", `{"pattern":"SCOPED-627"}`),
		childCall("r1", "read_file", `{"path":".ssh/id_rsa"}`),
	}}
	d, err := NewDispatch(child, agent.ContextManager{}, NewFileToolsForWorkspace(mustWorkspace(t, root)), DispatchLimits{}, defaultInvariants(t))
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":[{"task":"inspect","scope":"home"}]}`))
	if err != nil || out.IsError {
		t.Fatalf("dispatch = %+v, %v", out, err)
	}
	obs := child.observations()
	if !strings.Contains(obs["s1"], "notes.txt:1: SCOPED-627 allowed") {
		t.Fatalf("child search = %q, want the allowed line", obs["s1"])
	}
	if !strings.Contains(obs["r1"], "tool call blocked by interceptor invariants (credential_path)") {
		t.Fatalf("child key read = %q, want the credential_path block", obs["r1"])
	}
	for id, o := range obs {
		if strings.Contains(o, "-secret") {
			t.Fatalf("observation %s carried credential content: %q", id, o)
		}
	}
}
