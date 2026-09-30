//go:build unix

package consult

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A hard link to this test executable is the actual configured CLI. Recognize
// only its private fixture basename so its argv and environment need no shell.
func init() {
	if filepath.Base(os.Args[0]) == "546-app-server-fake" {
		runAppServerChild()
		syscall.Exit(0)
	}
}

type runAppLaunch struct {
	Args []string
	Env  []string
	Cwd  string
	PID  int
}

func fakeRunAppServer(t *testing.T, mode string, disabledMCPServers ...string) (Consultant, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := realTempDir(t)
	command := filepath.Join(dir, "546-app-server-fake")
	if err := os.Link(exe, command); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"mode": mode, "version": "codex-cli 0.153.4\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := strings.Replace(codexConfig, `"adapter":"codex"`, `"adapter":"codex","transport":"app-server","timeout_seconds":5`, 1)
	if disabledMCPServers != nil {
		names, err := json.Marshal(disabledMCPServers)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.Replace(body, `"transport":"app-server"`, `"transport":"app-server","disabled_mcp_servers":`+string(names), 1)
	}
	body = strings.Replace(body, `"@CMD@"`, strconvQuote(command), 1)
	path := filepath.Join(dir, "consultants.json")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	assertRunAppStarts(t, dir, "") // Load is offline, including model discovery.
	return cs["codex"], dir
}

func assertRunAppStarts(t *testing.T, dir, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "starts"))
	if err != nil && (want != "" || !errors.Is(err, os.ErrNotExist)) {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("starts = %q, want %q", got, want)
	}
	for _, phase := range strings.Fields(want) {
		launch := readRunAppLaunch(t, dir, phase)
		if _, err := os.Stat(filepath.Dir(launch.Cwd)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s envelope survived: %v", phase, err)
		}
		if err := syscall.Kill(launch.PID, 0); err != syscall.ESRCH {
			t.Fatalf("%s leader %d survived: %v", phase, launch.PID, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "descendant")); err == nil {
		waitForDeath(t, filepath.Join(dir, "descendant"))
	}
}

