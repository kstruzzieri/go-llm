//go:build agentflow_integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestAgentflowSmoke drives the full spine over the fixture with the real CLI to
// a verify-proof pass. Honors AGENTFLOW_SRC when set, otherwise uses an installed
// binary or skips. Asserts driver.run returns a non-empty proof path and no error, the
// fixture gate passes, and the proof pack exists on disk.
func TestAgentflowSmoke(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	gitInit(t, dir) // record-file-change shells out to `git status`; the root must be a repo

	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewClient(runner, dir)

	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}

	runStep := func(ctx context.Context, step agentflow.Step, attempt, _ string) error {
		if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("expected\n"), 0o600); err != nil {
			return err
		}
		return client.RecordFileChange(ctx, step.ID, attempt, "src/answer.txt")
	}

	d := &driver{
		af:        client,
		plan:      &plan,
		planPath:  filepath.Join(dir, "plan.json"),
		taskBrief: agentflow.TaskBriefFromPlan(plan, "feature"),
		runStep:   runStep,
		out:       io.Discard,
	}
	proof, err := d.run(context.Background())
	if err != nil {
		t.Fatalf("driver.run: %v", err)
	}
	t.Logf("proof pack: %s", proof)
	if proof == "" {
		t.Fatal("driver.run returned an empty proof path")
	}
	if _, err := os.Stat(proof); err != nil {
		t.Fatalf("proof pack %s not found on disk: %v", proof, err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "src", "answer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "expected\n" {
		t.Fatalf("src/answer.txt = %q, want %q", got, "expected\n")
	}
}

type parallelSmokeCall struct {
	root          string
	args          []string
	projectedStep string
	promoted      map[string]string
	seq           int
}

type parallelSmokeRecorder struct {
	mu    sync.Mutex
	calls []parallelSmokeCall
}

func (r *parallelSmokeRecorder) add(call parallelSmokeCall) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call.seq = len(r.calls)
	r.calls = append(r.calls, call)
}

func (r *parallelSmokeRecorder) snapshot() []parallelSmokeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]parallelSmokeCall(nil), r.calls...)
}

type parallelSmokeRunner struct {
	root     string
	delegate agentflow.Runner
	recorder *parallelSmokeRecorder
}

func (r *parallelSmokeRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int, error) {
	call := parallelSmokeCall{root: r.root, args: append([]string(nil), args...)}
	if len(args) > 0 && args[0] == "aggregate-ledgers" && slices.Contains(args, "--input") {
		call.promoted = map[string]string{}
		for _, name := range []string{"parallel-one.txt", "parallel-two.txt"} {
			b, err := os.ReadFile(filepath.Join(r.root, "src", name))
			if err != nil {
				call.promoted[name] = err.Error()
				continue
			}
			call.promoted[name] = string(b)
		}
	}
	out, errb, exit, err := r.delegate.Run(ctx, args, stdin)
	if len(args) > 0 && args[0] == "next-action" && err == nil && exit == 0 {
		var state struct {
			Resumability struct {
				Step *struct {
					ID string `json:"id"`
				} `json:"step"`
			} `json:"resumability"`
		}
		if json.Unmarshal(out, &state) == nil && state.Resumability.Step != nil {
			call.projectedStep = state.Resumability.Step.ID
		}
	}
	r.recorder.add(call)
	return out, errb, exit, err
}

type parallelSmokeBarrier struct {
	mu       sync.Mutex
	arrivals int
	ready    chan struct{}
}

func (b *parallelSmokeBarrier) wait(ctx context.Context) error {
	b.mu.Lock()
	b.arrivals++
	if b.arrivals == 2 {
		close(b.ready)
	}
	b.mu.Unlock()
	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *parallelSmokeBarrier) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.arrivals
}

type parallelSmokeCaller struct{ barrier *parallelSmokeBarrier }

