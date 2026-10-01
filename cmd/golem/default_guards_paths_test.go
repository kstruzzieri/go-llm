package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/recipe"
)

// TestRunOneShotDefaultGuardsBlockCredentialRead: the startup orchestrator a
// plain -p run builds (no -interceptors) blocks a .env read on the wire.
func TestRunOneShotDefaultGuardsBlockCredentialRead(t *testing.T) {
	configPath, root, requests := fenceWireHarness(t, func(req wireRequest) []string {
		if req.hasToolMessage() {
			return sseAnswer("final answer")
		}
		return sseToolCall("r1", "read_file", `{"path":".env"}`)
	})
	writeEnvSentinel(t, root)
	runFenceOneShot(t, configPath, root)
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
			if bytes.Contains([]byte(m.Content), []byte("guard-probe-575")) {
				t.Fatalf("request %d leaked the sentinel", i)
			}
		}
	}
}

// TestModelSetRebuildKeepsDefaultGuards: /model set republishes an
// orchestrator and factory that still block.
func TestModelSetRebuildKeepsDefaultGuards(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.alt.chatResponse = guardProbeResponse("alt-model")
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

// TestRecipeModelPublicationAndRestorationKeepDefaultGuards: a recipe model
// hint publishes a guarded orchestrator for its turn and restores a guarded
// one afterwards.
func TestRecipeModelPublicationAndRestorationKeepDefaultGuards(t *testing.T) {
	fx := newModelSwitchFixture(t, "")
	fx.alt.chatResponse = guardProbeResponse("alt-model")
	fx.primary.chatResponse = guardProbeResponse("agent-model")
	writeEnvSentinel(t, fx.root)
	fx.withSession(t, nil, func(t *testing.T, sess *replSession) {
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

// TestAgentflowAuthorRunsSessionOrchestratorGuards: the planner runs on
// sess.orch, which production builds through the factory.
func TestAgentflowAuthorRunsSessionOrchestratorGuards(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	caller := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{
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

// TestTaskStepRunnerRunsGivenOrchestratorGuards: a task step runs on the
// orchestrator it is handed (production hands it sess.orch) and builds none
// of its own.
func TestTaskStepRunnerRunsGivenOrchestratorGuards(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	plan := &agentflow.Plan{AllowedFiles: []string{"out.txt"}, Steps: []agentflow.Step{{ID: "P1", Files: []string{"out.txt"}}}}
	caller := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		toolStep("w1", "write_file", `{"path":"out.txt","content":"worker\n"}`),
		finalStep("done"),
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
}

// TestParallelWorkerRunsSessionFactoryGuards: each parallel worker builds its
// orchestrator from sess.newOrchestrator, which production binds to the
// factory.
func TestParallelWorkerRunsSessionFactoryGuards(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	plan := &agentflow.Plan{AllowedFiles: []string{"worker.go"}, Steps: []agentflow.Step{{
		ID: "P2", Files: []string{"worker.go"}, Validation: []string{"unit"},
		Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test", "./worker"}}},
	}}}
	runner := &assignedWorkerRunner{nextAction: `{"resumability":{"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},"agent_id":"golem-w2","step":{"id":"P1","state":"pending","completed":false},"attempt":null,"diagnostics":[]}}`}
	caller := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{
		toolStep("r1", "read_file", `{"path":".env"}`),
		finalStep("done"),
	}}}
	sess := &replSession{maxSteps: 4, newOrchestrator: newOrchestratorFactory(caller, flags{}, nil, nil)}
	run := newAssignedParallelWorker(plan, sess, true, io.Discard, func(string) agentflow.Runner { return runner })
	if err := run(context.Background(), parallelWorker{step: plan.Steps[0], root: root, ownerID: "golem-w2", sourceID: "w2"}); err != nil {
		t.Fatal(err)
	}
	assertBlockedObservationSeen(t, caller.reqs, credentialBlocked)
}

// TestDispatchChildRunsDefaultGuards: newDispatchTool derives the child chain
// from the flags value it is given, so a child .env read is blocked and scored.
func TestDispatchChildRunsDefaultGuards(t *testing.T) {
	root := t.TempDir()
	writeEnvSentinel(t, root)
	readers, err := buildTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	route := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	read := toolStep("c1", "read_file", `{"path":".env"}`)
	read.RouteOutcome = route
	answer := finalStep("done")
	answer.RouteOutcome = route
	child := &recordingScript{scriptCaller: scriptCaller{responses: []agent.ModelResult{read, answer}}}
	d, err := newDispatchTool(child, flags{dispatch: true}, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, readers, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string][]string{"tasks": {"read the env"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), raw)
	if err != nil || out.IsError {
		t.Fatalf("Invoke = %+v, %v", out, err)
	}
	var env dispatchTestEnvelope
	if err := json.Unmarshal([]byte(out.Content), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Results) != 1 || env.Results[0].RiskScore != 30 || env.Results[0].Error != "" || env.Results[0].Summary != "done" {
		t.Fatalf("envelope = %s", out.Content)
	}
	assertBlockedObservationSeen(t, child.reqs, credentialBlocked)
}