// All replies and expected requests below are source-derived test literals;
// no fixture reads ignored research artifacts or invokes a vendor executable.
func runAppServerChild() {
	dir := filepath.Dir(os.Args[0])
	modeBytes, _ := os.ReadFile(filepath.Join(dir, "mode"))
	mode := string(modeBytes)
	fail := func(message string) {
		_ = os.WriteFile(filepath.Join(dir, "peer-error"), []byte(message), 0600)
		syscall.Exit(71)
	}
	phase := "app-server"
	if reflect.DeepEqual(os.Args[1:], []string{"--version"}) {
		phase = "version"
	}
	f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fail(err.Error())
	}
	_, _ = io.WriteString(f, phase+"\n")
	_ = f.Close()
	cwd, err := os.Getwd()
	if err != nil {
		fail(err.Error())
	}
	launch, _ := json.Marshal(runAppLaunch{os.Args[1:], os.Environ(), cwd, os.Getpid()})
	_ = os.WriteFile(filepath.Join(dir, phase+".json"), launch, 0600)
	if phase == "version" {
		stdin, _ := io.ReadAll(os.Stdin)
		_ = os.WriteFile(filepath.Join(dir, "version-stdin"), stdin, 0600)
		if mode == "total-deadline" {
			time.Sleep(700 * time.Millisecond)
		}
		if mode == "version-drift" {
			// Replace the fixture path, never mutate the hard-linked test binary.
			other := filepath.Join(dir, "replacement")
			_ = os.WriteFile(other, []byte("#!/bin/sh\nexit 90\n"), 0700)
			_ = os.Rename(other, os.Args[0])
		}
		version, _ := os.ReadFile(filepath.Join(dir, "version"))
		_, _ = os.Stdout.Write(version)
		return
	}
	wantArgs := appTestArgs
	disabledMCP := strings.HasPrefix(mode, "mcp-disabled-")
	if disabledMCP {
		wantArgs = appTestDisabledMCPArgs
		mode = strings.TrimPrefix(mode, "mcp-disabled-")
	}
	if !reflect.DeepEqual(os.Args[1:], wantArgs) {
		fail("unexpected App Server argv")
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1048576)
	requests, _ := os.OpenFile(filepath.Join(dir, "requests"), os.O_CREATE|os.O_WRONLY, 0600)
	defer func() { _ = requests.Close() }()
	read := func(want string) {
		if !scanner.Scan() {
			fail("missing request: " + want)
		}
		_, _ = io.WriteString(requests, scanner.Text()+"\n")
		var gotValue, wantValue any
		if json.Unmarshal(scanner.Bytes(), &gotValue) != nil || json.Unmarshal([]byte(want), &wantValue) != nil || !reflect.DeepEqual(gotValue, wantValue) {
			fail("unexpected request: " + scanner.Text())
		}
	}
	emit := func(value string) {
		value = strings.ReplaceAll(value, "/synthetic/consult", cwd)
		if _, err := io.WriteString(os.Stdout, value+"\n"); err != nil {
			fail(err.Error())
		}
	}
	eof := func() {
		if scanner.Scan() || scanner.Err() != nil {
			fail("extra request after completion/rejection")
		}
	}
	read(appTestInitialize)
	if mode == "early-eof" {
		return
	}
	if mode == "missing-lf" {
		_, _ = io.WriteString(os.Stdout, appTestInitReply)
		return
	}
	if mode == "malformed-frame" {
		emit("PRIVATE-DIAGNOSTIC\r")
		eof()
		return
	}
	if mode == "config-warning" {
		emit(appTestInitReply)
		read(`{"method":"initialized"}`)
		emit(`{"method":"configWarning","params":{"message":"PRIVATE-DIAGNOSTIC"}}`)
		eof()
		return
	}
	// Native initialization metadata need not wait for initialized.
	emit(appTestInitReply + "\n" + appTestRemote)
	read(`{"method":"initialized"}`)
	read(`{"id":2,"method":"model/list","params":{"includeHidden":true,"limit":100}}`)
	model := appTestModel
	if mode == "unavailable" {
		model = strings.Replace(model, `"model":"gpt-6-astra"`, `"model":"gpt-5.5"`, 1)
	}
	emit(`{"id":2,"result":{"data":[` + model + `],"nextCursor":null}}`)
	if mode == "unavailable" {
		eof()
		return
	}
	read(`{"id":3,"method":"thread/start","params":{"model":"gpt-6-astra","cwd":` + strconvQuote(cwd) + `,"approvalPolicy":"never","approvalsReviewer":"user","sandbox":"read-only","ephemeral":true}}`)
	reply := appTestThreadReply
	if mode == "model-mismatch" {
		reply = strings.ReplaceAll(reply, "gpt-6-astra", "gpt-5.5")
	}
	emit(reply)
	if mode == "model-mismatch" {
		eof()
		return
	}
	emit(appTestThreadStarted)
	read(`{"id":4,"method":"turn/start","params":{"threadId":"thread-one","input":[{"type":"text","text":"Reply OK.","text_elements":[]}]}}`)
	emit(appTestTurnStarted)
	switch mode {
	case "mcp-startup-failed":
		name := "SECRET-SERVER"
		if disabledMCP {
			name = "Calendar_2"
		}
		emit(`{"method":"mcpServer/startupStatus/updated","params":{"name":"` + name + `","status":"failed","error":"PRIVATE-DIAGNOSTIC /private/native/path","failureReason":null,"threadId":"thread-one"}}`)
		eof()
		return
	case "error-notification", "error-retry", "system-error":
		if mode == "system-error" {
			emit(`{"method":"thread/status/changed","params":{"threadId":"thread-one","status":{"type":"systemError"}}}`)
		}
		retry := "false"
		if mode == "error-retry" {
			retry = "true"
		}
		emit(`{"method":"error","params":{"threadId":"thread-one","turnId":"turn-one","willRetry":` + retry + `,"error":{"message":"PRIVATE-DIAGNOSTIC SECRET /private/native/path","codexErrorInfo":null,"additionalDetails":null,"misalignment":null}}}`)
		eof()
		return
	case "warning", "deprecation-notice", "account-updated":
		method := map[string]string{"warning": "warning", "deprecation-notice": "deprecationNotice", "account-updated": "account/updated"}[mode]
		emit(`{"method":"` + method + `","params":"PRIVATE-DIAGNOSTIC"}`)
		eof()
		return
	case "approval":
		emit(`{"id":"SECRET-ID","method":"item/fileChange/requestApproval","params":{"threadId":"thread-one","turnId":"turn-one","itemId":"SECRET-ITEM","startedAtMs":1}}`)
		read(`{"id":"SECRET-ID","result":{"decision":"cancel"}}`)
		eof()
		return
	case "vendor":
		emit(`{"id":4,"error":{"code":-32001,"message":"SECRET /private/native/path token","data":{"id":"SECRET-ID"}}}`)
		eof()
		return
	case "protocol", "protocol-nonzero", "protocol-cap", "protocol-timeout", "protocol-cancel":
		emit(`{"method":"SECRET-UNKNOWN","params":{"path":"/private/native/path"}}`)
		eof()
		switch mode {
		case "protocol-nonzero":
			syscall.Exit(7)
		case "protocol-cap":
			_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 5000))
		case "protocol-timeout", "protocol-cancel":
			_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("true"), 0600)
			time.Sleep(30 * time.Second)
		}
		return
	}
	if mode == "total-deadline" {
		time.Sleep(700 * time.Millisecond)
	}
	if mode == "usage" || mode == "bad-usage" {
		emit(appTestUsage)
		later := strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":21`, 1)
		if mode == "bad-usage" {
			later = strings.Replace(appTestUsage, `"inputTokens":20`, `"inputTokens":19`, 1)
		}
		emit(later)
		if mode == "bad-usage" {
			eof()
			return
		}
		emit(later) // Repeated totals are not added together.
	}
	if mode == "zero-usage" {
		emit(`{"method":"thread/tokenUsage/updated","params":{"threadId":"thread-one","turnId":"turn-one","tokenUsage":{"total":{"totalTokens":0,"inputTokens":0,"cachedInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0},"last":{"totalTokens":0,"inputTokens":0,"cachedInputTokens":0,"outputTokens":0,"reasoningOutputTokens":0}}}}`)
	}
	answer := `{"type":"agentMessage","id":"answer-one","text":"OK","phase":"final_answer"}`
	if mode == "sanitize" {
		answer = appTestAnswer
	}
	if mode == "answer-limit" {
		answer = strings.Replace(answer, `"text":"OK"`, `"text":"`+strings.Repeat("x", 65537)+`"`, 1)
	}
	if mode != "no-answer" {
		emit(strings.Replace(appTestCompleted, appTestAnswer, answer, 1))
		emit(strings.Replace(appTestTerminal, appTestAnswer, answer, 1))
	} else {
		emit(`{"method":"turn/completed","params":{"threadId":"thread-one","turn":{"id":"turn-one","items":[],"itemsView":"notLoaded","status":"completed"}}}`)
		eof()
		return
	}
	emit(appTestTurnReply)
	eof()
	switch mode {
	case "late-error":
		emit(`{"method":"error","params":{"message":"SECRET /private/native/path"}}`)
	case "late-truncated":
		_, _ = io.WriteString(os.Stdout, `{"method":`)
	case "stderr-cap":
		_, _ = io.WriteString(os.Stderr, strings.Repeat("x", 5000))
	case "nonzero":
		syscall.Exit(7)
	case "drain":
		child := exec.Command("sleep", "30")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			fail(err.Error())
		}
		_ = os.WriteFile(filepath.Join(dir, "descendant"), []byte(strconv.Itoa(child.Process.Pid)), 0600)
		syscall.Exit(7)
	}
}

