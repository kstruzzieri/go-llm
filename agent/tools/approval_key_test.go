package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// planKeyRC builds a RunCommand rooted in a temp workspace and returns the
// root (the Workspace exposes no root accessor). nil runner: Plan never spawns.
func planKeyRC(t *testing.T) (*RunCommand, string) {
	t.Helper()
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return NewRunCommand(ws, nil), root
}

func execPlanKey(t *testing.T, rc *RunCommand, raw string) string {
	t.Helper()
	p, err := rc.Plan(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("Plan(%s): %v", raw, err)
	}
	return p.ApprovalKey
}

func TestExecApprovalKeyStableAndNamespaced(t *testing.T) {
	rc, _ := planKeyRC(t)
	const raw = `{"argv":["go","version"]}`
	k1 := execPlanKey(t, rc, raw)
	k2 := execPlanKey(t, rc, raw)
	if k1 == "" || k1 != k2 {
		t.Fatalf("identical plans must share one non-empty key: %q vs %q", k1, k2)
	}
	// White-box namespace + recipe pin: an exec key must never be
	// constructible as a class key, and the recipe tag makes any future
	// fingerprint change an explicit migration.
	if !strings.HasPrefix(k1, "exec:v3:") {
		t.Fatalf("exec key %q must be namespaced with \"exec:v3:\"", k1)
	}
}

// TestExecApprovalKeyVariesByWorkspaceRoot pins the #442 v3 recipe: the
// canonical workspace root bounds a sandbox backend's write allowance, so two
// nested workspaces resolving the SAME cwd with the SAME argv/env must never
// share a foreground grant.
func TestExecApprovalKeyVariesByWorkspaceRoot(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	wsOuter, err := NewWorkspace(outer)
	if err != nil {
		t.Fatal(err)
	}
	wsInner, err := NewWorkspace(inner)
	if err != nil {
		t.Fatal(err)
	}
	outerKey := execPlanKey(t, NewRunCommand(wsOuter, nil), `{"argv":["go","version"],"dir":"inner"}`)
	innerKey := execPlanKey(t, NewRunCommand(wsInner, nil), `{"argv":["go","version"]}`)
	if outerKey == innerKey {
		t.Fatalf("nested workspaces with one cwd must not share a foreground key (%q)", outerKey)
	}
}

