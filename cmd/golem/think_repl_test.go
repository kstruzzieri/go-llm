package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
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
	sess.modelOptions = provider.ModelOptions{Think: provider.Ptr(true), ThinkEffort: "high"}

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
			sess.modelOptions = provider.ModelOptions{Think: &falseValue}

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
