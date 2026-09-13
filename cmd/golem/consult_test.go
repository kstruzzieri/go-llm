//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/consult"
	"github.com/kstruzzieri/go-llm/conversation"
)

// The three transcript records below are duplicated from consult's own test
// fixtures (consult/protocol_test.go is not importable). validInit's cwd is
// the synthetic placeholder the fake substitutes with its real cwd.
const consultValidInit = `{"type":"system","subtype":"init","claude_code_version":"2.1.240","apiKeySource":"none","permissionMode":"default","tools":[],"mcp_servers":[],"plugins":[],"skills":[],"slash_commands":[],"model":"claude-opus-4-8","cwd":"/private/synthetic","session_id":"synthetic-session","uuid":"synthetic-uuid"}`
const consultAssistantOK = `{"type":"assistant","parent_tool_use_id":null,"session_id":"synthetic-session","uuid":"synthetic-uuid","message":{"model":"claude-opus-4-8","role":"assistant","content":[{"type":"text","text":"OK"}]}}`
const consultGoodResult = `{"type":"result","subtype":"success","is_error":false,"result":"OK","num_turns":1,"stop_reason":"end_turn","permission_denials":[],"session_id":"synthetic-session","uuid":"synthetic-uuid"}`

// consultTranscript is an admissible transcript whose single answer is text.
// The assistant block and the result must agree for admission.
func consultTranscript(text string) string {
	return strings.Join([]string{
		consultValidInit,
		strings.Replace(consultAssistantOK, `"text":"OK"`, `"text":"`+text+`"`, 1),
		strings.Replace(consultGoodResult, `"result":"OK"`, `"result":"`+text+`"`, 1),
	}, "\n")
}

// fakeConsultants writes a one-consultant config whose command is a shell fake
// replaying stdout, and returns it loaded (so Load's defaults and validation
// apply). The temp dir is symlink-resolved: consult.Load rejects a command
// path that traverses one, and macOS temp roots live under /var -> /private/var.
func fakeConsultants(t *testing.T, name, stdout string) map[string]consult.Consultant {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := filepath.Join(dir, "claude-2.1.240")
	// A data file plus sed keeps the fixture bytes out of the shell's hands.
	data := filepath.Join(dir, "stdout")
	if err := os.WriteFile(data, []byte(stdout+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\ncat >/dev/null; sed \"s|/private/synthetic|$PWD|g\" " + data + "\n"
	if err := os.WriteFile(cmd, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "consultants.json")
	cfg := `{"version":1,"consultants":[{"name":"` + name + `","adapter":"claude","command":"` + cmd +
		`","model":"opus","trusted_process_egress":true}]}`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := consult.Load(p)
	if err != nil {
		t.Fatalf("consult.Load: %v", err)
	}
	return loaded
}

func TestConsultStagesAdviceForNextGoalThenClears(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude how should I fix this?")
	if sess.advisory == nil || !strings.Contains(out.String(), "claude (claude 2.1.240") ||
		!strings.Contains(out.String(), "OK") || !strings.Contains(out.String(), "staged for the next goal") {
		t.Fatalf("consult did not stage: %s adv=%v", out.String(), sess.advisory)
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "fix it", nil); err != nil {
		t.Fatal(err)
	}
	if sess.advisory != nil {
		t.Fatal("advisory not cleared after a successful turn")
	}
	staged := caller.lastRequest.Messages[len(caller.lastRequest.Messages)-1].Content
	if !strings.Contains(staged, "<<<CONSULT_ADVICE ") || !strings.Contains(staged, "fix it") {
		t.Fatalf("advisory not projected on the goal: %q", staged)
	}
	// The next goal must be unaccompanied: staging is one-shot.
	if _, err := runOnce(context.Background(), &out, nil, sess, "and again", nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range caller.lastRequest.Messages {
		if strings.Contains(m.Content, "CONSULT_ADVICE") {
			t.Fatalf("advisory projected onto a second goal: %q", m.Content)
		}
	}
}

func TestConsultRetainsOnFailureClearsOnReset(t *testing.T) {
	// A provider failure, so the turn reaches a real Run and fails there
	// rather than returning before the model call.
	sess := newTestSession(t, &failingCaller{err: errors.New("provider unavailable")}, t.TempDir())
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if sess.advisory == nil {
		t.Fatalf("consult did not stage: %s", out.String())
	}
	if _, err := runOnce(context.Background(), &out, nil, sess, "goal", nil); err == nil {
		t.Fatal("failed turn reported success")
	}
	if sess.advisory == nil {
		t.Fatal("advisory dropped on a failed turn")
	}
	dispatchSlash(context.Background(), &out, sess, "/clear")
	if sess.advisory != nil {
		t.Fatal("advisory survived /clear")
	}
}

func TestConsultClearedByNewAndResume(t *testing.T) {
	for _, cmd := range []string{"/new", "/resume user:other"} {
		t.Run(cmd, func(t *testing.T) {
			sess := newSessionedTestSession(t, &scriptCaller{}, t.TempDir(), "user:consult")
			sess.interceptorsOn = true
			sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
			if cmd != "/new" {
				if err := sess.session.store.Save(context.Background(), conversation.Conversation{
					ID: "user:other", Messages: []conversation.Message{{Role: "user", Content: "old"}},
				}); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			dispatchSlash(context.Background(), &out, sess, "/consult claude q")
			if sess.advisory == nil {
				t.Fatalf("consult did not stage: %s", out.String())
			}
			out.Reset()
			dispatchSlash(context.Background(), &out, sess, cmd)
			if strings.Contains(out.String(), "failed") {
				t.Fatalf("%s failed: %s", cmd, out.String())
			}
			if sess.advisory != nil {
				t.Fatalf("advisory survived %s", cmd)
			}
		})
	}
}

func TestConsultReplacesStagedSlotWithNotice(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	for name, c := range fakeConsultants(t, "second", consultTranscript("OK2")) {
		sess.consultants[name] = c
	}
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if strings.Contains(out.String(), "replaced staged advice") {
		t.Fatalf("first consult announced a replacement: %s", out.String())
	}
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult second q")
	if !strings.Contains(out.String(), "replaced staged advice from claude") {
		t.Fatalf("replacement not announced: %s", out.String())
	}
	if sess.advisory == nil || sess.advisory.Content != "OK2" || sess.advisory.Source != "second" {
		t.Fatalf("slot does not hold the second answer: %+v", sess.advisory)
	}
}

// blockAdvice returns one finding with the given verdict for any inspected
// content carrying the marker.
type blockAdvice struct {
	marker  string
	verdict agent.Verdict
}

func (blockAdvice) Name() string { return "blocker" }

func (b blockAdvice) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	for _, m := range in.Messages {
		if strings.Contains(m.Content, b.marker) {
			return []agent.Finding{{
				Interceptor: "blocker", Rule: "advice-refused", Verdict: b.verdict,
			}}, nil
		}
	}
	return nil, nil
}

func (blockAdvice) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

func (blockAdvice) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}

func TestConsultBlockedByInterceptorIsNotStaged(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	sess.orch = agent.New(caller, agent.ContextManager{},
		agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictBlock}))
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "consult failed: blocked by interceptor policy (advice-refused)") {
		t.Fatalf("refusal not reported: %s", out.String())
	}
	if sess.advisory != nil {
		t.Fatalf("blocked advice was staged: %+v", sess.advisory)
	}
}