var appTestDisabledMCPArgs = []string{
	"-c", `approval_policy="never"`, "-c", `approvals_reviewer="user"`,
	"-c", "project_doc_max_bytes=0", "-c", `web_search="disabled"`,
	"-c", "features.hooks=false", "-c", "features.apps=false", "-c", "features.plugins=false",
	"-c", "features.external_agent_memory_import=false", "-c", "features.memories=false",
	"-c", "features.goals=false", "-c", "features.image_generation=false",
	"-c", "features.multi_agent_v2=false", "-c", "features.current_time_reminder=false",
	"-c", "agents.enabled=false", "-c", "orchestrator.skills.enabled=false",
	"-c", "skills.bundled.enabled=false", "-c", "orchestrator.mcp.enabled=false",
	"-c", "tools.update_plan.enabled=false", "-c", "tools.experimental_request_user_input.enabled=false",
	"-c", "mcp_servers.Calendar_2.enabled=false", "-c", "mcp_servers.docs-search.enabled=false",
	"app-server", "--listen", "stdio://", "--strict-config",
}

func TestRunAppServerDisabledMCPServers(t *testing.T) {
	for _, mode := range []string{"success", "mcp-startup-failed"} {
		t.Run(mode, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "mcp-disabled-"+mode, "Calendar_2", "docs-search")
			r, err := Run(context.Background(), c, "Reply OK.")
			if mode == "success" {
				if err != nil || r.Answer != "OK" {
					t.Fatalf("Run(disabled MCP servers) = %+v, %v; want OK", r, err)
				}
			} else {
				var ce *Error
				want := Error{Code: "protocol", Reason: "codex-app-server-invalid-protocol-turn-mcp-startup-failed"}
				if !errors.As(err, &ce) || *ce != want || r != (Receipt{}) {
					t.Fatalf("Run(disabled MCP servers, failed startup) = %+v, %v; want zero receipt and %v", r, err, want)
				}
			}
			for _, phase := range []string{"version", "app-server"} {
				want := []string{"--version"}
				if phase == "app-server" {
					want = appTestDisabledMCPArgs
				}
				if got := readRunAppLaunch(t, dir, phase).Args; !reflect.DeepEqual(got, want) {
					t.Fatalf("Run(disabled MCP servers) %s argv = %v, want %v", phase, got, want)
				}
			}
			assertRunAppStarts(t, dir, "version\napp-server\n")
		})
	}
	// Other declarations with omitted/empty lists retain exactly the default argv.
	for _, servers := range [][]string{nil, {}} {
		c, dir := fakeRunAppServer(t, "success", servers...)
		if r, err := Run(context.Background(), c, "Reply OK."); err != nil || r.Answer != "OK" {
			t.Fatalf("Run(other consultant) = %+v, %v; want OK", r, err)
		}
		if got := readRunAppLaunch(t, dir, "app-server").Args; !reflect.DeepEqual(got, appTestArgs) {
			t.Fatalf("Run(other consultant) argv = %v, want %v", got, appTestArgs)
		}
		assertRunAppStarts(t, dir, "version\napp-server\n")
	}
}

