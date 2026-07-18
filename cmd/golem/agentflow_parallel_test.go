package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

type assignedWorkerRunner struct {
	calls      [][]string
	nextAction string
}

type aggregationRootRunner struct{ calls [][]string }

func (r *aggregationRootRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) == 0 || args[0] != "aggregate-ledgers" {
		return nil, nil, 0, fmt.Errorf("unexpected Agentflow command %v", args)
	}
	return []byte(`{"status":"ok","sources":[],"collisions":[],"planned":{}}`), nil, 0, nil
}

type concurrentWriteDetector struct {
	active     atomic.Int32
	overlapped atomic.Bool
}

func (w *concurrentWriteDetector) Write(p []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlapped.Store(true)
	}
	time.Sleep(time.Millisecond)
	w.active.Add(-1)
	return len(p), nil
}

func (r *assignedWorkerRunner) Run(_ context.Context, args []string, _ []byte) ([]byte, []byte, int, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) == 0 {
		return nil, nil, 0, errors.New("unexpected empty Agentflow command")
	}
	switch args[0] {
	case "next-action":
		return []byte(r.nextAction), nil, 0, nil
	case "claim-step":
		return []byte(`{"attempt_id":"A1"}`), nil, 0, nil
	case "record-file-change", "run", "finish-step":
		return []byte(`{}`), nil, 0, nil
	default:
		return nil, nil, 0, fmt.Errorf("unexpected Agentflow command %q", args[0])
	}
}

func TestAssignedParallelWorkerRunsOnlyItsOwnedFreshStep(t *testing.T) {
	root := t.TempDir()
	plan := &agentflow.Plan{AllowedFiles: []string{"worker.go"}, Steps: []agentflow.Step{{
		ID: "P2", Files: []string{"worker.go"}, Validation: []string{"unit"},
		Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./worker"}}},
	}}}
	runner := &assignedWorkerRunner{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w2","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`}
	factoryCalls := 0
	sess := &replSession{maxSteps: 2, newOrchestrator: func() *agent.Orchestrator {
		factoryCalls++
		return agent.New(&scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "done"}}}}, agent.ContextManager{})
	}}
	var runnerRoot string
	run := newAssignedParallelWorker(plan, sess, true, io.Discard, func(root string) agentflow.Runner {
		runnerRoot = root
		return runner
	})
	worker := parallelWorker{step: plan.Steps[0], root: root, ownerID: "golem-w2", sourceID: "w2"}
	if err := run(context.Background(), worker); err != nil {
		t.Fatal(err)
	}
	if factoryCalls != 1 || runnerRoot != root {
		t.Fatalf("runtime factories = orchestrator:%d runner-root:%q", factoryCalls, runnerRoot)
	}
	var commands []string
	for _, call := range runner.calls {
		commands = append(commands, call[0])
		if !strings.Contains(strings.Join(call, " "), "--agent golem-w2") {
			t.Fatalf("owned call missing worker agent: %v", call)
		}
	}
	if want := []string{"next-action", "claim-step", "run", "finish-step"}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("Agentflow commands = %v, want %v", commands, want)
	}
	if got := strings.Join(runner.calls[1], " "); !strings.Contains(got, "claim-step P2") {
		t.Fatalf("assigned claim = %q", got)
	}
}

func TestAssignedParallelWorkerRejectsNonFreshProjectionBeforeClaim(t *testing.T) {
	plan := &agentflow.Plan{Steps: []agentflow.Step{{ID: "P1"}}}
	runner := &assignedWorkerRunner{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"other","attempt":null,"diagnostics":[]}}`}
	sess := &replSession{newOrchestrator: func() *agent.Orchestrator {
		return agent.New(&scriptCaller{}, agent.ContextManager{})
	}}
	run := newAssignedParallelWorker(plan, sess, true, io.Discard, func(string) agentflow.Runner { return runner })
	err := run(context.Background(), parallelWorker{step: plan.Steps[0], root: t.TempDir(), ownerID: "golem-w1", sourceID: "w1"})
	if err == nil || !strings.Contains(err.Error(), "resumability agent") {
		t.Fatalf("freshness error = %v", err)
	}
	if len(runner.calls) != 1 || runner.calls[0][0] != "next-action" {
		t.Fatalf("non-fresh worker advanced: %v", runner.calls)
	}
}

