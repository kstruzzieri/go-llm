package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
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
	ws := harnessWorkspace(t, root) // the host's folders above root must not decide the key (#784)
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

// harnessFixture creates rel under root holding harnessFixtureContent(rel),
// parents included.
func harnessFixture(t *testing.T, root, rel string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(harnessFixtureContent(rel)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// harnessFixtureContent holds exactly one "old\n", which the edit rows
// replace, and stays valid for a D4 configuration file, which the reference
// set reads and would otherwise find malformed (#784): "// old\n{}\n" for
// JSON, "# old\n" for .codex/config.toml, "old\n" for anything else.
func harnessFixtureContent(rel string) string {
	keys, ok := commandFileKeysFor(interceptor.HarnessKey(rel))
	switch {
	case !ok:
		return "old\n"
	case keys == nil:
		return "# old\n"
	}
	return "// old\n{}\n"
}

// harnessWorkspace returns a Workspace on root whose reference set (#784)
// stops its ancestor scan at root's parent, so the host's own folders above
// the test never change a result.
func harnessWorkspace(t *testing.T, root string) *Workspace {
	t.Helper()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	ws.harnessAncestorStop = filepath.Dir(ws.root)
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
		// The ordinary twin holds the same content, so the previews match up
		// to the path (#784 made the D4 fixtures valid configuration).
		if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte(harnessFixtureContent(rel)), 0o644); err != nil {
			t.Fatal(err)
		}
		ordinary := map[string]agent.ToolPlan{
			"write_file": harnessWritePlan(t, ws, "notes.txt"),
			"edit_file":  harnessEditPlan(t, ws, "notes.txt"),
		}
		got := map[string]agent.ToolPlan{
			"write_file": harnessWritePlan(t, ws, rel),
			"edit_file":  harnessEditPlan(t, ws, rel),
		}
		for tool, p := range got {
			// containment: GRANT-HARNESS
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
			// containment: PATH-HARNESS
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
		// Darwin proves an existing component's spelling without listing its parent (provenSpelling), so this path is Linux's.
		t.Skip("capability:linux")
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
	// containment: PATH-HARNESS
	for _, rel := range []string{"d/x.txt", "d/sub/new.txt"} {
		if _, verified, err := ws.resolvedWriteRel(rel); err != nil || verified {
			t.Fatalf("resolvedWriteRel(%s) = (verified %v, %v), want unverified", rel, verified, err)
		}
	}
	if k := harnessWritePlan(t, ws, "d/sub/new.txt").ApprovalKey; k != "" {
		t.Fatalf("write_file d/sub/new.txt key = %q, want empty", k)
	}
}

// TestHarnessPlanUnverifiedRoot: a root component strictly below the
// repository boundary whose spelling cannot be listed is part of the
// classified prefix, so every write prompts; an unlistable folder that holds
// the repository, directly or further up, changes nothing.
func TestHarnessPlanUnverifiedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission fixture")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permission")
	}
	tmp := t.TempDir()
	for _, d := range []string{"repo/.git", "repo/a/b", "one/repo/.git", "one/repo/a", "edge/repo/.git", "outer/mid/repo/.git"} {
		if err := os.MkdirAll(filepath.Join(tmp, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"repo/a", "one/repo", "edge", "outer"} {
		p := filepath.Join(tmp, filepath.FromSlash(d))
		if err := os.Chmod(p, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o755) })
	}
	ws := harnessWorkspace(t, filepath.Join(tmp, "repo", "a", "b"))
	// containment: PATH-HARNESS
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != "" {
		t.Fatalf("unverified root below the boundary: notes.txt key = %q, want empty", k)
	}
	// The repository folder itself cannot be listed, so a's spelling is
	// unverified one level below the boundary.
	ws = harnessWorkspace(t, filepath.Join(tmp, "one", "repo", "a"))
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != "" {
		t.Fatalf("unverified root one level below the boundary: notes.txt key = %q, want empty", k)
	}
	// Control: the unlistable edge holds the repository directly, so only the
	// repository folder's own name is unverified, and that changes nothing.
	ws = harnessWorkspace(t, filepath.Join(tmp, "edge", "repo"))
	if k := harnessWritePlan(t, ws, "notes.txt").ApprovalKey; k != WriteClassApprovalKey {
		t.Fatalf("control, unlistable folder holding the repository: notes.txt key = %q, want %q", k, WriteClassApprovalKey)
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
			// containment: PATH-HARNESS
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

// harnessPlanKeys asserts that write_file, and edit_file when rel exists,
// plan want for rel. rel's parent is created first, as acceptance requires.
func harnessPlanKeys(t *testing.T, ws *Workspace, root, rel, want string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	plans := map[string]func(*testing.T, *Workspace, string) agent.ToolPlan{"write_file": harnessWritePlan}
	if _, err := os.Stat(p); err == nil {
		plans["edit_file"] = harnessEditPlan
	}
	for tool, plan := range plans {
		// containment: PATH-HARNESS-REF
		if k := plan(t, ws, rel).ApprovalKey; k != want {
			_, why := ws.fileToolApprovalKey(context.Background(), rel)
			t.Errorf("%s %s key = %q (reason %q), want %q", tool, rel, k, why, want)
		}
	}
}

// runHarnessPlanRow builds r and checks its targets through the real Plan,
// with notes.txt as the completeness control.
func runHarnessPlanRow(t *testing.T, r harnessRefRow) {
	t.Helper()
	_, root, ws := harnessRefFixture(t, r)
	harnessPlanKeys(t, ws, root, "notes.txt", WriteClassApprovalKey)
	for _, rel := range r.covered {
		harnessPlanKeys(t, ws, root, rel, "")
	}
	for _, rel := range r.ordinary {
		harnessPlanKeys(t, ws, root, rel, WriteClassApprovalKey)
	}
}

// TestHarnessPlanReferencesKinds: a file each kind of harness reference
// names plans the empty key (#784, acceptance 2).
func TestHarnessPlanReferencesKinds(t *testing.T) {
	hook := func(cmd string) string {
		return `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"` + cmd + `"}]}]}}`
	}
	rows := []harnessRefRow{
		{name: "CLAUDE.md import", files: map[string]string{"CLAUDE.md": "@docs/a.md\n"}, covered: []string{"docs/a.md"}},
		{name: "GEMINI.md import", files: map[string]string{"GEMINI.md": "@docs/g.md\n"}, covered: []string{"docs/g.md"}},
		{name: "AGENTS.md import", files: map[string]string{"AGENTS.md": "@docs/c.md\n"}, covered: []string{"docs/c.md"}},
		{name: "absolute import", files: map[string]string{"CLAUDE.md": "@{root}/docs/abs.md\n@/etc/hosts\n"}, covered: []string{"docs/abs.md"}},
		{name: "OpenCode path and glob", files: map[string]string{"opencode.json": `{"instructions":["docs/oc.md","rules/**/*.md"]}`},
			covered: []string{"docs/oc.md", "rules/x/new.md"}},
		{name: "OpenCode glob through a linked folder", unix: true,
			files:   map[string]string{"opencode.json": `{"instructions":["rules/*/policy.md"]}`, "docs/team/policy.md": "old\n"},
			links:   map[string]string{"rules/team": "../docs/team"},
			covered: []string{"docs/team/policy.md"}},
		{name: "Codex inline table", files: map[string]string{".codex/config.toml": `mcp_servers.demo = { command = "python3", args = ["scripts/server.py"] }` + "\n"},
			covered: []string{"scripts/server.py"}},
		{name: "VS Code MCP with a comment", files: map[string]string{".vscode/mcp.json": "// local servers\n{\"servers\":{\"s\":{\"command\":\"./scripts/vs.sh\"}}}\n"},
			covered: []string{"scripts/vs.sh"}},
		{name: "Copilot hook under cwd", files: map[string]string{".github/hooks/h.json": `{"hooks":{"sessionStart":[{"bash":"./run.sh","cwd":"tools"}]}}`},
			covered: []string{"tools/run.sh"}},
		{name: "Golem verify under dir", files: map[string]string{".golem.json": `{"verify":{"argv":["./v.sh"],"dir":"sub"}}`}, covered: []string{"sub/v.sh"}},
		{name: "absent MCP operand", files: map[string]string{".mcp.json": `{"command":"python3","args":["server.py"]}`}, covered: []string{"server.py"}},
		{name: "absent MCP operand under cwd", files: map[string]string{".mcp.json": `{"command":"python3","args":["server.py"],"cwd":"svc"}`},
			covered: []string{"svc/server.py"}},
		{name: "bare program on a workspace PATH entry", unix: true,
			files: map[string]string{".mcp.json": `{"command":"tool1"}`, "bin/tool1": "#!/bin/sh\nold\n"},
			prep: func(t *testing.T, _, root string, _ *Workspace) {
				if err := os.Chmod(filepath.Join(root, "bin", "tool1"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", filepath.Join(root, "bin"))
			},
			covered: []string{"bin/tool1"}},
		{name: "linked rules folder and what it imports", unix: true,
			files:   map[string]string{"docs/shared/x.md": "old\n", "docs/shared/r.md": "@../outside.md\nold\n"},
			links:   map[string]string{".claude/rules/shared": "../../docs/shared"},
			covered: []string{"docs/shared/x.md", "docs/outside.md"}},
		{name: "linked CLAUDE.md and what it imports", unix: true,
			files:   map[string]string{"docs/guide.md": "@g2.md\nold\n"},
			links:   map[string]string{"CLAUDE.md": "docs/guide.md"},
			covered: []string{"docs/guide.md", "docs/g2.md"}},
		{name: "linked .vscode", unix: true,
			files:   map[string]string{"config/editor/mcp.json": "// old\n{\"servers\":{\"s\":{\"command\":\"./scripts/v.sh\"}}}\n"},
			links:   map[string]string{".vscode": "config/editor"},
			covered: []string{"scripts/v.sh", "config/editor/mcp.json"}},
		{name: "import depth five", files: harnessChain(5), covered: []string{"docs/h2.md", "docs/h5.md"}, ordinary: []string{"docs/h6.md"}},
		{name: "home import", files: map[string]string{"CLAUDE.md": "@~/ws/docs/home.md\n"},
			prep: func(t *testing.T, tmp, _ string, _ *Workspace) {
				t.Setenv("HOME", tmp)
				t.Setenv("USERPROFILE", tmp)
			},
			covered: []string{"docs/home.md"}},
		{name: "import through a linked parent of the root", unix: true, root: "real/ws",
			files: map[string]string{"CLAUDE.md": "@{tmp}/alias/ws/docs/sl.md\n"},
			prep: func(t *testing.T, tmp, _ string, _ *Workspace) {
				if err := os.Symlink(filepath.Join(tmp, "real"), filepath.Join(tmp, "alias")); err != nil {
					t.Fatal(err)
				}
			},
			covered: []string{"docs/sl.md"}},
		{name: "ancestor CLAUDE.md and settings", root: "outer/ws",
			files:   map[string]string{"../CLAUDE.md": "@ws/docs/up.md\n", "../.claude/settings.json": hook("ws/scripts/up.sh")},
			prep:    func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp },
			covered: []string{"docs/up.md", "scripts/up.sh"}},
		// Fix round 1.
		{name: "absent hook script behind a linked folder", unix: true,
			files:   map[string]string{".claude/settings.json": hook("./scripts/x.sh"), "tools/keep.txt": "k\n"},
			links:   map[string]string{"scripts": "tools"},
			covered: []string{"tools/x.sh"}},
		{name: "absent import behind a linked folder", unix: true,
			files:   map[string]string{"CLAUDE.md": "@docs/shared/new.md\n", "common/keep.txt": "k\n"},
			links:   map[string]string{"docs/shared": "../common"},
			covered: []string{"common/new.md"}},
		{name: "absolute OpenCode glob", files: map[string]string{"opencode.json": `{"instructions":["{root}/rules/*.md"]}`},
			covered: []string{"rules/new.md"}},
		{name: "ancestor .clinerules folder", root: "outer/ws",
			files:   map[string]string{"../.clinerules/a.md": "@../ws/docs/cl.md\n"},
			prep:    func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp },
			covered: []string{"docs/cl.md"}},
		{name: "workspace .clinerules folder", files: map[string]string{".clinerules/team.md": "@../docs/cw.md\n"},
			covered: []string{"docs/cw.md"}},
		{name: "repository settings with the root under a .codex folder", root: "x/.codex/w/repo/sub",
			files: map[string]string{"../.git/HEAD": "ref: refs/heads/main\n", "../.claude/settings.json": hook("sub/scripts/a.sh")},
			prep: func(_ *testing.T, tmp, _ string, ws *Workspace) {
				ws.harnessAncestorStop = filepath.Join(tmp, "x", ".codex", "w")
			},
			covered: []string{"scripts/a.sh"}},
		{name: ".github and .vscode linked to one folder", unix: true,
			files: map[string]string{
				"cfg/mcp.json":     `{"servers":{"s":{"command":"./scripts/v.sh"}}}`,
				"cfg/hooks/h.json": `{"hooks":{"sessionStart":[{"bash":"./scripts/h.sh"}]}}`,
			},
			links:   map[string]string{".github": "cfg", ".vscode": "cfg"},
			covered: []string{"scripts/v.sh", "scripts/h.sh"}},
		{name: "root inside a .github folder", root: "repo/.github/sub",
			files:   map[string]string{"../../.git/HEAD": "ref: refs/heads/main\n", ".claude/settings.json": hook("scripts/x.sh")},
			covered: []string{"scripts/x.sh"}},
		{name: "dangling link chain", unix: true,
			links:   map[string]string{"CLAUDE.md": "b", "b": "c"},
			covered: []string{"c"}},
		// Fix round 2.
		{name: "workspace glob with [ in the root path", root: "proj[1]/ws",
			files: map[string]string{"opencode.json": `{"instructions":["rules/*.md"]}`}, covered: []string{"rules/new.md"}},
		{name: "ancestor glob with [ in the root path", root: "proj[1]/ws",
			files:   map[string]string{"../opencode.json": `{"instructions":["**/*.md"]}`},
			prep:    func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp },
			covered: []string{"rules/new.md"}},
		{name: "glob in a folder named with [", files: map[string]string{"x[1]/opencode.json": `{"instructions":["*.md"]}`},
			covered: []string{"x[1]/new.md"}},
		{name: "dangling chain through a linked folder", unix: true,
			files:   map[string]string{"deep/x/keep.txt": "k\n"},
			links:   map[string]string{"CLAUDE.md": "sub/b", "sub": "deep/x", "deep/x/b": "../c.md"},
			covered: []string{"deep/c.md"}},
		{name: "folderless D4 file under a harness folder", files: map[string]string{".claude/skills/foo/.mcp.json": `{"command":"./scripts/n.sh"}`},
			covered: []string{"scripts/n.sh"}},
		{name: "absent config under a linked .vscode", unix: true,
			files:    map[string]string{"config/editor/keep.txt": "k\n"},
			links:    map[string]string{".vscode": "config/editor"},
			covered:  []string{"config/editor/mcp.json", "config/editor/settings.json"},
			ordinary: []string{"config/editor/notes.txt"}},
		{name: "absent hook under a linked .github", unix: true,
			files:    map[string]string{"cfg/hooks/keep.json": "{}"},
			links:    map[string]string{".github": "cfg"},
			covered:  []string{"cfg/hooks/new.json"},
			ordinary: []string{"cfg/notes.txt"}},
	}
	// One script row per D4 file.
	for _, f := range HarnessCommandFiles() {
		file := f
		if path.Ext(f) == "" {
			file = f + "/h.json"
		}
		script := "scripts/" + strings.NewReplacer("/", "-", ".", "").Replace(file) + ".sh"
		content := `{"command":"./` + script + `"}`
		switch f {
		case ".golem.json":
			content = `{"argv":["./` + script + `"]}`
		case ".codex/config.toml":
			content = `command = "./` + script + `"` + "\n"
		}
		rows = append(rows, harnessRefRow{name: "D4 " + file, files: map[string]string{file: content}, covered: []string{script}})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) { runHarnessPlanRow(t, r) })
	}
}

