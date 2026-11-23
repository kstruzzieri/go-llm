package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestExitCodeTaxonomy pins the #352 contract end to end through run(): 0 is a
// completed run, 1 is a run/provider failure, 2 is a pre-run usage failure —
// and the taxonomy applies ONLY to -p invocations (spec R3): the same class of
// failure without -p keeps the pre-#352 exit 1, because -agentflow-status
// already owns exits 2 and 3 for its own semantics.
func TestExitCodeTaxonomy(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		want  int
	}{
		{"unknown flag with -p", []string{"-p", "hi", "-nope"}, "", 2},
		{"unknown flag without -p", []string{"-nope"}, "", 1},
		{"bad output-format", []string{"-p", "hi", "-output-format", "yaml"}, "", 2},
		{"empty output-format", []string{"-p", "hi", "-output-format="}, "", 2},
		{"output-format without -p", []string{"-output-format", "json"}, "", 1},
		{"text output-format without -p", []string{"-output-format", "text"}, "", 1},
		{"unknown allow-tool", []string{"-p", "hi", "-allow-tool", "rm"}, "", 2},
		{"excluded allow-tool", []string{"-p", "hi", "-allow-tool", "submit_plan"}, "", 2},
		{"allow-tool without -p", []string{"-allow-tool", "run_command"}, "", 1},
		{"empty stdin prompt", []string{"-p", "-"}, "  \n", 2},
		{"oversize stdin prompt", []string{"-p", "-"}, strings.Repeat("a", maxGoalBytes+1), 2},
		{"invalid UTF-8 stdin prompt", []string{"-p", "-"}, "\xff", 2},
		{"oversize direct prompt", []string{"-p", strings.Repeat("a", maxGoalBytes+1)}, "", 2},
		{"invalid UTF-8 direct prompt", []string{"-p", "\xff"}, "", 2},
		{"incompatible modes", []string{"-p", "hi", "-session", "s"}, "", 2},
		{"version with one-shot", []string{"-version", "-p", "hi", "-output-format", "json"}, "", 2},
		{"version ignores unrelated validation", []string{"-version", "-pressure-warn", "999"}, "", 0},
		{"missing config with -p", []string{"-p", "hi", "-config", "/nonexistent/models.json"}, "", 2},
		{"validation failure without -p", []string{"-pressure-warn", "999"}, "", 1},
		// An Agentflow mode combined with -p must NOT exit 2: -agentflow-status
		// consumers read 2 as "resume serially" (agentflow_recovery.go), so a
		// mode-conflict caller bug has to fail loudly with the pre-#352 exit 1
		// rather than impersonate a state-machine instruction.
		{"agentflow-status with -p", []string{"-agentflow-status", "-p", "hi"}, "", 1},
		{"agentflow-status with -p and a bad flag", []string{"-agentflow-status", "-p", "hi", "-nope"}, "", 1},
		{"agentflow-status with a bad flag", []string{"-agentflow-status", "-nope"}, "", 1},
		{"agentflow-resume with -p", []string{"-agentflow-resume", "-p", "hi"}, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdin, stdout, stderr := stdinFileWith(t, tc.stdin)
			err := run(tc.args, stdin, stdout, stderr)
			if got := exitCodeFor(err); got != tc.want {
				t.Errorf("run(%v) exit = %d (err %v), want %d", tc.args, got, err, tc.want)
			}
			if tc.want == 2 && readRunTestFile(t, stdout) != "" {
				t.Errorf("pre-run caller error wrote to stdout")
			}
			if tc.name == "version ignores unrelated validation" && readRunTestFile(t, stdout) != versionString()+"\n" {
				t.Errorf("-version did not preserve its legacy output")
			}
		})
	}
}

// main's stderr line for a question that could not be shown is one line even
// when errors.Join added a cause, and the error keeps its chain. Other errors
// keep their text.
func TestCLIFailureLineConsentNotShownIsOneLine(t *testing.T) {
	err := errors.Join(questionNotShown(errors.New("write /dev/stdout: broken pipe")), context.Canceled)
	const want = "the question could not be shown: write /dev/stdout: broken pipe; context canceled"
	if got := cliFailureLine(err); got != want {
		t.Errorf("cliFailureLine = %q, want %q", got, want)
	}
	if !errors.Is(err, errConsentNotShown) || !errors.Is(err, context.Canceled) || exitCodeFor(err) != 1 {
		t.Errorf("chain or exit code changed: %v (exit %d)", err, exitCodeFor(err))
	}
	other := errors.Join(errors.New("a"), errors.New("b"))
	if got := cliFailureLine(other); got != "a\nb" {
		t.Errorf("cliFailureLine(other) = %q, want it unchanged", got)
	}
}