func TestAssignedParallelWorkersIsolateRuntimeRootAndJournal(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	plan := &agentflow.Plan{AllowedFiles: []string{"a.go", "b.go"}, Steps: []agentflow.Step{
		{ID: "P1", Files: []string{"a.go"}},
		{ID: "P2", Files: []string{"b.go"}},
	}}
	runners := []*assignedWorkerRunner{
		{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w1","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`},
		{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w2","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`},
	}
	writes := []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "w1", Type: "function", Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{"path":"a.go","content":"one\n"}`)}}}},
		{ToolCalls: []provider.ToolCall{{ID: "w2", Type: "function", Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{"path":"b.go","content":"two\n"}`)}}}},
	}
	factoryCalls := 0
	sess := &replSession{maxSteps: 4, newOrchestrator: func() *agent.Orchestrator {
		response := writes[factoryCalls]
		factoryCalls++
		return agent.New(&scriptCaller{responses: []agent.ModelResult{
			{Response: response}, {Response: provider.ChatResponse{Content: "done"}},
		}}, agent.ContextManager{})
	}}
	var runnerRoots []string
	run := newAssignedParallelWorker(plan, sess, true, io.Discard, func(root string) agentflow.Runner {
		runnerRoots = append(runnerRoots, root)
		if root == roots[0] {
			return runners[0]
		}
		return runners[1]
	})
	for i := range 2 {
		worker := parallelWorker{step: plan.Steps[i], root: roots[i], ownerID: fmt.Sprintf("golem-w%d", i+1), sourceID: fmt.Sprintf("w%d", i+1)}
		if err := run(context.Background(), worker); err != nil {
			t.Fatalf("worker %d: %v", i+1, err)
		}
	}
	if factoryCalls != 2 || !reflect.DeepEqual(runnerRoots, roots) {
		t.Fatalf("isolated factories = orchestrators:%d roots:%v", factoryCalls, runnerRoots)
	}
	for i, file := range []string{"a.go", "b.go"} {
		if _, err := os.Stat(filepath.Join(roots[i], file)); err != nil {
			t.Fatalf("worker %d output: %v", i+1, err)
		}
		other := []string{"b.go", "a.go"}[i]
		if _, err := os.Stat(filepath.Join(roots[i], other)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("worker %d leaked %s into its root: %v", i+1, other, err)
		}
		var recorded bool
		for _, call := range runners[i].calls {
			if call[0] == "record-file-change" {
				recorded = strings.Contains(strings.Join(call, " "), "--path "+file+" --agent golem-w"+fmt.Sprint(i+1))
			}
		}
		if !recorded {
			t.Fatalf("worker %d journal calls = %v", i+1, runners[i].calls)
		}
	}
}

func TestAssignedParallelWorkerFailsClosedWithoutFreshOrchestrator(t *testing.T) {
	plan := &agentflow.Plan{Steps: []agentflow.Step{{ID: "P1"}}}
	for _, tt := range []struct {
		name string
		sess *replSession
		want string
	}{
		{name: "missing factory", sess: &replSession{}, want: "factory is nil"},
		{name: "nil result", sess: &replSession{newOrchestrator: func() *agent.Orchestrator { return nil }}, want: "factory returned nil"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runnerCalls := 0
			run := newAssignedParallelWorker(plan, tt.sess, true, io.Discard, func(string) agentflow.Runner {
				runnerCalls++
				return &assignedWorkerRunner{}
			})
			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Errorf("worker panicked instead of failing closed: %v", recovered)
					}
				}()
				err = run(context.Background(), parallelWorker{step: plan.Steps[0], root: t.TempDir(), ownerID: "golem-w1"})
			}()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("factory error = %v, want %q", err, tt.want)
			}
			if runnerCalls != 0 {
				t.Fatalf("Agentflow runner created after factory failure: %d", runnerCalls)
			}
		})
	}
}

