package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/provider"
)

type thinkSpecTool struct {
	t       *testing.T
	name    string
	calls   atomic.Int32
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (t *thinkSpecTool) Spec() agent.ToolSpec {
	t.calls.Add(1)
	if t.armed.Load() {
		t.once.Do(func() { close(t.entered) })
		select {
		case <-t.release:
		case <-time.After(10 * time.Second):
			t.t.Error("timed out waiting to release Tool.Spec")
		}
	}
	return agent.ToolSpec{Name: t.name}
}

func waitThink[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for /think barrier")
		var zero T
		return zero
	}
}

func (*thinkSpecTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (*thinkSpecTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func newThinkSession(t *testing.T, caller agent.ModelCaller, initial provider.ModelOptions, tool *thinkSpecTool) (*replSession, *thinkFakeReg) {
	t.Helper()
	var host []agent.Tool
	if tool != nil {
		host = append(host, tool)
	}
	sess := newMountSession(t, caller, t.TempDir(), host...)
	if err := sess.runtime.Replace(sess.baseSystem, sess.tools[sess.readToolCount:], initial); err != nil {
		t.Fatalf("set initial runtime options: %v", err)
	}
	if tool != nil {
		tool.calls.Store(0)
	}
	key := provider.ModelKey{Provider: "test", Model: "thinking"}
	reg := &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{key: provider.ThinkToggle}}
	sess.thinkModels = reg
	sess.thinkChain = []string{"test/thinking"}
	return sess, reg
}

func assertThinkOptions(t *testing.T, got provider.ModelOptions, wantThink *bool, wantEffort string) {
	t.Helper()
	if (got.Think == nil) != (wantThink == nil) {
		t.Fatalf("Think = %v, want %v", got.Think, wantThink)
	}
	if got.Think != nil && *got.Think != *wantThink {
		t.Fatalf("Think = %v, want %v", *got.Think, *wantThink)
	}
	if got.ThinkEffort != wantEffort {
		t.Fatalf("ThinkEffort = %q, want %q", got.ThinkEffort, wantEffort)
	}
}

func assertUnrelatedThinkOptions(t *testing.T, got provider.ModelOptions) {
	t.Helper()
	if got.Temperature == nil || *got.Temperature != 0.25 || got.TopK == nil || *got.TopK != 7 ||
		got.NumPredict != 99 || len(got.Stop) != 1 || got.Stop[0] != "END" {
		t.Fatalf("unrelated options changed: %+v", got)
	}
}

func TestThinkStatusUsesRuntimeOptions(t *testing.T) {
	sess := newMountSession(t, &captureCaller{answer: "ok"}, t.TempDir())
	sess.startupModelOptions = provider.ModelOptions{Think: provider.Ptr(true), ThinkEffort: "high"}

	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think")
	if got := out.String(); got != "think: default (model decides)\n" {
		t.Fatalf("output = %q, want %q", got, "think: default (model decides)\n")
	}
}

func TestThinkStatusFormatting(t *testing.T) {
	falseValue, trueValue := false, true
	for _, tt := range []struct {
		name string
		opts provider.ModelOptions
		want string
	}{
		{name: "unset", opts: provider.ModelOptions{}, want: "think: default (model decides)"},
		{name: "off wins over stale effort", opts: provider.ModelOptions{Think: &falseValue, ThinkEffort: "high"}, want: "think: off"},
		{name: "on", opts: provider.ModelOptions{Think: &trueValue}, want: "think: on"},
		{name: "nil think with effort", opts: provider.ModelOptions{ThinkEffort: "low"}, want: "think: low"},
		{name: "true think with effort", opts: provider.ModelOptions{Think: &trueValue, ThinkEffort: "medium"}, want: "think: medium"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatThinkOptions(tt.opts); got != tt.want {
				t.Fatalf("formatThinkOptions() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestThinkCommandModes(t *testing.T) {
	falseValue, trueValue := false, true
	tests := []struct {
		name       string
		command    string
		initial    provider.ModelOptions
		wantOutput string
		wantThink  *bool
		wantEffort string
		wantLookup int
	}{
		{name: "off", command: "/think off", wantOutput: "think: off\n", wantThink: &falseValue, wantLookup: 1},
		{name: "on", command: "/think on", wantOutput: "think: on\n", wantThink: &trueValue, wantLookup: 1},
		{name: "low", command: "/think low", wantOutput: "think: low\n", wantThink: &trueValue, wantEffort: "low", wantLookup: 1},
		{name: "medium", command: "/think medium", wantOutput: "think: medium\n", wantThink: &trueValue, wantEffort: "medium", wantLookup: 1},
		{name: "high", command: "/think high", wantOutput: "think: high\n", wantThink: &trueValue, wantEffort: "high", wantLookup: 1},
		{name: "uppercase", command: "/think HIGH", wantOutput: "think: high\n", wantThink: &trueValue, wantEffort: "high", wantLookup: 1},
		{name: "default", command: "/think DEFAULT", initial: provider.ModelOptions{Think: &trueValue, ThinkEffort: "high"}, wantOutput: "think: default (model decides)\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initial := tt.initial
			initial.Temperature = provider.Ptr(0.25)
			initial.TopK = provider.Ptr(7)
			initial.NumPredict = 99
			initial.Stop = []string{"END"}
			tool := &thinkSpecTool{name: "host_tool"}
			sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, initial, tool)
			sess.startupModelOptions = provider.ModelOptions{Think: &falseValue}

			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, tt.command)
			if got := out.String(); got != tt.wantOutput {
				t.Fatalf("output = %q, want %q", got, tt.wantOutput)
			}
			assertThinkOptions(t, sess.runtime.ModelOptions(), tt.wantThink, tt.wantEffort)
			assertUnrelatedThinkOptions(t, sess.runtime.ModelOptions())
			if got := len(reg.lookupCalls); got != tt.wantLookup {
				t.Fatalf("metadata lookups = %d, want %d", got, tt.wantLookup)
			}
			if got := tool.calls.Load(); got != 1 {
				t.Fatalf("host tool Spec calls = %d, want 1 full replacement validation", got)
			}
		})
	}
}

func TestThinkDefaultAndStatusAreMetadataFree(t *testing.T) {
	trueValue := true
	for _, tc := range []struct {
		name   string
		models capChecker
	}{
		{name: "all none", models: &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{{Provider: "test", Model: "thinking"}: provider.ThinkNone}}},
		{name: "metadata absent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &thinkSpecTool{name: "host_tool"}
			initial := provider.ModelOptions{Temperature: provider.Ptr(0.25), TopK: provider.Ptr(7), NumPredict: 99, Stop: []string{"END"}, Think: &trueValue, ThinkEffort: "high"}
			sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, initial, tool)
			sess.thinkModels = tc.models

			var out strings.Builder
			for _, command := range []string{"/think default", "/think", "/think default"} {
				_, _ = dispatchSlash(context.Background(), &out, sess, command)
			}
			if got := out.String(); got != "think: default (model decides)\nthink: default (model decides)\nthink: default (model decides)\n" {
				t.Fatalf("output = %q", got)
			}
			assertThinkOptions(t, sess.runtime.ModelOptions(), nil, "")
			assertUnrelatedThinkOptions(t, sess.runtime.ModelOptions())
			if got := tool.calls.Load(); got != 1 {
				t.Fatalf("Spec calls = %d, want only the changed reset to publish", got)
			}
			if reg, ok := tc.models.(*thinkFakeReg); ok && (len(reg.lookupCalls) != 0 || len(reg.lookupAnyCalls) != 0) {
				t.Fatalf("default/status performed metadata lookup: Lookup=%v LookupAny=%v", reg.lookupCalls, reg.lookupAnyCalls)
			}
		})
	}
}

