package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

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

func TestInterceptorsFor(t *testing.T) {
	if got := interceptorsFor(flags{}); got != nil {
		t.Fatalf("off: chain = %v, want nil", got)
	}
	var names []string
	for _, ic := range interceptorsFor(flags{interceptors: true}) {
		names = append(names, ic.Name())
	}
	if want := []string{"zero_width", "encoding", "typoglycemia"}; !slices.Equal(names, want) {
		t.Fatalf("on: chain = %v, want %v", names, want)
	}
}

func TestStartupNotices_Interceptors(t *testing.T) {
	on := strings.Join(startupNotices(startupInfo{
		workspace:       "/w",
		interceptorLine: interceptorsNotice(interceptorsFor(flags{interceptors: true})),
	}), "\n")
	if want := "workspace: /w\ninterceptors: enabled (zero_width, encoding, typoglycemia)"; on != want {
		t.Fatalf("notices with the flag = %q, want %q", on, want)
	}
	off := strings.Join(startupNotices(startupInfo{workspace: "/w"}), "\n")
	if want := "workspace: /w"; off != want {
		t.Fatalf("notices without the flag = %q, want %q", off, want)
	}
}

// TestRunWiresInterceptorsStartupNotice drives the real startup path to its
// post-construction hook, with and without the flag. It pins both the
// run-local line calculation and the startupInfo literal assignment; the
// formatter-only test above cannot, and a line computed unconditionally in
// run would pass the formatter test's off case.
func TestRunWiresInterceptorsStartupNotice(t *testing.T) {
	const want = "interceptors: enabled (zero_width, encoding, typoglycemia)"
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
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop after startup")
			args := append([]string{
				"-config", configPath, "-root", root,
				"-no-probe", "-no-cap-probe", "-no-session", "-no-memory",
				"-no-rag", "-no-project-context", "-no-auto-index",
			}, tc.flag...)
			err := run(args, stdin, stdout, stderr, runHooks{
				afterSessionReady: func(*replSession) error { return errStop },
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
	on, _ := invoke(flags{interceptors: true})
	if got := on.Results[0].Summary; got != blockedInjection {
		t.Fatalf("on: summary = %q, want %q", got, blockedInjection)
	}
	if on.Results[0].RiskScore != 30 {
		t.Fatalf("on: risk_score = %d, want 30", on.Results[0].RiskScore)
	}
	off, raw := invoke(flags{})
	if got := off.Results[0].Summary; got != injection {
		t.Fatalf("off: summary = %q, want %q", got, injection)
	}
	if strings.Contains(raw, "risk_score") {
		t.Fatalf("off: envelope must carry no risk_score key: %s", raw)
	}
}
