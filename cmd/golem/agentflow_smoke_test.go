//go:build agentflow_integration

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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

func TestAgentflowResumeStatusAndProof_RealCLI(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewOwnedClient(runner, dir, "golem")
	ctx := context.Background()
	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}
	plan.Objective = "resume café <>& 😀"
	planBytes, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var extendedPlan map[string]any
	if err := json.Unmarshal(planBytes, &extendedPlan); err != nil {
		t.Fatal(err)
	}
	extendedPlan["future_numeric"] = json.RawMessage(`1e400`)
	planBytes, err = json.Marshal(extendedPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan.json"), planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir)
	recommendation, err := client.RecommendWorkflow(ctx, agentflow.TaskBriefFromPlan(plan, "feature"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.LockPlan(ctx, filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	if err := client.MaterializeWorkflowContract(ctx, recommendation); err != nil {
		t.Fatal(err)
	}
	if err := client.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := client.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("expected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.RecordFileChange(ctx, "P1", attempt, "src/answer.txt"); err != nil {
		t.Fatal(err)
	}

	state, err := client.NextAction(ctx)
	if err != nil || state.State != "validation_missing" {
		t.Fatalf("state=%q err=%v", state.State, err)
	}
	wantPlanDigest, err := canonicalPlanJSONSHA256(planBytes)
	if err != nil {
		t.Fatal(err)
	}
	if state.Resumability == nil || state.Resumability.Contract == nil || state.Resumability.Contract.PlanSHA256 != wantPlanDigest {
		t.Fatalf("plan binding = %+v, want %s", state.Resumability, wantPlanDigest)
	}
	executionBytes, err := os.ReadFile(filepath.Join(dir, ".agent", "execution.contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256(executionBytes)); state.Resumability.Contract.ExecutionContractSHA256 != want {
		t.Fatalf("execution binding = %s, want %s", state.Resumability.Contract.ExecutionContractSHA256, want)
	}

	before := snapshotSmokeAgentTree(t, dir)
	var status bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(ctx, &status, dir, false, runner)
	var exit *agentflowStatusExit
	if !errors.As(statusErr, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("status error = %v", statusErr)
	}
	if after := snapshotSmokeAgentTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("read-only status mutated .agent")
	}
	if !strings.Contains(status.String(), "state: validation_missing") || !strings.Contains(status.String(), "advisory (display only):") {
		t.Fatalf("status output = %s", status.String())
	}

	d := &driver{
		af: client, plan: &plan,
		runStep: func(context.Context, agentflow.Step, string, string) error {
			return errors.New("resume reran an already-started model step")
		},
	}
	final, err := d.resume(ctx, dir, planBytes, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("resume state=%q err=%v", final.State, err)
	}
	summary, err := client.ProofSummary(ctx)
	if err != nil || summary.Total == 0 || summary.Failed != 0 {
		t.Fatalf("proof summary = %+v, err=%v", summary, err)
	}
	out, errOut, exitCode, runErr := runner.Run(ctx, []string{"verify-proof", "--root", dir}, nil)
	if runErr != nil || exitCode != 0 {
		t.Fatalf("verify-proof: exit=%d err=%v stdout=%s stderr=%s", exitCode, runErr, out, errOut)
	}

	if n := commandReceiptCount(t, dir); n != 1 {
		t.Fatalf("command receipts = %d, want exactly one", n)
	}
	stepRuns, err := os.ReadFile(filepath.Join(dir, ".agent", "step-runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	claims := 0
	for _, line := range strings.Split(strings.TrimSpace(string(stepRuns)), "\n") {
		var event struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "claimed" {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("claim events = %d, want exactly one", claims)
	}
	// R5 must accept the ledger rows a real AgentFlow 1.0 run leaves behind.
	if err := checkAgentflowStateMajor(dir); err != nil {
		t.Fatalf("guard refused a real AgentFlow 1.0 tree: %v", err)
	}
}

// #651: a fresh -plan over a workspace whose only step holds an attempt left
// open by a failed gate must not report success. next-step skips the open
// step and a non-strict finish-run would pass (agentflow#58), so before the
// fix this printed a proof pack recording steps_completed 0 of 1.
func TestAgentflowFreshPlanRefusesOpenStepWork_RealCLI(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	gitInit(t, dir)
	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewClient(runner, dir)
	ctx := context.Background()
	planPath := filepath.Join(dir, "plan.json")
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}

	// Run 1: the step writes the wrong token, so its gate fails and the
	// attempt stays open.
	first := &driver{
		af: client, plan: &plan, planPath: planPath,
		taskBrief: agentflow.TaskBriefFromPlan(plan, "feature"), out: io.Discard,
		runStep: func(ctx context.Context, step agentflow.Step, attempt, _ string) error {
			if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("wrong\n"), 0o600); err != nil {
				return err
			}
			return client.RecordFileChange(ctx, step.ID, attempt, "src/answer.txt")
		},
	}
	if _, err := first.run(ctx); err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("run 1 error = %v, want the failed gate", err)
	}
	if state, err := client.NextAction(ctx); err != nil || state.State != "validation_missing" {
		t.Fatalf("after run 1: state=%q err=%v, want an open attempt in validation_missing", state.State, err)
	}

	// Run 2: a fresh -plan through the production entry. The recording caller
	// proves the model is never asked; an empty scriptCaller alone answers.
	caller := &recordingCaller{next: &scriptCaller{}}
	sess := &replSession{orch: agent.New(caller, agent.ContextManager{}), maxSteps: 4, clock: time.Now}
	var stdout, stderr bytes.Buffer
	err = runAgentflowTask(ctx, &stdout, &stderr, nil, sess, flags{
		planPath: planPath, approveEdits: true, approveGates: true,
		agentflowSrc: os.Getenv("AGENTFLOW_SRC"),
	}, dir)
	if !errors.Is(err, errAgentflowTaskFailed) {
		t.Fatalf("run 2 error = %v, want errAgentflowTaskFailed\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if len(caller.reqs) != 0 {
		t.Fatalf("run 2 called the model %d times", len(caller.reqs))
	}
	if strings.Contains(stdout.String(), "proof pack:") {
		t.Fatalf("run 2 reported success:\n%s", stdout.String())
	}
	// The gate failed, which resume cannot settle yet (#652), so the refusal
	// must not send the user to -agentflow-resume.
	_, refusal, _ := strings.Cut(stderr.String(), "agentflow task failed: ")
	refusal, _, _ = strings.Cut(refusal, "\n")
	for _, want := range []string{"still reports step work before finish-run", `state \"validation_missing\" step \"P1\"`} {
		if !strings.Contains(refusal, want) {
			t.Fatalf("run 2 refusal %q does not mention %s\nstderr:\n%s", refusal, want, stderr.String())
		}
	}
	if !strings.HasSuffix(refusal, "; inspect with -agentflow-status") {
		t.Fatalf("run 2 refusal %q, want only the -agentflow-status hint", refusal)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent", "proof-pack.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("proof-pack.json stat err = %v, want it never built", err)
	}
}

// #612 R-new: AgentFlow 1.0 rejects a 0.x plan before it looks at execution
// state, and Golem status reports that as state_invalid without touching
// .agent/. Only the early old-plan rejection is covered here; the
// retained-state guard's artifact coverage is U5's job.
func TestAgentflowStatusReportsZeroXState_RealCLI(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewOwnedClient(runner, dir, "golem")
	ctx := context.Background()
	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}
	gitInit(t, dir)
	recommendation, err := client.RecommendWorkflow(ctx, agentflow.TaskBriefFromPlan(plan, "feature"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.LockPlan(ctx, filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	if err := client.MaterializeWorkflowContract(ctx, recommendation); err != nil {
		t.Fatal(err)
	}
	if err := client.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := client.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("expected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.RecordFileChange(ctx, "P1", attempt, "src/answer.txt"); err != nil {
		t.Fatal(err)
	}
	for name, version := range map[string]string{"plan.lock.json": "0.4.0", "execution.contract.json": "0.3.0"} {
		path := filepath.Join(dir, ".agent", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		doc["schema_version"] = json.RawMessage(strconv.Quote(version))
		if b, err = json.Marshal(doc); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	before := snapshotSmokeAgentTree(t, dir)
	var status bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(ctx, &status, dir, false, runner)
	var exit *agentflowStatusExit
	if !errors.As(statusErr, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("status error = %v\n%s", statusErr, status.String())
	}
	if !strings.Contains(status.String(), "state: state_invalid") || !strings.Contains(status.String(), "incompatible with supported 1.0.0") {
		t.Fatalf("status output = %s", status.String())
	}
	if after := snapshotSmokeAgentTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatal("status mutated a 0.x .agent/ tree")
	}
}

func TestAgentflowResumeRefusesFiniteEnforcedRecovery_RealCLI(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	gitInit(t, dir)
	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewOwnedClient(runner, dir, "golem")
	ctx := context.Background()
	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}
	recommendation, err := client.RecommendWorkflow(ctx, agentflow.TaskBriefFromPlan(plan, "feature"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.LockPlan(ctx, filepath.Join(dir, "plan.json")); err != nil {
		t.Fatal(err)
	}
	if err := client.MaterializeWorkflowContract(ctx, recommendation); err != nil {
		t.Fatal(err)
	}
	if err := client.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}

	contractPath := filepath.Join(dir, ".agent", "execution.contract.json")
	contractBytes, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var contract map[string]any
	if err := json.Unmarshal(contractBytes, &contract); err != nil {
		t.Fatal(err)
	}
	concurrency := contract["concurrency"].(map[string]any)
	concurrency["lease_policy"] = "enforce"
	concurrency["lease_ttl_minutes"] = 1
	concurrency["lease_grace_seconds"] = 0
	contractBytes, err = json.MarshalIndent(contract, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contractPath, append(contractBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	attempt, err := client.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("expected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.RecordFileChange(ctx, "P1", attempt, "src/answer.txt"); err != nil {
		t.Fatal(err)
	}
	state, err := client.NextAction(ctx)
	if err != nil || state.State != "validation_missing" || state.Resumability == nil || state.Resumability.Lease == nil {
		t.Fatalf("state=%q projection=%+v err=%v", state.State, state.Resumability, err)
	}
	if state.Resumability.Lease.Policy == nil || *state.Resumability.Lease.Policy != "enforce" || state.Resumability.Lease.State != "live" {
		t.Fatalf("lease = %+v, want finite enforced live lease", state.Resumability.Lease)
	}

	beforeRuns, err := os.ReadFile(filepath.Join(dir, ".agent", "step-runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	beforeReceipts, err := os.ReadFile(filepath.Join(dir, ".agent", "command-receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var status bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(ctx, &status, dir, false, runner)
	var statusExit *agentflowStatusExit
	if !errors.As(statusErr, &statusExit) || statusExit.ExitCode() != 3 || !strings.Contains(status.String(), "resume: blocked") {
		t.Fatalf("short enforced lease status = %v\n%s", statusErr, status.String())
	}
	d := &driver{af: client, plan: &plan}
	if _, err := d.resume(ctx, dir, planBytes, nil); err == nil || !strings.Contains(err.Error(), "finite enforced lease") {
		t.Fatalf("resume error = %v, want finite enforced lease refusal", err)
	}
	afterRuns, err := os.ReadFile(filepath.Join(dir, ".agent", "step-runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	afterReceipts, err := os.ReadFile(filepath.Join(dir, ".agent", "command-receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRuns, beforeRuns) || !bytes.Equal(afterReceipts, beforeReceipts) || bytes.Contains(afterRuns, []byte(`"event": "lease_renewed"`)) {
		t.Fatalf("resume mutated enforced short-lease state\nstep runs before=%s\nafter=%s\nreceipts before=%s\nafter=%s", beforeRuns, afterRuns, beforeReceipts, afterReceipts)
	}
}

func snapshotSmokeAgentTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, ".agent"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		snapshot[rel] = fmt.Sprintf("%04o:%x", info.Mode().Perm(), sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
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
	dir, plan, base := writeParallelSmokeFixture(t)

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

	assertParallelSmokeProof(t, dir, proof, base)
	// Aggregated worker rows are real AgentFlow 1.0 ledger rows too (R5).
	if err := checkAgentflowStateMajor(dir); err != nil {
		t.Fatalf("guard refused a real AgentFlow 1.0 tree: %v", err)
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
		if aggregate.promoted["parallel-one.txt"] != parallelSmokeWant["parallel-one.txt"] || aggregate.promoted["parallel-two.txt"] != parallelSmokeWant["parallel-two.txt"] {
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

// parallelSmokeWant is the content each parallel fixture step writes under src/.
var parallelSmokeWant = map[string]string{
	"parallel-one.txt": "worker-one\n", "parallel-two.txt": "worker-two\n", "parallel-three.txt": "canonical-three\n",
}

// writeParallelSmokeFixture builds a committed repo whose plan has two
// independent steps (P1, P2) and a dependent P3, and returns the root, the plan,
// and the baseline commit.
func writeParallelSmokeFixture(t *testing.T) (string, agentflow.Plan, string) {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	plan := agentflow.Plan{
		SchemaVersion: "1.0.0", Objective: "prove bounded parallel task execution", Scope: []string{"src"},
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
	return dir, plan, base
}

// assertParallelSmokeProof checks that every fixture step's bytes landed in dir
// and that the verified proof aggregated exactly the two worker ledgers across
// worktrees, both pinned to base.
func assertParallelSmokeProof(t *testing.T, dir, proof, base string) {
	t.Helper()
	for name, want := range parallelSmokeWant {
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
}

// agentflowRunnerOrSkip honors GO_LLM_REQUIRE_AGENTFLOW and the explicit
// AGENTFLOW_SRC checkout, otherwise uses an installed binary or skips. The
// chosen CLI must pass the AgentFlow 1.x version gate: a non-1.x install skips
// (or fails under GO_LLM_REQUIRE_AGENTFLOW) instead of failing on a raw schema
// rejection (#612). Mirrors agentflow.agentflowRunnerForTest, which is
// unexported in another package. CI's agentflow-compat job selects real-CLI
// tests by name, so a new test using this must be named Test*_RealCLI or
// Test*_RealCLI_<scenario>.
func agentflowRunnerOrSkip(t *testing.T, dir string) agentflow.Runner {
	t.Helper()
	mode, src := os.Getenv("GO_LLM_REQUIRE_AGENTFLOW"), os.Getenv("AGENTFLOW_SRC")
	_, lookErr := exec.LookPath("agentflow")
	installed := lookErr == nil
	switch mode {
	case "":
	case "installed":
		if src != "" {
			t.Fatal("GO_LLM_REQUIRE_AGENTFLOW=installed but AGENTFLOW_SRC is set")
		}
		if !installed {
			t.Fatal("GO_LLM_REQUIRE_AGENTFLOW=installed but agentflow is not on PATH")
		}
	case "source":
		if src == "" {
			t.Fatal("GO_LLM_REQUIRE_AGENTFLOW=source but AGENTFLOW_SRC is empty")
		}
	default:
		t.Fatalf("GO_LLM_REQUIRE_AGENTFLOW=%q, want installed or source", mode)
	}
	var runner agentflow.Runner
	switch {
	case src != "":
		runner = agentflow.NewSrcExecRunner(dir, src)
	case installed:
		runner = agentflow.NewExecRunner(dir)
	default:
		t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	}
	if err := agentflow.NewClient(runner, dir).CheckVersion(context.Background()); err != nil {
		if mode != "" {
			t.Fatalf("agentflow is not usable for the real-CLI tests: %v", err)
		}
		t.Skipf("agentflow is not AgentFlow 1.x (%v); install AgentFlow 1.x or set AGENTFLOW_SRC=<1.x checkout>", err)
	}
	return runner
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

// stepRunEvent is the subset of an AgentFlow step-runs.jsonl event the #611
// tests read.
type stepRunEvent struct {
	Event     string `json:"event"`
	AttemptID string `json:"attempt_id"`
	Reason    string `json:"reason"`
}

func readStepRunEvents(t *testing.T, dir string) []stepRunEvent {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".agent", "step-runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var events []stepRunEvent
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var event stepRunEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

func lastStepRunEvent(events []stepRunEvent, attempt string) stepRunEvent {
	var last stepRunEvent
	for _, event := range events {
		if event.AttemptID == attempt {
			last = event
		}
	}
	return last
}

// commandReceiptCount counts the command-receipt ledger's rows. init-execution
// always creates the ledger, so a missing file fails the test instead of
// reading as zero receipts.
func commandReceiptCount(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, ".agent", "command-receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(b)), "\n"))
}

// runStoppedSmokeStep drives the smoke fixture's first task run with a model
// that writes the step's file and then hits the step cap (#611). Without the
// fix this run would pass its grep gate and complete the step.
func runStoppedSmokeStep(t *testing.T) (string, agentflow.Runner, agentflow.Plan, []byte) {
	t.Helper()
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	gitInit(t, dir)
	runner := agentflowRunnerOrSkip(t, dir)
	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}
	client := agentflow.NewClient(runner, dir)
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	orch := agent.New(&scriptCaller{responses: []agent.ModelResult{
		toolStep("w1", "write_file", `{"path":"src/answer.txt","content":"expected\n"}`),
	}}, agent.ContextManager{})
	runStep, err := newTaskStepRunner(dir, &plan, client, orch, &replSession{maxSteps: 1}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{
		af: client, plan: &plan, planPath: filepath.Join(dir, "plan.json"),
		taskBrief: agentflow.TaskBriefFromPlan(plan, "feature"), runStep: runStep, out: io.Discard,
	}
	_, err = d.run(runCtx)
	if want := "step P1 attempt A1: agent run stopped: step_cap_reached; attempt recorded as blocked"; err == nil || err.Error() != want {
		t.Fatalf("first run error = %v, want %q", err, want)
	}
	if got := lastStepRunEvent(readStepRunEvents(t, dir), "A1"); got.Event != "blocked" || got.Reason != "golem: agent run stopped: step_cap_reached" {
		t.Fatalf("A1 last event = %+v, want blocked with the Golem reason", got)
	}
	if n := commandReceiptCount(t, dir); n != 0 {
		t.Fatalf("command receipts = %d, want 0: a gate ran before the block", n)
	}
	return dir, runner, plan, planBytes
}

func TestAgentflowStoppedStepIsBlockedAndResumes_RealCLI(t *testing.T) {
	dir, runner, plan, planBytes := runStoppedSmokeStep(t)
	ctx := context.Background()

	var status bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(ctx, &status, dir, false, runner)
	var exit *agentflowStatusExit
	if !errors.As(statusErr, &exit) || exit.ExitCode() != 2 || !strings.Contains(status.String(), "state: step_unclaimed") {
		t.Fatalf("status = %v\n%s", statusErr, status.String())
	}

	client := agentflow.NewOwnedClient(runner, dir, "golem")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	orch := agent.New(&scriptCaller{responses: []agent.ModelResult{
		toolStep("w2", "write_file", `{"path":"src/answer.txt","content":"expected\n"}`),
		answerStep("done"),
	}}, agent.ContextManager{})
	runStep, err := newTaskStepRunner(dir, &plan, client, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: client, plan: &plan, runStep: runStep}
	final, err := d.resume(runCtx, dir, planBytes, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("resume state=%q err=%v", final.State, err)
	}
	summary, err := client.ProofSummary(ctx)
	if err != nil || summary.Total == 0 || summary.Failed != 0 {
		t.Fatalf("proof summary = %+v, err=%v", summary, err)
	}
	events := readStepRunEvents(t, dir)
	claims := 0
	for _, event := range events {
		if event.Event == "claimed" {
			claims++
		}
	}
	if claims != 2 || lastStepRunEvent(events, "A2").Event != "completed" {
		t.Fatalf("claims=%d A2=%+v, want two claims and A2 completed", claims, lastStepRunEvent(events, "A2"))
	}
	if n := commandReceiptCount(t, dir); n != 1 {
		t.Fatalf("command receipts = %d, want exactly the resumed gate", n)
	}
}

func TestAgentflowStoppedStepPartialEditFailsClosed_RealCLI(t *testing.T) {
	dir, runner, plan, planBytes := runStoppedSmokeStep(t)
	ctx := context.Background()

	client := agentflow.NewOwnedClient(runner, dir, "golem")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	orch := agent.New(&scriptCaller{responses: []agent.ModelResult{answerStep("already done")}}, agent.ContextManager{})
	runStep, err := newTaskStepRunner(dir, &plan, client, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: client, plan: &plan, runStep: runStep}
	if _, err := d.resume(runCtx, dir, planBytes, nil); err == nil || !strings.Contains(err.Error(), "reached state \"file_receipts_missing\" before gates") {
		t.Fatalf("want the before-gates refusal in state file_receipts_missing, got resume err = %v", err)
	}
	if got := lastStepRunEvent(readStepRunEvents(t, dir), "A2").Event; got != "claimed" {
		t.Fatalf("A2 last event = %q after the refusal, want claimed: the refused attempt stays open", got)
	}
	if n := commandReceiptCount(t, dir); n != 0 {
		t.Fatalf("command receipts = %d, want no gate after the inherited edit", n)
	}
	var status bytes.Buffer
	statusErr := runAgentflowStatusWithRunner(ctx, &status, dir, false, runner)
	var exit *agentflowStatusExit
	if !errors.As(statusErr, &exit) || exit.ExitCode() != 3 || !strings.Contains(status.String(), "state: file_receipts_missing") {
		t.Fatalf("status = %v\n%s", statusErr, status.String())
	}

	// The documented remedy: A2 is still open, so restoring alone would let a
	// resume settle A2 on its gates. Block A2, restore the inherited edit, then
	// resume into a fresh A3.
	if err := client.BlockStep(ctx, "P1", "A2", "operator: discard inherited partial edit"); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "checkout", "--", "src/answer.txt")
	if got := lastStepRunEvent(readStepRunEvents(t, dir), "A2"); got.Event != "blocked" || got.Reason != "operator: discard inherited partial edit" {
		t.Fatalf("A2 last event = %+v, want blocked with the operator reason", got)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "src", "answer.txt")); err != nil || string(b) != "pending\n" {
		t.Fatalf("src/answer.txt = %q, err=%v after the restore, want the committed %q", b, err, "pending\n")
	}
	orch = agent.New(&scriptCaller{responses: []agent.ModelResult{
		toolStep("w3", "write_file", `{"path":"src/answer.txt","content":"expected\n"}`),
		answerStep("done"),
	}}, agent.ContextManager{})
	runStep, err = newTaskStepRunner(dir, &plan, client, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d = &driver{af: client, plan: &plan, runStep: runStep}
	final, err := d.resume(runCtx, dir, planBytes, nil)
	if err != nil || final.State != "complete" {
		t.Fatalf("resume after the remedy: state=%q err=%v", final.State, err)
	}
	events := readStepRunEvents(t, dir)
	if got := lastStepRunEvent(events, "A3").Event; got != "completed" {
		t.Fatalf("A3 last event = %q, want completed", got)
	}
	claims := 0
	for _, event := range events {
		if event.Event == "claimed" {
			claims++
		}
	}
	if claims != 3 {
		t.Fatalf("claim events = %d, want three (A1, A2, A3)", claims)
	}
}
