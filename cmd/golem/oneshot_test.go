package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestParseFlags_Prompt(t *testing.T) {
	f, err := parseFlags([]string{"-p", "summarize this diff"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.prompt != "summarize this diff" || !f.promptSet {
		t.Errorf("prompt = %q promptSet = %v, want set", f.prompt, f.promptSet)
	}
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags defaults: %v", err)
	}
	if def.prompt != "" || def.promptSet {
		t.Errorf("prompt must default to unset, got %+v", def)
	}
}

func TestValidateFlags_PromptEmpty(t *testing.T) {
	if err := validateFlags(flags{promptSet: true, prompt: ""}); err == nil {
		t.Error("-p with an empty prompt must error")
	}
	if err := validateFlags(flags{promptSet: true, prompt: "   "}); err == nil {
		t.Error("-p with a whitespace-only prompt must error")
	}
	if err := validateFlags(flags{promptSet: true, prompt: "do it"}); err != nil {
		t.Errorf("-p with a real prompt must be allowed, got %v", err)
	}
}

func TestValidateFlags_PromptSessionExclusive(t *testing.T) {
	if err := validateFlags(flags{promptSet: true, prompt: "x", sessionID: "s"}); err == nil {
		t.Error("-p with -session must error (one-shot never persists)")
	}
	if err := validateFlags(flags{promptSet: true, prompt: "x", fresh: true}); err == nil {
		t.Error("-p with -fresh must error (one-shot never persists)")
	}
}

func TestRun_OneShotEmptyPromptErrors(t *testing.T) {
	err := run([]string{"-p", ""}, os.Stdin, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "-p") {
		t.Fatalf("want empty -p prompt error, got %v", err)
	}
}

func TestApplyOneShotMode(t *testing.T) {
	got, warns := applyOneShotMode(flags{promptSet: true, prompt: "x", allowWrite: true, allowExec: true})
	if !got.noSession || !got.noCompress || !got.noMemory {
		t.Errorf("one-shot must force -no-session, -no-compress, and -no-memory, got %+v", got)
	}
	if got.allowWrite || got.allowExec {
		t.Errorf("one-shot must drop -allow-write/-allow-exec (no interactive approver), got %+v", got)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "one-shot") {
		t.Errorf("dropping approval-gated tools must warn, got %v", warns)
	}

	plain, warns := applyOneShotMode(flags{promptSet: true, prompt: "x"})
	if !plain.noSession || !plain.noCompress || !plain.noMemory || len(warns) != 0 {
		t.Errorf("plain one-shot: noSession/noCompress/noMemory forced without warnings, got %+v %v", plain, warns)
	}

	repl := flags{allowWrite: true}
	if got, warns := applyOneShotMode(repl); !reflect.DeepEqual(got, repl) || len(warns) != 0 {
		t.Errorf("without -p, flags must pass through untouched, got %+v %v", got, warns)
	}
}

// One-shot forcing -no-session must ripple into the #261 agent-memory gate:
// no session means the create/promote record tools are never registered.
func TestOneShotDisablesAgentMemoryWrites(t *testing.T) {
	f, _ := applyOneShotMode(flags{promptSet: true, prompt: "x", agentMemory: true})
	want, warn := agentMemoryRequest(f.agentMemory, f.noSession)
	if want || warn == "" {
		t.Errorf("agent memory must be refused in one-shot mode (want=%v warn=%q)", want, warn)
	}
}

func TestOneShot_PrintsOnlyAnswerToStdout(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(root+"/hello.txt", []byte("hi there"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"hello.txt"}`)},
		}},
	}
	answer := "# Result\n- item\n```go\nfmt.Println(\"hi\")\n```\n"
	finalAns := provider.ChatResponse{Content: answer}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: toolCall}, {Response: finalAns}}}
	sess := newTestSession(t, caller, root)
	sess.color = true

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "commit message please"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	// stdout purity: exactly the final answer plus one trailing newline.
	if stdout.String() != answer {
		t.Errorf("stdout = %q, want the bare answer with one trailing newline", stdout.String())
	}
	// All chrome (tool lines, footer) belongs to stderr.
	if !strings.Contains(stderr.String(), "read_file") {
		t.Errorf("tool-call progress missing from stderr:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "\x1b[") {
		t.Fatalf("test precondition failed: colored progress stream contains no ANSI:\n%s", stderr.String())
	}
	for _, banned := range []string{"golem>", "done ·", "\x1b["} {
		if strings.Contains(stdout.String(), banned) {
			t.Errorf("stdout must not contain %q:\n%s", banned, stdout.String())
		}
	}
}

// errCaller fails every model call.
type errCaller struct{ err error }

func (e errCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{}, e.err
}

func TestOneShot_RunErrorToStderrNonzero(t *testing.T) {
	sess := newTestSession(t, errCaller{err: errors.New("model unreachable")}, t.TempDir())

	var stdout, stderr strings.Builder
	err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "anything")
	if !errors.Is(err, errOneShotFailed) {
		t.Fatalf("want errOneShotFailed, got %v", err)
	}
	if stdout.String() != "" {
		t.Errorf("failed run must write nothing to stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "model unreachable") {
		t.Errorf("stderr must carry the failure reason:\n%s", stderr.String())
	}
}

func TestOneShot_EmptyAnswerIsError(t *testing.T) {
	// No tool calls + empty/whitespace content => Completed without a usable Answer.
	for _, content := range []string{"", "  \n\t\n"} {
		caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: content}}}}
		sess := newTestSession(t, caller, t.TempDir())

		var stdout, stderr strings.Builder
		err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "anything")
		if err == nil || !strings.Contains(err.Error(), "no final answer") {
			t.Fatalf("content %q: empty answer must be a hard error for scripting, got %v", content, err)
		}
		if stdout.String() != "" {
			t.Errorf("content %q: empty answer must write nothing to stdout, got %q", content, stdout.String())
		}
	}
}