func TestThinkRejectsUnsupportedAndMalformedWithoutPublishing(t *testing.T) {
	t.Run("all resolved candidates are ThinkNone", func(t *testing.T) {
		for _, value := range []string{"off", "on", "low", "medium", "high"} {
			t.Run(value, func(t *testing.T) {
				tool := &thinkSpecTool{name: "host_tool"}
				sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
				key := provider.ModelKey{Provider: "test", Model: "thinking"}
				reg.byKey[key] = provider.ThinkNone

				var out strings.Builder
				_, _ = dispatchSlash(context.Background(), &out, sess, "/think "+value)
				if got := out.String(); got != "think: model test/thinking does not support thinking; -think ignored\n" {
					t.Fatalf("output = %q", got)
				}
				assertThinkOptions(t, sess.runtime.ModelOptions(), nil, "")
				if got := tool.calls.Load(); got != 0 {
					t.Fatalf("rejected request validated tools %d times", got)
				}
			})
		}
	})

	t.Run("malformed", func(t *testing.T) {
		trueValue := true
		tool := &thinkSpecTool{name: "host_tool"}
		sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{Think: &trueValue, ThinkEffort: "high"}, tool)
		var out strings.Builder
		for _, command := range []string{"/think auto", "/think reset", "/think high extra"} {
			_, _ = dispatchSlash(context.Background(), &out, sess, command)
		}
		if got := out.String(); got != "usage: /think [off|on|low|medium|high|default]\nusage: /think [off|on|low|medium|high|default]\nusage: /think [off|on|low|medium|high|default]\n" {
			t.Fatalf("output = %q", got)
		}
		assertThinkOptions(t, sess.runtime.ModelOptions(), &trueValue, "high")
		if len(reg.lookupCalls) != 0 || len(reg.lookupAnyCalls) != 0 || tool.calls.Load() != 0 {
			t.Fatalf("malformed request touched metadata/runtime: Lookup=%v LookupAny=%v Spec=%d", reg.lookupCalls, reg.lookupAnyCalls, tool.calls.Load())
		}
	})
}