func TestParallelAggregateUsesSuppliedCanonicalOutputRoot(t *testing.T) {
	runner := &aggregationRootRunner{}
	var runnerRoots []string
	aggregate := newParallelAggregate(func(root string) agentflow.Runner {
		runnerRoots = append(runnerRoots, root)
		return runner
	})
	output := filepath.Join(t.TempDir(), "canonical")
	inputs := []agentflow.AggregationInput{{Root: "/worker-1", SourceID: "w1"}}
	if _, err := aggregate(context.Background(), inputs, output, "abc123", true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runnerRoots, []string{output}) {
		t.Fatalf("aggregation runner roots = %v, want %q", runnerRoots, output)
	}
	if got := strings.Join(runner.calls[0], " "); !strings.Contains(got, "--output "+output) {
		t.Fatalf("aggregation argv = %q", got)
	}
}

func TestSynchronizedWriterSerializesWorkerRendering(t *testing.T) {
	detector := &concurrentWriteDetector{}
	writer := &synchronizedWriter{out: detector}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _ = writer.Write([]byte("worker output\n"))
		}()
	}
	close(start)
	workers.Wait()
	if detector.overlapped.Load() {
		t.Fatal("underlying renderer writer received concurrent Write calls")
	}
}

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

func TestParallelCoordinatorFallsBackForTrackedPathsWithHiddenIndexEdits(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			runTestGit(t, root, "update-index", flag, "a.go")
			writeTestFile(t, filepath.Join(root, "a.go"), "hidden caller edit\n", 0o600)
			if got := runTestGit(t, root, "status", "--porcelain=v1"); got != "" {
				t.Fatalf("hidden edit unexpectedly visible to status: %q", got)
			}

			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 3, nil)
			selected, err := c.selectWorkers(context.Background())
			defer func() { _ = c.releaseWorkspaceLock() }()
			if err != nil {
				t.Fatal(err)
			}
			if selected || len(c.workers) != 0 {
				t.Fatalf("selected = %v, workers = %v", selected, workerStepIDs(c.workers))
			}
			if got := readTestFile(t, filepath.Join(root, "a.go")); got != "hidden caller edit\n" {
				t.Fatalf("hidden edit changed to %q", got)
			}
		})
	}
}

func TestParallelCoordinatorFallsBackForMissingTrackedPathsWithHiddenIndexFlags(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			runTestGit(t, root, "update-index", flag, "a.go")
			if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
				t.Fatal(err)
			}
			if got := runTestGit(t, root, "status", "--porcelain=v1"); got != "" {
				t.Fatalf("hidden deletion unexpectedly visible to status: %q", got)
			}

			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 3, nil)
			selected, err := c.selectWorkers(context.Background())
			defer func() { _ = c.releaseWorkspaceLock() }()
			if err != nil {
				t.Fatal(err)
			}
			if selected || len(c.workers) != 0 {
				t.Fatalf("selected = %v, workers = %v", selected, workerStepIDs(c.workers))
			}
		})
	}
}

func TestParallelCoordinatorRecheckRejectsCandidateIndexFlagDrift(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, nil)
			selected, err := c.selectWorkers(context.Background())
			defer func() { _ = c.releaseWorkspaceLock() }()
			if err != nil || !selected {
				t.Fatalf("selectWorkers() = %v, %v", selected, err)
			}

			runTestGit(t, root, "update-index", flag, "a.go")
			if got := runTestGit(t, root, "status", "--porcelain=v1"); got != "" {
				t.Fatalf("index drift unexpectedly visible to status: %q", got)
			}
			if err := c.recheckRoot(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected index flags") {
				t.Fatalf("recheckRoot() error = %v", err)
			}
		})
	}
}

func TestParallelCoordinatorFallsBackForHiddenUnassignedCanonicalEdit(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			writeTestFile(t, filepath.Join(root, "c.go"), "c\n", 0o600)
			runTestGit(t, root, "add", "c.go")
			runTestGit(t, root, "commit", "-m", "add unassigned file")
			runTestGit(t, root, "update-index", flag, "c.go")
			writeTestFile(t, filepath.Join(root, "c.go"), "hidden caller edit\n", 0o600)
			if got := runTestGit(t, root, "status", "--porcelain=v1"); got != "" {
				t.Fatalf("hidden edit unexpectedly visible to status: %q", got)
			}

			c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, nil)
			selected, err := c.selectWorkers(context.Background())
			defer func() { _ = c.releaseWorkspaceLock() }()
			if err != nil || selected || len(c.workers) != 0 {
				t.Fatalf("selectWorkers() = %v, %v", selected, err)
			}
		})
	}
}

