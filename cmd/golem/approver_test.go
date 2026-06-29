package main

import (
	"context"
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
		lr := newLineReader(strings.NewReader(in))
		ap := newReplApprover(lr, &out, false)
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
		lr := newLineReader(strings.NewReader(in))
		ap := newReplApprover(lr, &out, false)
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
	lr := newLineReader(pr)
	ap := newReplApprover(lr, &out, false)
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
	a := newReplApprover(newLineReader(in), &out, false)
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

func TestApproverMCPPromptShowsRunTool(t *testing.T) {
	in := strings.NewReader("y\n")
	var out strings.Builder
	a := newReplApprover(newLineReader(in), &out, false)
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
	lr := newLineReader(strings.NewReader("n\n"))
	ap := newReplApprover(lr, &out, true) // color on
	_, _ = ap.Approve(context.Background(), newCall(), "+added\n-removed\n context\n")
	s := out.String()
	if !strings.Contains(s, "\x1b[32m+added") {
		t.Fatalf("added line not green:\n%q", s)
	}
	if !strings.Contains(s, "\x1b[31m-removed") {
		t.Fatalf("removed line not red:\n%q", s)
	}
}