// TestBackgroundApprovalKeyVariesByWorkspaceRoot is the background twin of the
// nested-root pin, plus the exec-bg:v2: recipe-tag pin.
func TestBackgroundApprovalKeyVariesByWorkspaceRoot(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewBackgroundManager()
	t.Cleanup(m.Shutdown)
	wsOuter, err := NewWorkspace(outer)
	if err != nil {
		t.Fatal(err)
	}
	wsInner, err := NewWorkspace(inner)
	if err != nil {
		t.Fatal(err)
	}
	outerPlan, err := NewStartCommand(wsOuter, m).Plan(context.Background(),
		json.RawMessage(`{"argv":["go","version"],"dir":"inner"}`))
	if err != nil {
		t.Fatal(err)
	}
	innerPlan, err := NewStartCommand(wsInner, m).Plan(context.Background(),
		json.RawMessage(`{"argv":["go","version"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(outerPlan.ApprovalKey, "exec-bg:v2:") {
		t.Fatalf("background key %q must be namespaced with \"exec-bg:v2:\"", outerPlan.ApprovalKey)
	}
	if outerPlan.ApprovalKey == innerPlan.ApprovalKey {
		t.Fatalf("nested workspaces with one cwd must not share a background key (%q)", outerPlan.ApprovalKey)
	}
}

func TestExecApprovalKeyVariesByEachInput(t *testing.T) {
	t.Setenv("LANG", "C") // pin env shape so the baseline is deterministic
	rc, root := planKeyRC(t)
	base := execPlanKey(t, rc, `{"argv":["go","version"]}`)
	variants := map[string]string{
		"argv":    `{"argv":["go","env"]}`,
		"timeout": `{"argv":["go","version"],"timeout_seconds":30}`,
	}
	for name, raw := range variants {
		if got := execPlanKey(t, rc, raw); got == base {
			t.Errorf("%s change did not change the key (%q)", name, got)
		}
	}
	// cwd: same argv from a subdirectory.
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := execPlanKey(t, rc, `{"argv":["go","version"],"dir":"sub"}`); got == base {
		t.Errorf("cwd change did not change the key (%q)", got)
	}
	// env VALUE: same shape (LANG still present), different value (D13/REV 4:
	// values materially change behavior; "exact command" hashes them).
	t.Setenv("LANG", "en_US.UTF-8")
	if got := execPlanKey(t, rc, `{"argv":["go","version"]}`); got == base {
		t.Errorf("env-value change did not change the key (%q)", got)
	}
	// env shape: drop LANG from the allowlisted parent env entirely.
	if err := os.Unsetenv("LANG"); err != nil {
		t.Fatal(err)
	}
	if got := execPlanKey(t, rc, `{"argv":["go","version"]}`); got == base {
		t.Errorf("env-shape change did not change the key (%q)", got)
	}
}

func TestExecApprovalKeyVariesByRequestedTimeout(t *testing.T) {
	rc, _ := planKeyRC(t)
	omitted := execPlanKey(t, rc, `{"argv":["go","version"]}`)
	explicitDefault := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":60}`)
	if omitted == explicitDefault {
		t.Fatalf("omitted and explicit default timeouts must not share a key (%q)", omitted)
	}
	key601 := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":601}`)
	key999 := execPlanKey(t, rc, `{"argv":["go","version"],"timeout_seconds":999}`)
	if key601 == key999 {
		t.Fatalf("different requested timeouts must not share a key (%q)", key601)
	}
}

func TestExecApprovalKeyVariesWithResolvedExecutable(t *testing.T) {
	// Same argv and environment, but a new binary shadows the old executable in
	// an earlier PATH directory. The resolved path must independently change the
	// key or an old grant silently authorizes a different executable.
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH-resolution fixture")
	}
	rc, _ := planKeyRC(t)
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dirA+string(os.PathListSeparator)+dirB)
	keyB := execPlanKey(t, rc, `{"argv":["mytool"]}`)
	if err := os.WriteFile(filepath.Join(dirA, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	keyA := execPlanKey(t, rc, `{"argv":["mytool"]}`)
	if keyA == keyB {
		t.Fatalf("a different resolved executable must change the key (%q)", keyA)
	}
}

func TestWriteAndEditShareTheWriteClassKey(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	wp, err := NewWriteFile(ws, nil).Plan(context.Background(),
		json.RawMessage(`{"path":"f.txt","content":"new\n"}`))
	if err != nil {
		t.Fatalf("write Plan: %v", err)
	}
	ep, err := NewEditFile(ws, nil).Plan(context.Background(),
		json.RawMessage(`{"path":"f.txt","old_string":"old\n","new_string":"new\n"}`))
	if err != nil {
		t.Fatalf("edit Plan: %v", err)
	}
	if wp.ApprovalKey != WriteClassApprovalKey || ep.ApprovalKey != WriteClassApprovalKey {
		t.Fatalf("write=%q edit=%q, want both == WriteClassApprovalKey (%q)",
			wp.ApprovalKey, ep.ApprovalKey, WriteClassApprovalKey)
	}
}

// harnessFixture creates rel under root holding "old\n", parents included.
func harnessFixture(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func harnessWorkspace(t *testing.T, root string) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// harnessWritePlan plans write_file of "new\n" to rel.
func harnessWritePlan(t *testing.T, ws *Workspace, rel string) agent.ToolPlan {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"path": rel, "content": "new\n"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewWriteFile(ws, nil).Plan(context.Background(), raw)
	if err != nil {
		t.Fatalf("write_file Plan(%s): %v", rel, err)
	}
	return p
}

// harnessEditPlan plans edit_file of rel's "old\n" to "new\n".
func harnessEditPlan(t *testing.T, ws *Workspace, rel string) agent.ToolPlan {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"path": rel, "old_string": "old\n", "new_string": "new\n"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewEditFile(ws, nil).Plan(context.Background(), raw)
	if err != nil {
		t.Fatalf("edit_file Plan(%s): %v", rel, err)
	}
	return p
}

// TestHarnessPlanEmptyKey pins #725 through the real Plan: a harness target
// plans with the ungrantable empty key, an ordinary one keeps the write-class
// key, and only the key differs (Effect and Preview match up to the path).
func TestHarnessPlanEmptyKey(t *testing.T) {
	root := t.TempDir()
	harness := []string{"CLAUDE.md", ".claude/settings.json", ".github/hooks/x.json", "Models.json"}
	for _, rel := range append([]string{"notes.txt"}, harness...) {
		harnessFixture(t, root, rel)
	}
	ws := harnessWorkspace(t, root)
	ordinary := map[string]agent.ToolPlan{
		"write_file": harnessWritePlan(t, ws, "notes.txt"),
		"edit_file":  harnessEditPlan(t, ws, "notes.txt"),
	}
	for tool, p := range ordinary {
		if p.ApprovalKey != WriteClassApprovalKey {
			t.Fatalf("%s notes.txt key = %q, want %q", tool, p.ApprovalKey, WriteClassApprovalKey)
		}
	}
	for _, rel := range harness {
		got := map[string]agent.ToolPlan{
			"write_file": harnessWritePlan(t, ws, rel),
			"edit_file":  harnessEditPlan(t, ws, rel),
		}
		for tool, p := range got {
			if p.ApprovalKey != "" {
				t.Errorf("%s %s key = %q, want empty", tool, rel, p.ApprovalKey)
			}
			if !reflect.DeepEqual(p.Effect, ordinary[tool].Effect) {
				t.Errorf("%s %s Effect = %+v, want %+v", tool, rel, p.Effect, ordinary[tool].Effect)
			}
			if want := strings.ReplaceAll(ordinary[tool].Preview, "notes.txt", rel); p.Preview != want {
				t.Errorf("%s %s Preview = %q, want %q", tool, rel, p.Preview, want)
			}
		}
	}
}

// TestHarnessPlanResolvedSpelling drives the key through the resolver seam:
// the spelling the filesystem resolves is classified too, and an unverified
// or failed resolution prompts.
func TestHarnessPlanResolvedSpelling(t *testing.T) {
	root := t.TempDir()
	harnessFixture(t, root, "odd/x.json")
	harnessFixture(t, root, "CLAUDE.md")
	ws := harnessWorkspace(t, root)
	cases := []struct {
		name     string
		req, rel string
		verified bool
		err      error
		want     string
	}{
		{"resolved harness spelling", "odd/x.json", ".claude/settings.json", true, nil, ""},
		{"unverified spelling", "odd/x.json", "notes.txt", false, nil, ""},
		{"verified ordinary spelling", "odd/x.json", "notes.txt", true, nil, WriteClassApprovalKey},
		{"resolver error", "odd/x.json", "notes.txt", true, errors.New("resolve failed"), ""},
		// Either spelling decides: an ordinary resolution does not clear a
		// harness request.
		{"requested harness spelling", "CLAUDE.md", "notes.txt", true, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var asked []string
			ws.resolveForKey = func(p string) (string, bool, error) {
				asked = append(asked, p)
				return c.rel, c.verified, c.err
			}
			w := harnessWritePlan(t, ws, c.req)
			e := harnessEditPlan(t, ws, c.req)
			if w.ApprovalKey != c.want || e.ApprovalKey != c.want {
				t.Fatalf("write=%q edit=%q, want both %q", w.ApprovalKey, e.ApprovalKey, c.want)
			}
			if c.want != "" && !reflect.DeepEqual(asked, []string{c.req, c.req}) {
				t.Fatalf("resolver asked %q, want the requested path once per Plan", asked)
			}
		})
	}
}

// TestHarnessPlanResolverStoredSpelling: on a case-insensitive filesystem the
// real resolver reports the stored spelling of an existing harness file.
func TestHarnessPlanResolverStoredSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Probe"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "probe")); err != nil {
		t.Skip("temp dir is case-sensitive")
	}
	if err := os.Remove(filepath.Join(root, "Probe")); err != nil {
		t.Fatal(err)
	}
	harnessFixture(t, root, "CLAUDE.md")
	ws := harnessWorkspace(t, root)
	rel, verified, err := ws.resolvedWriteRel("claude.md")
	if err != nil || !verified || rel != "CLAUDE.md" {
		t.Fatalf("resolvedWriteRel(claude.md) = (%q, %v, %v), want (CLAUDE.md, true, nil)", rel, verified, err)
	}
	// The requested spelling alone already catches claude.md (IsHarnessPath lower-cases); the resolver's
	// part is proven by the assertion above and, through Plan, only by the Linux unverified rows.
	if k := harnessWritePlan(t, ws, "claude.md").ApprovalKey; k != "" {
		t.Fatalf("write_file claude.md key = %q, want empty", k)
	}
}

