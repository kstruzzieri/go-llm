//go:build unix

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/consult"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
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
	loaded, err := consult.Load(fakeConsultantsFile(t, name, stdout, 0))
	if err != nil {
		t.Fatalf("consult.Load: %v", err)
	}
	return loaded
}

// fakeConsultantsFile writes the config and its fake command and returns the
// config path. A non-zero delay makes the fake sleep before it answers, so a
// test can interrupt a consultation in flight.
func fakeConsultantsFile(t *testing.T, name, stdout string, delay time.Duration) string {
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
	// stdin is captured rather than discarded so a test can pin the exact
	// bytes handed to the consultant (see consultStdin).
	sleep := ""
	if delay > 0 {
		sleep = "sleep " + strconv.FormatFloat(delay.Seconds(), 'f', 3, 64) + "; "
	}
	body := "#!/bin/sh\ncat >" + filepath.Join(dir, "stdin") + "; " + sleep +
		"sed \"s|/private/synthetic|$PWD|g\" " + data + "\n"
	if err := os.WriteFile(cmd, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "consultants.json")
	cfg := `{"version":1,"consultants":[{"name":"` + name + `","adapter":"claude","command":"` + cmd +
		`","model":"opus","trusted_process_egress":true}]}`
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// consultStdin returns the bytes the fake consultant read on stdin.
func consultStdin(t *testing.T, c consult.Consultant) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(c.Command), "stdin"))
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	return string(b)
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
	// Rule 1's byte-exact user echo depends on the prompt reaching the
	// consultant terminated by exactly one newline, as in the E2.2 launch.
	if got := consultStdin(t, sess.consultants["claude"]); got != "q\n" {
		t.Fatalf("consultant stdin = %q, want %q", got, "q\n")
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
	rules   int // >1 emits that many distinct rules, to grow the trailer block
}

func (blockAdvice) Name() string { return "blocker" }

func (b blockAdvice) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	for _, m := range in.Messages {
		if !strings.Contains(m.Content, b.marker) {
			continue
		}
		n := max(b.rules, 1)
		out := make([]agent.Finding, 0, n)
		for i := 0; i < n; i++ {
			rule := "advice-refused"
			if b.rules > 1 {
				rule += strconv.Itoa(i)
			}
			out = append(out, agent.Finding{Interceptor: "blocker", Rule: rule, Verdict: b.verdict})
		}
		return out, nil
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
	// A disabled or ungated /consult reports that, not a state line that
	// implies the command would work.
	dispatchSlash(context.Background(), &out, sess, "/consult")
	if !strings.Contains(out.String(), "consult disabled") || strings.Contains(out.String(), "staged:") {
		t.Fatalf("disabled /consult reported a status line: %s", out.String())
	}
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	out.Reset()
	dispatchSlash(context.Background(), &out, sess, "/consult")
	if !strings.Contains(out.String(), "requires -interceptors") || strings.Contains(out.String(), "staged:") {
		t.Fatalf("ungated /consult reported a status line: %s", out.String())
	}
	sess.interceptorsOn = true
	out.Reset()
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

func TestConsultInterruptCancelsTheConsultation(t *testing.T) {
	sess := newTestSession(t, &scriptCaller{}, t.TempDir())
	sess.interceptorsOn = true
	loaded, err := consult.Load(fakeConsultantsFile(t, "claude", consultTranscript("OK"), 3*time.Second))
	if err != nil {
		t.Fatalf("consult.Load: %v", err)
	}
	sess.consultants = loaded
	interrupts := make(chan struct{}, 1)
	sess.interrupts = interrupts
	go func() {
		time.Sleep(300 * time.Millisecond)
		interrupts <- struct{}{}
	}()
	var out bytes.Buffer
	start := time.Now()
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Ctrl-C did not cut the consultation short: took %s", elapsed)
	}
	if !strings.Contains(out.String(), "consult failed: canceled") || sess.advisory != nil {
		t.Fatalf("interrupted consult = %q, adv=%v", out.String(), sess.advisory)
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

// --- staged-slot lifetime against the post-Run error reconciliation (#382) ---

// sealBreakingCaller answers normally but opens a checkpoint and then closes
// the store's database while the model call is in flight, so beginTurn has
// already succeeded and sealTurn's store.seal fails after Run returns.
type sealBreakingCaller struct {
	t *testing.T
	j *checkpointJournal
}

func (c *sealBreakingCaller) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if _, err := c.j.Prepare(testRec("a.txt", "A0", true)); err != nil {
		c.t.Errorf("Prepare: %v", err)
	}
	if err := c.j.store.db.Close(); err != nil {
		c.t.Errorf("close store db: %v", err)
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "completed answer"}}, nil
}

func TestConsultAdvisoryRetainedWhenTheTurnFailsToSeal(t *testing.T) {
	j, _, _ := newJournalFixture(t)
	caller := &sealBreakingCaller{t: t, j: j}
	sess := newTestSession(t, caller, t.TempDir())
	sess.journal = j
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if sess.advisory == nil {
		t.Fatalf("consult did not stage: %s", out.String())
	}
	res, err := runOnce(context.Background(), &out, nil, sess, "goal", nil)
	if err == nil || res.Answer == "" {
		t.Fatalf("want an answered turn that failed to seal, got %+v / %v", res, err)
	}
	if sess.advisory == nil {
		t.Fatalf("advisory consumed by a turn that failed to seal: %s", out.String())
	}
}

