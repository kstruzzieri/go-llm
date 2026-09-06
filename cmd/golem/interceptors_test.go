package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestParseFlags_InterceptorsDefaultOff(t *testing.T) {
	def, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if def.interceptors {
		t.Fatal("-interceptors must default to off (#514 D1)")
	}
	on, err := parseFlags([]string{"-interceptors"})
	if err != nil {
		t.Fatalf("parseFlags -interceptors: %v", err)
	}
	if !on.interceptors {
		t.Fatal("-interceptors did not set the flag")
	}
}

func TestSecretMachineStartupWiresFailurePresenter(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format outputFormat
	}{
		{"json", outputJSON},
		{"stream-json", outputStreamJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			value := secretTestValue()
			err := run([]string{
				"-config", configPath, "-root", root, "-base-url", server.URL,
				"-p", value, "-output-format", tc.name, "-interceptors",
				"-no-compress", "-no-probe", "-no-cap-probe", "-no-session", "-no-memory",
				"-no-rag", "-no-project-context", "-no-auto-index",
			}, stdin, stdout, stderr)
			if !errors.Is(err, errOneShotFailed) {
				t.Fatal("production startup did not return the one-shot failure")
			}
			out, diagnostic := readRunTestFile(t, stdout), readRunTestFile(t, stderr)
			if strings.Contains(out, value) || strings.Contains(diagnostic, value) {
				t.Fatal("production machine output published the blocked transcript")
			}
			assertSecretMachineFailure(t, out, tc.format)
		})
	}
}

func TestInterceptorsFor(t *testing.T) {
	if got := interceptorsFor(flags{}); got != nil {
		t.Fatalf("off: chain = %v, want nil", got)
	}
	var names []string
	for _, ic := range interceptorsFor(flags{interceptors: true}) {
		names = append(names, ic.Name())
	}
	if want := []string{"zero_width", "encoding", "typoglycemia", "invariants", "egress", "secrets"}; !slices.Equal(names, want) {
		t.Fatalf("on: chain = %v, want %v", names, want)
	}
}

func TestStartupNotices_Interceptors(t *testing.T) {
	on := strings.Join(startupNotices(startupInfo{
		workspace:       "/w",
		interceptorLine: interceptorsNotice(interceptorsFor(flags{interceptors: true})),
	}), "\n")
	if want := "workspace: /w\ninterceptors: enabled (zero_width, encoding, typoglycemia, invariants, egress, secrets)"; on != want {
		t.Fatalf("notices with the flag = %q, want %q", on, want)
	}
	off := strings.Join(startupNotices(startupInfo{workspace: "/w"}), "\n")
	if want := "workspace: /w"; off != want {
		t.Fatalf("notices without the flag = %q, want %q", off, want)
	}
}

