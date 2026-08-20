package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/tools"
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

func TestApproverGrantPreviewNeutralizesTerminalControls(t *testing.T) {
	preview := "+safe\x1b[2J\u009b\r\b\t\u202e.go\n context\n"
	cases := []struct {
		name  string
		call  provider.ToolCall
		key   string
		color bool
	}{
		{name: "diff", call: newCall(), key: tools.WriteClassApprovalKey, color: true},
		{name: "exec", call: execCall(), key: "exec:abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			grants := newApprovalGrants()
			ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, tc.color)
			ap.grants = grants
			d, err := ap.ApproveKeyed(context.Background(), tc.call, preview, tc.key)
			if err != nil || !d.Approved {
				t.Fatalf("ApproveKeyed: d=%+v err=%v", d, err)
			}

			got := out.String()
			for _, raw := range []string{"\x1b[2J", "\u009b", "\r", "\b", "\t", "\u202e"} {
				if strings.Contains(got, raw) {
					t.Fatalf("approval output retained terminal control %q: %q", raw, got)
				}
			}
			for _, escaped := range []string{`\x1b`, `\u009b`, `\r`, `\b`, `\t`, `\u202e`} {
				if !strings.Contains(got, escaped) {
					t.Fatalf("approval output omitted visible escape %q: %q", escaped, got)
				}
			}
		})
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

func TestReplApproverFlushesPendingMarkdownBeforePreview(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, true, 4, nil, false)
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "```go\npart"}); err != nil {
		t.Fatalf("OnToken: %v", err)
	}

	ap := newReplApprover(newScannerSource(strings.NewReader("n\n"), &out), &out, true)
	ap.beforeWrite = r.breakLine
	if _, err := ap.Approve(context.Background(), newCall(), "+added\n"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	got := out.String()
	wantOrder := "\x1b[36mpart\x1b[0m\n\x1b[32m+added\x1b[0m\n"
	if !strings.Contains(got, wantOrder) {
		t.Fatalf("pending Markdown did not flush before neutral diff output: %q", got)
	}
}

func TestReplApproverWritesThroughRendererCursor(t *testing.T) {
	var out strings.Builder
	r := newRenderer(&out, false, 4, nil, false)
	src := newScannerSource(strings.NewReader("n\n"), &out)
	ap := newReplApprover(src, r.rawWriter(), false)
	ap.beforeWrite = r.breakLine
	if ok, err := ap.Approve(context.Background(), newCall(), "preview"); err != nil || ok {
		t.Fatalf("Approve: ok=%v err=%v", ok, err)
	}
	if err := r.OnToolResult(context.Background(), agent.ToolResultEvent{
		Call:   newCall(),
		Result: agent.ToolResult{IsError: true, Content: "tool call denied by approver"},
	}); err != nil {
		t.Fatalf("OnToolResult: %v", err)
	}

	if got := out.String(); !strings.Contains(got, "Apply this change? [y/N] \n< error: tool call denied by approver\n") {
		t.Fatalf("tool result did not observe the approver cursor: %q", got)
	}
}

// promptFatalSource fails the test if the approver prompts at all.
type promptFatalSource struct{ t *testing.T }

func (s *promptFatalSource) ReadGoal(context.Context, string) (string, bool, error) {
	return "", false, nil
}
func (s *promptFatalSource) ReadAnswer(context.Context, string) (string, bool, error) {
	s.t.Fatal("prompted despite a session grant")
	return "", false, nil
}
func (s *promptFatalSource) RecordGoal(string)  {}
func (s *promptFatalSource) IdleDisplay(string) {}
func (s *promptFatalSource) Close() error       { return nil }

func execCall() provider.ToolCall {
	return provider.ToolCall{ID: "c1", Type: "function",
		Function: provider.ToolCallFunction{Name: "run_command"}}
}

func TestApproverGrantHitSkipsPrompt(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeExec, "exec:abc")
	ap := newReplApprover(&promptFatalSource{t: t}, &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), execCall(), "run command:\n  argv: go test\n", "exec:abc")
	if err != nil || !d.Approved {
		t.Fatalf("granted key must auto-approve: d=%+v err=%v", d, err)
	}
	if !d.ViaGrant {
		t.Fatal("a grant hit must report ViaGrant for record attribution")
	}
	s := out.String()
	if !strings.Contains(s, "argv: go test") {
		t.Fatalf("auto-approval must still render the preview:\n%s", s)
	}
	if !strings.Contains(s, "auto-approved (session grant)") {
		t.Fatalf("auto-approval must announce itself:\n%s", s)
	}
}