func TestRunDisabledMCPServersRejectsBeforeStart(t *testing.T) {
	for _, tc := range invalidDisabledMCPDeclarations() {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "success")
			c.Adapter, c.Transport, c.DisabledMCPServers = tc.adapter, tc.transport, tc.servers
			if c.Adapter == "claude" {
				c.Model = "opus"
			}
			raw, err := json.Marshal(Config{Version: 1, Consultants: []Consultant{c}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "consultants.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "disabled_mcp_servers") {
				t.Fatalf("Load(%s) = %v, want disabled_mcp_servers rejection", tc.name, err)
			}
			assertRunAppStarts(t, dir, "")
			ce := mustFail(t, context.Background(), c, "Reply OK.")
			if *ce != (Error{Code: "input-invalid", Reason: "consultant"}) {
				t.Fatalf("Run(%s) = %v, want input-invalid/consultant", tc.name, ce)
			}
			assertRunAppStarts(t, dir, "")
		})
	}
}

func TestRunDisabledMCPServersSnapshotsBeforePreflight(t *testing.T) {
	c, dir := fakeRunAppServer(t, "mcp-disabled-success", "Calendar_2", "docs-search")
	identity := realUID(t)
	mutated := false
	stubLookupUID(t, func(string) (*user.User, error) {
		// The host lookup occurs during preflight, after Run has taken ownership
		// of its options. Mutating here is deterministic and needs no race.
		c.DisabledMCPServers[0] = "changed.server"
		c.DisabledMCPServers[1] = "changed-server"
		mutated = true
		return identity, nil
	})
	r, err := Run(context.Background(), c, "Reply OK.")
	if !mutated || err != nil || r.Answer != "OK" {
		t.Fatalf("Run(mutated caller list) = %+v, %v, mutated=%t; want OK", r, err, mutated)
	}
	if got := readRunAppLaunch(t, dir, "app-server").Args; !reflect.DeepEqual(got, appTestDisabledMCPArgs) {
		t.Fatalf("Run(mutated caller list) argv = %v, want %v", got, appTestDisabledMCPArgs)
	}
	assertRunAppStarts(t, dir, "version\napp-server\n")
}