func TestThinkUnchangedRegatesWithoutPublishing(t *testing.T) {
	trueValue := true
	tool := &thinkSpecTool{name: "host_tool"}
	sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{Think: &trueValue, ThinkEffort: "high"}, tool)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
	if got := out.String(); got != "think: high\n" {
		t.Fatalf("output = %q", got)
	}
	if len(reg.lookupCalls) != 1 || tool.calls.Load() != 0 {
		t.Fatalf("unchanged request: lookups=%d Spec=%d, want 1 and 0", len(reg.lookupCalls), tool.calls.Load())
	}
}

func TestThinkBareStatusAfterSettingHighIsReadOnly(t *testing.T) {
	tool := &thinkSpecTool{name: "host_tool"}
	sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think")
	if got := out.String(); got != "think: high\nthink: high\n" {
		t.Fatalf("output = %q", got)
	}
	if len(reg.lookupCalls) != 1 || tool.calls.Load() != 1 {
		t.Fatalf("lookups=%d Spec=%d, want only the setting command to touch them", len(reg.lookupCalls), tool.calls.Load())
	}
}

func TestThinkConsultsActiveChainAgain(t *testing.T) {
	tool := &thinkSpecTool{name: "host_tool"}
	sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
	key := provider.ModelKey{Provider: "test", Model: "thinking"}
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
	reg.byKey[key] = provider.ThinkNone
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think on")
	if got := out.String(); got != "think: high\nthink: model test/thinking does not support thinking; -think ignored\n" {
		t.Fatalf("output = %q", got)
	}
	trueValue := true
	assertThinkOptions(t, sess.runtime.ModelOptions(), &trueValue, "high")
	if len(reg.lookupCalls) != 2 || tool.calls.Load() != 1 {
		t.Fatalf("lookups=%d Spec=%d, want 2 and 1", len(reg.lookupCalls), tool.calls.Load())
	}
}

func TestThinkAcceptedChangeReachesNextRuntimeRequest(t *testing.T) {
	trueValue := true
	for _, tc := range []struct {
		name       string
		command    string
		wantOutput string
		wantThink  bool
	}{
		{name: "high to on clears effort", command: "/think on", wantOutput: "think: on\n", wantThink: true},
		{name: "high to off clears effort", command: "/think off", wantOutput: "think: off\n", wantThink: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := &captureCaller{answer: "ok"}
			initial := provider.ModelOptions{Temperature: provider.Ptr(0.25), TopK: provider.Ptr(7), NumPredict: 99, Stop: []string{"END"}, Think: &trueValue, ThinkEffort: "high"}
			sess, _ := newThinkSession(t, caller, initial, nil)
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, tc.command)
			if got := out.String(); got != tc.wantOutput {
				t.Fatalf("output = %q, want %q", got, tc.wantOutput)
			}
			if _, err := runOnce(context.Background(), &out, nil, sess, "next turn", nil); err != nil {
				t.Fatalf("runOnce: %v", err)
			}
			if caller.options.Think == nil || *caller.options.Think != tc.wantThink || caller.options.ThinkEffort != "" {
				t.Fatalf("next request think options = %+v", caller.options)
			}
			assertUnrelatedThinkOptions(t, caller.options)
		})
	}
}