// TestHarnessPlanReferencesNFD: an import written in NFC covers the file
// APFS stores in NFD, and an absent target written in the other form.
func TestHarnessPlanReferencesNFD(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("capability:darwin")
	}
	nfc, nfd := "café", "café"
	runHarnessPlanRow(t, harnessRefRow{
		files: map[string]string{nfd + "/p.md": "old\n", "CLAUDE.md": "@" + nfc + "/p.md\n@" + nfc + "2/new.md\n"},
		prep: func(t *testing.T, _, root string, _ *Workspace) {
			if _, err := os.Stat(filepath.Join(root, nfc)); err != nil {
				t.Skip("volume does not alias NFC and NFD")
			}
		},
		covered: []string{nfd + "/p.md", nfd + "2/new.md"},
	})
}

// TestHarnessPlanReferencesStoredSpelling: an import spelled with a final
// sigma covers the file the volume stores with a medial one.
func TestHarnessPlanReferencesStoredSpelling(t *testing.T) {
	runHarnessPlanRow(t, harnessRefRow{
		files: map[string]string{"docs/σ.md": "old\n", "CLAUDE.md": "@docs/ς.md\n"},
		prep: func(t *testing.T, _, root string, _ *Workspace) {
			if _, err := os.Stat(filepath.Join(root, "docs", "ς.md")); err != nil {
				t.Skip("volume does not fold ς to σ")
			}
		},
		covered: []string{"docs/σ.md"},
	})
}