func TestHeadlessCallerConfigurationFailuresExitTwo(t *testing.T) {
	configPath, root, _ := admissionHarness(t, "https://opencode.invalid/zen/go")
	base := admissionArgs(configPath, root)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"invalid base URL", append(append([]string{}, base...), "-base-url", "://")},
		{"invalid destination", append(append([]string{}, base...), "-allow-destination", "not-a-destination")},
		{"unknown dispatch role", append(append([]string{}, base...), "-dispatch", "-dispatch-role", "missing")},
		{"unknown delegate role", append(append([]string{}, base...), "-delegate", "-delegate-role", "missing")},
		{"invalid MCP HTTP server", append(append([]string{}, base...), "-mcp-http=")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdin, stdout, stderr := runTestFiles(t)
			err := run(tc.args, stdin, stdout, stderr)
			if got := exitCodeFor(err); got != 2 {
				t.Fatalf("exit = %d (err %v), want 2", got, err)
			}
		})
	}
}

func TestHeadlessMaterializationFailureExitsTwo(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"local": {"base_url": "http://127.0.0.1:11434?token=secret", "api_format": "openai-compat"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	err := run(admissionArgs(configPath, root), stdin, stdout, stderr)
	if got := exitCodeFor(err); got != 2 {
		t.Fatalf("exit = %d (err %v), want 2", got, err)
	}
}

func TestHeadlessDeclaredCapabilityFailuresExitTwo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"agent-model"},{"id":"dispatch-model"}]}`))
	}))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name      string
		agentCaps string
		extraArgs []string
	}{
		{"active route", `["chat", "generate", "stream"]`, nil},
		{"dispatch route", `["chat", "generate", "stream", "tool_call"]`, []string{"-dispatch", "-dispatch-role", "analysis"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "models.json")
			configJSON := `{
  "providers": {"local": {"base_url": "` + server.URL + `", "api_format": "openai-compat"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768, "capabilities": ` + tc.agentCaps + `},
    "analysis": {"name": "dispatch-model", "provider": "local", "type": "dense", "context_window": 32768, "capabilities": ["chat", "generate", "stream"]}
  },
  "defaults": {"agent": "agent", "analysis": "analysis"}
}`
			if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append(admissionArgs(configPath, root), "-output-format", "json")
			args = append(args, tc.extraArgs...)
			stdin, stdout, stderr := runTestFiles(t)
			err := run(args, stdin, stdout, stderr)
			if got := exitCodeFor(err); got != 2 {
				t.Fatalf("exit = %d (err %v), want 2", got, err)
			}
			if got := readRunTestFile(t, stdout); got != "" {
				t.Fatalf("pure config failure wrote machine stdout: %s", got)
			}
		})
	}
}

// TestExitZeroWhenAToolFailsButTheRunCompletes: acceptance criterion — an
// agent-handled tool failure does NOT change a completed run's exit code.
func TestExitZeroWhenAToolFailsButTheRunCompletes(t *testing.T) {
	root := t.TempDir() // no such file: the read_file call fails, the agent recovers
	failingRead := provider.ChatResponse{
		ToolCalls: []provider.ToolCall{{
			ID: "c1", Type: "function",
			Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"missing.txt"}`)},
		}},
	}
	finalAns := provider.ChatResponse{Content: "recovered fine"}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: failingRead}, {Response: finalAns}}}
	sess := newTestSession(t, caller, root)

	var stdout, stderr strings.Builder
	err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "read it")
	if err != nil {
		t.Fatalf("a run that recovered from a tool failure must succeed: %v", err)
	}
	if got := exitCodeFor(err); got != 0 {
		t.Fatalf("exit = %d, want 0", got)
	}
	if stdout.String() != "recovered fine\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestExitOneWhenTheProviderFails: a turn that began and failed is 1, never 2.
func TestExitOneWhenTheProviderFails(t *testing.T) {
	sess := newTestSession(t, errCaller{err: errors.New("model unreachable")}, t.TempDir())
	var stdout, stderr strings.Builder
	err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "anything")
	if !errors.Is(err, errOneShotFailed) {
		t.Fatalf("want errOneShotFailed, got %v", err)
	}
	if got := exitCodeFor(err); got != 1 {
		t.Fatalf("exit = %d, want 1 — a run failure is never caller misuse", got)
	}
}

