//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// plantBare copies a fresh bare repository's HEAD, objects, refs and config
// into dir, the way a sandboxed command can leave one in a workspace.
func plantBare(t *testing.T, dir string) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "plant.git")
	gitContextTestRun(t, filepath.Dir(src), "init", "-q", "--bare", src)
	for _, name := range []string{"HEAD", "objects", "refs", "config"} {
		if out, err := exec.Command("cp", "-R", filepath.Join(src, name), filepath.Join(dir, name)).CombinedOutput(); err != nil {
			t.Fatalf("cp %s: %v\n%s", name, err, out)
		}
	}
}

func requireNoMarker(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted program ran: the marker exists")
	}
}

func TestSnapshotRefusesOldGit(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if real, err = filepath.Abs(real); err != nil {
		t.Fatal(err)
	}
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	marker := filepath.Join(t.TempDir(), "marker")
	bin := t.TempDir()
	// Write the script directly: fakeGitOnPath's own version answer would win.
	writeExecutable(t, filepath.Join(bin, "git"), fakeGitScript("2.37.9", marker))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err = loadGitContext(context.Background(), "git", root)
	if err == nil || !strings.Contains(err.Error(), "git 2.38 or later") {
		t.Fatalf("loadGitContext(old git) err = %v, want git 2.38 or later", err)
	}
	requireNoMarker(t, marker)

	// Control: the same wrapper reporting 2.38.0 and running the real git.
	writeExecutable(t, filepath.Join(bin, "git"), `if [ "$1" = "--version" ]; then echo "git version 2.38.0"; exit 0; fi
exec `+shellQuote(real)+` "$@"
`)
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil || snap.Absence != gitContextPresent {
		t.Fatalf("control capture: err=%v absence=%v", err, snap.Absence)
	}
}

func TestSnapshotRefusesImplicitBareRepository(t *testing.T) {
	isolateGitConfig(t)
	globalCfg := filepath.Join(t.TempDir(), "config")
	writeTestFile(t, globalCfg, "[safe]\n\tbareRepository = all\n", 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	repo := gitContextTestRepo(t)
	gitContextTestCommit(t, repo, "tracked.go", "package x\n", "base")
	ws := filepath.Join(repo, "ws")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	fsmonitor := filepath.Join(t.TempDir(), "fsmonitor")
	writeExecutable(t, fsmonitor, "echo ran > "+shellQuote(marker)+"\nprintf 'token\\000/\\000'\n")
	plantBare(t, ws)
	for _, kv := range [][2]string{{"core.bare", "false"}, {"core.worktree", ws}, {"core.fsmonitor", fsmonitor}} {
		gitContextTestRun(t, ws, "config", "--file", filepath.Join(ws, "config"), kv[0], kv[1])
	}
	// Control: git selects the plant, not repo/.git.
	if got := strings.TrimSpace(gitContextTestRun(t, ws, "rev-parse", "--absolute-git-dir")); got != ws {
		t.Fatalf("control: git selected %q, want the plant %q", got, ws)
	}
	_ = os.Remove(marker)

	_, err := loadGitContext(context.Background(), "git", ws)
	if err == nil || !strings.Contains(err.Error(), "safe.bareRepository") {
		t.Fatalf("loadGitContext(planted bare) err = %v, want safe.bareRepository refusal", err)
	}
	requireNoMarker(t, marker)
}

func TestSnapshotRefusesWorkspaceExecutable(t *testing.T) {
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	marker := filepath.Join(t.TempDir(), "marker")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "git"), "echo ran > "+shellQuote(marker)+"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := loadGitContext(context.Background(), "git", root)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("loadGitContext(workspace git) err = %v, want a refusal naming the workspace", err)
	}
	requireNoMarker(t, marker)
}

func TestSnapshotPrecheckRefusesLocalIncludes(t *testing.T) {
	for _, key := range []string{"include.path", "includeIf.onbranch:main.path"} {
		t.Run(key, func(t *testing.T) {
			isolateGitConfig(t)
			root := gitContextTestRepo(t)
			gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
			extra := filepath.Join(root, "extra.cfg")
			writeTestFile(t, extra, "", 0o600)

			// Control: the same include in the user's global config is allowed.
			global := filepath.Join(t.TempDir(), "config")
			writeTestFile(t, global, "", 0o600)
			gitContextTestRun(t, root, "config", "--file", global, key, extra)
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			if snap, err := loadGitContext(context.Background(), "git", root); err != nil || snap.Absence != gitContextPresent {
				t.Fatalf("control (global include): err=%v absence=%v", err, snap.Absence)
			}
			unsetenvForTest(t, "GIT_CONFIG_GLOBAL")

			gitContextTestRun(t, root, "config", key, extra)
			records := installGitRecorder(t)
			_, err := loadGitContext(context.Background(), "git", root)
			if err == nil || !strings.Contains(err.Error(), "includes another file") {
				t.Fatalf("loadGitContext(local %s) err = %v, want includes another file", key, err)
			}
			for _, r := range readGitRecords(t, records) {
				for _, a := range r.argv {
					if a == "status" {
						t.Fatalf("status ran despite a local include: %q", r.argv)
					}
				}
			}
		})
	}
}

