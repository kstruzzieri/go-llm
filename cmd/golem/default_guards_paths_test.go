package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/recipe"
)

// TestDefaultGuardsPath_OneShot: the startup orchestrator a plain -p run
// builds (no -interceptors) blocks a .env read on the wire, and the footer
// shows the guards' score.
func TestDefaultGuardsPath_OneShot(t *testing.T) {
	configPath, root, requests := fenceWireHarness(t, guardProbeWire(sseAnswer("final answer")))
	writeEnvSentinel(t, root)
	stderr := runFenceOneShot(t, configPath, root)
	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	tools := reqs[1].toolMessages()
	if len(tools) != 1 || tools[0].Content != framedToolResult(toolFrameKey(t, tools[0].Content), credentialBlocked) {
		t.Fatalf("tool messages = %+v, want the framed blocked observation", tools)
	}
	for i, r := range reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, envSentinelToken) {
				t.Fatalf("request %d leaked the sentinel", i)
			}
		}
	}
	// finalFooter ends a completed run's line with the score and no stop suffix.
	assertFooterTail(t, stderr, " · risk 30")
}

// TestDefaultGuardsPath_ProductionAgentflowModes: the -goal and -plan
// startups hand AgentFlow a guarded sess.orch and a guarded factory. The
// author, driver, and worker tests below only prove that those paths run the
// orchestrator they are handed.
func TestDefaultGuardsPath_ProductionAgentflowModes(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	approve := []string{"-approve-plan-edits", "-approve-plan-gates"}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"goal", []string{"-goal", "x"}},
		{"plan", append([]string{"-plan", plan}, approve...)},
		{"plan workers", append([]string{"-plan", plan, "-plan-workers", "2"}, approve...)},
		{"resume", append([]string{"-agentflow-resume", "-plan", plan}, approve...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// fenceWireHarness isolates HOME, XDG_CONFIG_HOME and XDG_DATA_HOME.
			configPath, root, _ := fenceWireHarness(t, guardProbeWire(sseAnswer("final answer")))
			writeEnvSentinel(t, root)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			args := append([]string{"-config", configPath, "-root", root,
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag",
				"-no-project-context", "-no-auto-index"}, tc.args...)
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(sess *replSession) error {
				req := agent.Request{Goal: "read the env", MaxSteps: 4, Tools: sess.tools}
				res, err := sess.orch.Run(t.Context(), req, nil)
				assertCredentialBlocked(t, res, err)
				fresh, err := sess.newOrchestrator().Run(t.Context(), req, nil)
				assertCredentialBlocked(t, fresh, err)
				return errStop
			}})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v, want the session-ready stop\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
		})
	}
}

// TestDefaultGuardsPath_ModelSetRebuild: /model set republishes an
// orchestrator and factory that still block.
func TestDefaultGuardsPath_ModelSetRebuild(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.alt.chatResponse = guardProbeResponse()
	writeEnvSentinel(t, fx.root)
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		slash(t, sess, "/model set swap")
		res, err := sess.runtime.Run(t.Context(), golemruntime.Turn{RunID: "guard-model-set", Message: "read the env"}, sess.machine.sink())
		assertCredentialBlocked(t, res, err)
		fresh, err := sess.newOrchestrator().Run(t.Context(), agent.Request{Goal: "read the env", MaxSteps: 4, Tools: sess.tools}, nil)
		assertCredentialBlocked(t, fresh, err)
		// Each backend labels its default answer, so these name the server:
		// the rebuilt orchestrator and factory, not the startup ones.
		if res.Answer != "alt answer" || fresh.Answer != "alt answer" {
			t.Fatalf("answers = %q, %q; want both served by the switched backend", res.Answer, fresh.Answer)
		}
	})
}

// TestDefaultGuardsPath_RecipePublicationAndRestoration: the REPL startup
// runtime blocks, a recipe model hint publishes a guarded orchestrator for
// its turn, and restoration leaves a guarded runtime and factory.
func TestDefaultGuardsPath_RecipePublicationAndRestoration(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.alt.chatResponse = guardProbeResponse()
	fx.primary.chatResponse = guardProbeResponse()
	writeEnvSentinel(t, fx.root)
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
		// Before any switch: the runtime REPL startup published.
		startup, err := sess.runtime.Run(t.Context(), golemruntime.Turn{RunID: "guard-startup", Message: "read the env"}, sess.machine.sink())
		assertCredentialBlocked(t, startup, err)
		if startup.Answer != "primary answer" {
			t.Fatalf("startup answer = %q, want the primary backend's", startup.Answer)
		}
		res, err := runOnceWithRecipeHint(t.Context(), io.Discard, nil, sess, "read the env", nil,
			&recipeInvocationHint{command: "/review", hint: recipe.ModelHint{Role: "swap"}})
		assertCredentialBlocked(t, res, err)
		if fx.alt.chats.Load() == 0 || res.Answer != "alt answer" {
			t.Fatalf("recipe turn answer = %q; it did not route to the published model", res.Answer)
		}
		restored, err := sess.runtime.Run(t.Context(), golemruntime.Turn{RunID: "guard-restored", Message: "read the env"}, sess.machine.sink())
		assertCredentialBlocked(t, restored, err)
		fresh, err := sess.newOrchestrator().Run(t.Context(), agent.Request{Goal: "read the env", MaxSteps: 4, Tools: sess.tools}, nil)
		assertCredentialBlocked(t, fresh, err)
		if restored.Answer != "primary answer" || fresh.Answer != "primary answer" {
			t.Fatalf("answers = %q, %q; want both served by the restored backend", restored.Answer, fresh.Answer)
		}
	})
}