func (c parallelSmokeCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	for _, message := range req.Messages {
		if message.Role == "tool" {
			response := provider.ChatResponse{Content: "done"}
			if onToken != nil {
				if err := onToken(response); err != nil {
					return agent.ModelResult{}, err
				}
			}
			return agent.ModelResult{Response: response}, nil
		}
	}
	goal := ""
	for _, message := range req.Messages {
		if message.Role == "user" {
			goal = message.Content
		}
	}
	targets := []struct {
		path    string
		content string
		worker  bool
	}{
		{"src/parallel-one.txt", "worker-one\n", true},
		{"src/parallel-two.txt", "worker-two\n", true},
		{"src/parallel-three.txt", "canonical-three\n", false},
	}
	for _, target := range targets {
		if !strings.Contains(goal, target.path) {
			continue
		}
		if target.worker {
			if err := c.barrier.wait(ctx); err != nil {
				return agent.ModelResult{}, err
			}
		}
		arguments, err := json.Marshal(map[string]string{"path": target.path, "content": target.content})
		if err != nil {
			return agent.ModelResult{}, err
		}
		response := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "write", Type: "function",
			Function: provider.ToolCallFunction{Name: "write_file", Arguments: arguments},
		}}}
		return agent.ModelResult{Response: response}, nil
	}
	return agent.ModelResult{}, fmt.Errorf("parallel smoke caller received unknown goal %q", goal)
}