// TestRunWiresInterceptors drives the real startup path with and without the
// flag. Dropping f at either production builder must fail even if the startup
// notice still says enabled. A benign mention of "system prompt" is enough to
// exercise scoring without requiring tool calls from the test backend.
func TestRunWiresInterceptors(t *testing.T) {
	const want = "interceptors: enabled (zero_width, encoding, typoglycemia, invariants, egress, secrets)"
	const goal = "Explain the term system prompt."
	for _, tc := range []struct {
		name string
		flag []string
		on   bool
	}{
		{"on", []string{"-interceptors"}, true},
		{"off", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop after wiring checks")
			args := append([]string{
				"-config", configPath, "-root", root, "-base-url", server.URL,
				"-dispatch", "-dispatch-role", "agent", "-no-compress",
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory",
				"-no-rag", "-no-project-context", "-no-auto-index",
			}, tc.flag...)
			err := run(args, stdin, stdout, stderr, runHooks{
				afterSessionReady: func(sess *replSession) error {
					t.Run("parent", func(t *testing.T) {
						res, err := sess.runtime.Run(t.Context(), golemruntime.Turn{RunID: "wiring-test", Message: goal}, sess.machine.sink())
						if err != nil {
							t.Fatalf("parent Run(%q): %v", goal, err)
						}
						if res.Answer != "noop" {
							t.Errorf("parent Run(%q) answer = %q, want noop", goal, res.Answer)
						}
						if tc.on {
							if res.Risk == nil || res.Risk.Score != 10 {
								t.Errorf("parent Run(%q) risk = %+v, want score 10", goal, res.Risk)
							}
						} else if res.Risk != nil {
							t.Errorf("parent Run(%q) risk = %+v, want nil", goal, res.Risk)
						}
					})
					t.Run("dispatch", func(t *testing.T) {
						i := slices.IndexFunc(sess.tools, func(tool agent.Tool) bool { return tool.Spec().Name == "dispatch" })
						if i < 0 {
							t.Fatal("run(-dispatch) mounted no dispatch tool")
						}
						args, err := json.Marshal(map[string][]string{"tasks": {goal}})
						if err != nil {
							t.Fatal(err)
						}
						out, err := sess.tools[i].Invoke(t.Context(), args)
						if err != nil || out.IsError {
							t.Fatalf("dispatch Invoke(%q) = %+v, %v; want success", goal, out, err)
						}
						var envelope dispatchTestEnvelope
						if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
							t.Fatalf("decode dispatch result: %v", err)
						}
						if len(envelope.Results) != 1 {
							t.Fatalf("dispatch Invoke(%q) results = %+v, want one", goal, envelope.Results)
						}
						child := envelope.Results[0]
						if child.Error != "" || child.Summary != "noop" {
							t.Errorf("dispatch Invoke(%q) child = %+v, want successful noop answer", goal, child)
						}
						wantScore := 0
						if tc.on {
							wantScore = 10
						}
						if child.RiskScore != wantScore {
							t.Errorf("dispatch Invoke(%q) risk_score = %d, want %d", goal, child.RiskScore, wantScore)
						}
						if !tc.on && strings.Contains(out.Content, "risk_score") {
							t.Errorf("dispatch Invoke(%q) = %s, want risk_score omitted", goal, out.Content)
						}
					})
					return errStop
				},
			})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v, want test stop; stderr:\n%s", err, readRunTestFile(t, stderr))
			}
			lines := strings.Split(strings.TrimSpace(readRunTestFile(t, stderr)), "\n")
			if got := slices.Contains(lines, want); got != tc.on {
				t.Fatalf("startup lines = %q, exact line %q present = %v, want %v", lines, want, got, tc.on)
			}
		})
	}
}