func TestParallelCoordinatorExcludesConcurrentWorkspaceOwner(t *testing.T) {
	root := newParallelTestRepo(t)
	first := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil)
	selected, err := first.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("first selectWorkers() = %v, %v", selected, err)
	}
	defer func() { _ = first.releaseWorkspaceLock() }()

	second := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil)
	if _, err := second.selectWorkers(context.Background()); err == nil || !strings.Contains(err.Error(), "already locked") {
		t.Fatalf("second selectWorkers() error = %v", err)
	}
	if err := first.releaseWorkspaceLock(); err != nil {
		t.Fatal(err)
	}
	selected, err = second.selectWorkers(context.Background())
	defer func() { _ = second.releaseWorkspaceLock() }()
	if err != nil || !selected {
		t.Fatalf("second selectWorkers() after release = %v, %v", selected, err)
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

func TestParallelCoordinatorRejectsUnmergedRootInsteadOfFallingBack(t *testing.T) {
	root := newParallelTestRepo(t)
	baseBranch := strings.TrimSpace(runTestGit(t, root, "branch", "--show-current"))
	runTestGit(t, root, "checkout", "-b", "parallel-conflict")
	writeTestFile(t, filepath.Join(root, "a.go"), "branch edit\n", 0o600)
	runTestGit(t, root, "add", "a.go")
	runTestGit(t, root, "commit", "-m", "branch edit")
	runTestGit(t, root, "checkout", baseBranch)
	writeTestFile(t, filepath.Join(root, "a.go"), "root edit\n", 0o600)
	runTestGit(t, root, "add", "a.go")
	runTestGit(t, root, "commit", "-m", "root edit")
	cmd := exec.Command("git", "merge", "parallel-conflict")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=golem-test", "GIT_AUTHOR_EMAIL=golem-test@example.com",
		"GIT_COMMITTER_NAME=golem-test", "GIT_COMMITTER_EMAIL=golem-test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", out)
	} else if unmerged := runTestGit(t, root, "ls-files", "-u", "--", "a.go"); strings.TrimSpace(unmerged) == "" {
		t.Fatalf("git merge did not create an unmerged index: %v\n%s", err, out)
	}

	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, nil)
	selected, err := c.selectWorkers(context.Background())
	defer func() { _ = c.releaseWorkspaceLock() }()
	if err == nil || selected || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
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

func TestParallelWorkersRejectHiddenAssignedEdits(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			worker := func(ctx context.Context, w parallelWorker) error {
				if w.sourceID != "w1" {
					return nil
				}
				if err := os.WriteFile(filepath.Join(w.root, filepath.FromSlash(w.paths[0])), []byte("hidden worker edit\n"), 0o600); err != nil {
					return err
				}
				_, err := runParallelGit(ctx, w.root, "update-index", flag, "--", w.paths[0])
				return err
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

			err = c.runWorkers(context.Background())
			if err == nil || !strings.Contains(err.Error(), "worker w1 assigned path \"a.go\" has unexpected index flags") {
				t.Fatalf("runWorkers() error = %v", err)
			}
		})
	}
}

func TestParallelWorkersRejectHiddenUnassignedEdits(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			root := newParallelTestRepo(t)
			writeTestFile(t, filepath.Join(root, "c.go"), "c\n", 0o600)
			runTestGit(t, root, "add", "c.go")
			runTestGit(t, root, "commit", "-m", "add unassigned file")
			worker := func(ctx context.Context, w parallelWorker) error {
				if w.sourceID != "w1" {
					return nil
				}
				if err := os.WriteFile(filepath.Join(w.root, "c.go"), []byte("hidden gate edit\n"), 0o600); err != nil {
					return err
				}
				_, err := runParallelGit(ctx, w.root, "update-index", flag, "--", "c.go")
				return err
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

			err = c.runWorkers(context.Background())
			if err == nil || !strings.Contains(err.Error(), "worker w1 tracked path \"c.go\" has unexpected index flags") {
				t.Fatalf("runWorkers() error = %v", err)
			}
		})
	}
}

