package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
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

func TestParallelWorkersRejectUnpreparedCohortBeforeCallbacks(t *testing.T) {
	t.Run("zero workers", func(t *testing.T) {
		var calls atomic.Int32
		c := newParallelCoordinator(t.TempDir(), &agentflow.Plan{}, 2, func(context.Context, parallelWorker) error {
			calls.Add(1)
			return nil
		})
		if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "prepared") {
			t.Fatalf("runWorkers() error = %v", err)
		}
		if calls.Load() != 0 {
			t.Fatalf("worker callback calls = %d", calls.Load())
		}
	})

	t.Run("selected but not prepared", func(t *testing.T) {
		root := newParallelTestRepo(t)
		var calls atomic.Int32
		c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, func(context.Context, parallelWorker) error {
			calls.Add(1)
			return nil
		})
		selected, err := c.selectWorkers(context.Background())
		if err != nil || !selected {
			t.Fatalf("selectWorkers() = %v, %v", selected, err)
		}
		if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "prepared") {
			t.Fatalf("runWorkers() error = %v", err)
		}
		if calls.Load() != 0 {
			t.Fatalf("worker callback calls = %d", calls.Load())
		}
	})

	t.Run("missing prepared root", func(t *testing.T) {
		root := newParallelTestRepo(t)
		var calls atomic.Int32
		c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, func(context.Context, parallelWorker) error {
			calls.Add(1)
			return nil
		})
		selected, err := c.selectWorkers(context.Background())
		if err != nil || !selected {
			t.Fatalf("selectWorkers() = %v, %v", selected, err)
		}
		if err := c.prepareWorkers(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.cleanup(context.Background()) })
		root1 := c.workers[1].root
		c.workers[1].root = filepath.Join(c.tempParent, "missing")
		if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "prepared") {
			t.Fatalf("runWorkers() error = %v", err)
		}
		c.workers[1].root = root1
		if calls.Load() != 0 {
			t.Fatalf("worker callback calls = %d", calls.Load())
		}
	})
}

func TestParallelWorkersRejectChangedHeadOrAttachedBranch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		want   string
		mutate func(parallelWorker) error
	}{
		{
			name: "changed HEAD",
			want: "HEAD",
			mutate: func(w parallelWorker) error {
				if err := os.WriteFile(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), []byte("committed\n"), 0o600); err != nil {
					return err
				}
				if err := runParallelTestGit(w.root, "add", "--", w.paths[0]); err != nil {
					return err
				}
				return runParallelTestGit(w.root, "commit", "-m", "worker commit")
			},
		},
		{
			name: "attached branch",
			want: "detached",
			mutate: func(w parallelWorker) error {
				return runParallelTestGit(w.root, "switch", "-c", "worker-branch")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newParallelTestRepo(t)
			worker := func(_ context.Context, w parallelWorker) error {
				if w.sourceID == "w1" {
					return tt.mutate(w)
				}
				return nil
			}
			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, worker)
			selected, err := c.selectWorkers(context.Background())
			if err != nil || !selected {
				t.Fatalf("selectWorkers() = %v, %v", selected, err)
			}
			if err := c.prepareWorkers(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.cleanup(context.Background()) })
			if err := c.runWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runWorkers() error = %v, want %q", err, tt.want)
			}
		})
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

