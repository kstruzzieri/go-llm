package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agentflow"
)

func TestParallelSelectionUsesPlanOrderAndWorkerBound(t *testing.T) {
	plan := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"a.go"}},
		{ID: "P2", Files: []string{"b.go"}},
		{ID: "P3", Files: []string{"c.go"}, DependsOn: []string{"P1"}},
		{ID: "P4", Files: []string{"d.go"}},
	}}
	got := selectParallelSteps(plan, 2, func(agentflow.Step, []string) bool { return true })
	if want := []string{"P1", "P2"}; !reflect.DeepEqual(stepIDs(got), want) {
		t.Fatalf("selected = %v, want %v", stepIDs(got), want)
	}
}

func TestParallelSelectionFallsBackForSerialGraphsAndSingleCandidate(t *testing.T) {
	serial := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"a.go"}}, {ID: "P2", Files: []string{"b.go"}}}}
	if got := selectParallelSteps(serial, 2, func(agentflow.Step, []string) bool { return true }); got != nil {
		t.Fatalf("all-empty dependency graph selected %v", stepIDs(got))
	}

	dag := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"a.go"}},
		{ID: "P2", Files: []string{"b.go"}, DependsOn: []string{"P1"}},
	}}
	if got := selectParallelSteps(dag, 2, func(agentflow.Step, []string) bool { return true }); got != nil {
		t.Fatalf("single root selected %v", stepIDs(got))
	}
	if got := selectParallelSteps(dag, 1, func(agentflow.Step, []string) bool { return true }); got != nil {
		t.Fatalf("one worker selected %v", stepIDs(got))
	}
}

func TestParallelCoordinatorSerialGraphFallsBackWithoutGit(t *testing.T) {
	root := t.TempDir()
	plan := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"a.go"}},
		{ID: "P2", Files: []string{"b.go"}},
	}}
	c := newParallelCoordinator(root, plan, 2, nil)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || selected {
		t.Fatalf("selectWorkers() = %v, %v; want serial fallback", selected, err)
	}
	if c.tempParent != "" || len(c.preservedRoots()) != 0 {
		t.Fatalf("serial fallback created worktree state: parent=%q roots=%v", c.tempParent, c.preservedRoots())
	}
}

func TestParallelLiteralPathsRejectUnsafeOrIneffectiveScope(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		allowed []string
		blocked []string
	}{
		{name: "empty", file: ""},
		{name: "absolute", file: "/tmp/a.go"},
		{name: "dot prefix", file: "./a.go"},
		{name: "traversal", file: "a/../b.go"},
		{name: "backslash", file: `a\b.go`},
		{name: "glob star", file: "a/*.go"},
		{name: "glob question", file: "a/?.go"},
		{name: "glob class", file: "a/[x].go"},
		{name: "directory scope", file: "a/"},
		{name: "agent", file: ".AgEnT/state.json"},
		{name: "git", file: ".GIT/config"},
		{name: "not allowed", file: "a.go", allowed: []string{"b.go"}},
		{name: "blocked", file: "a.go", allowed: []string{"*"}, blocked: []string{"a.go"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &agentflow.Plan{AllowedFiles: tt.allowed, BlockedFiles: tt.blocked, Steps: []agentflow.Step{{ID: "P1", Files: []string{tt.file}}}}
			if paths, ok := parallelLiteralPaths(plan, plan.Steps[0]); ok {
				t.Fatalf("parallelLiteralPaths() = %v, true", paths)
			}
		})
	}
}

func TestParallelLiteralPathsAcceptExactFilesAndRejectDuplicates(t *testing.T) {
	plan := &agentflow.Plan{AllowedFiles: []string{"src/*"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"src/a.go", "src/b.go"}}}}
	got, ok := parallelLiteralPaths(plan, plan.Steps[0])
	if !ok || !reflect.DeepEqual(got, plan.Steps[0].Files) {
		t.Fatalf("parallelLiteralPaths() = %v, %v", got, ok)
	}
	plan.Steps[0].Files = []string{"src/a.go", "src/a.go"}
	if _, ok := parallelLiteralPaths(plan, plan.Steps[0]); ok {
		t.Fatal("duplicate literal path accepted")
	}
}