func TestParallelWaveInterruptCancelsEveryWorker(t *testing.T) {
	root := newParallelTestRepo(t)
	started := make(chan struct{}, 2)
	var canceled, aggregateCalls atomic.Int32
	worker := func(ctx context.Context, _ parallelWorker) error {
		started <- struct{}{}
		<-ctx.Done()
		canceled.Add(1)
		return ctx.Err()
	}
	interrupts := make(chan struct{}, 1)
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, worker)
	c.interrupts = interrupts
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		aggregateCalls.Add(1)
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.runCohort(context.Background())
		done <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("parallel workers did not start")
		}
	}
	interrupts <- struct{}{}
	if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("wave interrupt error = %v", err)
	}
	if canceled.Load() != 2 || aggregateCalls.Load() != 0 {
		t.Fatalf("wave cancellation = workers:%d aggregate:%d", canceled.Load(), aggregateCalls.Load())
	}
	if len(c.preservedRoots()) != 2 {
		t.Fatalf("interrupted roots = %v", c.preservedRoots())
	}
	if err := c.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestParallelWaveDrainsOneStaleInterrupt(t *testing.T) {
	root := newParallelTestRepo(t)
	interrupts := make(chan struct{}, 1)
	interrupts <- struct{}{}
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, func(context.Context, parallelWorker) error { return nil })
	c.interrupts = interrupts
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	ran, err := c.runCohort(context.Background())
	if err != nil || !ran {
		t.Fatalf("stale interrupt canceled wave: ran=%v err=%v", ran, err)
	}
	if err := c.cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRunTaskDriverProofGatesParallelCleanup(t *testing.T) {
	prepare := func(t *testing.T) (*parallelCoordinator, []string) {
		t.Helper()
		root := newParallelTestRepo(t)
		c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go"), 2, nil)
		selected, err := c.selectWorkers(context.Background())
		if err != nil || !selected {
			t.Fatalf("select workers = %v, %v", selected, err)
		}
		if err := c.prepareWorkers(context.Background()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.cleanup(context.Background()) })
		return c, append([]string(nil), c.preservedRoots()...)
	}
	newDriver := func(proofErr error) *driver {
		return &driver{
			af: &fakeAF{proofError: proofErr}, plan: &agentflow.Plan{}, planPath: "plan.json", out: io.Discard,
			runStep: func(context.Context, agentflow.Step, string, string) error { return nil },
		}
	}

	t.Run("failure preserves and reports every root", func(t *testing.T) {
		c, roots := prepare(t)
		var out strings.Builder
		if _, err := runTaskDriver(context.Background(), newDriver(errors.New("proof failed")), c, &out); err == nil {
			t.Fatal("expected proof failure")
		}
		for _, root := range roots {
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("root %s was cleaned: %v", root, err)
			}
			if !strings.Contains(out.String(), root) {
				t.Fatalf("preserved root %s not reported: %q", root, out.String())
			}
		}
		if err := c.cleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("verified proof cleans roots", func(t *testing.T) {
		c, roots := prepare(t)
		proof, err := runTaskDriver(context.Background(), newDriver(nil), c, io.Discard)
		if err != nil || proof != "proof-pack.md" {
			t.Fatalf("proof = %q, %v", proof, err)
		}
		for _, root := range roots {
			if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("proved root %s still exists: %v", root, err)
			}
		}
	})

	t.Run("cleanup warning does not invalidate proof", func(t *testing.T) {
		c, roots := prepare(t)
		canonicalRoot := c.root
		c.root = filepath.Join(canonicalRoot, "missing")
		var out strings.Builder
		proof, err := runTaskDriver(context.Background(), newDriver(nil), c, &out)
		if err != nil || proof != "proof-pack.md" {
			t.Fatalf("proof = %q, %v", proof, err)
		}
		if !strings.Contains(out.String(), "warning") {
			t.Fatalf("cleanup warning missing: %q", out.String())
		}
		for _, root := range roots {
			if _, err := os.Stat(root); err != nil {
				t.Fatalf("failed-cleanup root %s missing: %v", root, err)
			}
		}
		c.root = canonicalRoot
		if err := c.cleanup(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
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
	}, baseSnapshots: map[string]parallelFileSnapshot{
		"conflict":          {path: "conflict"},
		"conflict/child.go": {path: "conflict/child.go"},
	}}
	if _, err := c.promoteWorkers(); err == nil || !strings.Contains(err.Error(), "promote") {
		t.Fatalf("promoteWorkers() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "conflict")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("earlier write not rolled back: %v", err)
	}
}

func TestParallelPromotionRefusesCanonicalEditAfterSnapshot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.go")
	writeTestFile(t, target, "original\n", 0o600)
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	promotion := &parallelPromotion{root: rootFS}
	defer func() { _ = promotion.close() }()
	snapshot, err := parallelSnapshot(rootFS, "target.go")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "caller edit\n", 0o600)

	err = promotion.apply(parallelPromotionChange{
		path: "target.go", data: []byte("worker edit\n"), mode: 0o600,
	}, snapshot)
	if err == nil || !strings.Contains(err.Error(), "changed after snapshot") {
		t.Fatalf("apply() error = %v", err)
	}
	if got := readTestFile(t, target); got != "caller edit\n" {
		t.Fatalf("canonical target = %q", got)
	}
}