func TestConsultAdvisoryClearedWhenAnUnpersistedAnswerIsDemoted(t *testing.T) {
	root := t.TempDir()
	caller := &scriptCaller{responses: []agent.ModelResult{{
		Response: provider.ChatResponse{Content: "completed answer"},
	}}}
	sess := newSessionedTestSession(t, caller, root, "workspace:consult-save-failure")
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	// Neither a lost CAS nor a lock timeout, so the answered turn is demoted
	// to a success with a warning.
	failConversationSave(t, sess.session.db)
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if sess.advisory == nil {
		t.Fatalf("consult did not stage: %s", out.String())
	}
	res, err := runOnce(context.Background(), &out, nil, sess, "goal", nil)
	if err != nil || res.Answer != "completed answer" ||
		!strings.Contains(out.String(), "warning: session not saved:") {
		t.Fatalf("want a demoted-to-success answered turn, got %+v / %v:\n%s", res, err, out.String())
	}
	if sess.advisory != nil {
		t.Fatalf("advisory survived a successful turn: %+v", sess.advisory)
	}
}

func TestConsultNonPolicyInspectFailureIsInternal(t *testing.T) {
	caller := &scriptCaller{}
	sess := newTestSession(t, caller, t.TempDir())
	// 200 distinct tag rules overrun the 4 KiB annotation cap: a fail-closed
	// host error, not a policy refusal.
	sess.orch = agent.New(caller, agent.ContextManager{},
		agent.WithInterceptors(blockAdvice{marker: "OK", verdict: agent.VerdictTag, rules: 200}))
	sess.interceptorsOn = true
	sess.consultants = fakeConsultants(t, "claude", consultTranscript("OK"))
	var out bytes.Buffer
	dispatchSlash(context.Background(), &out, sess, "/consult claude q")
	if !strings.Contains(out.String(), "consult failed: internal") ||
		strings.Contains(out.String(), "blocked") || sess.advisory != nil {
		t.Fatalf("cap overflow reported as a policy refusal: %s adv=%v", out.String(), sess.advisory)
	}
}

// --- main wiring (#382) ---

func TestStartupNoticesConsultLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{"consult: 1 consultant", "consult: 1 consultant"},
		{"consult: 2 consultants", "consult: 2 consultants"},
	} {
		joined := strings.Join(startupNotices(startupInfo{workspace: "/r", consultLine: tc.line}), "\n")
		if !strings.Contains(joined, tc.want) {
			t.Fatalf("notices missing %q in:\n%s", tc.want, joined)
		}
	}
	if joined := strings.Join(startupNotices(startupInfo{workspace: "/r"}), "\n"); strings.Contains(joined, "consult:") {
		t.Fatalf("disabled /consult still announced:\n%s", joined)
	}
}

func TestConsultNoticeCountsMatchTheLoadedFile(t *testing.T) {
	for n, want := range map[int]string{1: "consult: 1 consultant", 2: "consult: 2 consultants"} {
		m := fakeConsultants(t, "claude", consultTranscript("OK"))
		if n == 2 {
			for name, c := range fakeConsultants(t, "second", consultTranscript("OK")) {
				m[name] = c
			}
		}
		if got := fmt.Sprintf("consult: %s", plural(len(m), "consultant", "consultants")); got != want {
			t.Fatalf("notice for %d consultants = %q, want %q", n, got, want)
		}
	}
}

func TestRunLoadsConsultantsAndReportsConfigFailures(t *testing.T) {
	config, root, _ := dispatchOneShotHarness(t)
	base := []string{"-config", config, "-root", root, "-no-project-context", "-no-git-context",
		"-no-probe", "-no-cap-probe", "-no-rag"}

	// A valid file is loaded and announced on stderr.
	path := fakeConsultantsFile(t, "claude", "", 0)
	in, out, diag := runTestFiles(t)
	if err := run(append(append([]string{}, base...), "-consultants-config", path, "-p", "hi"), in, out, diag); err != nil {
		t.Fatalf("run with a valid consultants file: %v", err)
	}
	if !strings.Contains(readRunTestFile(t, diag), "consult: 1 consultant") {
		t.Fatalf("startup did not announce the loaded consultant:\n%s", readRunTestFile(t, diag))
	}

	// A malformed file is a misconfiguration, not a silent disable.
	bad := filepath.Join(t.TempDir(), "consultants.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, out, diag = runTestFiles(t)
	err := run(append(append([]string{}, base...), "-consultants-config", bad, "-p", "hi"), in, out, diag)
	if exitCodeFor(err) != 2 {
		t.Fatalf("malformed consultants file exit=%d, want 2 (%v)", exitCodeFor(err), err)
	}
	if err == nil || !strings.Contains(err.Error(), "consult: parse config") {
		t.Fatalf("error does not name the parse failure: %v", err)
	}
}
