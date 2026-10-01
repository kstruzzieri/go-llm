package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/provider"
)

type countingVerifier struct{ calls int }

func (v *countingVerifier) Verify(context.Context, agent.Approver) (string, error) {
	v.calls++
	return "status: ok", nil
}

// TestDefaultGuardsCapAfterWriteSkipsVerificationKeepsUndo (#575): an applied
// write followed by three blocked writes in one batch ends the run before
// verification; the write stays, the checkpoint seals, and /undo restores.
// The protected parents exist, so without guards the three writes would
// succeed (Plan needs an existing parent) and the run would verify: the test
// can only pass through the guards. newCheckpointWriteSession pre-grants the
// write class, so the first write must auto-approve through that grant: the
// answer source fails the test if it is ever consulted. The grant also means
// a blocked write that reached approval would be approved silently, never
// prompting, so the fatal source cannot prove block-before-approval; the
// blocked records must instead not be AutoApproved. That a files grant cannot
// bypass a block is also pinned by TestDefaultGuardsBlockDespiteFilesGrant.
func TestDefaultGuardsCapAfterWriteSkipsVerificationKeepsUndo(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".git/hooks", ".ssh", ".aws"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	batch := agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
		toolStep("w1", "write_file", `{"path":"a.txt","content":"A\n"}`).Response.ToolCalls[0],
		toolStep("w2", "write_file", `{"path":".git/hooks/pre-commit","content":"x"}`).Response.ToolCalls[0],
		toolStep("w3", "write_file", `{"path":".ssh/config","content":"x"}`).Response.ToolCalls[0],
		toolStep("w4", "write_file", `{"path":".aws/config","content":"x"}`).Response.ToolCalls[0],
	}}}
	caller := &scriptCaller{responses: []agent.ModelResult{batch}}
	sess, j := newCheckpointWriteSession(t, caller, root)
	v := &countingVerifier{}
	sess.orch = newOrchestratorFactory(caller, flags{}, v, nil)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, sess.tools)
	var out strings.Builder
	res, err := runOnce(context.Background(), &out, nil, sess, "write then probe", &promptFatalSource{t: t})
	if err != nil {
		t.Fatalf("runOnce: %v\n%s", err, out.String())
	}
	if res.StopReason != agent.ToolErrorCapReached || v.calls != 0 {
		t.Fatalf("stop = %v verifier calls = %d, want the cap and no verification", res.StopReason, v.calls)
	}
	requireRecords(t, res, 4)
	if r := res.ToolCalls[0]; !r.Invoked || r.IsError || r.Blocked || !r.AutoApproved {
		t.Fatalf("first write = %+v, want applied through the grant", r)
	}
	assertBlockedAt(t, res.ToolCalls, 1, 2, 3)
	for i := 1; i <= 3; i++ {
		if res.ToolCalls[i].AutoApproved {
			t.Fatalf("record %d = %+v: a blocked write reached approval and the grant approved it", i, res.ToolCalls[i])
		}
	}
	if res.Risk == nil || res.Risk.Score != 90 {
		t.Fatalf("risk = %+v, want 90", res.Risk)
	}
	protected := []string{".git/hooks/pre-commit", ".ssh/config", ".aws/config"}
	for _, p := range protected {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Fatalf("%s exists (%v); a blocked write landed", p, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(root, "a.txt")); err != nil || string(got) != "A\n" {
		t.Fatalf("a.txt = %q, %v; the applied write must stay", got, err)
	}
	infos, err := j.store.list(context.Background())
	if err != nil || len(infos) != 1 || infos[0].state != checkpointCompleted || infos[0].files != 1 {
		t.Fatalf("checkpoints = %+v, %v; want one sealed checkpoint", infos, err)
	}
	var undo strings.Builder
	dispatchSlash(context.Background(), &undo, sess, "/undo 1")
	for _, want := range []string{"undid a.txt\n", "[receipts verified]"} {
		if !strings.Contains(undo.String(), want) {
			t.Fatalf("/undo output = %q, want it to contain %q", undo.String(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("/undo left a.txt (%v): %s", err, undo.String())
	}
	for _, p := range protected {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Fatalf("%s exists after /undo (%v)", p, err)
		}
	}
}

// TestDefaultGuardsBlockedObservationPersists (#575): a real default run (the
// factory's own chain, the runtime's real save path, a real session store)
// persists the raw blocked observation and the call's arguments, and never the
// credential bytes. The golem package test composes its own guard chain, so
// this is the test that follows defaultGuards(): swapping a guard there, or
// dropping the chain, stores the sentinel. Every credential file exists, so an
// unguarded read succeeds.
func TestDefaultGuardsBlockedObservationPersists(t *testing.T) {
	root := t.TempDir()
	writeCredentialFiles(t, root)
	caller := &scriptCaller{responses: []agent.ModelResult{
		toolStep("b1", "read_file", `{"path":".env"}`),
		answerStep("answered"),
	}}
	sess := newSessionedTestSession(t, caller, root, "user:guards")
	sess.root = root
	sess.orch = newOrchestratorFactory(caller, flags{}, nil, nil)()
	installCompactRuntime(t, sess, golemruntime.Options{})
	res, err := runOnce(context.Background(), &strings.Builder{}, nil, sess, "read the env", nil)
	if err != nil || res.Answer != "answered" {
		t.Fatalf("run = %q, %v; want the run to complete and answer", res.Answer, err)
	}
	stored, err := sess.session.store.Load(context.Background(), sess.session.id)
	if err != nil {
		t.Fatalf("the answered turn was not saved: %v", err)
	}
	var roles []string
	for _, m := range stored.Messages {
		roles = append(roles, m.Role)
		if strings.Contains(m.Content, envSentinelToken) || strings.Contains(string(m.ToolCalls), envSentinelToken) {
			t.Fatalf("stored message leaked the credential sentinel: %+v", m)
		}
	}
	if want := []string{"user", "assistant", "tool", "assistant"}; !slices.Equal(roles, want) {
		t.Fatalf("stored roles = %v, want %v: %+v", roles, want, stored.Messages)
	}
	var calls []provider.ToolCall
	if err := json.Unmarshal(stored.Messages[1].ToolCalls, &calls); err != nil {
		t.Fatalf("stored tool calls = %s: %v", stored.Messages[1].ToolCalls, err)
	}
	if len(calls) != 1 || calls[0].ID != "b1" || calls[0].Function.Name != "read_file" || string(calls[0].Function.Arguments) != `{"path":".env"}` {
		t.Fatalf("stored tool calls = %+v, want read_file b1 with its arguments", calls)
	}
	if m := stored.Messages[2]; m.Content != credentialBlocked || m.ToolCallID != "b1" {
		t.Fatalf("stored observation = %+v, want the raw blocked observation for b1", m)
	}
	// Last, so a missing guard is reported by what it stored, not by the
	// record check that would otherwise stop the test first.
	assertCredentialBlocked(t, res, err)
}

// TestDefaultGuardsCapTracesAsCompleted (#575): a nil-error cap is classified
// "completed" by trace status, as for any governor stop today, and the
// content-full trace keeps the whole blocked observation.
func TestDefaultGuardsCapTracesAsCompleted(t *testing.T) {
	caller := &scriptCaller{responses: []agent.ModelResult{
		toolStep("b1", "read_file", `{"path":".env"}`),
		toolStep("b2", "read_file", `{"path":".ssh/id_ed25519"}`),
		toolStep("b3", "read_file", `{"path":".aws/credentials"}`),
	}}
	sess, traceDir := newTracingSession(t, caller)
	// Every credential file exists, so an unguarded read succeeds and the
	// sentinel would land in the trace.
	root := t.TempDir()
	writeCredentialFiles(t, root)
	sess.orch = newOrchestratorFactory(caller, flags{}, nil, nil)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, nil)
	res, err := runOnce(context.Background(), &strings.Builder{}, nil, sess, "probe", nil)
	if err != nil || res.StopReason != agent.ToolErrorCapReached {
		t.Fatalf("run = %v %v, want the cap", res.StopReason, err)
	}
	requireRecords(t, res, 3)
	assertBlockedAt(t, res.ToolCalls, 0, 1, 2)
	if res.Risk == nil || res.Risk.Score != 90 {
		t.Fatalf("risk = %+v, want 90", res.Risk)
	}
	if status := recordedTraceStatus(t, traceDir); status != "completed" {
		t.Fatalf("trace status = %q, want completed", status)
	}
	// recordedTraceStatus decodes only status; decode the whole record too, so
	// a trace that dropped the blocked calls or their observations fails.
	entries, err := os.ReadDir(traceDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trace files = %v, %v; want 1", entries, err)
	}
	raw, err := os.ReadFile(filepath.Join(traceDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), envSentinelToken) {
		t.Fatal("the trace carries the credential sentinel")
	}
	var trace agenttrace.TraceRecord
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatal(err)
	}
	if trace.Partial || trace.Result.StopReason != agent.ToolErrorCapReached {
		t.Fatalf("trace partial = %v stop = %v, want a complete record of the cap", trace.Partial, trace.Result.StopReason)
	}
	blocked := 0
	for _, r := range trace.Result.ToolCalls {
		if r.Blocked && !r.Invoked {
			blocked++
		}
	}
	observed := 0
	for _, m := range trace.Result.Messages {
		if m.Role == "tool" && m.Content == credentialBlocked {
			observed++
		}
	}
	if blocked != 3 || observed != 3 {
		t.Fatalf("trace blocked records = %d observations = %d, want 3 and 3", blocked, observed)
	}
}