func TestParallelPromotionUsesPreWorkerCanonicalSnapshot(t *testing.T) {
	root := newParallelTestRepo(t)
	c := newParallelCoordinator(root, parallelTestPlan("a.go", "b.go", "c.go"), 2, nil)
	selected, err := c.selectWorkers(context.Background())
	if err != nil || !selected {
		t.Fatalf("selectWorkers() = %v, %v", selected, err)
	}
	defer func() { _ = c.releaseWorkspaceLock() }()
	if err := c.prepareWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.cleanup(context.Background()) }()
	writeTestFile(t, filepath.Join(c.workers[0].root, "a.go"), "worker edit\n", 0o600)
	c.workers[0].changedPaths = []string{"a.go"}
	writeTestFile(t, filepath.Join(root, "a.go"), "caller edit\n", 0o600)

	if _, err := c.promoteWorkers(); err == nil || !strings.Contains(err.Error(), "changed after snapshot") {
		t.Fatalf("promoteWorkers() error = %v", err)
	}
	if got := readTestFile(t, filepath.Join(root, "a.go")); got != "caller edit\n" {
		t.Fatalf("canonical target = %q", got)
	}
}

func TestParallelRollbackRefusesCanonicalEditAfterPromotion(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.go")
	writeTestFile(t, target, "original\n", 0o600)
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	promotion := &parallelPromotion{root: rootFS}
	defer func() { _ = promotion.close() }()
	snapshot, err := parallelSnapshot(rootFS, "target.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := promotion.apply(parallelPromotionChange{
		path: "target.go", data: []byte("worker edit\n"), mode: 0o600,
	}, snapshot); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, target, "caller edit\n", 0o600)

	err = promotion.rollback()
	if err == nil || !strings.Contains(err.Error(), "refuse to restore changed path") {
		t.Fatalf("rollback() error = %v", err)
	}
	if got := readTestFile(t, target); got != "caller edit\n" {
		t.Fatalf("canonical target = %q", got)
	}
}

func TestParallelPromotionCannotEscapeSwappedParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "owned"), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideTarget := filepath.Join(outside, "target.go")
	writeTestFile(t, outsideTarget, "outside\n", 0o600)
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	promotion := &parallelPromotion{root: rootFS}
	defer func() { _ = promotion.close() }()
	snapshot, err := parallelSnapshot(rootFS, "owned/target.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "owned")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "owned")); err != nil {
		t.Fatal(err)
	}

	err = promotion.apply(parallelPromotionChange{
		path: "owned/target.go", data: []byte("worker edit\n"), mode: 0o600,
	}, snapshot)
	if err == nil {
		t.Fatal("apply() unexpectedly followed swapped parent")
	}
	if got := readTestFile(t, outsideTarget); got != "outside\n" {
		t.Fatalf("outside target = %q", got)
	}
}