func TestRunAppServerReceipt(t *testing.T) {
	for _, mode := range []string{"success", "usage", "zero-usage", "sanitize"} {
		t.Run(mode, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, mode)
			r, err := Run(context.Background(), c, "Reply OK.")
			if err != nil {
				peer, _ := os.ReadFile(filepath.Join(dir, "peer-error"))
				t.Fatalf("Run(%s) = %v; peer=%s", mode, err, peer)
			}
			answer := "OK"
			if mode == "sanitize" {
				answer = "OK.\nReady�"
			}
			if r.Answer != answer || r.ContentSHA256 != sha256Hex(answer) || r.ContentForm != "consult-result/v1" || r.Consultant != "codex" || r.Adapter != "codex" || r.Version != "0.153.4" || r.Model != "gpt-6-astra" || r.ExitCode != 0 || r.Duration <= 0 {
				t.Fatalf("Run(%s) receipt = %+v", mode, r)
			}
			want := Evidence{WaitStatus: "exited(0)", WaitErrorKind: "none", TrustedVendorRuntime: true,
				CodexTransport: "app-server", CodexAppServerUsagePresent: mode == "usage" || mode == "zero-usage"}
			if mode == "usage" {
				want.CodexInputTokens, want.CodexCachedInputTokens, want.CodexCacheWriteInputTokens = 21, 5, 2
				want.CodexOutputTokens, want.CodexReasoningOutputTokens = 8, 3
			}
			if r.Evidence != want {
				t.Fatalf("Run(%s) evidence = %+v, want %+v", mode, r.Evidence, want)
			}
			raw, _ := json.Marshal(r.Evidence)
			for _, secret := range []string{"PRIVATE-DIAGNOSTIC", "SECRET", "thread-one", "turn-one", "answer-one", "synthetic", dir} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("evidence leaked %q: %s", secret, raw)
				}
			}
			assertRunAppStarts(t, dir, "version\napp-server\n")
		})
	}
}

