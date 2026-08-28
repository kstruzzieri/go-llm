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

func startCall() provider.ToolCall {
	return provider.ToolCall{ID: "s1", Type: "function",
		Function: provider.ToolCallFunction{Name: "start_command"}}
}

func stopCall() provider.ToolCall {
	return provider.ToolCall{ID: "s2", Type: "function",
		Function: provider.ToolCallFunction{Name: "stop_command"}}
}

func TestApproverStartPromptOffersExactCommandGrant(t *testing.T) {
	// #346: start_command gets its own prompt with the one-time and
	// "always this exact background command" choices, and "a" stores the
	// exec-bg key under the exec scope.
	var out strings.Builder
	g := newApprovalGrants()
	ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), startCall(),
		"start background command:\n  argv: sleep 30\n", "exec-bg:v1:abc")
	if err != nil || !d.Approved {
		t.Fatalf("a must approve: d=%+v err=%v", d, err)
	}
	if d.ViaGrant {
		t.Fatalf("a prompted approval is not ViaGrant: %+v", d)
	}
	if !g.granted(grantScopeExec, "exec-bg:v1:abc") {
		t.Fatal("a must store the exec-bg key under the exec scope")
	}
	s := out.String()
	if !strings.Contains(s, "Start this background command? [y/N/a=always this command] ") {
		t.Fatalf("start prompt must offer the exact-command grant:\n%s", s)
	}
	if !strings.Contains(s, "argv: sleep 30") {
		t.Fatalf("start approval must render the preview:\n%s", s)
	}
	for _, wrong := range []string{"Run this command?", "Apply this change?"} {
		if strings.Contains(s, wrong) {
			t.Fatalf("start_command must not reuse the %q prompt:\n%s", wrong, s)
		}
	}
}

func TestApproverStartGrantAutoApprovesSameKey(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeExec, "exec-bg:v1:abc")
	ap := newReplApprover(&promptFatalSource{t: t}, &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), startCall(),
		"start background command:\n  argv: sleep 30\n", "exec-bg:v1:abc")
	if err != nil || !d.Approved || !d.ViaGrant {
		t.Fatalf("granted start key must auto-approve ViaGrant: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "auto-approved (session grant)") {
		t.Fatalf("auto-approval must announce itself:\n%s", out.String())
	}
}