func TestThinkReplacementFailurePreservesOptions(t *testing.T) {
	falseValue := false
	tool := &thinkSpecTool{name: "host_tool"}
	initial := provider.ModelOptions{Think: &falseValue}
	sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, initial, tool)
	tool.name = ""
	var out strings.Builder
	_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
	if got := out.String(); got != "think: golem: tool with empty name\n" {
		t.Fatalf("output = %q", got)
	}
	assertThinkOptions(t, sess.runtime.ModelOptions(), &falseValue, "")
	if tool.calls.Load() != 1 {
		t.Fatalf("Spec calls = %d, want 1", tool.calls.Load())
	}
}

func TestThinkUnavailableStates(t *testing.T) {
	t.Run("runtime absent", func(t *testing.T) {
		sess := &replSession{}
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think")
		if got := out.String(); got != "think: runtime unavailable\n" {
			t.Fatalf("output = %q", got)
		}
	})

	t.Run("model metadata absent", func(t *testing.T) {
		tool := &thinkSpecTool{name: "host_tool"}
		sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
		sess.thinkModels = nil
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
		if got := out.String(); got != "think: model configuration unavailable\n" {
			t.Fatalf("output = %q", got)
		}
		assertThinkOptions(t, sess.runtime.ModelOptions(), nil, "")
		if tool.calls.Load() != 0 {
			t.Fatalf("unavailable metadata published a replacement")
		}
	})

	t.Run("closed runtime", func(t *testing.T) {
		sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, nil)
		if err := sess.runtime.Close(); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think")
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think default")
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
		if got := out.String(); got != "think: default (model decides)\nthink: default (model decides)\nthink: golem: runtime is closed\n" {
			t.Fatalf("output = %q", got)
		}
		assertThinkOptions(t, sess.runtime.ModelOptions(), nil, "")
	})

	t.Run("closed runtime unchanged request", func(t *testing.T) {
		trueValue := true
		sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{Think: &trueValue, ThinkEffort: "high"}, nil)
		if err := sess.runtime.Close(); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
		if got := out.String(); got != "think: high\n" {
			t.Fatalf("output = %q", got)
		}
		assertThinkOptions(t, sess.runtime.ModelOptions(), &trueValue, "high")
	})
}

func TestThinkCancellationBeforeReplacePreservesOptions(t *testing.T) {
	falseValue := false
	tool := &thinkSpecTool{name: "host_tool"}
	sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{Think: &falseValue}, tool)
	ctx, cancel := context.WithCancel(context.Background())
	reg.onLookup = cancel
	var out strings.Builder
	_, _ = dispatchSlash(ctx, &out, sess, "/think high")
	if got := out.String(); got != "think: context canceled\n" {
		t.Fatalf("output = %q", got)
	}
	assertThinkOptions(t, sess.runtime.ModelOptions(), &falseValue, "")
	if len(reg.lookupCalls) != 1 || tool.calls.Load() != 0 {
		t.Fatalf("lookups=%d Spec=%d, want 1 and 0", len(reg.lookupCalls), tool.calls.Load())
	}
}

func TestThinkAlreadyCanceledSkipsResolution(t *testing.T) {
	tool := &thinkSpecTool{name: "host_tool"}
	sess, reg := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out strings.Builder
	_, _ = dispatchSlash(ctx, &out, sess, "/think high")
	if got := out.String(); got != "think: context canceled\n" {
		t.Fatalf("output = %q", got)
	}
	if len(reg.lookupCalls) != 0 || len(reg.lookupAnyCalls) != 0 || tool.calls.Load() != 0 {
		t.Fatalf("canceled request touched metadata/runtime: Lookup=%v LookupAny=%v Spec=%d", reg.lookupCalls, reg.lookupAnyCalls, tool.calls.Load())
	}
}

func TestThinkCancellationAfterReplaceBeginsDoesNotRollback(t *testing.T) {
	tool := &thinkSpecTool{t: t, name: "host_tool", entered: make(chan struct{}), release: make(chan struct{})}
	sess, _ := newThinkSession(t, &captureCaller{answer: "ok"}, provider.ModelOptions{}, tool)
	tool.armed.Store(true)
	unblock := sync.OnceFunc(func() { close(tool.release) })
	t.Cleanup(unblock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		_, _ = dispatchSlash(ctx, &out, sess, "/think high")
		done <- out.String()
	}()
	waitThink(t, tool.entered)
	cancel()
	unblock()
	if got := waitThink(t, done); got != "think: high\n" {
		t.Fatalf("output = %q", got)
	}
	trueValue := true
	assertThinkOptions(t, sess.runtime.ModelOptions(), &trueValue, "high")
}