func TestParallelPathOverlapIsCaseFoldedAndSegmentAware(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		{[]string{"src/a.go"}, []string{"SRC/A.GO"}, true},
		{[]string{"foo"}, []string{"FOO/bar.go"}, true},
		{[]string{"foo/bar.go"}, []string{"foo"}, true},
		{[]string{"foo"}, []string{"foobar/a.go"}, false},
	}
	for _, tt := range tests {
		if got := parallelPathsOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("parallelPathsOverlap(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParallelStatusExemptsOnlyExactAgentSegment(t *testing.T) {
	if !isParallelAgentPath(".agent/state.json") {
		t.Fatal("exact .agent path was not exempted")
	}
	if isParallelAgentPath(".AGENT/state.json") {
		t.Fatal("case-variant .AGENT path was exempted")
	}
}

func TestParallelSafePathRejectsSpecialFile(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := t.TempDir()
	cmd := exec.Command(mkfifo, filepath.Join(root, "new.go"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	if _, err := parallelSafePath(root, "new.go"); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("parallelSafePath() error = %v", err)
	}
}

func TestParallelSelectionSkipsOverlappingCandidate(t *testing.T) {
	plan := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"foo"}},
		{ID: "P2", Files: []string{"FOO/bar.go"}},
		{ID: "P3", Files: []string{"other.go"}},
		{ID: "P4", Files: []string{"later.go"}, DependsOn: []string{"P1"}},
	}}
	got := selectParallelSteps(plan, 3, func(agentflow.Step, []string) bool { return true })
	if want := []string{"P1", "P3"}; !reflect.DeepEqual(stepIDs(got), want) {
		t.Fatalf("selected = %v, want %v", stepIDs(got), want)
	}
}

func TestParallelCoordinatorSelectsOnlyGitSafeCandidates(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*testing.T, string)
		file   string
		commit bool
	}{
		{name: "ignored new", file: "ignored.txt"},
		{name: "directory", file: "dir", setup: func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", file: "link.go", commit: true, setup: func(t *testing.T, root string) {
			if err := os.Symlink("a.go", filepath.Join(root, "link.go")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := newParallelTestRepo(t)
			if tt.setup != nil {
				tt.setup(t, root)
			}
			if tt.commit {
				runTestGit(t, root, "add", "-A")
				runTestGit(t, root, "commit", "-m", "unsafe candidate")
			}
			plan := parallelTestPlan(tt.file, "a.go", "b.go")
			c := newParallelCoordinator(root, plan, 3, nil)
			selected, err := c.selectWorkers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !selected || !reflect.DeepEqual(workerStepIDs(c.workers), []string{"P2", "P3"}) {
				t.Fatalf("selected = %v, workers = %v", selected, workerStepIDs(c.workers))
			}
		})
	}
}

func TestParallelCoordinatorAcceptsTrackedAndNewUnignoredFiles(t *testing.T) {
	root := newParallelTestRepo(t)
	plan := parallelTestPlan("a.go", "new.go", "b.go")
	c := newParallelCoordinator(root, plan, 2, nil)
	selected, err := c.selectWorkers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !selected || !reflect.DeepEqual(workerStepIDs(c.workers), []string{"P1", "P2"}) {
		t.Fatalf("selected = %v, workers = %v", selected, workerStepIDs(c.workers))
	}
}

func TestParallelCoordinatorRejectsNonTopLevelOrDirtyRoot(t *testing.T) {
	root := newParallelTestRepo(t)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := newParallelCoordinator(filepath.Join(root, "sub"), parallelTestPlan("a.go", "b.go", "c.go"), 2, nil).selectWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "toplevel") {
		t.Fatalf("nested root error = %v", err)
	}

	writeTestFile(t, filepath.Join(root, "drift.go"), "dirty\n", 0o600)
	if _, err := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil).selectWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("dirty root error = %v", err)
	}
}