func TestConsultShowsInterceptorAnnotationOutsideTheFrozenAnswer(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	sess.orch = agent.New(caller, agent.ContextManager{},
		agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictTag}))
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if sess.advisory == nil {
		t.Fatalf("tagged advice was not staged: %s", out.String())
	}
	if sess.advisory.Annotation == "" || sess.advisory.Content != "OK" {
		t.Fatalf("tag did not annotate out of band: %+v", sess.advisory)
	}
	if !strings.Contains(out.String(), strings.TrimRight(sess.advisory.Annotation, "\n")) {
		t.Fatalf("annotation not shown: %s", out.String())
	}
	// The digest still covers the frozen answer, not the annotated display.
	if sess.advisory.Digest != sha256Hex("OK") {
		t.Fatalf("digest %q no longer covers the frozen answer", sess.advisory.Digest)
	}
}

// sha256Hex is the receipt digest form (consult.ContentForm v1).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestConsultStatusLineWhenEmptyPrompt(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult")
	if !strings.Contains(out.String(), "staged: none") {
		t.Fatalf("empty slot not reported: %s", out.String())
	}
	sess.advisory = &agent.Advisory{
		Source: "claude", Digest: strings.Repeat("a", 64), Content: "x", Origin: agent.OriginModel,
	}
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult")
	if !strings.Contains(out.String(), "staged: claude (sha256:"+strings.Repeat("a", 12)+")") {
		t.Fatalf("staged slot not reported: %s", out.String())
	}
}

func TestConsultRefusals(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "consult disabled") {
		t.Fatalf("missing config not reported: %s", out.String())
	}
	// Init only: an admissible launch with no answer and no terminal record.
	sess.consultants = fakeConsultants(t, "claude", consultValidInit)
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "requires -interceptors") {
		t.Fatalf("interceptor requirement not enforced: %s", out.String())
	}
	sess.interceptorsOn = true
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult nope q")
	if !strings.Contains(out.String(), `unknown consultant "nope"`) {
		t.Fatalf("unknown name accepted: %s", out.String())
	}
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult claude")
	if !strings.Contains(out.String(), "usage: /consult") {
		t.Fatalf("empty prompt accepted: %s", out.String())
	}
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "consult failed: protocol") || sess.advisory != nil {
		t.Fatalf("failure not reported as a fixed code: %s adv=%v", out.String(), sess.advisory)
	}
}

func TestConsultOutputNeverContainsRawVendorBytes(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	sess.interceptorsOn = true
	// An auth-source failure: the answer is present in the transcript but the
	// launch is inadmissible, so none of its bytes may reach the terminal.
	bad := strings.Replace(consultTranscript("SENSITIVE-ANSWER"),
		`"apiKeySource":"none"`, `"apiKeySource":"ANTHROPIC_API_KEY"`, 1)
	sess.consultants = fakeConsultants(t, "claude", bad)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "consult failed: auth") {
		t.Fatalf("auth failure not reported: %s", out.String())
	}
	if strings.Contains(out.String(), "SENSITIVE-ANSWER") || sess.advisory != nil {
		t.Fatalf("vendor bytes leaked: %s adv=%v", out.String(), sess.advisory)
	}
}

func TestLoadConsultantsDisabledWithoutUserConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	m, err := loadConsultants("")
	if m != nil || err != nil {
		t.Fatalf("loadConsultants = %v, %v; want a disabled /consult", m, err)
	}
	// An explicit path is still a hard requirement.
	if _, err := loadConsultants(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("a missing explicit consultants file was accepted")
	}
}

// consultHelpDocumented pins the help entry so the command stays discoverable.
func TestConsultHelpDocumented(t *testing.T) {
	if !strings.Contains(golemHelp, "  /consult <name> <prompt>\n") {
		t.Fatalf("help must document /consult:\n%s", golemHelp)
	}
}