func TestGitContextRefreshKeepsBlockOnRefusal(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"fatal: cannot use bare repository '/x/not a git repository' (safe.bareRepository is 'explicit')", false},
		{"fatal: not a git repository (or any of the parent directories): .git", true},
	} {
		if got := gitNotRepository(&gitExitError{code: 128, stderr: tc.stderr}); got != tc.want {
			t.Errorf("gitNotRepository(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}

	isolateGitConfig(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "not a git repository")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, root, "-c", "init.defaultBranch=main", "init", "-q")
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	sess := newMountSession(t, &captureCaller{answer: "ok"}, root)
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatal(err)
	}
	in := projectContextInputs(sess.sysInputs, nil, snap, false)
	if err := sess.mount(sess.mountAt, nil, in); err != nil {
		t.Fatal(err)
	}
	snap.Block = in.gitContext
	sess.gitSnapshot = snap
	system := sess.baseSystem
	if !strings.Contains(system, "branch: main\n") {
		t.Fatalf("startup state not installed: %q", system)
	}

	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	plantBare(t, root)
	got := refresh(t, sess)
	if !strings.HasPrefix(got, "git context refresh failed: ") {
		t.Fatalf("refresh output = %q, want a failure line", got)
	}
	if sess.baseSystem != system || sess.gitSnapshot.Block != snap.Block {
		t.Fatal("a refused refresh changed the session's git block")
	}
}

// Startup prints exactly one warning for a refused layout, sanitized, and
// injects no git block. The root path holds an ESC byte, which must not reach
// the warning raw.
func TestStartupWarnsOnceForRefusedGitLayout(t *testing.T) {
	isolateGitConfig(t)
	globalCfg := filepath.Join(t.TempDir(), "config")
	writeTestFile(t, globalCfg, "[safe]\n\tbareRepository = all\n", 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	configPath, _ := writeRunLifecycleConfig(t)
	root := filepath.Join(resolvedTemp(t), "a\x1b[31mb")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plantBare(t, root)

	stdin, stdout, stderr := runTestFiles(t)
	stop := errors.New("stop after startup")
	var system string
	err := run([]string{"-config", configPath, "-root", root, "-p", "goal", "-no-probe", "-no-cap-probe", "-no-compress", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index", "-no-session"}, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
		system = s.sysInputs.gitContext
		return stop
	}})
	if !errors.Is(err, stop) {
		t.Fatalf("run() err = %v, want the hook's stop", err)
	}
	var lines []string
	for _, f := range []*os.File{stdout, stderr} {
		for _, l := range strings.Split(readRunTestFile(t, f), "\n") {
			if strings.Contains(l, "git context disabled:") {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "safe.bareRepository") {
		t.Fatalf("warning lines = %q, want exactly one naming safe.bareRepository", lines)
	}
	if strings.ContainsRune(lines[0], 0x1b) {
		t.Fatalf("raw ESC in startup warning: %q", lines[0])
	}
	if system != "" {
		t.Fatalf("git block injected despite refusal: %q", system)
	}
}

// TestStartupWarningSanitizesStartError: a git on PATH under an ESC-named
// directory whose interpreter is missing fails at fork/exec with an error
// carrying the raw path; the startup warning must not echo that ESC.
func TestStartupWarningSanitizesStartError(t *testing.T) {
	isolateGitConfig(t)
	configPath, _ := writeRunLifecycleConfig(t)
	bin := filepath.Join(resolvedTemp(t), "b\x1b[31mx")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/nonexistent/interp\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdin, stdout, stderr := runTestFiles(t)
	stop := errors.New("stop")
	err := run([]string{"-config", configPath, "-root", resolvedTemp(t), "-p", "goal", "-no-probe", "-no-cap-probe", "-no-compress", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index", "-no-session"}, stdin, stdout, stderr, runHooks{afterSessionReady: func(*replSession) error { return stop }})
	if !errors.Is(err, stop) {
		t.Fatalf("run err = %v", err)
	}
	var lines []string
	for _, f := range []*os.File{stdout, stderr} {
		for _, l := range strings.Split(readRunTestFile(t, f), "\n") {
			if strings.Contains(l, "git context disabled:") {
				lines = append(lines, l)
			}
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want one warning, got %q", lines)
	}
	if strings.ContainsRune(lines[0], 0x1b) {
		t.Fatalf("raw ESC in startup warning: %q", lines[0])
	}
}
