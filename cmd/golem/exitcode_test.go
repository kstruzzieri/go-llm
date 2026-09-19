package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
