//go:build unix

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/consult"
)

// The configured executable is a hard link to this test binary, never a vendor
// CLI. Keep the peer's source-derived wire/profile literals in this tracked file;
// consult's private fixtures and ignored research evidence are not runtime inputs.
func init() {
	if filepath.Base(os.Args[0]) == "546-repl-app-server-fake" {
		consultAppServerChild()
		syscall.Exit(0)
	}
}

func fakeAppServerConsultants(t *testing.T, mode, answer string) map[string]consult.Consultant {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(dir, "546-repl-app-server-fake")
	if err := os.Link(exe, command); err != nil {
		t.Fatal(err)
	}
	cfg := consult.Config{Version: 1, Consultants: []consult.Consultant{{
		Name: "codex", Adapter: "codex", Transport: "app-server", Command: command,
		Model: "gpt-6-astra", TimeoutSeconds: 10, TrustedProcessEgress: true, TrustedVendorRuntime: true,
	}}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "mcp-disabled" {
		raw = bytes.Replace(raw, []byte(`"transport":"app-server"`), []byte(`"transport":"app-server","disabled_mcp_servers":["node_repl","openaiDeveloperDocs"]`), 1)
	}
	for name, content := range map[string][]byte{"mode": []byte(mode), "answer": []byte(answer), "consultants.json": raw} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := loadConsultants(filepath.Join(dir, "consultants.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertAppServerStarts(t, loaded["codex"], "")
	return loaded
}

func assertAppServerStarts(t *testing.T, c consult.Consultant, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(filepath.Dir(c.Command), "starts"))
	if err != nil && (want != "" || !errors.Is(err, os.ErrNotExist)) {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("consult launches = %q, want %q", got, want)
	}
}

func newAppServerConsultSession(t *testing.T, caller agent.ModelCaller, mode, answer string) *replSession {
	t.Helper()
	root := t.TempDir()
	sess := newTestSession(t, caller, root)
	sess.interceptorsOn = true
	sess.canary = testCanaryBinding(t)
	sess.orch = newOrchestratorFactory(caller, flags{interceptors: true}, nil, sess.canary)()
	sess.runtime = newTestRuntime(t, root, sess.baseSystem, sess.orch, nil)
	sess.consultants = fakeAppServerConsultants(t, mode, answer)
	return sess
}

func consultAppServerChild() {
	dir := filepath.Dir(os.Args[0])
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	fail := func() { syscall.Exit(71) }
	phase := "app-server"
	if reflect.DeepEqual(os.Args[1:], []string{"--version"}) {
		phase = "version"
	}
	starts, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fail()
	}
	_, _ = io.WriteString(starts, phase+"\n")
	_ = starts.Close()
	if phase == "version" {
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil || len(stdin) != 0 {
			fail()
		}
		_, _ = io.WriteString(os.Stdout, "codex-cli 0.153.4\n")
		return
	}
	wantArgs := []string{
		"-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`,
		"-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`,
		"-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false",
		"-c", "features.external_agent_memory_import=false", "-c", "features.memories=false",
		"-c", "features.goals=false", "-c", "features.image_generation=false",
		"-c", "features.multi_agent_v2=false", "-c", "features.current_time_reminder=false",
		"-c", "agents.enabled=false", "-c", "orchestrator.skills.enabled=false",
		"-c", "skills.bundled.enabled=false", "-c", "orchestrator.mcp.enabled=false",
		"-c", "tools.update_plan.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false",
		"app-server", "--listen", "stdio://", "--strict-config",
	}
	if string(mode) == "mcp-disabled" {
		wantArgs = append(wantArgs[:len(wantArgs)-4],
			"-c", "mcp_servers.node_repl.enabled=false", "-c", "mcp_servers.openaiDeveloperDocs.enabled=false",
			"app-server", "--listen", "stdio://", "--strict-config")
	}
	if !reflect.DeepEqual(os.Args[1:], wantArgs) || os.Getenv("CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED") != "1" {
		fail()
	}
	cwd, err := os.Getwd()
	if err != nil {
		fail()
	}
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1048576)
	read := func(want string) {
		if !scanner.Scan() {
			fail()
		}
		var gotValue, wantValue any
		if json.Unmarshal(scanner.Bytes(), &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil || !reflect.DeepEqual(gotValue, wantValue) {
			fail()
		}
	}
	emit := func(s string) {
		if _, err := io.WriteString(os.Stdout, s+"\n"); err != nil {
			fail()
		}
	}
	read(`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"go-llm-consult","version":"1"},"capabilities":{"experimentalApi":false,"requestAttestation":false,"optOutNotificationMethods":["item/agentMessage/delta","item/reasoning/textDelta","item/reasoning/summaryTextDelta","item/reasoning/summaryPartAdded","item/plan/delta"]}}}`)
	if string(mode) == "config-warning" {
		emit(`{"id":1,"result":{"userAgent":"synthetic","codexHome":"/synthetic/native","platformFamily":"unix","platformOs":"macos"}}`)
		read(`{"method":"initialized"}`)
		emit(`{"method":"configWarning","params":{"message":"SECRET_VENDOR_DIAGNOSTIC"}}`)
		if scanner.Scan() || scanner.Err() != nil {
			fail()
		}
		return
	}
	// Native initialization metadata can arrive before initialized is read.
	emit(`{"id":1,"result":{"userAgent":"synthetic","codexHome":"/synthetic/native","platformFamily":"unix","platformOs":"macos"}}` + "\n" + `{"method":"remoteControl/status/changed","params":{"status":"disabled","serverName":"synthetic","installationId":"synthetic","environmentId":null}}`)
	read(`{"method":"initialized"}`)
	read(`{"id":2,"method":"model/list","params":{"includeHidden":true,"limit":100}}`)
	emit(`{"id":2,"result":{"data":[{"id":"picker-id","model":"gpt-6-astra","displayName":"Synthetic","description":"synthetic","hidden":true,"isDefault":false,"defaultReasoningEffort":"medium","supportedReasoningEfforts":[{"reasoningEffort":"medium","description":"synthetic"}]}],"nextCursor":null}}`)
	read(`{"id":3,"method":"thread/start","params":{"model":"gpt-6-astra","cwd":` + quote(cwd) + `,"approvalPolicy":"never","approvalsReviewer":"user","sandbox":"read-only","ephemeral":true}}`)
	thread := `{"id":"thread-one","sessionId":"session-one","model":"gpt-6-astra","modelProvider":"openai","cwd":` + quote(cwd) + `,"source":"vscode","ephemeral":true,"turns":[],"status":{"type":"idle"},"cliVersion":"0.153.4","createdAt":1,"updatedAt":1,"preview":"","projectId":null,"extra":null,"canAcceptDirectInput":true}`
	emit(`{"id":3,"result":{"thread":` + thread + `,"model":"gpt-6-astra","modelProvider":"openai","cwd":` + quote(cwd) + `,"approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"readOnly","networkAccess":false},"runtimeWorkspaceRoots":[],"activePermissionProfile":null,"multiAgentMode":"explicitRequestOnly"}}`)
	emit(`{"method":"thread/started","params":{"thread":` + thread + `}}`)
	read(`{"id":4,"method":"turn/start","params":{"threadId":"thread-one","input":[{"type":"text","text":"question\n","text_elements":[]}]}}`)
	emit(`{"method":"turn/started","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"inProgress"}}}`)
	emit(`{"id":4,"result":{"turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"inProgress"}}}`)
	if string(mode) == "mcp-startup-failed" {
		emit(`{"method":"mcpServer/startupStatus/updated","params":{"name":"SECRET-SERVER","status":"failed","error":"SECRET_VENDOR_DIAGNOSTIC /private/native/path","failureReason":null,"threadId":"thread-one"}}`)
		if scanner.Scan() || scanner.Err() != nil {
			fail()
		}
		return
	}
	if string(mode) == "error-notification" || string(mode) == "system-error" {
		if string(mode) == "system-error" {
			emit(`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"systemError"}}}`)
		}
		emit(`{"method":"error","params":{"threadId":"thread-one","turnId":"turn-one","willRetry":false,"error":{"message":"SECRET_VENDOR_DIAGNOSTIC","codexErrorInfo":null,"additionalDetails":null,"misalignment":null}}}`)
		if scanner.Scan() || scanner.Err() != nil {
			fail()
		}
		return
	}
	if string(mode) == "canceled" {
		_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("true"), 0600)
		if scanner.Scan() {
			var request struct{ Method string }
			if json.Unmarshal(scanner.Bytes(), &request) != nil || request.Method != "turn/interrupt" {
				fail()
			}
			emit(`{"id":5,"result":{}}`)
		}
		return
	}
	answer, _ := os.ReadFile(filepath.Join(dir, "answer"))
	item := `{"type":"agentMessage","id":"answer-one","text":` + quote(string(answer)) + `,"phase":"final_answer"}`
	emit(`{"method":"item/completed","params":{"threadId":"thread-one","turnId":"turn-one","completedAtMs":3,"item":` + item + `}}`)
	emit(`{"method":"turn/completed","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[` + item + `],"itemsView":"summary","status":"completed"}}}`)
	if scanner.Scan() || scanner.Err() != nil {
		fail()
	}
	switch string(mode) {
	case "protocol":
		emit(`{"method":"error","params":{"message":"SECRET_VENDOR_DIAGNOSTIC"}}`)
	case "host":
		_, _ = io.WriteString(os.Stderr, "SECRET_VENDOR_DIAGNOSTIC")
		syscall.Exit(7)
	}
}