// TestExitOneWhenTheAnswerIsEmpty covers both the text path (plain error) and
// the machine path (errOneShotFailed after the record is written).
func TestExitOneWhenTheAnswerIsEmpty(t *testing.T) {
	for _, machine := range []bool{false, true} {
		caller := &scriptCaller{responses: []agent.ModelResult{{Response: provider.ChatResponse{Content: "  \n"}}}}
		sess := newTestSession(t, caller, t.TempDir())
		var stdout, stderr strings.Builder
		if machine {
			sess.machine = newMachineWriter(&stdout, outputJSON)
		}
		err := runOneShot(context.Background(), &stdout, &stderr, nil, sess, "anything")
		if err == nil {
			t.Fatalf("machine=%v: empty answer must be an error", machine)
		}
		if got := exitCodeFor(err); got != 1 {
			t.Fatalf("machine=%v: exit = %d, want 1", machine, got)
		}
		if machine {
			m := decodeResult(t, strings.TrimSuffix(stdout.String(), "\n"))
			var recErr struct{ Code string }
			if uerr := json.Unmarshal(m["error"], &recErr); uerr != nil || recErr.Code != "empty_answer" {
				t.Errorf("record error = %s, want empty_answer", m["error"])
			}
		}
	}
}

// TestExitOneWhenPreRunProviderProbeFails (spec R3): a provider failure during
// pre-run bootstrap is a provider failure (1), never caller misuse (2) — and
// in a machine mode it still writes the provider_unavailable record.
func TestExitOneWhenPreRunProviderProbeFails(t *testing.T) {
	// A dead port: listen, note the address, close.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadURL := "http://" + l.Addr().String()
	_ = l.Close()

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"local": {"base_url": "` + deadURL + `", "api_format": "openai-compat", "timeout": "1s"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	runErr := run([]string{"-config", configPath, "-root", root, "-p", "say done", "-output-format", "json",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"},
		stdin, stdout, stderr)
	if runErr == nil {
		t.Fatal("a dead backend must fail the run")
	}
	if got := exitCodeFor(runErr); got != 1 {
		t.Fatalf("exit = %d (err %v), want 1 — pre-run provider failure is a provider failure", got, runErr)
	}
	out := readRunTestFile(t, stdout)
	if strings.TrimSpace(out) == "" {
		t.Fatalf("machine mode must write the provider_unavailable record; stderr:\n%s", readRunTestFile(t, stderr))
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one result line, got %d:\n%s", len(lines), out)
	}
	m := decodeResult(t, lines[0])
	var recErr struct{ Code string }
	if uerr := json.Unmarshal(m["error"], &recErr); uerr != nil || recErr.Code != "provider_unavailable" {
		t.Errorf("record error = %s, want provider_unavailable", m["error"])
	}
}

// An explicit dispatch route has its own capability preflight. A backend
// failure there is still a provider failure, so machine mode must receive the
// same single provider_unavailable result as a failure on the primary route.
func TestDispatchPreflightProviderFailureWritesResultAndExitsOne(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"agent-model"},{"id":"dispatch-model"}]}`))
		case "/v1/chat/completions":
			http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"local": {"base_url": "` + server.URL + `", "api_format": "openai-compat", "timeout": "1s"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "analysis": {"name": "dispatch-model", "provider": "local", "type": "dense", "context_window": 32768}
  },
  "defaults": {"agent": "agent", "analysis": "analysis"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	runErr := run([]string{"-config", configPath, "-root", root, "-p", "say done", "-output-format", "json",
		"-dispatch", "-dispatch-role", "analysis", "-no-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"},
		stdin, stdout, stderr)
	if got := exitCodeFor(runErr); got != 1 {
		t.Fatalf("exit = %d (err %v), want 1", got, runErr)
	}
	out := readRunTestFile(t, stdout)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 || strings.TrimSpace(lines[0]) == "" {
		t.Fatalf("dispatch preflight failure must write exactly one result line:\n%s", out)
	}
	m := decodeResult(t, lines[0])
	var recErr struct{ Code string }
	if err := json.Unmarshal(m["error"], &recErr); err != nil || recErr.Code != "provider_unavailable" {
		t.Errorf("record error = %s, want provider_unavailable", m["error"])
	}
}

// TestHeadlessProviderRejectionCodes wires the real openai-compat client,
// router and runtime to a stub server whose chat endpoint refuses the request
// (#740). A 4xx other than 401, 403 and 429 is provider_rejected in both the
// run.failed event and the golem.result.v1 record; the message is the
// existing diagnostic, unchanged, and the payload has no other field.
func TestHeadlessProviderRejectionCodes(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "provider_rejected"},
		{http.StatusNotFound, "provider_rejected"},
		{http.StatusRequestEntityTooLarge, "provider_rejected"},
		{http.StatusUnprocessableEntity, "provider_rejected"},
		{http.StatusTooManyRequests, "provider_unavailable"},
		{http.StatusUnauthorized, "internal"},
		{http.StatusForbidden, "internal"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			var chatPosts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/models":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"agent-model"}]}`))
				case "/v1/chat/completions":
					chatPosts.Add(1)
					http.Error(w, "stub refused", tc.status)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)

			root := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "models.json")
			configJSON := `{
  "providers": {"local": {"base_url": "` + server.URL + `", "api_format": "openai-compat", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
			if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
				t.Fatal(err)
			}
			stdin, stdout, stderr := runTestFiles(t)
			runErr := run([]string{"-config", configPath, "-root", root, "-base-url", server.URL,
				"-p", "say done", "-output-format", "stream-json",
				"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"},
				stdin, stdout, stderr)
			if got := chatPosts.Load(); got != 1 {
				t.Fatalf("chat POSTs = %d, want 1; stderr:\n%s", got, readRunTestFile(t, stderr))
			}
			if got := exitCodeFor(runErr); got != 1 {
				t.Fatalf("exit = %d (err %v), want 1", got, runErr)
			}

			lines := strings.Split(strings.TrimSuffix(readRunTestFile(t, stdout), "\n"), "\n")
			var failed []map[string]json.RawMessage
			for _, line := range lines[:len(lines)-1] {
				var ev struct {
					Type    string                     `json:"type"`
					Payload map[string]json.RawMessage `json:"payload"`
				}
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					t.Fatalf("event line %q: %v", line, err)
				}
				if ev.Type == "run.failed" {
					failed = append(failed, ev.Payload)
				}
			}
			if len(failed) != 1 || len(failed[0]) != 2 {
				t.Fatalf("run.failed payloads = %v, want one with exactly code and message", failed)
			}
			var code, message string
			if json.Unmarshal(failed[0]["code"], &code) != nil || json.Unmarshal(failed[0]["message"], &message) != nil {
				t.Fatalf("run.failed payload = %v", failed[0])
			}
			wantMessage := fmt.Sprintf("provider: openaicompat: chat stream: openaicompat: %d %s: stub refused", tc.status, http.StatusText(tc.status))
			if code != tc.code || message != wantMessage {
				t.Fatalf("run.failed = %q / %q, want %q / %q", code, message, tc.code, wantMessage)
			}

			rec := decodeResult(t, lines[len(lines)-1])
			var status string
			var recErr struct{ Code, Message string }
			if json.Unmarshal(rec["status"], &status) != nil || json.Unmarshal(rec["error"], &recErr) != nil {
				t.Fatalf("record = %v", rec)
			}
			if status != "error" || recErr.Code != code || recErr.Message != message {
				t.Fatalf("record status %q error %+v, want error with the event's code and message", status, recErr)
			}
		})
	}
}

// TestHeadlessOllamaProviderRejection is the Ollama sibling of
// TestHeadlessProviderRejectionCodes: the native client's 404 for a model
// the server does not have keeps its status through provider/ollama.go and
// the router, so the run fails with provider_rejected (#740).
func TestHeadlessOllamaProviderRejection(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var chatPosts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = w.Write([]byte(`{"models":[{"name":"agent-model"}]}`))
		case "/api/show":
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion","tools"]}`))
		case "/api/chat":
			chatPosts.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"local": {"base_url": "` + server.URL + `", "api_format": "ollama", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	runErr := run([]string{"-config", configPath, "-root", root, "-ollama-url", server.URL,
		"-p", "say done", "-output-format", "json",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"},
		stdin, stdout, stderr)
	if got := chatPosts.Load(); got != 1 {
		t.Fatalf("chat POSTs = %d, want 1; stderr:\n%s", got, readRunTestFile(t, stderr))
	}
	if got := exitCodeFor(runErr); got != 1 {
		t.Fatalf("exit = %d (err %v), want 1", got, runErr)
	}
	rec := decodeResult(t, strings.TrimSpace(readRunTestFile(t, stdout)))
	var recErr struct{ Code, Message string }
	if err := json.Unmarshal(rec["error"], &recErr); err != nil {
		t.Fatalf("record error = %s: %v", rec["error"], err)
	}
	const wantMessage = "provider: ollama: chat stream: ollama: HTTP 404: model not found"
	if recErr.Code != "provider_rejected" || recErr.Message != wantMessage {
		t.Fatalf("record error = %+v, want provider_rejected / %q", recErr, wantMessage)
	}
}