func TestAgentflowParallelSmoke(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	plan := agentflow.Plan{
		SchemaVersion: "0.3.0", Objective: "prove bounded parallel task execution", Scope: []string{"src"},
		NonGoals: []string{}, Invariants: []string{"only declared files change"}, RiskLevel: "low",
		DriftBudget:  agentflow.DriftBudget{UnrelatedEdits: 0, NewDependencies: 0, FormattingDrift: "minimal", ArchitectureDrift: "requires_approval"},
		AllowedFiles: []string{"src/*", ".agent/"}, BlockedFiles: []string{},
		ValidationGates: []string{"p1", "p2", "p3"}, RollbackPlan: "git checkout -- .", EvidenceIDs: []string{},
		Steps: []agentflow.Step{
			{ID: "P1", Action: "write worker one", Files: []string{"src/parallel-one.txt"}, Preconditions: []string{}, ExpectedDiff: []string{"worker-one"}, Validation: []string{"p1"}, EvidenceIDs: []string{}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"grep", "-qx", "worker-one", "src/parallel-one.txt"}}}},
			{ID: "P2", Action: "write worker two", Files: []string{"src/parallel-two.txt"}, Preconditions: []string{}, ExpectedDiff: []string{"worker-two"}, Validation: []string{"p2"}, EvidenceIDs: []string{}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"grep", "-qx", "worker-two", "src/parallel-two.txt"}}}},
			{ID: "P3", Action: "write canonical three", Files: []string{"src/parallel-three.txt"}, Preconditions: []string{}, ExpectedDiff: []string{"canonical-three"}, Validation: []string{"p3"}, EvidenceIDs: []string{}, DependsOn: []string{"P1", "P2"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"grep", "-qx", "canonical-three", "src/parallel-three.txt"}}}},
		},
	}
	planBytes, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), append(planBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".agent/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"parallel-one.txt", "parallel-two.txt", "parallel-three.txt"} {
		if err := os.WriteFile(filepath.Join(dir, "src", name), []byte("pending\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitInit(t, dir)
	base := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "HEAD"))

	// Skip before goroutines start when neither a source checkout nor installed
	// CLI is available, then create one real runner per production root.
	_ = agentflowRunnerOrSkip(t, dir)
	src := os.Getenv("AGENTFLOW_SRC")
	recorder := &parallelSmokeRecorder{}
	runnerForRoot := func(root string) agentflow.Runner {
		var runner agentflow.Runner
		if src != "" {
			runner = agentflow.NewSrcExecRunner(root, src)
		} else {
			runner = agentflow.NewExecRunner(root)
		}
		return &parallelSmokeRunner{root: root, delegate: runner, recorder: recorder}
	}
	client := agentflow.NewClient(runnerForRoot(dir), dir)
	barrier := &parallelSmokeBarrier{ready: make(chan struct{})}
	newOrchestrator := func() *agent.Orchestrator {
		return agent.New(parallelSmokeCaller{barrier: barrier}, agent.ContextManager{})
	}
	sess := &replSession{orch: newOrchestrator(), newOrchestrator: newOrchestrator, maxSteps: 4, clock: time.Now}
	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runStep, err := newTaskStepRunner(dir, &plan, client, sess.orch, sess, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := newParallelCoordinator(dir, &plan, 2, newAssignedParallelWorker(&plan, sess, true, io.Discard, runnerForRoot))
	coordinator.aggregate = newParallelAggregate(runnerForRoot)
	d := &driver{
		af: client, plan: &plan, planPath: filepath.Join(dir, "plan.json"), taskBrief: agentflow.TaskBriefFromPlan(plan, "feature"),
		runStep: runStep, parallelCohort: func(ctx context.Context) error {
			ran, err := coordinator.runCohort(ctx)
			if err == nil && !ran {
				return fmt.Errorf("parallel cohort fell back to serial")
			}
			return err
		}, out: io.Discard,
	}
	var stderr bytes.Buffer
	proof, err := runTaskDriver(runCtx, d, coordinator, &stderr)
	if err != nil {
		t.Fatalf("parallel driver: %v\n%s", err, stderr.String())
	}

	wantBytes := map[string]string{
		"parallel-one.txt": "worker-one\n", "parallel-two.txt": "worker-two\n", "parallel-three.txt": "canonical-three\n",
	}
	for name, want := range wantBytes {
		got, err := os.ReadFile(filepath.Join(dir, "src", name))
		if err != nil || string(got) != want {
			t.Fatalf("src/%s = %q, %v; want %q", name, got, err, want)
		}
	}
	proofBytes, err := os.ReadFile(proof)
	if err != nil || len(bytes.TrimSpace(proofBytes)) == 0 {
		t.Fatalf("verified proof %q is empty or missing: %v", proof, err)
	}
	var proofState struct {
		Aggregation *struct {
			SchemaVersion string `json:"schema_version"`
			Mode          string `json:"mode"`
			SourceCount   int    `json:"source_count"`
			Sources       []struct {
				SourceID         string  `json:"source_id"`
				BaseCommit       *string `json:"base_commit"`
				HeadCommit       *string `json:"head_commit"`
				NamespacedPrefix string  `json:"namespaced_prefix"`
			} `json:"sources"`
		} `json:"aggregation"`
	}
	if err := json.Unmarshal(proofBytes, &proofState); err != nil {
		t.Fatal(err)
	}
	if proofState.Aggregation == nil || proofState.Aggregation.SchemaVersion != "0.1.0" || proofState.Aggregation.Mode != "cross_worktree" || proofState.Aggregation.SourceCount != 2 {
		t.Fatalf("aggregation provenance = %#v", proofState.Aggregation)
	}
	provenance := map[string]string{}
	for _, source := range proofState.Aggregation.Sources {
		if source.BaseCommit == nil || source.HeadCommit == nil || *source.BaseCommit != base || *source.HeadCommit != base {
			t.Fatalf("source %s commits = %v/%v, want %s", source.SourceID, source.BaseCommit, source.HeadCommit, base)
		}
		provenance[source.SourceID] = source.NamespacedPrefix
	}
	if provenance["w1"] != "WTw1-" || provenance["w2"] != "WTw2-" || len(provenance) != 2 {
		t.Fatalf("aggregation sources = %v", provenance)
	}

	calls := recorder.snapshot()
	claims := map[string]parallelSmokeCall{}
	projections := map[string]string{}
	var aggregates []parallelSmokeCall
	var finishes []parallelSmokeCall
	for _, call := range calls {
		if len(call.args) == 0 {
			continue
		}
		switch call.args[0] {
		case "claim-step":
			if len(call.args) < 2 {
				t.Fatalf("claim-step argv = %v, want step id", call.args)
			}
			claims[call.args[1]] = call
		case "next-action":
			projections[call.root] = call.projectedStep
		case "aggregate-ledgers":
			if slices.Contains(call.args, "--input") {
				aggregates = append(aggregates, call)
			}
		case "finish-run":
			if slices.Contains(call.args, "--root") {
				finishes = append(finishes, call)
			}
		}
	}
	if claims["P1"].root == "" || claims["P1"].root == claims["P2"].root || claims["P1"].root == dir || claims["P2"].root == dir {
		t.Fatalf("worker claim roots = P1:%q P2:%q canonical:%q", claims["P1"].root, claims["P2"].root, dir)
	}
	if got := barrier.count(); got != 2 {
		t.Fatalf("parallel worker barrier arrivals = %d, want 2", got)
	}
	if len(aggregates) != 2 || !slices.Contains(aggregates[0].args, "--dry-run") || slices.Contains(aggregates[1].args, "--dry-run") {
		t.Fatalf("aggregate calls = %v", aggregates)
	}
	if claims["P3"].root != dir || claims["P3"].seq < aggregates[1].seq {
		t.Fatalf("dependent claim = root:%q seq:%d; aggregates:%v", claims["P3"].root, claims["P3"].seq, aggregates)
	}
	if projections[claims["P2"].root] != "P1" {
		t.Fatalf("P2 worker advisory projection = %q, want P1 while assigned P2", projections[claims["P2"].root])
	}
	for _, aggregate := range aggregates {
		if aggregate.promoted["parallel-one.txt"] != wantBytes["parallel-one.txt"] || aggregate.promoted["parallel-two.txt"] != wantBytes["parallel-two.txt"] {
			t.Fatalf("source was not promoted before aggregation: %v", aggregate.promoted)
		}
	}
	if len(finishes) != 1 || finishes[0].root != dir {
		t.Fatalf("finish-run calls = %v", finishes)
	}
	for _, root := range []string{claims["P1"].root, claims["P2"].root} {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("successful worker root %q was not cleaned: %v", root, err)
		}
	}
	if roots := coordinator.preservedRoots(); len(roots) != 0 {
		t.Fatalf("successful coordinator preserved roots: %v", roots)
	}
}