func TestRunAppServerFailures(t *testing.T) {
	for _, tc := range []struct{ mode, code, reason string }{
		{"malformed-frame", "protocol", "codex-app-server-invalid-protocol-initialize-record-decode"},
		{"config-warning", "protocol", "codex-app-server-config-warning-rejected"},
		{"warning", "protocol", "codex-app-server-warning-rejected"},
		{"deprecation-notice", "protocol", "codex-app-server-deprecation-notice-rejected"},
		{"account-updated", "protocol", "codex-app-server-account-updated-rejected"},
		{"protocol", "protocol", "codex-app-server-invalid-protocol-turn-notification-unknown"},
		{"error-notification", "protocol", "codex-app-server-invalid-protocol-turn-error-notification-rejected"},
		{"error-retry", "protocol", "codex-app-server-invalid-protocol-turn-error-notification-rejected"},
		{"system-error", "protocol", "codex-app-server-invalid-protocol-turn-system-error-rejected"},
		{"mcp-startup-failed", "protocol", "codex-app-server-invalid-protocol-turn-mcp-startup-failed"},
		{"approval", "tool-activity", "codex-app-server-request"},
		{"vendor", "protocol", "codex-app-server-vendor-error"},
		{"unavailable", "protocol", "codex-app-server-model-unavailable"},
		{"model-mismatch", "protocol", "codex-app-server-invalid-protocol-thread-thread-reply-profile"},
		{"early-eof", "protocol", "codex-app-server-incomplete"},
		{"missing-lf", "protocol", "codex-app-server-incomplete"},
		{"no-answer", "protocol", "codex-app-server-invalid-protocol-turn-terminal-answer-missing"},
		{"bad-usage", "protocol", "codex-app-server-invalid-protocol-turn-usage-monotonic"},
		{"answer-limit", "protocol", "codex-app-server-invalid-answer"},
		{"late-error", "protocol", "codex-app-server-invalid-protocol-shutdown-error-notification-rejected"},
		{"late-truncated", "protocol", "codex-app-server-incomplete"},
		{"nonzero", "process-exit", "exited(7)"},
		{"drain", "drain-incomplete", "wait-delay"},
		{"stderr-cap", "output-limit", "cap"},
		{"protocol-nonzero", "process-exit", "exited(7)"},
		{"protocol-cap", "output-limit", "cap"},
		{"protocol-timeout", "timeout", "deadline"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, tc.mode)
			c.MaxOutputBytes = 4096
			if tc.mode == "answer-limit" {
				c.MaxOutputBytes = 1048576
			}
			if tc.mode == "protocol-timeout" {
				c.TimeoutSeconds = 1
			}
			restore := runWaitDelay
			t.Cleanup(func() { runWaitDelay = restore })
			runWaitDelay = 100 * time.Millisecond
			ce := mustFail(t, context.Background(), c, "Reply OK.")
			if ce.Code != tc.code || ce.Reason != tc.reason {
				t.Fatalf("Run(%s) fixed classification mismatch; want %s/%s", tc.mode, tc.code, tc.reason)
			}
			for _, secret := range []string{"PRIVATE-DIAGNOSTIC", "SECRET", "thread-one", "turn-one", "answer-one", "/private", dir, c.Command} {
				if strings.Contains(ce.Error(), secret) {
					t.Fatalf("Run(%s) error leaked private data", tc.mode)
				}
			}
			assertRunAppStarts(t, dir, "version\napp-server\n")
		})
	}
}

func TestRunAppServerRejectsBeforeStart(t *testing.T) {
	for name, change := range map[string]func(*Consultant){
		"unknown":           func(c *Consultant) { c.Transport = "SECRET-unknown" },
		"claude-app-server": func(c *Consultant) { c.Adapter, c.Model = "claude", "opus" },
		"other-model":       func(c *Consultant) { c.Model = "gpt-5.5" },
		"model-case":        func(c *Consultant) { c.Model = "GPT-6-ASTRA" },
		"runtime":           func(c *Consultant) { c.TrustedVendorRuntime = false },
		"egress":            func(c *Consultant) { c.TrustedProcessEgress = false },
		"digest":            func(c *Consultant) { c.SHA256 = "invalid" },
		"zero-timeout":      func(c *Consultant) { c.TimeoutSeconds = 0 },
		"timeout-max":       func(c *Consultant) { c.TimeoutSeconds = 301 },
		"zero-cap":          func(c *Consultant) { c.MaxOutputBytes = 0 },
		"cap-max":           func(c *Consultant) { c.MaxOutputBytes = 1048577 },
	} {
		t.Run(name, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "success")
			change(&c)
			ce := mustFail(t, context.Background(), c, "Reply OK.")
			if *ce != (Error{Code: "input-invalid", Reason: "consultant"}) {
				t.Fatalf("Run(%s) = %v; want input-invalid/consultant", name, ce)
			}
			assertRunAppStarts(t, dir, "")
		})
	}
}