func TestParallelRunCohortPromotesFilesAndAggregatesInOrder(t *testing.T) {
	root := newParallelTestRepo(t)
	plan := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"nested/created.go", "a.go"}},
		{ID: "P2", Files: []string{"z.go", "b.go"}},
		{ID: "PD", Files: []string{"dependent.go"}, DependsOn: []string{"P1", "P2"}},
	}}
	var active atomic.Int32
	worker := func(_ context.Context, w parallelWorker) error {
		active.Add(1)
		defer active.Add(-1)
		switch w.sourceID {
		case "w1":
			if err := os.MkdirAll(filepath.Join(w.root, "nested"), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(w.root, "nested", "created.go"), []byte("created\n"), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(w.root, "a.go"), []byte("modified\n"), 0o600); err != nil {
				return err
			}
			return os.Chmod(filepath.Join(w.root, "a.go"), 0o750)
		case "w2":
			if err := os.WriteFile(filepath.Join(w.root, "z.go"), []byte("z\n"), 0o640); err != nil {
				return err
			}
			return os.Remove(filepath.Join(w.root, "b.go"))
		default:
			return fmt.Errorf("unexpected worker %s", w.sourceID)
		}
	}
	type aggregateCall struct {
		inputs []agentflow.AggregationInput
		output string
		base   string
		dryRun bool
	}
	var calls []aggregateCall
	c := newParallelCoordinator(root, plan, 2, worker)
	c.aggregate = func(_ context.Context, inputs []agentflow.AggregationInput, output, base string, dryRun bool) (agentflow.AggregationResult, error) {
		if got := active.Load(); got != 0 {
			return agentflow.AggregationResult{}, fmt.Errorf("aggregate called with %d active workers", got)
		}
		calls = append(calls, aggregateCall{inputs: append([]agentflow.AggregationInput(nil), inputs...), output: output, base: base, dryRun: dryRun})
		if got := readTestFile(t, filepath.Join(root, "a.go")); got != "modified\n" {
			t.Fatalf("canonical a.go = %q during aggregation", got)
		}
		return agentflow.AggregationResult{Status: "ok"}, nil
	}

	ran, err := c.runCohort(context.Background())
	if err != nil || !ran {
		t.Fatalf("runCohort() = %v, %v", ran, err)
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
	if roots := c.preservedRoots(); len(roots) != 2 {
		t.Fatalf("preserved roots = %v", roots)
	}
	for _, preserved := range c.preservedRoots() {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatalf("runCohort cleaned worker root %s: %v", preserved, err)
		}
	}
	if want := []aggregateCall{
		{inputs: []agentflow.AggregationInput{{Root: c.workers[0].root, SourceID: "w1"}, {Root: c.workers[1].root, SourceID: "w2"}}, output: c.root, base: c.head, dryRun: true},
		{inputs: []agentflow.AggregationInput{{Root: c.workers[0].root, SourceID: "w1"}, {Root: c.workers[1].root, SourceID: "w2"}}, output: c.root, base: c.head, dryRun: false},
	}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("aggregate calls = %#v, want %#v", calls, want)
	}
	if want := []string{"nested/created.go", "a.go"}; !reflect.DeepEqual(c.workers[0].changedPaths, want) {
		t.Fatalf("w1 changed paths = %v, want %v", c.workers[0].changedPaths, want)
	}
	if want := []string{"z.go", "b.go"}; !reflect.DeepEqual(c.workers[1].changedPaths, want) {
		t.Fatalf("w2 changed paths = %v, want %v", c.workers[1].changedPaths, want)
	}
	if got := readTestFile(t, filepath.Join(root, "nested", "created.go")); got != "created\n" {
		t.Fatalf("created file = %q", got)
	}
	if info, err := os.Stat(filepath.Join(root, "nested", "created.go")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("created mode = %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(root, "a.go")); err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("modified mode = %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "b.go")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("deleted b.go still exists: %v", err)
	}
}

func TestParallelRunCohortRollbackBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name          string
		aggregate     func(bool) error
		wantPromoted  bool
		wantAmbiguous bool
	}{
		{name: "dry failure", aggregate: func(dry bool) error {
			if dry {
				return errors.New("dry failed")
			}
			return nil
		}},
		{name: "dry collision", aggregate: func(dry bool) error {
			if dry {
				return &agentflow.AggregationCollisionError{Result: agentflow.AggregationResult{Status: "collision"}}
			}
			return nil
		}},
		{name: "real collision", aggregate: func(dry bool) error {
			if !dry {
				return &agentflow.AggregationCollisionError{Result: agentflow.AggregationResult{Status: "collision"}}
			}
			return nil
		}},
		{name: "ambiguous real failure", wantPromoted: true, wantAmbiguous: true, aggregate: func(dry bool) error {
			if !dry {
				return errors.New("transport failed")
			}
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newParallelTestRepo(t)
			worker := func(_ context.Context, w parallelWorker) error {
				if w.sourceID == "w1" {
					if err := os.MkdirAll(filepath.Join(w.root, "new", "parent"), 0o700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(w.root, "new", "parent", "created.go"), []byte("created\n"), 0o640)
				}
				if err := os.WriteFile(filepath.Join(w.root, "a.go"), []byte("promoted\n"), 0o600); err != nil {
					return err
				}
				if err := os.Chmod(filepath.Join(w.root, "a.go"), 0o750); err != nil {
					return err
				}
				return os.Remove(filepath.Join(w.root, "b.go"))
			}
			plan := &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
				{ID: "P1", Files: []string{"new/parent/created.go"}},
				{ID: "P2", Files: []string{"a.go", "b.go"}},
				{ID: "PD", Files: []string{"dependent.go"}, DependsOn: []string{"P1", "P2"}},
			}}
			c := newParallelCoordinator(root, plan, 2, worker)
			var dryRuns []bool
			c.aggregate = func(_ context.Context, _ []agentflow.AggregationInput, _, _ string, dry bool) (agentflow.AggregationResult, error) {
				dryRuns = append(dryRuns, dry)
				return agentflow.AggregationResult{Status: "ok"}, tt.aggregate(dry)
			}
			ran, err := c.runCohort(context.Background())
			if err == nil || !ran {
				t.Fatalf("runCohort() = %v, %v", ran, err)
			}
			t.Cleanup(func() { _ = c.cleanup(context.Background()) })
			if tt.wantAmbiguous != strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("runCohort() error = %v, ambiguous=%v", err, tt.wantAmbiguous)
			}
			if tt.wantPromoted {
				if got := readTestFile(t, filepath.Join(root, "a.go")); got != "promoted\n" {
					t.Fatalf("promoted a.go = %q", got)
				}
				if _, err := os.Stat(filepath.Join(root, "new", "parent", "created.go")); err != nil {
					t.Fatalf("promoted new file: %v", err)
				}
				if _, err := os.Lstat(filepath.Join(root, "b.go")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("promoted deletion: %v", err)
				}
			} else {
				if got := readTestFile(t, filepath.Join(root, "a.go")); got != "a\n" {
					t.Fatalf("rolled back a.go = %q", got)
				}
				if _, err := os.Lstat(filepath.Join(root, "new")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("created parent not removed: %v", err)
				}
				if got := readTestFile(t, filepath.Join(root, "b.go")); got != "b\n" {
					t.Fatalf("rolled back b.go = %q", got)
				}
				if info, err := os.Stat(filepath.Join(root, "a.go")); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("rolled back mode = %v, %v", info, err)
				}
			}
			if tt.name == "real collision" || tt.wantAmbiguous {
				if !reflect.DeepEqual(dryRuns, []bool{true, false}) {
					t.Fatalf("aggregate order = %v", dryRuns)
				}
			} else if !reflect.DeepEqual(dryRuns, []bool{true}) {
				t.Fatalf("aggregate order = %v", dryRuns)
			}
		})
	}
}