func TestParallelWorktreesShareDetachedBaseAndCopiedAgentTree(t *testing.T) {
	root := newParallelTestRepo(t)
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
	}
	if err := c.prepareWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
	if len(c.workers) != 2 {
		t.Fatalf("workers = %d", len(c.workers))
	}
	for i, worker := range c.workers {
		if got := strings.TrimSpace(runTestGit(t, worker.root, "rev-parse", "HEAD")); got != c.head {
			t.Errorf("worker %d HEAD = %q, want %q", i, got, c.head)
		}
		cmd := exec.Command("git", "symbolic-ref", "--quiet", "HEAD")
		cmd.Dir = worker.root
		if err := cmd.Run(); err == nil {
			t.Errorf("worker %d is not detached", i)
		}
		assertTreesEqual(t, filepath.Join(root, ".agent"), filepath.Join(worker.root, ".agent"))
		if worker.ownerID != "golem-w"+string(rune('1'+i)) || worker.sourceID != "w"+string(rune('1'+i)) {
			t.Errorf("worker IDs = %q/%q", worker.ownerID, worker.sourceID)
		}
	}
}

func TestParallelWorkersRunConcurrentlyAndValidateExactDiff(t *testing.T) {
	root := newParallelTestRepo(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	worker := func(ctx context.Context, w parallelWorker) error {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return os.WriteFile(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), []byte(w.step.ID+"\n"), 0o600)
	}
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, worker)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
	}
	if err := c.prepareWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
	done := make(chan error, 1)
	go func() { done <- c.runWorkers(context.Background()) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("workers did not overlap")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, w := range c.workers {
		if !reflect.DeepEqual(w.changedPaths, w.paths[:1]) {
			t.Errorf("worker %s changed paths = %v, want %v", w.sourceID, w.changedPaths, w.paths[:1])
		}
	}
	if got := readTestFile(t, filepath.Join(root, "a.go")); got != "a\n" {
		t.Fatalf("canonical a.go changed to %q", got)
	}
}

func TestParallelWorkerFailuresAndDriftPreserveCanonicalAndWorktrees(t *testing.T) {
	for _, tt := range []struct {
		name   string
		worker parallelWorkerFunc
		want   string
	}{
		{name: "worker failure", want: "worker failed", worker: func(context.Context, parallelWorker) error {
			return errors.New("worker failed")
		}},
		{name: "unexpected ignored drift", want: "unexpected", worker: func(_ context.Context, w parallelWorker) error {
			return os.WriteFile(filepath.Join(w.root, "ignored.txt"), []byte("drift\n"), 0o600)
		}},
		{name: "assigned symlink result", want: "unsafe", worker: func(_ context.Context, w parallelWorker) error {
			assigned := filepath.Join(w.root, filepath.FromSlash(w.paths[0]))
			if err := os.Remove(assigned); err != nil {
				return err
			}
			return os.Symlink("b.go", assigned)
		}},
		{name: "assigned directory result", want: "unsafe", worker: func(_ context.Context, w parallelWorker) error {
			assigned := filepath.Join(w.root, filepath.FromSlash(w.paths[0]))
			if err := os.Remove(assigned); err != nil {
				return err
			}
			return os.Mkdir(assigned, 0o700)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newParallelTestRepo(t)
			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, tt.worker)
			selected, err := c.selectWorkers(context.Background())
			if err != nil || !selected {
				t.Fatalf("selectWorkers() = %v, %v", selected, err)
			}
			if err := c.prepareWorkers(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runWorkers() error = %v", err)
			}
			if got := readTestFile(t, filepath.Join(root, "a.go")); got != "a\n" {
				t.Fatalf("canonical a.go changed to %q", got)
			}
			for _, preserved := range c.preservedRoots() {
				if _, err := os.Stat(preserved); err != nil {
					t.Errorf("preserved root %s: %v", preserved, err)
				}
			}
			_ = c.cleanup(context.Background())
		})
	}
}