func TestApproverAlwaysAnswerGrantsKey(t *testing.T) {
	for _, in := range []string{"a\n", "A\n", "always\n"} {
		var out strings.Builder
		g := newApprovalGrants()
		ap := newReplApprover(newScannerSource(strings.NewReader(in), &out), &out, false)
		ap.grants = g
		d, err := ap.ApproveKeyed(context.Background(), execCall(), "preview", "exec:abc")
		if err != nil || !d.Approved {
			t.Fatalf("input %q must approve: d=%+v err=%v", in, d, err)
		}
		if d.ViaGrant {
			t.Fatalf("input %q: a prompted approval is not ViaGrant: %+v", in, d)
		}
		if !g.granted(grantScopeExec, "exec:abc") {
			t.Fatalf("input %q must store the grant under the exec scope", in)
		}
		if !strings.Contains(out.String(), "Run this command? [y/N/a=always this command] ") {
			t.Fatalf("grantable exec prompt must offer a:\n%s", out.String())
		}
	}
}

func TestApproverDifferentKeyPrompts(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeExec, "exec:aaa")
	ap := newReplApprover(newScannerSource(strings.NewReader("n\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), execCall(), "preview", "exec:bbb")
	if err != nil || d.Approved {
		t.Fatalf("an ungranted key must prompt and here deny: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "Run this command? [y/N/a=always this command] ") {
		t.Fatalf("prompt missing for ungranted key:\n%s", out.String())
	}
}

func TestApproverYesAnswerCarriesNoGrant(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	ap := newReplApprover(newScannerSource(strings.NewReader("y\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), execCall(), "preview", "exec:abc")
	if err != nil || !d.Approved {
		t.Fatalf("y must approve: d=%+v err=%v", d, err)
	}
	if d.ViaGrant {
		t.Fatalf("plain y must not carry grant provenance: %+v", d)
	}
	if g.granted(grantScopeExec, "exec:abc") {
		t.Fatal("plain y must not store a grant")
	}
}

func TestApproverWriteClassGrantCoversBothTools(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	src := newScannerSource(strings.NewReader("a\n"), &out)
	ap := newReplApprover(src, &out, false)
	ap.grants = g
	// "a" on a write_file prompt...
	d, err := ap.ApproveKeyed(context.Background(), newCall(), "+new\n", tools.WriteClassApprovalKey)
	if err != nil || !d.Approved {
		t.Fatalf("a on write prompt: d=%+v err=%v", d, err)
	}
	// ...must silence the edit_file prompt too (same scope + class key).
	editCall := provider.ToolCall{ID: "c2", Type: "function",
		Function: provider.ToolCallFunction{Name: "edit_file"}}
	ap2 := newReplApprover(&promptFatalSource{t: t}, &out, false)
	ap2.grants = g
	d, err = ap2.ApproveKeyed(context.Background(), editCall, "-old\n+new\n", tools.WriteClassApprovalKey)
	if err != nil || !d.Approved || !d.ViaGrant {
		t.Fatalf("class grant must cover edit_file: d=%+v err=%v", d, err)
	}
}

func TestApproverScopeAllowlistBlocksKeyCollision(t *testing.T) {
	// D12 adversarial case: a tool OUTSIDE the allowlist presents the
	// write-class key while that key is granted. Scope comes from the tool
	// name, never the key, so it must prompt — and "a" must deny.
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeFiles, tools.WriteClassApprovalKey)
	ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
	ap.grants = g
	call := provider.ToolCall{ID: "c1", Type: "function",
		Function: provider.ToolCallFunction{Name: "evil_tool"}}
	d, err := ap.ApproveKeyed(context.Background(), call, "preview", tools.WriteClassApprovalKey)
	if err != nil || d.Approved {
		t.Fatalf("unlisted tool with a colliding key must prompt and deny here: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "Apply this change? [y/N] ") {
		t.Fatalf("unlisted tool must get the ungrantable prompt:\n%s", out.String())
	}
}

func TestApproverMCPAndPlanNeverGrantable(t *testing.T) {
	cases := []struct {
		name, question string
	}{
		{"mcp__fs__read", "Run this MCP tool? [y/N] "},
		{"submit_plan", "Lock this plan? [y/N] "},
	}
	for _, tc := range cases {
		var out strings.Builder
		g := newApprovalGrants()
		// Adversarial: a non-empty key AND a pre-existing grant for it under
		// every scope. The allowlist must refuse regardless.
		g.grant(grantScopeExec, "exec:evil")
		g.grant(grantScopeFiles, "exec:evil")
		ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
		ap.grants = g
		call := provider.ToolCall{ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: tc.name}}
		d, err := ap.ApproveKeyed(context.Background(), call, "preview", "exec:evil")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if d.Approved {
			t.Fatalf("%s: excluded class auto-approved or accepted a", tc.name)
		}
		if !strings.Contains(out.String(), tc.question) {
			t.Fatalf("%s: prompt must stay %q:\n%s", tc.name, tc.question, out.String())
		}
	}
}

func TestApproverNilGrantsKeepsLegacyPrompt(t *testing.T) {
	var out strings.Builder
	ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
	// grants nil: the agentflow-author construction. "a" must deny, prompt [y/N].
	d, err := ap.ApproveKeyed(context.Background(), execCall(), "preview", "exec:abc")
	if err != nil || d.Approved {
		t.Fatalf("nil grants must keep per-invocation prompting: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "Run this command? [y/N] ") {
		t.Fatalf("nil-grants prompt must not offer a:\n%s", out.String())
	}
}