func TestParallelPromotionFailureRollsBackEarlierWrites(t *testing.T) {
	root := t.TempDir()
	w1 := t.TempDir()
	w2 := t.TempDir()
	writeTestFile(t, filepath.Join(w1, "conflict"), "first\n", 0o600)
	if err := os.Mkdir(filepath.Join(w2, "conflict"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(w2, "conflict", "child.go"), "second\n", 0o600)
	c := &parallelCoordinator{root: root, workers: []parallelWorker{
		{root: w1, paths: []string{"conflict"}, changedPaths: []string{"conflict"}},
		{root: w2, paths: []string{"conflict/child.go"}, changedPaths: []string{"conflict/child.go"}},
	}}
	if _, err := c.promoteWorkers(); err == nil || !strings.Contains(err.Error(), "promote") {
		t.Fatalf("promoteWorkers() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "conflict")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("earlier write not rolled back: %v", err)
	}
}

func TestParallelPromotionRejectsUnsafeStateBeforeWriting(t *testing.T) {
	for _, tt := range []struct {
		name  string
		file  string
		setup func(*testing.T, string, string)
	}{
		{name: "escaping path", file: "../escape.go"},
		{name: "worker symlink", file: "unsafe.go", setup: func(t *testing.T, _, worker string) {
			if err := os.Symlink("target", filepath.Join(worker, "unsafe.go")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "worker directory", file: "unsafe.go", setup: func(t *testing.T, _, worker string) {
			if err := os.Mkdir(filepath.Join(worker, "unsafe.go"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "canonical symlink parent", file: "unsafe/file.go", setup: func(t *testing.T, root, worker string) {
			if err := os.Mkdir(filepath.Join(worker, "unsafe"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(worker, "unsafe", "file.go"), "worker\n", 0o600)
			if err := os.Symlink("elsewhere", filepath.Join(root, "unsafe")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "canonical symlink", file: "unsafe.go", setup: func(t *testing.T, root, worker string) {
			writeTestFile(t, filepath.Join(worker, "unsafe.go"), "worker\n", 0o600)
			if err := os.Symlink("sentinel.go", filepath.Join(root, "unsafe.go")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			worker := t.TempDir()
			writeTestFile(t, filepath.Join(root, "sentinel.go"), "original\n", 0o600)
			if tt.setup != nil {
				tt.setup(t, root, worker)
			}
			c := &parallelCoordinator{root: root, workers: []parallelWorker{{
				root: worker, paths: []string{"sentinel.go", tt.file}, changedPaths: []string{"sentinel.go", tt.file},
			}}}
			writeTestFile(t, filepath.Join(worker, "sentinel.go"), "promoted\n", 0o600)
			if _, err := c.promoteWorkers(); err == nil {
				t.Fatal("promoteWorkers() unexpectedly succeeded")
			}
			if got := readTestFile(t, filepath.Join(root, "sentinel.go")); got != "original\n" {
				t.Fatalf("promotion wrote before validating every path: %q", got)
			}
		})
	}
}

func TestParallelPromotionRejectsSpecialFile(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	root := t.TempDir()
	worker := t.TempDir()
	cmd := exec.Command(mkfifo, filepath.Join(worker, "unsafe.go"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v: %s", err, out)
	}
	c := &parallelCoordinator{root: root, workers: []parallelWorker{{
		root: worker, paths: []string{"unsafe.go"}, changedPaths: []string{"unsafe.go"},
	}}}
	if _, err := c.promoteWorkers(); err == nil || !strings.Contains(err.Error(), "regular") {
		t.Fatalf("promoteWorkers() error = %v", err)
	}
}

func TestParallelRunCohortReportsRollbackFailure(t *testing.T) {
	root := newParallelTestRepo(t)
	worker := func(_ context.Context, w parallelWorker) error {
		return os.WriteFile(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), []byte("promoted\n"), 0o600)
	}
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, worker)
	c.aggregate = func(_ context.Context, _ []agentflow.AggregationInput, _, _ string, dry bool) (agentflow.AggregationResult, error) {
		if dry {
			if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
				return agentflow.AggregationResult{}, err
			}
			if err := os.Mkdir(filepath.Join(root, "a.go"), 0o700); err != nil {
				return agentflow.AggregationResult{}, err
			}
			if err := os.WriteFile(filepath.Join(root, "a.go", "external"), []byte("keep\n"), 0o600); err != nil {
				return agentflow.AggregationResult{}, err
			}
			return agentflow.AggregationResult{}, errors.New("dry failed")
		}
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	_, err := c.runCohort(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dry failed") || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("runCohort() error = %v", err)
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
}

func TestParallelRunCohortRechecksCanonicalBeforePromotion(t *testing.T) {
	for _, tt := range []struct {
		name   string
		want   string
		mutate func(*parallelCoordinator, string) error
	}{
		{name: "dirty", want: "clean", mutate: func(_ *parallelCoordinator, root string) error {
			return os.WriteFile(filepath.Join(root, "drift.go"), []byte("drift\n"), 0o600)
		}},
		{name: "head", want: "HEAD", mutate: func(_ *parallelCoordinator, root string) error {
			return runParallelTestGit(root, "commit", "--allow-empty", "-m", "canonical drift")
		}},
		{name: "toplevel", want: "toplevel", mutate: func(c *parallelCoordinator, _ string) error {
			c.topLevel = filepath.Join(c.topLevel, "changed")
			return nil
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := newParallelTestRepo(t)
			var c *parallelCoordinator
			var mutated atomic.Bool
			worker := func(_ context.Context, w parallelWorker) error {
				if w.sourceID == "w1" && mutated.CompareAndSwap(false, true) {
					if err := tt.mutate(c, root); err != nil {
						return err
					}
				}
				return os.WriteFile(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), []byte("worker\n"), 0o600)
			}
			c = newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, worker)
			var aggregateCalls atomic.Int32
			c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
				aggregateCalls.Add(1)
				return agentflow.AggregationResult{Status: "ok"}, nil
			}
			ran, err := c.runCohort(context.Background())
			if err == nil || !ran || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runCohort() = %v, %v; want %q", ran, err, tt.want)
			}
			if aggregateCalls.Load() != 0 {
				t.Fatalf("aggregate calls = %d", aggregateCalls.Load())
			}
			if got := readTestFile(t, filepath.Join(root, "a.go")); got != "a\n" {
				t.Fatalf("canonical promoted before recheck: %q", got)
			}
			t.Cleanup(func() { _ = c.cleanup(context.Background()) })
		})
	}
}

func TestParallelRunCohortSerialFallbackDoesNothing(t *testing.T) {
	var workerCalls, aggregateCalls atomic.Int32
	c := newParallelCoordinator(t.TempDir(), &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"a.go"}}, {ID: "P2", Files: []string{"b.go"}},
	}}, 2, func(context.Context, parallelWorker) error {
		workerCalls.Add(1)
		return nil
	})
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		aggregateCalls.Add(1)
		return agentflow.AggregationResult{}, nil
	}
	ran, err := c.runCohort(context.Background())
	if err != nil || ran {
		t.Fatalf("runCohort() = %v, %v", ran, err)
	}
	if workerCalls.Load() != 0 || aggregateCalls.Load() != 0 || len(c.preservedRoots()) != 0 {
		t.Fatalf("fallback side effects: worker=%d aggregate=%d roots=%v", workerCalls.Load(), aggregateCalls.Load(), c.preservedRoots())
	}
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

func runParallelTestGit(root string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=golem-test", "GIT_AUTHOR_EMAIL=golem-test@example.com",
		"GIT_COMMITTER_NAME=golem-test", "GIT_COMMITTER_EMAIL=golem-test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, out)
	}
	return nil
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
