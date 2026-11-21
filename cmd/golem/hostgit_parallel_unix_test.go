//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

// resolvedParallelRepo is newParallelTestRepo with symlinks resolved, the form
// git reports for a toplevel (macOS temp dirs sit behind /var -> /private/var).
func resolvedParallelRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(newParallelTestRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// selectForRefusal runs selection on a coordinator whose plan reaches
// recordBase, so the first git call is the one under test.
func selectForRefusal(t *testing.T, root string) error {
	t.Helper()
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, nil)
	_, err := c.selectWorkers(context.Background())
	_ = c.releaseWorkspaceLock()
	return err
}

func TestParallelRefusesOldGit(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	root := newParallelTestRepo(t)
	marker := filepath.Join(t.TempDir(), "marker")
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "git"), fakeGitScript("2.37.9", marker))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := selectForRefusal(t, root); err == nil || !strings.Contains(err.Error(), "git 2.38 or later") {
		t.Fatalf("selectWorkers(old git) err = %v, want git 2.38 or later", err)
	}
	requireNoMarker(t, marker)

	// Control: the same wrapper reporting 2.38.0 and running the real git.
	writeExecutable(t, filepath.Join(bin, "git"), `if [ "$1" = "--version" ]; then echo "git version 2.38.0"; exit 0; fi
exec `+shellQuote(real)+` "$@"
`)
	if err := selectForRefusal(t, root); err != nil {
		t.Fatalf("control selection: %v", err)
	}
}

func TestParallelRefusesImplicitBareRepository(t *testing.T) {
	isolateGitConfig(t)
	globalCfg := filepath.Join(t.TempDir(), "config")
	writeTestFile(t, globalCfg, "[safe]\n\tbareRepository = all\n", 0o600)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	repo := resolvedParallelRepo(t)
	ws := filepath.Join(repo, "ws")
	if err := os.Mkdir(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	fsmonitor := filepath.Join(t.TempDir(), "fsmonitor")
	writeExecutable(t, fsmonitor, "echo ran > "+shellQuote(marker)+"\nprintf 'token\\000/\\000'\n")
	plantBare(t, ws)
	for _, kv := range [][2]string{{"core.bare", "false"}, {"core.worktree", ws}, {"core.fsmonitor", fsmonitor}} {
		runTestGit(t, ws, "config", "--file", filepath.Join(ws, "config"), kv[0], kv[1])
	}
	if got := strings.TrimSpace(runTestGit(t, ws, "rev-parse", "--absolute-git-dir")); got != ws {
		t.Fatalf("control: git selected %q, want the plant %q", got, ws)
	}
	_ = os.Remove(marker)

	err := selectForRefusal(t, ws)
	if err == nil || !strings.Contains(err.Error(), "safe.bareRepository") {
		t.Fatalf("selectWorkers(planted bare) err = %v, want safe.bareRepository refusal", err)
	}
	requireNoMarker(t, marker)
}

func TestParallelRefusesWorkspaceExecutable(t *testing.T) {
	root := newParallelTestRepo(t)
	marker := filepath.Join(t.TempDir(), "marker")
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(bin, "git"), "echo ran > "+shellQuote(marker)+"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := selectForRefusal(t, root)
	if err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("selectWorkers(workspace git) err = %v, want a refusal naming the workspace", err)
	}
	requireNoMarker(t, marker)
}

// A linked worktree keeps its git directory under <repo>/.git/worktrees, so
// the hardened calls must still resolve it, run a cohort and place the lock.
func TestHostGitWorksInLinkedWorktree(t *testing.T) {
	isolateGitConfig(t)
	repo := resolvedParallelRepo(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(tmp, "linked")
	runTestGit(t, repo, "worktree", "add", "--detach", linked, "HEAD")
	t.Cleanup(func() {
		_, _ = runParallelGit(context.Background(), repo, repo, "worktree", "remove", "--force", linked)
	})
	if err := os.Mkdir(filepath.Join(linked, ".agent"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(linked, ".agent", "state.json"), "{\"state\":true}\n", 0o640)

	snap, err := loadGitContext(context.Background(), "git", linked)
	if err != nil || snap.Absence != gitContextPresent {
		t.Fatalf("capture: err=%v absence=%v", err, snap.Absence)
	}
	wantTop, _ := filepath.EvalSymlinks(linked)
	if gotTop, _ := filepath.EvalSymlinks(snap.State.Toplevel); gotTop != wantTop {
		t.Fatalf("Toplevel = %q, want %q", snap.State.Toplevel, linked)
	}

	worker := func(_ context.Context, w parallelWorker) error {
		switch w.sourceID {
		case "w1":
			return os.WriteFile(filepath.Join(w.root, "created.go"), []byte("created\n"), 0o600)
		case "w2":
			return os.WriteFile(filepath.Join(w.root, "b.go"), []byte("b changed\n"), 0o600)
		default:
			return fmt.Errorf("unexpected worker %s", w.sourceID)
		}
	}
	c := newParallelCoordinator(linked, parallelTestPlan("created.go", "b.go"), 2, worker)
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
	if ran, err := c.runCohort(context.Background()); err != nil || !ran {
		t.Fatalf("runCohort() = %v, %v", ran, err)
	}
	if err := c.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	out, err := runParallelGit(context.Background(), linked, linked, "rev-parse", "--path-format=absolute", "--git-path", "golem-parallel.lock")
	if err != nil {
		t.Fatal(err)
	}
	lockFile := strings.TrimSpace(string(out))
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("lock file: %v", err)
	}
	lock, _ := filepath.EvalSymlinks(filepath.Dir(lockFile))
	wantDir, _ := filepath.EvalSymlinks(filepath.Join(repo, ".git", "worktrees"))
	if !strings.HasPrefix(lock, wantDir+string(filepath.Separator)) {
		t.Fatalf("lock dir %q is not under %q", lock, wantDir)
	}
}

// Worker code runs before the first worker-side git call, so a git planted in
// the workspace by a worker must be refused there: worker-side calls take the
// coordinator root, not the worker root, as the workspace.
func TestParallelWorkerSideGitRefusesWorkspaceExecutable(t *testing.T) {
	isolateGitConfig(t)
	root := resolvedParallelRepo(t)
	bin := filepath.Join(root, "bin")
	// bin does not exist yet, so selection resolves the real git past it.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(t.TempDir(), "marker")
	worker := func(_ context.Context, w parallelWorker) error {
		if w.sourceID != "w1" {
			return nil
		}
		if err := os.Mkdir(bin, 0o700); err != nil {
			return err
		}
		writeExecutable(t, filepath.Join(bin, "git"), "echo ran > "+shellQuote(marker)+"\n")
		return nil
	}
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, worker)
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })

	_, err := c.runCohort(context.Background())
	if err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Fatalf("runCohort() err = %v, want a refusal: executable inside the workspace", err)
	}
	requireNoMarker(t, marker)
}