// TestDefaultGuardsPath_AgentflowAuthor: the planner runs on sess.orch,
// whatever orchestrator that is; here the factory's.
func TestDefaultGuardsPath_AgentflowAuthor(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	caller := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		submitPlanCall(validIRJSON(t)),
	}}}
	sess := newTestSession(t, caller, root)
	sess.orch = newOrchestratorFactory(caller, flags{}, nil, nil)()
	var out, errb bytes.Buffer
	if err := runAgentflowAuthorWithClient(context.Background(), &out, &errb, nil, sess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, fixedApprover(true)); err != nil {
		t.Fatalf("author: %v\nstderr:\n%s", err, errb.String())
	}
	assertBlockedObservationSeen(t, caller.reqs, credentialBlocked)
}

// TestDefaultGuardsPath_TaskStepRunner: a task step runs on the orchestrator
// it is handed (production hands it sess.orch), builds none of its own, and
// carries on past the block to the step's allowed write.
func TestDefaultGuardsPath_TaskStepRunner(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	plan := &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"out.txt"}}}}
	caller := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		toolStep("w1", "write_file", `{"path":"out.txt","content":"worker\n"}`),
		answerStep("done"),
	}}}
	orch := newOrchestratorFactory(caller, flags{}, nil, nil)()
	af := &fakeAF{}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, af, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: af, plan: plan, runStep: runStep}
	if err := d.runOneStep(runCtx, "P1"); err != nil {
		t.Fatal(err)
	}
	assertBlockedObservationSeen(t, caller.reqs, credentialBlocked)
	if got, err := os.ReadFile(filepath.Join(root, "out.txt")); err != nil || string(got) != "worker\n" {
		t.Fatalf("out.txt = %q, %v; want the allowed write after the block", got, err)
	}
}

// TestDefaultGuardsPath_ParallelWorker: each parallel worker builds its
// orchestrator from sess.newOrchestrator, whatever that is; here the factory.
func TestDefaultGuardsPath_ParallelWorker(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	plan := &agentflow.Plan{AllowedFiles: []string{"worker.go"}, Steps: []agentflow.Step{{
		ID: "P2", Files: []string{"worker.go"}, Validation: []string{"unit"},
		Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./worker"}}},
	}}}
	runner := &assignedWorkerRunner{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w2","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`}
	caller := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		answerStep("done"),
	}}}
	sess := &replSession{maxSteps: 4, newOrchestrator: newOrchestratorFactory(caller, flags{}, nil, nil)}
	run := newAssignedParallelWorker(plan, sess, true, io.Discard, func(string) agentflow.Runner { return runner })
	if err := run(context.Background(), parallelWorker{step: plan.Steps[0], root: root, ownerID: "golem-w2", sourceID: "w2"}); err != nil {
		t.Fatal(err)
	}
	assertBlockedObservationSeen(t, caller.reqs, credentialBlocked)
}

// TestDefaultGuardsPath_DispatchChild: newDispatchTool derives the child chain
// from the flags value it is given, so a child .env read is blocked and scored.
func TestDefaultGuardsPath_DispatchChild(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	child := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		routed(toolStep("c1", "read_file", `{"path":".env"}`)),
		routed(answerStep("done")),
	}}}
	out, env := dispatchOnce(t, child, root)
	if out.IsError {
		t.Fatalf("Invoke = %+v, want no error", out)
	}
	if r := env.Results[0]; r.RiskScore != 30 || r.Error != "" || r.Summary != "done" {
		t.Fatalf("envelope = %s", out.Content)
	}
	assertBlockedObservationSeen(t, child.reqs, credentialBlocked)
}

// #611 with #575: three default-guard denials stop the step at
// tool_error_cap_reached, which blocks the attempt before its gates.
func TestDefaultGuardsPath_TaskStepStoppedByDenialsIsBlocked(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	plan := stopTestPlan()
	caller := &recordingCaller{next: &scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		toolStep("r2", "read_file", `{"path":".env"}`),
		toolStep("r3", "read_file", `{"path":".env"}`),
	}}}
	orch := newOrchestratorFactory(caller, flags{}, nil, nil)()
	af := &fakeAF{}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runStep, err := newTaskStepRunner(root, plan, af, orch, &replSession{maxSteps: 4}, true, io.Discard, nil, cancel)
	if err != nil {
		t.Fatal(err)
	}
	d := &driver{af: af, plan: plan, runStep: runStep}
	err = d.runOneStep(runCtx, "P1")
	if want := "step P1 attempt A-P1: agent run stopped: tool_error_cap_reached; attempt recorded as blocked"; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if want := []string{"claim:P1", "block-step:P1:A-P1:golem: agent run stopped: tool_error_cap_reached"}; !equalSeq(af.seq, want) {
		t.Fatalf("agentflow calls = %v, want %v", af.seq, want)
	}
	assertBlockedObservationSeen(t, caller.reqs, credentialBlocked)
}