// TestHarnessPlanReferencesFold: references and targets compare through
// HarnessKey, so a differently-cased import covers an absent target.
func TestHarnessPlanReferencesFold(t *testing.T) {
	runHarnessPlanRow(t, harnessRefRow{files: map[string]string{"CLAUDE.md": "@Docs/Policy.md\n"}, covered: []string{"docs/policy.md"}})
}

// TestHarnessPlanReferencesResolvedSpelling: the resolved spelling of the
// target is checked against the set too.
func TestHarnessPlanReferencesResolvedSpelling(t *testing.T) {
	runHarnessPlanRow(t, harnessRefRow{
		files: map[string]string{"CLAUDE.md": "@docs/policy.md\n", "odd/x.md": "old\n"},
		prep: func(_ *testing.T, _, _ string, ws *Workspace) {
			ws.resolveForKey = func(p string) (string, bool, error) {
				if p == "odd/x.md" {
					return "docs/policy.md", true, nil
				}
				return p, true, nil
			}
		},
		covered: []string{"odd/x.md"},
	})
}

// TestHarnessPlanReferencesAntiGoals: a permission rule, AgentFlow's .agent
// state and a glob-shaped command token cover nothing.
func TestHarnessPlanReferencesAntiGoals(t *testing.T) {
	runHarnessPlanRow(t, harnessRefRow{
		files: map[string]string{
			".claude/settings.local.json": `{"permissions":{"allow":["Bash(cp src/a.go b)"]}}`,
			".agent/notes.md":             "@../docs/n.md\n",
			".claude/settings.json":       `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"eslint src/*.js"}]}]}}`,
			"src/a.go":                    "old\n",
			"src/a.js":                    "old\n",
		},
		ordinary: []string{"src/a.go", "docs/n.md", "src/a.js"},
	})
}

