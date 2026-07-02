package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

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
	finalAns := provider.ChatResponse{Content: "feat: add greeting file\n"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: toolCall}, {Response: finalAns}}}
	sess := newTestSession(t, caller, root)

	var stdout, stderr strings.Builder
	if err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "commit message please"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	// stdout purity: exactly the final answer plus one trailing newline.
	if stdout.String() != "feat: add greeting file\n" {
		t.Errorf("stdout = %q, want the bare answer with one trailing newline", stdout.String())
	}
	// All chrome (tool lines, footer) belongs to stderr.
	if !strings.Contains(stderr.String(), "read_file") {
		t.Errorf("tool-call progress missing from stderr:\n%s", stderr.String())
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