func readRunAppLaunch(t *testing.T, dir, phase string) runAppLaunch {
	t.Helper()
	var launch runAppLaunch
	if err := json.Unmarshal([]byte(readTestFile(t, filepath.Join(dir, phase+".json"))), &launch); err != nil {
		t.Fatal(err)
	}
	return launch
}

func TestRunAppServerProfile(t *testing.T) {
	for _, key := range []string{"HOME", "USER", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "CODEX_HOME", "HTTP_PROXY", "HTTPS_PROXY", "CODEX_EXEC_SERVER_URL", "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED"} {
		t.Setenv(key, "POISON")
	}
	c, dir := fakeRunAppServer(t, "success")
	c.SHA256 = sha256File(t, c.Command)
	if _, err := Run(context.Background(), c, "Reply OK."); err != nil {
		t.Fatal(err)
	}
	identity, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"version", "app-server"} {
		launch := readRunAppLaunch(t, dir, phase)
		root := filepath.Dir(launch.Cwd)
		wantEnv := []string{
			"PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LC_ALL=C", "HOME=" + identity.HomeDir,
			"TMPDIR=" + filepath.Join(root, "tmp"), "USER=" + identity.Username,
			"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
			"XDG_STATE_HOME=" + filepath.Join(root, "state"), "CODEX_EXEC_SERVER_URL=none",
		}
		wantArgs := []string{"--version"}
		if phase == "app-server" {
			wantEnv = append(wantEnv, "CODEX_INTERNAL_APP_SERVER_REMOTE_CONTROL_DISABLED=1")
			wantArgs = appTestArgs // independent source literals, not production argv
		}
		if !reflect.DeepEqual(launch.Env, wantEnv) || !reflect.DeepEqual(launch.Args, wantArgs) {
			t.Fatalf("%s profile = %+v, want argv=%v env=%v", phase, launch, wantArgs, wantEnv)
		}
	}
	if got := readTestFile(t, filepath.Join(dir, "version-stdin")); got != "" {
		t.Fatalf("preflight stdin = %q, want empty", got)
	}
	if got := strings.Count(readTestFile(t, filepath.Join(dir, "requests")), "\n"); got != 5 {
		t.Fatalf("RPC records = %d, want 5", got)
	}
	assertRunAppStarts(t, dir, "version\napp-server\n")
}