// TestHarnessPlanReferencesFreshness (acceptance 3): the set is built on
// every Plan, so an import added or removed on disk counts at once.
func TestHarnessPlanReferencesFreshness(t *testing.T) {
	_, root, ws := harnessRefFixture(t, harnessRefRow{})
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	harnessPlanKeys(t, ws, root, "docs/new.md", WriteClassApprovalKey)
	write("CLAUDE.md", "@docs/new.md\n")
	harnessPlanKeys(t, ws, root, "docs/new.md", "")
	write("docs/new.md", "old\n")
	harnessPlanKeys(t, ws, root, "docs/new.md", "")
	harnessPlanKeys(t, ws, root, "notes.txt", WriteClassApprovalKey)
	write("CLAUDE.md", "no imports\n")
	harnessPlanKeys(t, ws, root, "docs/new.md", WriteClassApprovalKey)
}

// TestHarnessPlanReferencesRootInHarnessFolder (Review Focus 5): with the
// root at repo/.claude, its settings.json hook resolves against the file's
// folder to the workspace's scripts/x.sh, although the anchor (repo) lies
// outside the workspace. #725 already prompts for every file there, so the set
// itself is checked as well.
func TestHarnessPlanReferencesRootInHarnessFolder(t *testing.T) {
	_, root, ws := harnessRefFixture(t, harnessRefRow{
		root:  "repo/.claude",
		files: map[string]string{"../.git/HEAD": "ref: refs/heads/main\n", "settings.json": `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"scripts/x.sh"}]}]}}`},
	})
	harnessPlanKeys(t, ws, root, "scripts/x.sh", "")
	refs := ws.harnessReferences(context.Background())
	if !refs.complete {
		t.Fatalf("set incomplete: %s", refs.cause)
	}
	// containment: PATH-HARNESS-REF
	if ok, why := refs.covers("scripts/x.sh"); !ok || why != "settings.json runs scripts/x.sh" {
		t.Fatalf("covers(scripts/x.sh) = (%v, %q), want the settings.json reason", ok, why)
	}
	if ok, why := refs.covers("notes.txt"); ok {
		t.Fatalf("notes.txt covered: %s", why)
	}
}