func TestThinkHelpEntry(t *testing.T) {
	if !strings.Contains(golemHelp, "/think [off|on|low|medium|high|default]") {
		t.Fatalf("help does not list /think:\n%s", golemHelp)
	}
}

func TestStartupWiresThinkMetadata(t *testing.T) {
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after session wiring")
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag", "-no-project-context", "-no-auto-index"},
		stdin, stdout, stderr, runHooks{
			startAutoIndex: func() func() { return func() {} },
			afterSessionReady: func(sess *replSession) error {
				if sess.thinkModels == nil {
					t.Fatal("startup did not retain the model registry")
				}
				if len(sess.thinkChain) != 1 || sess.thinkChain[0] != "test/agent-model" {
					t.Fatalf("think chain = %v, want [test/agent-model]", sess.thinkChain)
				}
				return errStop
			},
		})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderr))
	}
}

// thinkTurnCaller retains every request; the hook synchronizes assertions with
// the actual model boundary, after slash dispatch and before the next answer.
type thinkTurnCaller struct {
	captureCaller
	requests []provider.ChatRequest
	before   func(context.Context)
}

func (c *thinkTurnCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if c.before != nil {
		c.before(ctx)
	}
	return c.captureCaller.Chat(ctx, req, onToken)
}

func TestThinkStartupFullChainChangesWireRequest(t *testing.T) {
	configPath, root, bodies := dispatchOneShotHarness(t)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), `"capabilities": ["chat", "generate", "stream", "tool_call"]`, `"think_mode": "toggle", "fallbacks": ["weak"], "capabilities": ["chat", "generate", "stream", "tool_call"]`, 1)
	text = strings.Replace(text, `"capabilities": ["chat", "generate", "stream"]`, `"think_mode": "none", "capabilities": ["chat", "generate", "stream", "tool_call"]`, 1)
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	if _, err := stdin.WriteString("first goal\n/think high\nsecond goal\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	err = run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session", "-no-memory", "-no-rag", "-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor"}, stdin, stdout, stderr, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(sess *replSession) error {
			if !reflect.DeepEqual(sess.thinkChain, []string{"test/agent-model", "test/weak-model"}) {
				t.Fatalf("startup think chain = %v, want full configured chain", sess.thinkChain)
			}
			if sess.thinkModels == nil {
				t.Fatal("startup registry missing")
			}
			for _, tc := range []struct {
				model string
				mode  provider.ThinkMode
			}{{"agent-model", provider.ThinkToggle}, {"weak-model", provider.ThinkNone}} {
				profile, err := sess.thinkModels.Lookup(context.Background(), provider.ModelKey{Provider: "test", Model: tc.model})
				if err != nil || profile == nil || profile.ThinkMode != tc.mode {
					t.Fatalf("startup registry profile(%q) = %+v, %v; want mode %v", tc.model, profile, err, tc.mode)
				}
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run: %v; stderr=%s", err, readRunTestFile(t, stderr))
	}
	requests := bodies()
	if len(requests) != 2 {
		t.Fatalf("wire requests = %d, want 2; stdout=%s", len(requests), readRunTestFile(t, stdout))
	}
	for i, body := range requests {
		var req struct {
			ReasoningEffort string          `json:"reasoning_effort"`
			Kwargs          map[string]bool `json:"chat_template_kwargs"`
		}
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		if i == 0 && (req.ReasoningEffort != "" || len(req.Kwargs) != 0) {
			t.Errorf("first request thinking = %+v, want no wire override", req)
		}
		if i == 1 && (req.ReasoningEffort != "high" || !req.Kwargs["enable_thinking"]) {
			t.Errorf("next request thinking = %+v, want high/enabled", req)
		}
	}
	if !strings.Contains(readRunTestFile(t, stdout), "think: high\n") {
		t.Fatal("startup REPL did not acknowledge the setting")
	}
}

