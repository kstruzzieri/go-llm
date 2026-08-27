package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// requestRecorder captures the messages handed to the model on each step, so
// the test can prove the verification reached the model in the SAME turn as
// the edit rather than merely landing in the final transcript.
type requestRecorder struct {
	responses []agent.ModelResult
	i         int
	requests  [][]provider.ChatMessage
}

func (r *requestRecorder) Chat(_ context.Context, req provider.ChatRequest,
	_ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	r.requests = append(r.requests, req.Messages)
	out := r.responses[r.i]
	r.i++
	return out, nil
}

// TestBreakingEditSurfacesInTheSameTurn is the acceptance criterion of #347,
// end to end through the real seam: the workspace declares a checker in
// .golem.json, the model makes an edit that breaks it, and the failure is on
// the edit's own observation in the request for the very next model step.
//
// The checker is a workspace script rather than a real compiler so the test
// stays hermetic and does not depend on a toolchain inside the CI image; it
// exercises exactly the same path a `go build ./...` would.
func TestBreakingEditSurfacesInTheSameTurn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix script semantics")
	}
	root := t.TempDir()
	source := filepath.Join(root, "a.txt")
	if err := os.WriteFile(source, []byte("healthy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checker := filepath.Join(root, "check.sh")
	// The success path deliberately prints: a passing verification must report
	// only its status, so without real output here a mutation that echoed the
	// body on success would be indistinguishable.
	if err := os.WriteFile(checker, []byte(
		"#!/bin/sh\nif grep -q BROKEN a.txt; then echo 'a.txt:1: undefined: BROKEN' >&2; exit 2; fi\n"+
			"echo 'checked 1 file, no problems'\nexit 0\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, verifyConfigName),
		[]byte(`{"verify":{"argv":["./check.sh"],"timeout_seconds":30}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	verifier, warn := buildVerifier(root)
	if warn != "" || verifier == nil {
		t.Fatalf("buildVerifier: verifier=%v warn=%q", verifier, warn)
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	edit := func(id, oldStr, newStr string) provider.ToolCall {
		args, merr := json.Marshal(map[string]string{
			"path": "a.txt", "old_string": oldStr, "new_string": newStr,
		})
		if merr != nil {
			t.Fatal(merr)
		}
		return provider.ToolCall{
			ID: id, Type: "function",
			Function: provider.ToolCallFunction{Name: "edit_file", Arguments: args},
		}
	}

	caller := &requestRecorder{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{edit("e1", "healthy", "BROKEN")}}},
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{edit("e2", "BROKEN", "healthy")}}},
		{Response: provider.ChatResponse{Content: "fixed", Done: true}},
	}}
	orch := newOrchestratorFactory(caller, flags{}, verifier)()
	res, err := orch.Run(context.Background(), agent.Request{
		Goal:     "break then fix",
		Tools:    agenttools.NewMutatingTools(ws, nil),
		Approver: allowAllApprover{},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "fixed" {
		t.Fatalf("answer = %q", res.Answer)
	}

	// Step 2's request is what the model read after the breaking edit.
	if len(caller.requests) != 3 {
		t.Fatalf("expected three model steps, got %d", len(caller.requests))
	}
	broken := toolMessage(t, caller.requests[1], "e1")
	if !strings.Contains(broken, "status: failed") {
		t.Fatalf("breaking edit must report a failed verification in the same turn:\n%s", broken)
	}
	if !strings.Contains(broken, "a.txt:1: undefined: BROKEN") {
		t.Fatalf("the checker's stderr must reach the model:\n%s", broken)
	}
	if !strings.Contains(broken, "exit_code: 2") {
		t.Fatalf("exit code must reach the model:\n%s", broken)
	}

	// And the repair verifies clean on its own batch.
	fixed := toolMessage(t, caller.requests[2], "e2")
	if !strings.Contains(fixed, "status: passed") {
		t.Fatalf("the repairing edit must verify clean:\n%s", fixed)
	}
	if strings.Contains(fixed, "checked 1 file") || strings.Contains(fixed, "--- stdout ---") {
		t.Fatalf("a passing verification must not echo output:\n%s", fixed)
	}

	// The write itself is still a success: verification informs, never fails.
	for _, rec := range res.ToolCalls {
		if rec.IsError {
			t.Fatalf("a failing verification must not fail the edit: %+v", rec)
		}
	}
	if got, err := os.ReadFile(source); err != nil || string(got) != "healthy\n" {
		t.Fatalf("workspace content = %q err=%v", got, err)
	}
}

func toolMessage(t *testing.T, msgs []provider.ChatMessage, id string) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	t.Fatalf("no tool observation for %q", id)
	return ""
}