// TestHarnessPlanReferencesUserSettings (Review Focus 1): a user-level
// settings.json with $HOME, absolute and $CLAUDE_PROJECT_DIR hooks leaves the
// set complete, and the variable means the workspace, not the folder above.
func TestHarnessPlanReferencesUserSettings(t *testing.T) {
	cmds := []string{`$HOME/.claude/hooks/x.sh`, `/usr/local/bin/x --flag`, `\"$CLAUDE_PROJECT_DIR\"/scripts/lint.sh`}
	var hooks []string
	for _, c := range cmds {
		hooks = append(hooks, `{"type":"command","command":"`+c+`"}`)
	}
	runHarnessPlanRow(t, harnessRefRow{
		root:    "outer/ws",
		files:   map[string]string{"../.claude/settings.json": `{"hooks":{"PreToolUse":[{"hooks":[` + strings.Join(hooks, ",") + `]}]}}`},
		prep:    func(_ *testing.T, tmp, _ string, ws *Workspace) { ws.harnessAncestorStop = tmp },
		covered: []string{"scripts/lint.sh"},
	})
}

// TestHarnessPlanReferencesUnreachableImportNames: Markdown "@" words that
// name no file anyone can read leave the set complete (fix round 1b). On
// Windows these names fail the stat as invalid or too long rather than absent.
func TestHarnessPlanReferencesUnreachableImportNames(t *testing.T) {
	runHarnessPlanRow(t, harnessRefRow{files: map[string]string{
		"CLAUDE.md": "contact @team: now\n@https://example.com/x.md\n@" + strings.Repeat("n", 300) + ".md\n",
	}})
}