func TestThinkREPLPreservesBufferedGoalsHistoryAndWrites(t *testing.T) {
	caller := &thinkTurnCaller{captureCaller: captureCaller{answer: "answer"}}
	sess := newSessionedTestSession(t, caller, t.TempDir(), "workspace:think-history")
	sess.readToolCount = len(sess.tools)
	key := provider.ModelKey{Provider: "test", Model: "thinking"}
	reg := &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{key: provider.ThinkToggle}}
	reg.onLookup = func() {
		if len(reg.lookupCalls) == 3 {
			reg.byKey[key] = provider.ThinkNone
		}
	}
	sess.thinkModels = reg
	sess.thinkChain = []string{"test/thinking"}
	sess.session.summary = &conversation.DurableSummary{Content: "prior summary", MessageCount: 2}
	if err := sess.session.store.Save(context.Background(), conversation.Conversation{ID: sess.session.id, DurableSummary: sess.session.summary}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.session.db.Exec(`CREATE TABLE think_writes(n INTEGER);
CREATE TRIGGER think_insert AFTER INSERT ON conversations BEGIN INSERT INTO think_writes VALUES(1); END;
CREATE TRIGGER think_update AFTER UPDATE ON conversations BEGIN INSERT INTO think_writes VALUES(1); END;`); err != nil {
		t.Fatal(err)
	}
	caller.before = func(context.Context) {
		var writes int
		if err := sess.session.db.QueryRow(`SELECT COUNT(*) FROM think_writes`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		want := 0
		if len(caller.requests) == 2 {
			want = 1
		}
		if writes != want {
			t.Errorf("writes before model turn %d = %d, want %d goal writes only", len(caller.requests), writes, want)
		}
	}
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("first goal\n/think high\n/think\n/think high\n/think off\n/think bogus\n  second goal  \n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.recorded, []string{"first goal", "second goal"}) {
		t.Errorf("recorded goals = %q, want only first/second goals", src.recorded)
	}
	if len(caller.requests) != 2 {
		t.Fatalf("model calls = %d, want 2", len(caller.requests))
	}
	var history []provider.ChatMessage
	for _, m := range caller.requests[1].Messages {
		if m.Role != "system" {
			history = append(history, m)
		}
	}
	wantHistory := []provider.ChatMessage{{Role: "user", Content: "first goal"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "second goal"}}
	if !reflect.DeepEqual(history, wantHistory) {
		t.Errorf("second request history = %+v, want %+v", history, wantHistory)
	}
	if sess.session.id != "workspace:think-history" || sess.session.historySummary() != "prior summary" {
		t.Errorf("session identity/summary = %q/%q, want original", sess.session.id, sess.session.historySummary())
	}
	stored, err := sess.session.store.Load(context.Background(), "workspace:think-history")
	if err != nil {
		t.Fatal(err)
	}
	wantStored := []conversation.Message{{Role: "user", Content: "first goal"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "second goal"}, {Role: "assistant", Content: "answer"}}
	if !reflect.DeepEqual(stored.Messages, wantStored) || stored.DurableSummary == nil || stored.DurableSummary.Content != "prior summary" {
		t.Errorf("stored conversation = %+v, want two goal/answer pairs and prior summary", stored)
	}
	var writes int
	if err := sess.session.db.QueryRow(`SELECT COUNT(*) FROM think_writes`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 2 {
		t.Errorf("persistence writes = %d, want exactly 2 goals", writes)
	}
	if strings.Count(out.String(), "think: high\n") != 3 || strings.Count(out.String(), "think: model test/thinking does not support thinking; -think ignored\n") != 1 || strings.Count(out.String(), "usage: /think [off|on|low|medium|high|default]\n") != 1 {
		t.Errorf("command outputs = %q", out.String())
	}
	assertThinkOptions(t, caller.requests[1].Options, provider.Ptr(true), "high")
}

func TestThinkEditTextRemainsForcedGoal(t *testing.T) {
	caller := &thinkTurnCaller{captureCaller: captureCaller{answer: "answer"}}
	sess, _ := newThinkSession(t, caller, provider.ModelOptions{}, nil)
	sess.goalEditor = &fakeGoalEditor{available: true, text: "/think high"}
	var out strings.Builder
	src := &recordingSource{scannerSource: newScannerSource(strings.NewReader("/edit\n"), &out)}
	if err := runREPL(context.Background(), src, &out, nil, sess); err != nil {
		t.Fatal(err)
	}
	if len(caller.requests) != 1 {
		t.Fatalf("model calls = %d, want edited goal once", len(caller.requests))
	}
	messages := caller.requests[0].Messages
	if messages[len(messages)-1].Content != "/think high" || !reflect.DeepEqual(src.recorded, []string{"/think high"}) {
		t.Errorf("edited goal = %+v, recorded=%q", messages, src.recorded)
	}
	assertThinkOptions(t, caller.requests[0].Options, nil, "")
	assertThinkOptions(t, sess.runtime.ModelOptions(), nil, "")
}

func TestThinkAfterIdleCtrlCAndStaleInterrupt(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "idle Ctrl-C"
		if stale {
			name = "stale run interrupt"
		}
		t.Run(name, func(t *testing.T) {
			check := func(t *testing.T) {
				caller := &thinkTurnCaller{captureCaller: captureCaller{answer: "answer"}}
				caller.before = func(ctx context.Context) {
					if stale {
						synctest.Wait()
					}
					if err := ctx.Err(); err != nil {
						t.Errorf("next model context = %v, want live", err)
					}
				}
				sess, _ := newThinkSession(t, caller, provider.ModelOptions{}, nil)
				out, errOut := &lockedBuffer{}, &lockedBuffer{}
				replCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				interrupts := make(chan struct{}, 1)
				ctrl := newReplControl(out, errOut, interrupts, cancel)
				sess.control = ctrl
				var src lineSource
				if stale {
					interrupts <- struct{}{}
					src = newScannerSource(strings.NewReader("/think high\nnext goal\n"), out)
				} else {
					stdin, stdout := tempDescriptors(t)
					ops := &fakeTermOps{ttys: map[int]bool{int(stdin.Fd()): true, int(stdout.Fd()): true}, sizes: [][2]int{{80, 24}}}
					chunks := [][]byte{[]byte("\x03"), []byte("/think high\r"), []byte("next goal\r")}
					src = newInput(inputConfig{Stdin: stdin, Stdout: stdout, Stderr: errOut, In: &chunkReader{chunks: chunks}, Out: out, UseHistory: false, Getenv: func(string) string { return "" }, Root: sess.root, Ops: ops, OnInterrupt: ctrl.interrupt})
					if _, ok := src.(*editorSource); !ok {
						t.Fatalf("input = %T, want editor", src)
					}
				}
				ctrl.setIdleDisplay(src.IdleDisplay)
				if err := withLineSource(src, func(s lineSource) error { return runREPL(replCtx, s, out, interrupts, sess) }); err != nil {
					t.Fatal(err)
				}
				if err := replCtx.Err(); err != nil {
					t.Errorf("REPL context = %v, want live", err)
				}
				if len(caller.requests) != 1 {
					t.Fatalf("model calls = %d, want next goal once; output=%q", len(caller.requests), out.String())
				}
				messages := caller.requests[0].Messages
				if got := messages[len(messages)-1].Content; got != "next goal" {
					t.Errorf("next model goal = %q, want next goal", got)
				}
				assertThinkOptions(t, caller.requests[0].Options, provider.Ptr(true), "high")
				assertThinkOptions(t, sess.runtime.ModelOptions(), provider.Ptr(true), "high")
				if !strings.Contains(out.String(), "think: high\n") || strings.Contains(out.String(), "canceled") || !strings.Contains(out.String(), "done ·") {
					t.Errorf("command/goal completion = %q", out.String())
				}
				if !stale && strings.Count(out.String(), ctrlCHint) != 1 {
					t.Errorf("idle Ctrl-C hint count = %d, want 1", strings.Count(out.String(), ctrlCHint))
				}
			}
			// Real signal registration stays outside a synthetic-time bubble; the
			// stale-channel case needs only the scanner and deterministic run watcher.
			if stale {
				synctest.Test(t, check)
			} else {
				check(t)
			}
		})
	}
}

func TestThinkSurvivesMountsAndRefresh(t *testing.T) {
	for _, before := range []bool{true, false} {
		name := "high before mounts"
		if !before {
			name = "off after mounts"
		}
		t.Run(name, func(t *testing.T) {
			caller := &captureCaller{answer: "answer"}
			sess, root := newRefreshSession(t, caller)
			sess.thinkModels = &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{{Provider: "test", Model: "thinking"}: provider.ThinkToggle}}
			sess.thinkChain = []string{"test/thinking"}
			obs, err := newObserv(os.Getenv, root, true, false, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			sess.obs = obs
			gitContextTestRun(t, root, "checkout", "-q", "-b", "feature")
			var out strings.Builder
			wantTools := []string{
				"read_file,search,glob,list,write_file,edit_file",
				"read_file,search,glob,list,write_file,edit_file,run_command,start_command,command_status,command_tail,stop_command",
				"read_file,search,glob,list,write_file,edit_file,run_command,start_command,command_status,command_tail,stop_command",
			}
			for i, command := range []string{"/allow-write", "/allow-exec", "/git-context refresh"} {
				_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
				_, _ = dispatchSlash(context.Background(), &out, sess, command)
				if !before {
					_, _ = dispatchSlash(context.Background(), &out, sess, "/think off")
				}
				caller.messages = nil
				caller.tools = nil
				if _, err := runOnce(context.Background(), &out, nil, sess, "goal", nil); err != nil {
					t.Fatal(err)
				}
				effort := ""
				if before {
					effort = "high"
				}
				assertThinkOptions(t, caller.options, &before, effort)
				if got := strings.Join(caller.tools, ","); got != wantTools[i] {
					t.Errorf("%s tool order = %q, want %q", command, got, wantTools[i])
				}
				if caller.system != sess.baseSystem {
					t.Errorf("%s runtime system differs from session system", command)
				}
			}
			if caller.system != sess.baseSystem || !strings.Contains(caller.system, "branch: feature\n") || !strings.Contains(caller.system, "write_file") || !strings.Contains(caller.system, "run_command") {
				t.Errorf("composed runtime system = %q", caller.system)
			}
			files, err := filepath.Glob(filepath.Join(obs.traceDir, "*.json"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 3 {
				t.Fatalf("trace files = %d, want one per goal", len(files))
			}
			var latest agenttrace.TraceRecord
			found := false
			for _, file := range files {
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &latest); err != nil {
					t.Fatal(err)
				}
				if latest.Request.System == caller.system {
					found = true
					if latest.Request.ToolSchemaHash != toolSchemaHash(sess.tools) {
						t.Errorf("trace tool schema hash = %q, want current mounted set", latest.Request.ToolSchemaHash)
					}
				}
			}
			if !found {
				t.Error("no trace records the final runtime system")
			}
		})
	}
}

func TestThinkSurvivesSessionResetCommands(t *testing.T) {
	for _, command := range []string{"/new", "/clear", "/resume user:other"} {
		t.Run(command, func(t *testing.T) {
			caller := &captureCaller{answer: "answer"}
			sess := newSessionedTestSession(t, caller, t.TempDir(), "workspace:reset-think")
			sess.readToolCount = len(sess.tools)
			sess.thinkModels = &thinkFakeReg{byKey: map[provider.ModelKey]provider.ThinkMode{{Provider: "test", Model: "thinking"}: provider.ThinkToggle}}
			sess.thinkChain = []string{"test/thinking"}
			sess.grants = newApprovalGrants()
			sess.grants.grant(grantScopeExec, "exec:test")
			if err := sess.session.record(context.Background(), "old question", "old answer"); err != nil {
				t.Fatal(err)
			}
			if err := sess.session.store.Save(context.Background(), conversation.Conversation{ID: "user:other", Messages: []conversation.Message{{Role: "user", Content: "resumed question"}, {Role: "assistant", Content: "resumed answer"}}}); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			_, _ = dispatchSlash(context.Background(), &out, sess, "/think high")
			_, _ = dispatchSlash(context.Background(), &out, sess, command)
			if sess.grants.count() != 0 {
				t.Errorf("%s grants = %d, want 0", command, sess.grants.count())
			}
			if command == "/resume user:other" {
				if sess.session.id != "user:other" || len(sess.session.msgs) != 2 {
					t.Errorf("resume session = %q/%+v", sess.session.id, sess.session.msgs)
				}
			} else if len(sess.session.msgs) != 0 {
				t.Errorf("%s history = %+v, want cleared", command, sess.session.msgs)
			}
			if _, err := runOnce(context.Background(), &out, nil, sess, "next goal", nil); err != nil {
				t.Fatal(err)
			}
			assertThinkOptions(t, caller.options, provider.Ptr(true), "high")
		})
	}
}