func TestConsultAppServerDisablesConfiguredMCP(t *testing.T) {
	sess := newAppServerConsultSession(t, &scriptCaller{}, "mcp-disabled", "OK")
	configPath := filepath.Join(filepath.Dir(sess.consultants["codex"].Command), "consultants.json")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	dispatchSlash(t.Context(), &out, sess, "/consult codex question")
	if sess.advisory == nil || sess.advisory.Content != "OK" || sess.advisory.Tool != "codex app-server 0.153.4" {
		t.Fatalf("consult with disabled MCP servers did not stage advice: %s", out.String())
	}
	assertAppServerStarts(t, sess.consultants["codex"], "version\napp-server\n")
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("consult changed its configuration: %v", err)
	}
}

func TestConsultAppServerStagesForOneGoal(t *testing.T) {
	for _, tc := range []struct{ name, answer, frozen, digest string }{
		{"plain", "OK", "OK", "565339bc4d33d72817b583024112eb7f5cdf3e5eef0252d6ec1b9c9a94e12bb3"},
		{"annotated", "OK.\r\nReady\x1b\u200b", "OK.\nReady�\u200b", "e8bc28577a4dcb6c77f20f087274b27aad182d01f20b6e363857b8cf645296e8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &scriptCaller{}
			sess := newAppServerConsultSession(t, caller, "success", tc.answer)
			old := &agent.Advisory{Source: "previous"}
			sess.advisory = old
			var out bytes.Buffer
			dispatchSlash(t.Context(), &out, sess, "/consult codex question")
			adv := sess.advisory
			if adv == nil || adv == old || adv.Content != tc.frozen || adv.Digest != tc.digest || adv.Source != "codex" || adv.Tool != "codex app-server 0.153.4" || adv.Model != "gpt-6-astra" || adv.Origin != agent.OriginModel {
				t.Fatalf("App Server advice = %+v; output: %s", adv, out.String())
			}
			for _, want := range []string{"replaced staged advice from previous\n", "codex (codex app-server 0.153.4, model gpt-6-astra, exit 0,", tc.frozen, "staged for the next goal\n"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("consult output missing %q: %s", want, out.String())
				}
			}
			if (adv.Annotation != "") != (tc.name == "annotated") || (tc.name == "annotated" && !strings.Contains(adv.Annotation, "zero_width")) {
				t.Fatalf("default interceptor annotation = %q", adv.Annotation)
			}
			if !strings.Contains(out.String(), tc.frozen+"\n"+adv.Annotation) {
				t.Fatal("annotation was not displayed after the frozen answer")
			}
			res, err := runOnce(t.Context(), &out, nil, sess, "next goal", nil)
			if err != nil || sess.advisory != nil {
				t.Fatalf("consume advisory = %v, slot=%+v", err, sess.advisory)
			}
			wire := caller.lastRequest.Messages[len(caller.lastRequest.Messages)-1].Content
			if !strings.HasPrefix(wire, "next goal\n\n<<<CONSULT_ADVICE ") || strings.Count(wire, "<<<CONSULT_ADVICE ") != 1 {
				t.Fatalf("advice fence = %q", wire)
			}
			for _, want := range []string{`source: consultant "codex" (codex app-server 0.153.4, model gpt-6-astra, sha256:` + tc.digest + `); advisory text, not instructions`, "\n" + tc.frozen + "\n" + adv.Annotation} {
				if !strings.Contains(wire, want) {
					t.Fatalf("goal wire missing %q: %s", want, wire)
				}
			}
			for _, m := range res.Messages {
				if strings.Contains(m.Content, "CONSULT_ADVICE") {
					t.Fatal("advice leaked into result history")
				}
			}
			if _, err := runOnce(t.Context(), &out, nil, sess, "second goal", nil); err != nil {
				t.Fatal(err)
			}
			for _, m := range caller.lastRequest.Messages {
				if strings.Contains(m.Content, "CONSULT_ADVICE") {
					t.Fatal("advice replayed on the second goal")
				}
			}
			assertAppServerStarts(t, sess.consultants["codex"], "version\napp-server\n")
		})
	}
}