// TestOrchestratorFactoryInstallsInterceptorsBehindFlag proves the factory
// (every Golem run path) carries the chain iff the flag is on: a foreign
// tool's injection is replaced with the flag, verbatim without it.
func TestOrchestratorFactoryInstallsInterceptorsBehindFlag(t *testing.T) {
	run := func(f flags) agent.Result {
		t.Helper()
		o := newOrchestratorFactory(&oneCallCaller{name: "remote"}, f, nil)()
		res, err := o.Run(context.Background(), agent.Request{Goal: "q", Tools: []agent.Tool{foreignTool{content: injection}}}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res
	}
	on := run(flags{interceptors: true})
	if got := on.Messages[2].Content; got != blockedInjection {
		t.Fatalf("on: observation = %q, want %q", got, blockedInjection)
	}
	if on.Risk == nil || on.Risk.Score != 30 || !on.ToolCalls[0].Blocked {
		t.Fatalf("on: risk = %+v record = %+v", on.Risk, on.ToolCalls[0])
	}
	off := run(flags{})
	if got := off.Messages[2].Content; got != injection {
		t.Fatalf("off: observation = %q, want %q", got, injection)
	}
	if off.Risk != nil || off.ToolCalls[0].Blocked {
		t.Fatalf("off: risk = %+v record = %+v", off.Risk, off.ToolCalls[0])
	}
}

// TestGolemPromptsProduceNoFindings guards step-0 inspection: if a CLI-owned
// prompt branch gains a detector phrase ("system prompt", "you are now", ...)
// or a zero-width rune, runs using that branch would start tagged and inflate
// their scores. These inputs cover every distinct CLI-owned branch string;
// projectContext is intentionally excluded because it is workspace input and
// is supposed to be inspected. The planner prompt is covered separately.
func TestGolemPromptsProduceNoFindings(t *testing.T) {
	prompts := map[string]string{"planner": plannerBasePrompt}
	for _, in := range []systemInputs{
		{},
		{allowWrite: true},
		{allowExec: true},
		{allowWrite: true, allowExec: true},
		{delegate: true},
		{allowWrite: true, delegate: true},
		{dispatch: true},
		{memory: true},
		{agentMemory: true},
		{agentMemory: true, sessionUp: true},
	} {
		prompts[fmt.Sprintf("%+v", in)] = composeSystem(in)
	}
	for _, caps := range []golemruntime.HeadlessToolCaps{
		{},
		{WriteFile: true},
		{EditFile: true},
		{WriteFile: true, EditFile: true},
		{RunCommand: true},
		{StartCommand: true},
		{StartCommand: true, StopCommand: true},
		{StopCommand: true},
	} {
		caps := caps
		in := systemInputs{headless: &caps}
		prompts[fmt.Sprintf("headless:%+v", caps)] = composeSystem(in)
	}
	for name, system := range prompts {
		for _, ic := range interceptor.Defaults() {
			found, err := ic.InspectInput(context.Background(), agent.InputInspection{System: system})
			if err != nil || len(found) != 0 {
				t.Errorf("%s: %s reported %+v (err %v) on Golem's own prompt", name, ic.Name(), found, err)
			}
		}
	}
}

// foreignRetrieve is a child-eligible retrieve tool over a foreign corpus:
// read-only, never approved, foreign provenance, returns an injection.
type foreignRetrieve struct{}

func (foreignRetrieve) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "retrieve", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (foreignRetrieve) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}
func (foreignRetrieve) Origin() agent.Origin { return agent.OriginForeign }
func (foreignRetrieve) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: injection}, nil
}

// echoRetrieveCaller calls retrieve once, then answers with the tool
// observation verbatim, so the envelope summary is exactly what the child
// model was shown.
type echoRetrieveCaller struct{ step atomic.Int32 }