// TestHarnessPlanUnverifiedSpelling: a component whose stored spelling cannot
// be listed is unverified, for an existing leaf and for an absent leaf below
// it, and an unverified spelling prompts.
func TestHarnessPlanUnverifiedSpelling(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux descriptor resolver fixture")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permission")
	}
	root := t.TempDir()
	harnessFixture(t, root, "d/x.txt")
	if err := os.Mkdir(filepath.Join(root, "d", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := harnessWorkspace(t, root)
	if _, verified, err := ws.resolvedWriteRel("d/sub/new.txt"); err != nil || !verified {
		t.Fatalf("control: listable resolvedWriteRel = (%v, %v), want verified", verified, err)
	}
	d := filepath.Join(root, "d")
	if err := os.Chmod(d, 0o311); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	for _, rel := range []string{"d/x.txt", "d/sub/new.txt"} {
		if _, verified, err := ws.resolvedWriteRel(rel); err != nil || verified {
			t.Fatalf("resolvedWriteRel(%s) = (verified %v, %v), want unverified", rel, verified, err)
		}
	}
	if k := harnessWritePlan(t, ws, "d/sub/new.txt").ApprovalKey; k != "" {
		t.Fatalf("write_file d/sub/new.txt key = %q, want empty", k)
	}
}

// TestHarnessPlanUnverifiedRoot: a root component at or below the repository
// boundary whose spelling cannot be listed is part of the classified prefix,
// so every write prompts; an unlistable folder above the repository is not.
func TestHarnessPlanUnverifiedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission fixture")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permission")
	}
	tmp := t.TempDir()
	for _, d := range []string{"repo/.git", "repo/a/b", "edge/repo/.git", "outer/mid/repo/.git"} {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"repo/a", "edge", "outer"} {
		p := filepath.Join(tmp, filepath.FromSlash(d))
		if err := os.Chmod(p, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
	ws := harnessWorkspace(t, filepath.Join(tmp, "repo", "a", "b"))
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != "" {
		t.Fatalf("unverified root below the boundary: notes.txt key = %q, want empty", k)
	}
	ws = harnessWorkspace(t, filepath.Join(tmp, "edge", "repo"))
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != "" {
		t.Fatalf("unverified boundary name: notes.txt key = %q, want empty", k)
	}
	ws = harnessWorkspace(t, filepath.Join(tmp, "outer", "mid", "repo"))
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != WriteClassApprovalKey {
		t.Fatalf("control, unlistable folder above the repository: notes.txt key = %q, want %q", k, WriteClassApprovalKey)
	}
}

// TestHarnessPlanRepositoryPrefix: the key classifies the path from the
// enclosing repository's own name down, so a root inside a harness folder, or
// a harness folder that is itself a repository, does not hide it, while a
// worktree (a .git file or link at its root) adds only its own name.
func TestHarnessPlanRepositoryPrefix(t *testing.T) {
	tmp := t.TempDir()
	for _, d := range []string{"repo/.git", "repo/.github/hooks", "repo/.vscode", "repo/.claude/worktrees/w/docs", "repo/docs", "plain/.claude", "gitdir", "dot/.claude/.git", "dotm/.claude"} {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"repo/.claude/worktrees/w/.git", "dotm/.claude/.git"} {
		if err := os.WriteFile(filepath.Join(tmp, filepath.FromSlash(f)), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		s := filepath.Join(tmp, "repo", ".claude", "worktrees", "s")
		if err := os.Mkdir(s, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(tmp, "gitdir"), filepath.Join(s, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(tmp, "repo", ".claude"), filepath.Join(tmp, "link")); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		name, root, rel, want string
		symlink               bool
	}{
		{".claude root", "repo/.claude", "settings.json", "", false},
		{".github root", "repo/.github", "hooks/x.json", "", false},
		{".vscode root", "repo/.vscode", "settings.json", "", false},
		{"worktree with .git file", "repo/.claude/worktrees/w", "notes.txt", WriteClassApprovalKey, false},
		{"folder inside that worktree", "repo/.claude/worktrees/w/docs", "notes.txt", WriteClassApprovalKey, false},
		{"worktree with .git symlink", "repo/.claude/worktrees/s", "notes.txt", WriteClassApprovalKey, true},
		{"ordinary folder", "repo/docs", "notes.txt", WriteClassApprovalKey, false},
		{"non-git .claude root", "plain/.claude", "notes.txt", "", false},
		{".claude that is a repository", "dot/.claude", "settings.json", "", false},
		{".claude that is a submodule", "dotm/.claude", "notes.txt", "", false},
		{"symlinked root", "link", "settings.json", "", true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if r.symlink && runtime.GOOS == "windows" {
				t.Skip("symlink fixture")
			}
			ws := harnessWorkspace(t, filepath.Join(tmp, filepath.FromSlash(r.root)))
			if k := harnessWritePlan(t, ws, r.rel).ApprovalKey; k != r.want {
				t.Fatalf("root %s, write_file %s key = %q, want %q", r.root, r.rel, k, r.want)
			}
		})
	}
}

// TestHarnessPlanScopeGuardNotConsulted: key resolution is a pure lookup. It
// never asks the scope guard for a write check, so an out-of-scope ordinary
// edit keeps the write-class key and fails at Invoke as before (#725).
func TestHarnessPlanScopeGuardNotConsulted(t *testing.T) {
	root := t.TempDir()
	harnessFixture(t, root, "notes.txt")
	harnessFixture(t, root, "CLAUDE.md")
	ws := harnessWorkspace(t, root)
	var writes []string
	ws.SetScopeGuard(func(rel string, write bool) error {
		if write {
			writes = append(writes, rel)
			return errors.New("writes denied")
		}
		return nil
	})
	if k := harnessEditPlan(t, ws, "notes.txt").ApprovalKey; k != WriteClassApprovalKey {
		t.Fatalf("edit_file notes.txt key = %q, want %q", k, WriteClassApprovalKey)
	}
	if k := harnessEditPlan(t, ws, "CLAUDE.md").ApprovalKey; k != "" {
		t.Fatalf("edit_file CLAUDE.md key = %q, want empty", k)
	}
	if len(writes) != 0 {
		t.Fatalf("Plan asked the guard for write checks on %q, want none", writes)
	}
}

// TestHarnessPlanSymlinkedInstructionFile: a symlinked instruction file is
// refused by the resolver as before, and its regular target still prompts.
func TestHarnessPlanSymlinkedInstructionFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink fixture")
	}
	root := t.TempDir()
	harnessFixture(t, root, "AGENTS.md")
	if err := os.Symlink("AGENTS.md", filepath.Join(root, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	ws := harnessWorkspace(t, root)
	if _, err := NewWriteFile(ws, nil).Plan(context.Background(),
		json.RawMessage(`{"path":"CLAUDE.md","content":"new\n"}`)); err == nil {
		t.Fatal("write_file through the CLAUDE.md symlink planned; want the resolver's refusal")
	}
	if k := harnessWritePlan(t, ws, "AGENTS.md").ApprovalKey; k != "" {
		t.Fatalf("write_file AGENTS.md key = %q, want empty", k)
	}
}