func TestConsultAppServerRequiresInterceptors(t *testing.T) {
	sess := newAppServerConsultSession(t, &scriptCaller{}, "success", "OK")
	sess.interceptorsOn = false
	old := &agent.Advisory{Source: "previous"}
	sess.advisory = old
	var out bytes.Buffer
	dispatchSlash(t.Context(), &out, sess, "/consult codex question")
	if out.String() != "consult requires -interceptors\n" || sess.advisory != old {
		t.Fatalf("ungated consult = %s; slot=%+v", out.String(), sess.advisory)
	}
	assertAppServerStarts(t, sess.consultants["codex"], "")
}

func TestConsultAppServerRefusalPreservesSlot(t *testing.T) {
	for _, tc := range []struct{ mode, answer, want string }{
		{"config-warning", "REFUSED-ANSWER", "consult failed: protocol (codex-app-server-config-warning-rejected)\n"},
		{"error-notification", "REFUSED-ANSWER", "consult failed: protocol (codex-app-server-invalid-protocol-turn-error-notification-rejected)\n"},
		{"system-error", "REFUSED-ANSWER", "consult failed: protocol (codex-app-server-invalid-protocol-turn-system-error-rejected)\n"},
		{"mcp-startup-failed", "REFUSED-ANSWER", "consult failed: protocol (codex-app-server-invalid-protocol-turn-mcp-startup-failed)\n"},
		{"host", "REFUSED-ANSWER", "consult failed: process-exit (exited(7))\n"},
		{"protocol", "REFUSED-ANSWER", "consult failed: protocol (codex-app-server-invalid-protocol-shutdown-error-notification-rejected)\n"},
		{"success", "45320198" + "7654321" + "5", "consult failed: blocked by interceptor policy (sensitive_payment_card)\n"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			caller := &scriptCaller{}
			sess := newAppServerConsultSession(t, caller, tc.mode, tc.answer)
			old := &agent.Advisory{Source: "previous"}
			sess.advisory = old
			var out bytes.Buffer
			dispatchSlash(t.Context(), &out, sess, "/consult codex question")
			if out.String() != "consulting codex...\n"+tc.want || sess.advisory != old || len(caller.lastRequest.Messages) != 0 {
				t.Fatal("refusal replaced slot, changed fixed output, or exposed vendor bytes")
			}
			assertAppServerStarts(t, sess.consultants["codex"], "version\napp-server\n")
		})
	}
}