// TestOneShot_AllowToolApprovesNamedToolWithoutGrants proves the #352 headless
// approver end to end: the named tool is actually INVOKED (its output reaches
// the progress stream), no approval prompt renders, and the session grant
// store stays empty — headless authorization is per-process, never a grant.
func TestOneShot_AllowToolApprovesNamedToolWithoutGrants(t *testing.T) {
	root := t.TempDir()
	execCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "e1", Type: "function",
			Function: provider.ToolCallFunction{Name: "run_command", Arguments: json.RawMessage(`{"argv":["echo","headless-ran"]}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "done running"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: execCall}, {Response: finalAns}}}
	sess := newExecOnlyTestSession(t, caller, root)
	set, err := newAllowToolSet([]string{"run_command"})
	if err != nil {
		t.Fatalf("newAllowToolSet: %v", err)
	}
	sess.headlessApprover = headlessApproverFor(set)

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "run something"); err != nil {
		t.Fatalf("runOneShot: %v; stderr=%s", err, stderr.String())
	}
	if stdout.String() != "done running\n" {
		t.Errorf("stdout = %q, want the final answer only", stdout.String())
	}
	if strings.Contains(stderr.String(), "denied") {
		t.Errorf("the named tool must be approved, not denied:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "headless-ran") {
		t.Errorf("run_command must actually execute (its output reaches the progress stream):\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Run this command?") {
		t.Error("headless approval must never render a prompt")
	}
	if got := sess.grants.count(); got != 0 {
		t.Errorf("grants.count() = %d, want 0 — headless runs create no session grants", got)
	}
}

// TestOneShot_AllowToolStillDeniesUnnamedGatedTool: acceptance criterion 2 —
// a gated call not explicitly authorized is still denied, even while another
// gated tool in the same group is authorized.
func TestOneShot_AllowToolStillDeniesUnnamedGatedTool(t *testing.T) {
	root := t.TempDir()
	execCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "e1", Type: "function",
			Function: provider.ToolCallFunction{Name: "run_command", Arguments: json.RawMessage(`{"argv":["echo","must-not-run"]}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "ok, skipped"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: execCall}, {Response: finalAns}}}
	sess := newExecOnlyTestSession(t, caller, root)
	set, err := newAllowToolSet([]string{"start_command"}) // run_command NOT named
	if err != nil {
		t.Fatalf("newAllowToolSet: %v", err)
	}
	sess.headlessApprover = headlessApproverFor(set)

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "run something"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if stdout.String() != "ok, skipped\n" {
		t.Errorf("stdout = %q, want the final answer only", stdout.String())
	}
	if strings.Contains(stderr.String(), "must-not-run") {
		t.Errorf("the unnamed tool must not execute:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "Run this command?") {
		t.Error("headless denial must never render a prompt")
	}
}

// mcpFakeTool is a mutating-class tool with an MCP name: approval-gated by
// effect, never authorizable by -allow-tool. invoked is the direct-observation
// seam — a stderr substring cannot prove non-invocation, because the renderer
// prints previews, not tool Content.
type mcpFakeTool struct{ invoked *bool }

func (m mcpFakeTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "mcp__fake__do", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (m mcpFakeTool) Effect() agent.Effect { return agent.Effect{Class: agent.Exec} }
func (m mcpFakeTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	*m.invoked = true
	return agent.ToolResult{Content: "mcp-tool-ran"}, nil
}

// TestOneShot_AllowToolStillDeniesMCPTool: the issue's explicit exclusion —
// MCP tools can never be authorized headlessly, even alongside -allow-tool.
func TestOneShot_AllowToolStillDeniesMCPTool(t *testing.T) {
	root := t.TempDir()
	mcpCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "m1", Type: "function",
			Function: provider.ToolCallFunction{Name: "mcp__fake__do", Arguments: json.RawMessage(`{}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "ok, skipped"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: mcpCall}, {Response: finalAns}}}
	system := buildSystemPrompt(false, false)
	orch := agent.New(caller, agent.ContextManager{})
	invoked := false
	sess := &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, []agent.Tool{mcpFakeTool{invoked: &invoked}}),
		baseSystem: system,
		maxSteps:   16,
		clock:      func() time.Time { return time.Unix(0, 0) },
		grants:     newApprovalGrants(),
	}
	set, err := newAllowToolSet([]string{"run_command"})
	if err != nil {
		t.Fatalf("newAllowToolSet: %v", err)
	}
	sess.headlessApprover = headlessApproverFor(set)

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "use the mcp tool"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if invoked {
		t.Error("the MCP tool must not be invoked under -allow-tool")
	}
	if stdout.String() != "ok, skipped\n" {
		t.Errorf("stdout = %q, want the final answer only", stdout.String())
	}
}

// A gated tool call in one-shot mode must be denied by the nil-approver
// fail-safe (never prompt, never panic on the absent line reader).
func TestOneShot_GatedExecDeniedWithoutApprover(t *testing.T) {
	root := t.TempDir()
	execCall := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "e1", Type: "function",
			Function: provider.ToolCallFunction{Name: "run_command", Arguments: json.RawMessage(`{"argv":["echo","hello"]}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "ok, skipped"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: execCall}, {Response: finalAns}}}
	sess := newExecOnlyTestSession(t, caller, root)

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "run something"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if stdout.String() != "ok, skipped\n" {
		t.Errorf("stdout = %q, want the final answer only", stdout.String())
	}
	if strings.Contains(stderr.String(), "Run this command?") || strings.Contains(stdout.String(), "Run this command?") {
		t.Error("one-shot must never render an approval prompt")
	}
}