func TestApproverStartDifferentKeyPrompts(t *testing.T) {
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeExec, "exec-bg:v1:aaa")
	ap := newReplApprover(newScannerSource(strings.NewReader("n\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), startCall(), "preview", "exec-bg:v1:bbb")
	if err != nil || d.Approved {
		t.Fatalf("an ungranted start key must prompt and here deny: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "Start this background command? [y/N/a=always this command] ") {
		t.Fatalf("prompt missing for ungranted start key:\n%s", out.String())
	}
}

func TestApproverStartNilGrantsKeepsPlainPrompt(t *testing.T) {
	var out strings.Builder
	ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
	// grants nil: no grant path. "a" must deny; the prompt must not offer it.
	d, err := ap.ApproveKeyed(context.Background(), startCall(), "preview", "exec-bg:v1:abc")
	if err != nil || d.Approved {
		t.Fatalf("nil grants must keep per-invocation prompting: d=%+v err=%v", d, err)
	}
	if !strings.Contains(out.String(), "Start this background command? [y/N] ") {
		t.Fatalf("nil-grants start prompt must not offer a:\n%s", out.String())
	}
}

func TestApproverStopNeverGrantable(t *testing.T) {
	// Adversarial: grants exist for the presented key under every scope, and a
	// non-empty key is passed (the real stop ApprovalKey is empty — structural
	// refusal must not depend on that). "a" must deny and the prompt must
	// never show a grant legend.
	var out strings.Builder
	g := newApprovalGrants()
	g.grant(grantScopeExec, "exec-bg:v1:evil")
	g.grant(grantScopeFiles, "exec-bg:v1:evil")
	ap := newReplApprover(newScannerSource(strings.NewReader("a\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), stopCall(),
		"stop background command:\n  handle: bg-1\n", "exec-bg:v1:evil")
	if err != nil {
		t.Fatalf("ApproveKeyed: %v", err)
	}
	if d.Approved {
		t.Fatalf("stop_command auto-approved or accepted a: %+v", d)
	}
	s := out.String()
	if !strings.Contains(s, "Stop this background command? [y/N] ") {
		t.Fatalf("stop prompt must stay yes/no only:\n%s", s)
	}
	if strings.Contains(s, "a=always") {
		t.Fatalf("stop prompt must never offer a grant legend:\n%s", s)
	}
}

func TestApproverStopYesApprovesOnceWithoutStoringGrant(t *testing.T) {
	// The real wiring: stop's ApprovalKey is "" so grantable is structurally
	// false; "y" approves this one call and nothing is stored.
	var out strings.Builder
	g := newApprovalGrants()
	ap := newReplApprover(newScannerSource(strings.NewReader("y\n"), &out), &out, false)
	ap.grants = g
	d, err := ap.ApproveKeyed(context.Background(), stopCall(),
		"stop background command:\n  handle: bg-1\n", "")
	if err != nil || !d.Approved {
		t.Fatalf("y must approve the stop: d=%+v err=%v", d, err)
	}
	if g.count() != 0 {
		t.Fatalf("stop approval must store no grant, count = %d", g.count())
	}
	if !strings.Contains(out.String(), "handle: bg-1") {
		t.Fatalf("stop approval must render the preview:\n%s", out.String())
	}
}

func TestApproverBackgroundPreviewsNeutralizeTerminalControls(t *testing.T) {
	preview := "background command:\x1b[2J\u009b\r\u202e\n  argv: x\n"
	cases := []struct {
		name string
		call provider.ToolCall
		key  string
	}{
		{name: "start", call: startCall(), key: "exec-bg:v1:abc"},
		{name: "stop", call: stopCall(), key: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			ap := newReplApprover(newScannerSource(strings.NewReader("y\n"), &out), &out, false)
			ap.grants = newApprovalGrants()
			d, err := ap.ApproveKeyed(context.Background(), tc.call, preview, tc.key)
			if err != nil || !d.Approved {
				t.Fatalf("ApproveKeyed: d=%+v err=%v", d, err)
			}
			got := out.String()
			for _, raw := range []string{"\x1b[2J", "\u009b", "\r", "\u202e"} {
				if strings.Contains(got, raw) {
					t.Fatalf("%s approval retained terminal control %q: %q", tc.name, raw, got)
				}
			}
			for _, escaped := range []string{`\x1b`, `\u009b`, `\r`, `\u202e`} {
				if !strings.Contains(got, escaped) {
					t.Fatalf("%s approval omitted visible escape %q: %q", tc.name, escaped, got)
				}
			}
		})
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

func verifyToolCall() provider.ToolCall {
	return provider.ToolCall{
		ID: "v1", Type: "function",
		Function: provider.ToolCallFunction{Name: verifyToolName},
	}
}

func TestGrantScopeGivesVerificationItsOwnNamespace(t *testing.T) {
	scope := grantScope(verifyToolName)
	if scope == "" {
		t.Fatal("verify_command must be grantable")
	}
	if scope == grantScope("run_command") || scope == grantScope("write_file") {
		t.Fatalf("verify scope %q must differ from the exec and files scopes", scope)
	}
	if scope != grantScopeVerify {
		t.Fatalf("scope = %q, want %q", scope, grantScopeVerify)
	}
}

// TestVerifyGrantsDoNotCrossAuthorize is the security pin: a grant is
// authorized by (scope, key), and scope comes from the TOOL NAME, so even an
// identical key cannot move authorization between verification and command
// execution in either direction.
func TestVerifyGrantsDoNotCrossAuthorize(t *testing.T) {
	const sharedKey = "identical-structural-key"
	cases := []struct {
		name          string
		grantedTool   string
		attemptedTool string
	}{
		{"verify grant does not authorize run_command", verifyToolName, "run_command"},
		{"verify grant does not authorize start_command", verifyToolName, "start_command"},
		{"exec grant does not authorize verification", "run_command", verifyToolName},
		{"files grant does not authorize verification", "write_file", verifyToolName},
		{"verify grant does not authorize write_file", verifyToolName, "write_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grants := newApprovalGrants()
			grants.grant(grantScope(tc.grantedTool), sharedKey)
			if !grants.granted(grantScope(tc.grantedTool), sharedKey) {
				t.Fatal("fixture invalid: the grant was not stored")
			}
			if grants.granted(grantScope(tc.attemptedTool), sharedKey) {
				t.Fatalf("a %s grant must not authorize %s", tc.grantedTool, tc.attemptedTool)
			}
		})
	}
}

func TestReplApproverVerifyPromptIsPlainAndGrantable(t *testing.T) {
	var out strings.Builder
	src := newScannerSource(strings.NewReader("y\n"), &out)
	ap := newReplApprover(src, &out, false)
	ap.grants = newApprovalGrants()

	preview := "post-write verification command:\n  argv:    go test ./...\n"
	d, err := ap.ApproveKeyed(context.Background(), verifyToolCall(), preview, "verify:v1:abc")
	if err != nil || !d.Approved {
		t.Fatalf("approve: d=%+v err=%v", d, err)
	}
	rendered := out.String()
	if !strings.Contains(rendered, "go test ./...") {
		t.Fatalf("verify preview must render the command:\n%s", rendered)
	}
	if strings.Contains(rendered, "Apply this change?") {
		t.Fatalf("verification must not use the diff/edit prompt:\n%s", rendered)
	}
	if !strings.Contains(rendered, "Run this verification command?") {
		t.Fatalf("verification prompt missing:\n%s", rendered)
	}
	if !strings.Contains(rendered, "a=always") {
		t.Fatalf("a grantable verify prompt must offer the grant legend:\n%s", rendered)
	}
}

func TestReplApproverVerifyGrantAutoApprovesTheSameCommandOnly(t *testing.T) {
	var out strings.Builder
	src := newScannerSource(strings.NewReader("a\n"), &out)
	ap := newReplApprover(src, &out, false)
	ap.grants = newApprovalGrants()

	if d, err := ap.ApproveKeyed(context.Background(), verifyToolCall(), "preview", "verify:v1:abc"); err != nil || !d.Approved {
		t.Fatalf("grant turn: d=%+v err=%v", d, err)
	}
	if ap.grants.count() != 1 {
		t.Fatalf("grant not stored, count=%d", ap.grants.count())
	}

	// A second batch with the SAME command auto-approves with no input left.
	ap.src = newScannerSource(strings.NewReader(""), &out)
	d, err := ap.ApproveKeyed(context.Background(), verifyToolCall(), "preview", "verify:v1:abc")
	if err != nil || !d.Approved || !d.ViaGrant {
		t.Fatalf("same command must auto-approve via grant: d=%+v err=%v", d, err)
	}

	// A CHANGED command (different key) must prompt again, and EOF denies.
	d, err = ap.ApproveKeyed(context.Background(), verifyToolCall(), "preview", "verify:v1:changed")
	if err != nil || d.Approved {
		t.Fatalf("a changed verify command must not inherit the grant: d=%+v err=%v", d, err)
	}
}

func TestVerifyGrantsAreClearedWithTheSession(t *testing.T) {
	grants := newApprovalGrants()
	grants.grant(grantScopeVerify, "verify:v1:abc")
	if grants.count() != 1 {
		t.Fatalf("count = %d, want 1", grants.count())
	}
	grants.clear()
	if grants.count() != 0 || grants.granted(grantScopeVerify, "verify:v1:abc") {
		t.Fatal("verify grants must not survive a session clear")
	}
}