// agentflowRunnerOrSkip honors the explicit AGENTFLOW_SRC checkout, otherwise
// uses an installed binary or skips. Mirrors
// agentflow.agentflowRunnerForTest, which is unexported in another package.
func agentflowRunnerOrSkip(t *testing.T, dir string) agentflow.Runner {
	t.Helper()
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
		return agentflow.NewSrcExecRunner(dir, src)
	}
	if _, err := exec.LookPath("agentflow"); err == nil {
		return agentflow.NewExecRunner(dir)
	}
	t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	return nil
}

// gitInit initializes a git repository at dir and commits the fixture as it
// stands. AgentFlow's record-file-change shells out to `git status`
// unconditionally, so the copy needs to be a repo; finish-run's drift audit
// additionally diffs against that baseline commit, so plan.json and the
// fixture's starting files must already be committed or drift audit flags
// them as out-of-scope changes (everything AgentFlow itself writes under
// .agent/ is separately exempted via the plan's own allowed_files entry).
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=agentflow-smoke", "GIT_AUTHOR_EMAIL=agentflow-smoke@example.com",
			"GIT_COMMITTER_NAME=agentflow-smoke", "GIT_COMMITTER_EMAIL=agentflow-smoke@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("add", "-A")
	run("commit", "-m", "fixture baseline")
}

// copyTree recursively copies the directory tree rooted at src into dst,
// preserving the subdirectory structure.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