func TestParallelWorkerRejectsNewEmptyDirectoryResult(t *testing.T) {
	root := newParallelTestRepo(t)
	worker := func(_ context.Context, w parallelWorker) error {
		if w.sourceID != "w1" {
			return nil
		}
		return os.Mkdir(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), 0o700)
	}
	c := newParallelCoordinator(root, parallelTestPlan("new.go", "b.go"), 2, worker)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
	}
	if err := c.prepareWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("runWorkers() error = %v", err)
	}
	_ = c.cleanup(context.Background())
}

func TestParallelSetupFailurePreservesCreatedWorktree(t *testing.T) {
	root := newParallelTestRepo(t)
	if err := os.Symlink("state.json", filepath.Join(root, ".agent", "link")); err != nil {
		t.Fatal(err)
	}
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
	}
	if err := c.prepareWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("prepareWorkers() error = %v", err)
	}
	if roots := c.preservedRoots(); len(roots) != 1 {
		t.Fatalf("preserved roots = %v", roots)
	} else if _, err := os.Stat(roots[0]); err != nil {
		t.Fatalf("preserved root: %v", err)
	}
	_ = c.cleanup(context.Background())
}

func newParallelTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runTestGit(t, root, "init")
	writeTestFile(t, filepath.Join(root, ".gitignore"), ".agent/\nignored.txt\nignored-dir/\n", 0o600)
	writeTestFile(t, filepath.Join(root, "a.go"), "a\n", 0o600)
	writeTestFile(t, filepath.Join(root, "b.go"), "b\n", 0o600)
	runTestGit(t, root, "add", "-A")
	runTestGit(t, root, "commit", "-m", "base")
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".agent", "state.json"), "{\"state\":true}\n", 0o640)
	if err := os.Mkdir(filepath.Join(root, ".agent", "receipts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ".agent", "receipts"), os.ModeSticky|0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".agent", "receipts", "one.json"), "{}\n", 0o600)
	return root
}

func parallelTestPlan(files ...string) *agentflow.Plan {
	steps := make([]agentflow.Step, len(files)+1)
	for i, file := range files {
		steps[i] = agentflow.Step{ID: "P" + string(rune('1'+i)), Files: []string{file}}
	}
	steps[len(files)] = agentflow.Step{ID: "PD", Files: []string{"dependent.go"}, DependsOn: []string{"P1"}}
	return &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: steps}
}

func runTestGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=golem-test", "GIT_AUTHOR_EMAIL=golem-test@example.com",
		"GIT_COMMITTER_NAME=golem-test", "GIT_COMMITTER_EMAIL=golem-test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func writeTestFile(t *testing.T, path, contents string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func workerStepIDs(workers []parallelWorker) []string {
	ids := make([]string, len(workers))
	for i := range workers {
		ids[i] = workers[i].step.ID
	}
	return ids
}

func assertTreesEqual(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	err := filepath.WalkDir(wantRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wantRoot, path)
		if err != nil {
			return err
		}
		wantInfo, err := os.Lstat(path)
		if err != nil {
			return err
		}
		gotInfo, err := os.Lstat(filepath.Join(gotRoot, rel))
		if err != nil {
			return err
		}
		if wantInfo.Mode() != gotInfo.Mode() {
			return errors.New("mode mismatch at " + rel)
		}
		if wantInfo.Mode().IsRegular() {
			want, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			got, err := os.ReadFile(filepath.Join(gotRoot, rel))
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(got, want) {
				return errors.New("content mismatch at " + rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func stepIDs(steps []agentflow.Step) []string {
	ids := make([]string, len(steps))
	for i := range steps {
		ids[i] = steps[i].ID
	}
	return ids
}