func TestRunAppServerVersionsAndDigest(t *testing.T) {
	for _, version := range []string{"codex-cli 0.153.5\n", "codex-cli 0.153.4", "codex-cli 0.153.4\r\n", " codex-cli 0.153.4\n", "codex-cli 0.153.4\nextra", "0.153.4\n", strings.Repeat("x", 4097)} {
		t.Run(strconv.Quote(version[:min(len(version), 32)]), func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "success")
			if err := os.WriteFile(filepath.Join(dir, "version"), []byte(version), 0600); err != nil {
				t.Fatal(err)
			}
			want := Error{Code: "unsupported-version", Reason: "version-mismatch"}
			if len(version) > 4096 {
				want = Error{Code: "output-limit", Reason: "cap"}
			}
			if ce := mustFail(t, context.Background(), c, "Reply OK."); *ce != want {
				t.Fatalf("version refusal = %v, want %v", ce, want)
			}
			assertRunAppStarts(t, dir, "version\n")
		})
	}
	for _, configured := range []bool{false, true} {
		t.Run("drift-configured-"+strconv.FormatBool(configured), func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "version-drift")
			if configured {
				c.SHA256 = sha256File(t, c.Command)
			}
			if ce := mustFail(t, context.Background(), c, "Reply OK."); *ce != (Error{Code: "target-drift", Reason: "target"}) {
				t.Fatalf("version-bound drift = %v", ce)
			}
			assertRunAppStarts(t, dir, "version\n")
		})
	}
	for _, mode := range []string{"digest", "path", "symlink", "lower-cap", "lower-app-cap", "prompt", "empty-prompt", "long-prompt", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			c, dir := fakeRunAppServer(t, "success")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prompt, starts := "Reply OK.", ""
			want := Error{Code: "target-invalid", Reason: "target"}
			switch mode {
			case "digest":
				c.SHA256 = strings.Repeat("a", 64)
				want.Code = "target-drift"
			case "path":
				c.Command = "relative-SECRET"
			case "symlink":
				link := filepath.Join(dir, "link")
				if err := os.Symlink(c.Command, link); err != nil {
					t.Fatal(err)
				}
				c.Command = link
			case "lower-cap":
				c.MaxOutputBytes = 8
				want, starts = Error{Code: "output-limit", Reason: "cap"}, "version\n"
			case "lower-app-cap":
				c.MaxOutputBytes = 1000
				want, starts = Error{Code: "output-limit", Reason: "cap"}, "version\napp-server\n"
			case "prompt", "empty-prompt", "long-prompt":
				prompt = "bad\xff"
				switch mode {
				case "empty-prompt":
					prompt = ""
				case "long-prompt":
					prompt = strings.Repeat("x", 65537)
				}
				want = Error{Code: "input-invalid", Reason: "prompt"}
			case "canceled":
				cancel()
				want = Error{Code: "canceled", Reason: "caller"}
			}
			if ce := mustFail(t, ctx, c, prompt); *ce != want {
				t.Fatalf("Run(%s) = %v, want %v", mode, ce, want)
			}
			assertRunAppStarts(t, dir, starts)
		})
	}
}

func TestRunAppServerTotalDeadline(t *testing.T) {
	c, dir := fakeRunAppServer(t, "total-deadline")
	c.TimeoutSeconds = 1
	started := time.Now()
	ce := mustFail(t, context.Background(), c, "Reply OK.")
	if *ce != (Error{Code: "timeout", Reason: "deadline"}) || time.Since(started) > 2*time.Second {
		t.Fatalf("shared deadline = %v after %s", ce, time.Since(started))
	}
	assertRunAppStarts(t, dir, "version\napp-server\n")
}

func TestRunAppServerCancelOutranksProtocol(t *testing.T) {
	c, dir := fakeRunAppServer(t, "protocol-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	ce := mustFail(t, ctx, c, "Reply OK.")
	if *ce != (Error{Code: "canceled", Reason: "caller"}) {
		t.Fatalf("cancel during rejected exchange = %v", ce)
	}
	if got := readTestFile(t, filepath.Join(dir, "ready")); got != "true" {
		t.Fatalf("protocol rejection did not precede cancellation: %q", got)
	}
	assertRunAppStarts(t, dir, "version\napp-server\n")
}

func TestRunExplicitExecCompatibility(t *testing.T) {
	for _, adapter := range []string{"claude", "codex"} {
		t.Run(adapter, func(t *testing.T) {
			var legacy Receipt
			for _, transport := range []string{"", "exec"} {
				c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
				if adapter == "codex" {
					c, _ = fakeCodex(t, success)
				}
				c.Transport = transport
				r, err := Run(context.Background(), c, "Reply OK.")
				if err != nil {
					t.Fatal(err)
				}
				if r.Evidence.CodexTransport != "" || r.Evidence.CodexAppServerUsagePresent {
					t.Fatalf("legacy %s transport=%q changed evidence: %+v", adapter, transport, r.Evidence)
				}
				r.Duration = 0
				if transport == "" {
					legacy = r
				} else if r != legacy {
					t.Fatalf("explicit exec %s = %+v, want %+v", adapter, r, legacy)
				}
			}
		})
	}
}