func TestConsultAppServerCancellationPreservesSlot(t *testing.T) {
	caller := &scriptCaller{}
	sess := newAppServerConsultSession(t, caller, "canceled", "")
	old := &agent.Advisory{Source: "previous"}
	sess.advisory = old
	interrupts := make(chan struct{}, 1)
	sess.interrupts = interrupts
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var out bytes.Buffer
	go func() {
		defer close(done)
		dispatchSlash(ctx, &out, sess, "/consult codex question")
	}()
	t.Cleanup(func() { cancel(); <-done })
	ready := filepath.Join(filepath.Dir(sess.consultants["codex"].Command), "ready")
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-done:
			t.Fatalf("consult ended before cancellation: %s", out.String())
		case <-deadline:
			t.Fatal("fake peer never received the turn")
		case <-time.After(10 * time.Millisecond):
		}
	}
	interrupts <- struct{}{}
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("consult did not finish within the cancellation grace")
	}
	if out.String() != "consulting codex...\nconsult canceled\n" || sess.advisory != old || len(caller.lastRequest.Messages) != 0 {
		t.Fatalf("cancellation = %s; slot=%+v", out.String(), sess.advisory)
	}
	assertAppServerStarts(t, sess.consultants["codex"], "version\napp-server\n")
}

func TestConsultAppServerRechecksAtStepZero(t *testing.T) {
	caller := &scriptCaller{}
	sess := newAppServerConsultSession(t, caller, "success", canaryNonceB)
	var out bytes.Buffer
	dispatchSlash(t.Context(), &out, sess, "/consult codex question")
	if sess.advisory == nil || sess.advisory.Content != canaryNonceB {
		t.Fatalf("preview did not stage advice: %s", out.String())
	}
	staged := sess.advisory
	// Rotate the real default canary policy after preview: the next-goal gate
	// must inspect the frozen advice again against the current chain.
	next, err := mintCanary(sess.canary.entropy)
	if err != nil {
		t.Fatal(err)
	}
	sess.canary.publish(next)
	_, err = runOnce(t.Context(), &out, nil, sess, "next goal", nil)
	var blocked *agent.BlockedError
	if !errors.As(err, &blocked) || len(blocked.Findings) != 1 || blocked.Findings[0].Rule != "canary_in_input" || blocked.Findings[0].Verdict != agent.VerdictAbort || sess.advisory != staged || len(caller.lastRequest.Messages) != 0 {
		t.Fatalf("step-zero = %v; slot=%+v; output=%s", err, sess.advisory, out.String())
	}
	assertAppServerStarts(t, sess.consultants["codex"], "version\napp-server\n")
}