func TestParallelPromotionPreMutationFailureDoesNotTrackOrReplaceTarget(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mode   fs.FileMode
		change parallelPromotionChange
	}{
		{name: "write", mode: 0o400, change: parallelPromotionChange{path: "target.go", data: []byte("replacement\n"), mode: 0o600}},
		{name: "delete", mode: 0o640, change: parallelPromotionChange{path: "target.go", delete: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target.go")
			alias := filepath.Join(root, "alias.go")
			writeTestFile(t, target, "original\n", tt.mode)
			if err := os.Link(target, alias); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			before, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, 0o500); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Chmod(root, 0o700) }()
			rootFS, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rootFS.Close() }()
			promotion := &parallelPromotion{root: rootFS}
			err = promotion.apply(tt.change, parallelFileSnapshot{
				path: "target.go", exists: true, data: []byte("original\n"), mode: tt.mode,
			})
			if chmodErr := os.Chmod(root, 0o700); chmodErr != nil {
				t.Fatal(chmodErr)
			}
			if err == nil {
				t.Skipf("filesystem permissions did not induce a pre-mutation %s failure", tt.name)
			}
			if len(promotion.applied) != 0 {
				t.Fatalf("applied snapshots = %d, want 0", len(promotion.applied))
			}
			after, statErr := os.Stat(target)
			if statErr != nil {
				t.Fatal(statErr)
			}
			aliasInfo, statErr := os.Stat(alias)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !os.SameFile(before, after) || !os.SameFile(after, aliasInfo) {
				t.Fatalf("pre-mutation %s failure replaced or severed the original inode", tt.name)
			}
			if got := readTestFile(t, target); got != "original\n" {
				t.Fatalf("target bytes = %q", got)
			}
			if after.Mode().Perm() != tt.mode.Perm() {
				t.Fatalf("target mode = %o", after.Mode().Perm())
			}
		})
	}
}

func TestParallelAtomicCleanupJoinsRemovalFailure(t *testing.T) {
	root := t.TempDir()
	temp, err := os.CreateTemp(root, ".golem-promote-*")
	if err != nil {
		t.Fatal(err)
	}
	name := temp.Name()
	if err := temp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(name, "leftover"), "keep\n", 0o600)
	primary := errors.New("pre-rename failure")
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rootFS.Close() }()
	err = parallelAtomicCleanup(primary, rootFS, temp, filepath.Base(name), true)
	if !errors.Is(err, primary) || !strings.Contains(err.Error(), "remove promotion temp") {
		t.Fatalf("parallelAtomicCleanup() error = %v", err)
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
			rootFS, err := os.OpenRoot(root)
			if err != nil {
				t.Fatal(err)
			}
			sentinel, err := parallelSnapshot(rootFS, "sentinel.go")
			if closeErr := rootFS.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			c.baseSnapshots = map[string]parallelFileSnapshot{
				"sentinel.go": sentinel,
				tt.file:       {path: tt.file},
			}
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

func TestParallelSerialFallbackLeavesNextInterruptForSerialLoop(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)

	for range 100 {
		interrupts := make(chan struct{}, 1)
		c := newParallelCoordinator(t.TempDir(), &agentflow.Plan{AllowedFiles: []string{"*"}, Steps: []agentflow.Step{
			{ID: "P1", Files: []string{"a.go"}}, {ID: "P2", Files: []string{"b.go"}},
		}}, 2, func(context.Context, parallelWorker) error { return nil })
		c.interrupts = interrupts
		ran, err := c.runCohort(context.Background())
		if err != nil || ran {
			t.Fatalf("runCohort() = %v, %v", ran, err)
		}

		interrupts <- struct{}{}
		runtime.Gosched()
		select {
		case <-interrupts:
		default:
			t.Fatal("completed parallel watcher consumed the serial loop interrupt")
		}
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