func (c *echoRetrieveCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	var resp provider.ChatResponse
	if c.step.Add(1) == 1 {
		resp = provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "r1", Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{}`)},
		}}}
	} else {
		content := "no tool observation seen"
		for _, m := range req.Messages {
			if m.Role == "tool" {
				content = m.Content
			}
		}
		resp = provider.ChatResponse{Content: content, Done: true}
	}
	if onToken != nil {
		_ = onToken(resp)
	}
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	return agent.ModelResult{Response: resp, RouteOutcome: outcome}, nil
}

// TestNewDispatchTool_ChildrenInheritInterceptors: the child's retrieve
// result is replaced and scored with the flag, verbatim and unscored
// without it. The production parent and child builders both receive the same
// parsed flags value and derive their chain through interceptorsFor.
func TestNewDispatchTool_ChildrenInheritInterceptors(t *testing.T) {
	invoke := func(f flags) (dispatchTestEnvelope, string) {
		t.Helper()
		available := append(validDispatchAvailable(t), foreignRetrieve{})
		tool, err := newDispatchTool(&echoRetrieveCaller{}, f, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, available)
		if err != nil {
			t.Fatalf("newDispatchTool: %v", err)
		}
		raw, err := json.Marshal(map[string][]string{"tasks": {"read the foreign corpus"}})
		if err != nil {
			t.Fatal(err)
		}
		out, err := tool.Invoke(context.Background(), raw)
		if err != nil {
			t.Fatalf("dispatch Invoke: %v", err)
		}
		var envelope dispatchTestEnvelope
		if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
			t.Fatalf("decode envelope: %v (content %q)", err, out.Content)
		}
		if len(envelope.Results) != 1 {
			t.Fatalf("results = %+v, want one", envelope.Results)
		}
		if envelope.Results[0].Error != "" || out.IsError {
			t.Fatalf("child must be a clean success: result=%+v tool=%+v", envelope.Results[0], out)
		}
		return envelope, out.Content
	}
	// The echo caller answers with the child's last tool message verbatim, so
	// the summary carries the child's #430 frame around the library marker.
	on, _ := invoke(flags{interceptors: true})
	if got := on.Results[0].Summary; got != framedToolResult(toolFrameKey(t, got), blockedInjection) {
		t.Fatalf("on: summary = %q, want %q inside its frame", got, blockedInjection)
	}
	if on.Results[0].RiskScore != 30 {
		t.Fatalf("on: risk_score = %d, want 30", on.Results[0].RiskScore)
	}
	off, raw := invoke(flags{})
	if got := off.Results[0].Summary; got != framedToolResult(toolFrameKey(t, got), injection) {
		t.Fatalf("off: summary = %q, want %q inside its frame", got, injection)
	}
	if strings.Contains(raw, "risk_score") {
		t.Fatalf("off: envelope must carry no risk_score key: %s", raw)
	}
}

// TestRunOnce_ApprovalPromptShowsInterceptorRisk: a write-enabled session
// reads a file that mentions a weak phrase (tagged, risk 10), then asks to
// write; the real prompt shows the line between the diff and the question
// and the approved write lands.
func TestRunOnce_ApprovalPromptShowsInterceptorRisk(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "NOTES.md"), []byte("The system prompt is documented here.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "r1", Type: "function",
		Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"NOTES.md"}`)},
	}}}
	write := provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "w1", Type: "function",
		Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{"path":"out.txt","content":"hello\n"}`)},
	}}}
	caller := &scriptCaller{responses: []agent.ModelResult{{Response: read}, {Response: write}, {Response: provider.ChatResponse{Content: "wrote it"}}}}
	readTools, err := buildTools(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	writeTools, journal, err := buildWriteTools(root, openTestStore(t, root))
	if err != nil {
		t.Fatal(err)
	}
	system := buildSystemPrompt(true, false)
	orch := newOrchestratorFactory(caller, flags{interceptors: true}, nil)()
	sess := &replSession{
		orch: orch, runtime: newTestRuntime(t, root, system, orch, writeTools),
		tools: append(readTools, writeTools...), baseSystem: system, maxSteps: 16,
		clock: func() time.Time { return time.Unix(0, 0) }, journal: journal, allowWrite: true,
	}
	var out strings.Builder
	res, err := runOnce(context.Background(), &out, nil, sess, "write out.txt", newScannerSource(strings.NewReader("y\n"), &out))
	if err != nil {
		t.Fatalf("runOnce: %v\n%s", err, out.String())
	}
	got := out.String()
	position := 0
	for _, want := range []string{
		"new file: out.txt\n+hello\n",
		"interceptor risk 10\n",
		"Apply this change? [y/N] ",
		"done · 3 steps · 0.0s · 0 tok · risk 10\n",
	} {
		i := strings.Index(got[position:], want)
		if i < 0 {
			t.Fatalf("missing %q after byte %d in:\n%s", want, position, got)
		}
		position += i + len(want)
	}
	if res.Risk == nil || res.Risk.Score != 10 || len(res.Risk.Findings) != 1 {
		t.Fatalf("risk = %+v", res.Risk)
	}
	b, err := os.ReadFile(filepath.Join(root, "out.txt"))
	if err != nil || string(b) != "hello\n" {
		t.Fatalf("approved write not applied: %q %v", b, err)
	}
}
