package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func newCall() provider.ToolCall {
	return provider.ToolCall{
		ID: "c1", Type: "function",
		Function: provider.ToolCallFunction{Name: "write_file"},
	}
}

func TestReplApproverApprovesOnYes(t *testing.T) {
	for _, in := range []string{"y\n", "yes\n", "Y\n"} {
		var out strings.Builder
		src := newScannerSource(strings.NewReader(in), &out)
		ap := newReplApprover(src, &out, false)
		ok, err := ap.Approve(context.Background(), newCall(), "--- a\n+old\n")
		if err != nil || !ok {
			t.Fatalf("input %q: ok=%v err=%v", in, ok, err)
		}
		if !strings.Contains(out.String(), "Apply this change?") {
			t.Fatalf("prompt missing for %q:\n%s", in, out.String())
		}
		if !strings.Contains(out.String(), "+old") {
			t.Fatalf("diff not rendered for %q:\n%s", in, out.String())
		}
	}
}

func TestReplApproverDeniesOnNoEmptyEOF(t *testing.T) {
	for _, in := range []string{"n\n", "\n", ""} {
		var out strings.Builder
		src := newScannerSource(strings.NewReader(in), &out)
		ap := newReplApprover(src, &out, false)
		ok, err := ap.Approve(context.Background(), newCall(), "preview")
		if err != nil || ok {
			t.Fatalf("input %q must deny: ok=%v err=%v", in, ok, err)
		}
	}
}

func TestReplApproverContextCancelAborts(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	var out strings.Builder
	src := newScannerSource(pr, &out)
	ap := newReplApprover(src, &out, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok, err := ap.Approve(ctx, newCall(), "preview")
	if ok || err == nil {
		t.Fatalf("canceled approval must deny with error: ok=%v err=%v", ok, err)
	}
}

func TestApproverExecPromptNeutral(t *testing.T) {
	in := strings.NewReader("y\n")
	var out strings.Builder
	a := newReplApprover(newScannerSource(in, &out), &out, false)
	call := provider.ToolCall{Function: provider.ToolCallFunction{Name: "run_command"}}
	ok, err := a.Approve(context.Background(), call, "run command:\n  argv: go test\n")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	s := out.String()
	if !strings.Contains(s, "Run this command?") {
		t.Errorf("exec prompt should say 'Run this command?':\n%s", s)
	}
	if strings.Contains(s, "Apply this change?") {
		t.Error("exec call must not use the diff prompt")
	}
}

func TestApproverPlanPromptShowsLockPreview(t *testing.T) {
	in := strings.NewReader("y\n")
	var out strings.Builder
	a := newReplApprover(newScannerSource(in, &out), &out, false)
	call := provider.ToolCall{Function: provider.ToolCallFunction{Name: "submit_plan"}}
	ok, err := a.Approve(context.Background(), call, "Plan preview\n\nObjective\n  x\n")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	s := out.String()
	if !strings.Contains(s, "Plan preview") || !strings.Contains(s, "Lock this plan?") {
		t.Errorf("plan approval omitted preview or lock prompt:\n%s", s)
	}
	if strings.Contains(s, "Apply this change?") {
		t.Error("plan approval must not use the edit prompt")
	}
}

func TestApproverMCPPromptShowsRunTool(t *testing.T) {
	in := strings.NewReader("y\n")
	var out strings.Builder
	a := newReplApprover(newScannerSource(in, &out), &out, false)
	call := provider.ToolCall{Function: provider.ToolCallFunction{Name: "mcp__fs__read"}}
	ok, err := a.Approve(context.Background(), call, "mcp tool call:\n  args: {}\n")
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	s := out.String()
	if !strings.Contains(s, "Run this MCP tool?") {
		t.Errorf("mcp prompt should say 'Run this MCP tool?':\n%s", s)
	}
	if strings.Contains(s, "Apply this change?") {
		t.Error("mcp call must not use the diff prompt")
	}
}

func TestReplApproverColorRendersAnsi(t *testing.T) {
	var out strings.Builder
	src := newScannerSource(strings.NewReader("n\n"), &out)
	ap := newReplApprover(src, &out, true) // color on
	_, _ = ap.Approve(context.Background(), newCall(), "+added\n-removed\n context\n")
	s := out.String()
	if !strings.Contains(s, "\x1b[32m+added") {
		t.Fatalf("added line not green:\n%q", s)
	}
	if !strings.Contains(s, "\x1b[31m-removed") {
		t.Fatalf("removed line not red:\n%q", s)
	}
}

// stubAnswerSource scripts ReadAnswer; every other lineSource method is inert.
type stubAnswerSource struct {
	line string
	ok   bool
	err  error
}

func (s *stubAnswerSource) ReadGoal(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *stubAnswerSource) ReadAnswer(context.Context, string) (string, bool, error) {
	return s.line, s.ok, s.err
}
func (s *stubAnswerSource) RecordGoal(string)  {}
func (s *stubAnswerSource) IdleDisplay(string) {}
func (s *stubAnswerSource) Close() error       { return nil }

func TestReplApproverMapsInterruptToCanceled(t *testing.T) {
	// The editor's Ctrl-C sentinel is normalized at the approval boundary so
	// every caller classifies one shared error. Leaking errInterrupted would
	// make runOnce render "error: interrupted" and the Agentflow author skip
	// its errPlannerInterrupted mapping.
	var out strings.Builder
	ap := newReplApprover(&stubAnswerSource{err: errInterrupted}, &out, false)
	ok, err := ap.Approve(context.Background(), newCall(), "preview")
	if ok {
		t.Fatal("interrupted approval must deny")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, errInterrupted) {
		t.Fatalf("err = %v: the editor sentinel must not leak past the approver", err)
	}
}
