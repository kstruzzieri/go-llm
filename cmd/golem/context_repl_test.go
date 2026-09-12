package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

func contextFixture() agent.PressureEvent {
	return agent.PressureEvent{Pressure: agent.Pressure{Level: agent.LevelWarn, Cause: agent.CauseHistory, Mitigation: agent.MitigationWarn, InputTokens: 7680, InputBudget: 10000, UsedPct: .768, Buckets: agent.PressureBuckets{Available: true, Pinned: 1000, ToolSchema: 680, History: 5000, ToolOutput: 800, Retrieval: 200}}}
}

func TestContextCaptureAndLiteralOutput(t *testing.T) {
	capture := &pressureCapture{runID: "run-1", threadID: "user:one"}
	if event, seen := capture.snapshot(); seen || event != (agent.PressureEvent{}) {
		t.Fatalf("empty snapshot = %+v, %v", event, seen)
	}
	sess := &replSession{runtime: &golemruntime.Runtime{}, pressure: capture, budget: agent.Budget{InputCeiling: 11024, OutputReserve: 1024}}
	for _, tc := range []struct {
		name  string
		event agent.PressureEvent
		want  string
	}{
		{"warning", contextFixture(), "context: last assembled request run-1, step 1\npressure: warn; cause: history; mitigation: warn\ninput estimate: 7680 / 10000 tokens (76.8%)\nbucket estimates: pinned 1000; tool_schema 680; history 5000; tool_output 800; retrieval 200 tokens\nconfigured input ceiling: 11024 tokens; explicit output reserve: 1024 tokens\n"},
		{"normal replaces warning", agent.PressureEvent{Step: 2, Pressure: agent.Pressure{InputTokens: 10, InputBudget: 1000, UsedPct: .01, Buckets: agent.PressureBuckets{Available: true, Pinned: 10}}}, "context: last assembled request run-1, step 3\npressure: ok; cause: unknown; mitigation: none\ninput estimate: 10 / 1000 tokens (1.0%)\nbucket estimates: pinned 10; tool_schema 0; history 0; tool_output 0; retrieval 0 tokens\nconfigured input ceiling: 11024 tokens; explicit output reserve: 1024 tokens\n"},
		{"exhausted unavailable", agent.PressureEvent{Step: 3, Pressure: agent.Pressure{InputTokens: 12000, InputBudget: 10000, UsedPct: 1.2, Level: agent.LevelCritical, Cause: agent.CausePinned, Mitigation: agent.MitigationHalt}}, "context: last assembled request run-1, step 4\npressure: critical; cause: pinned; mitigation: halt\ninput estimate: 12000 / 10000 tokens (120.0%)\nbucket estimates: unavailable for this assembly\nconfigured input ceiling: 11024 tokens; explicit output reserve: 1024 tokens\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := capture.OnPressure(t.Context(), tc.event); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				var out strings.Builder
				dispatchSlash(t.Context(), &out, sess, "/context")
				if out.String() != tc.want {
					t.Fatalf("/context = %q; want %q", out.String(), tc.want)
				}
			}
			copied, _ := capture.snapshot()
			copied.Pressure.Buckets.Pinned = 99
			if got, seen := capture.snapshot(); !seen || got != tc.event {
				t.Fatalf("snapshot after modifying copy = %+v, %v; want %+v, true", got, seen, tc.event)
			}
		})
	}
	if !strings.Contains(golemHelp, "  /context       inspect the last assembled request\n") {
		t.Fatal("help missing /context")
	}
}

func TestContextStepMatchesRenderer(t *testing.T) {
	var out strings.Builder
	render := newRenderer(&out, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	capture := &pressureCapture{runID: "run-1"}
	event := contextFixture()
	observer := composeObserver(capture, nil, render)
	if err := observer.(agent.PressureObserver).OnPressure(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	if err := observer.OnStep(t.Context(), agent.StepEvent{Index: event.Step, Pressure: event.Pressure}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "? · 0.0s · ctx 77% · step 1/16\n"; got != want {
		t.Fatalf("renderer = %q; want %q", got, want)
	}
	out.Reset()
	dispatchSlash(t.Context(), &out, &replSession{runtime: &golemruntime.Runtime{}, pressure: capture}, "/context")
	if !strings.HasPrefix(out.String(), "context: last assembled request run-1, step 1\n") {
		t.Fatalf("context step = %q", out.String())
	}
}

func TestContextRunOnceFailuresAndStateless(t *testing.T) {
	for _, mode := range []string{"normal", "model error", "observer error", "exhaustion", "before assembly"} {
		t.Run(mode, func(t *testing.T) {
			caller := &captureCaller{answer: "done"}
			sess := newTestSession(t, caller, t.TempDir())
			sess.pressure = &pressureCapture{runID: "old", seen: true, latest: contextFixture()}
			out := io.Discard
			switch mode {
			case "model error":
				sess.orch = agent.New(tokenThenErrorCaller{}, agent.ContextManager{})
				sess.runtime = newTestRuntime(t, t.TempDir(), "system", sess.orch, nil)
			case "observer error", "exhaustion":
				budget := agent.Budget{InputCeiling: 1}
				if mode == "observer error" {
					budget = agent.Budget{InputCeiling: 1000, Pressure: agent.PressureThresholds{}}
					out = contextErrorWriter{}
					sess.pressureWarn = true
				}
				rt, err := golemruntime.New(t.Context(), golemruntime.Options{Root: t.TempDir(), System: strings.Repeat("x", 3200), Budget: budget, Orchestrator: agent.New(caller, agent.ContextManager{})})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rt.Close() })
				sess.runtime = rt
			case "before assembly":
				if err := sess.runtime.Close(); err != nil {
					t.Fatal(err)
				}
			}
			_, err := runOnce(t.Context(), out, nil, sess, "goal", nil)
			if (err != nil) != (mode != "normal") {
				t.Fatalf("runOnce(%s) error = %v", mode, err)
			}
			event, seen := sess.pressure.snapshot()
			if sess.pressure.runID == "old" || sess.pressure.runID == "" || sess.pressure.threadID != "" || seen != (mode != "before assembly") {
				t.Fatalf("runOnce(%s) capture = %+v, seen %v", mode, sess.pressure, seen)
			}
			if mode == "normal" && (event.Pressure.Level != agent.LevelOK || !event.Pressure.Buckets.Available) {
				t.Fatalf("warning-disabled normal capture = %+v", event)
			}
			if mode == "exhaustion" && (event.Pressure.Mitigation != agent.MitigationHalt || caller.messages != nil) {
				t.Fatalf("exhaustion capture = %+v; model messages %v", event, caller.messages)
			}
			if mode == "observer error" && caller.messages != nil {
				t.Fatal("observer failure called model")
			}
			retained := sess.pressure
			for _, cmd := range []string{"/new", "/clear", "/resume user:other"} {
				dispatchSlash(t.Context(), io.Discard, sess, cmd)
				if sess.pressure != retained {
					t.Fatalf("stateless %s discarded capture", cmd)
				}
			}
		})
	}
}

type contextErrorWriter struct{}

func (contextErrorWriter) Write([]byte) (int, error) { return 0, errors.New("display failed") }

func TestContextSessionBoundaries(t *testing.T) {
	for _, cmd := range []string{"/clear", "/new", "/resume user:other"} {
		for _, fail := range []bool{false, true} {
			t.Run(cmd+map[bool]string{true: " failure", false: " success"}[fail], func(t *testing.T) {
				sess := newCanarySession(t, &captureCaller{answer: "ok"})
				if err := sess.session.store.Save(t.Context(), conversation.Conversation{ID: "user:other", Messages: []conversation.Message{{Role: "user", Content: "old"}}}); err != nil {
					t.Fatal(err)
				}
				capture := &pressureCapture{runID: "prior", seen: true, latest: contextFixture()}
				sess.pressure = capture
				if fail {
					if cmd == "/clear" {
						sess.session.store = canaryDeleteFailureStore{Store: sess.session.store}
					} else {
						sess.canary.entropy = errorCanaryReader{}
					}
				}
				var out strings.Builder
				dispatchSlash(t.Context(), &out, sess, cmd)
				if fail {
					if sess.pressure != capture || !strings.Contains(out.String(), "failed:") {
						t.Fatalf("failed %s = %q; capture retained %v", cmd, out.String(), sess.pressure == capture)
					}
				} else {
					out.Reset()
					dispatchSlash(t.Context(), &out, sess, "/context")
					if want := "context: no pressure sample for the current session\nconfigured input ceiling: 0 tokens; explicit output reserve: 0 tokens\n"; out.String() != want {
						t.Fatalf("after %s = %q; want %q", cmd, out.String(), want)
					}
				}
			})
		}
	}
}

func TestContextConcurrentRequestCaptures(t *testing.T) {
	caller := &contextBarrierCaller{entered: make(chan string, 4)}
	sess := newSessionedTestSession(t, caller, t.TempDir(), "user:fixture")
	orch := agent.New(caller, agent.ContextManager{Estimate: func(s string) int {
		if s == "" {
			return 0
		}
		if strings.HasPrefix(s, "goal-") {
			return len(s)
		}
		return 1
	}})
	rt, err := golemruntime.New(t.Context(), golemruntime.Options{Root: t.TempDir(), System: "system", Orchestrator: orch, SessionStore: sess.session.store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 4)
	cases := []struct {
		thread, runID, goal string
		input, pinned       int
		capture             *pressureCapture
	}{
		{thread: "user:first", runID: "run-first", goal: "goal-a", input: 8, pinned: 7},
		{thread: "user:second", runID: "run-second", goal: "goal-bb", input: 9, pinned: 8},
		{runID: "run-stateless-one", goal: "goal-ccc", input: 10, pinned: 9},
		{runID: "run-stateless-two", goal: "goal-dddd", input: 11, pinned: 10},
	}
	for i := range cases {
		tc := &cases[i]
		tc.capture = &pressureCapture{runID: tc.runID, threadID: tc.thread}
		go func() {
			_, err := rt.Run(ctx, golemruntime.Turn{ThreadID: tc.thread, RunID: tc.runID, Message: tc.goal, Observer: tc.capture}, sess.machine.sink())
			done <- err
		}()
	}
	entered := make(map[string]bool)
	for range cases {
		select {
		case goal := <-caller.entered:
			entered[goal] = true
		case err := <-done:
			t.Fatalf("Run before all models entered = %v", err)
		}
	}
	// Every request is assembled and blocked in Chat on the SAME Runtime.
	// Distinct estimates expose leakage between named and stateless requests.
	var readers sync.WaitGroup
	for _, tc := range cases {
		if !entered[tc.goal] {
			t.Fatalf("model goal %q did not enter", tc.goal)
		}
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 1000 {
				event, seen := tc.capture.snapshot()
				if !seen || event.Step != 0 || event.Pressure.InputTokens != tc.input || event.Pressure.Buckets != (agent.PressureBuckets{Available: true, Pinned: tc.pinned, ToolSchema: 1}) {
					t.Errorf("snapshot(%s/%s) = %+v, %v; want input %d, pinned %d", tc.thread, tc.runID, event, seen, tc.input, tc.pinned)
					return
				}
			}
		}()
	}
	readers.Wait()
	cancel()
	for range cases {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Run canceled = %v", err)
		}
	}
}

type contextBarrierCaller struct{ entered chan string }

func (c *contextBarrierCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.entered <- req.Messages[len(req.Messages)-1].Content
	<-ctx.Done()
	return agent.ModelResult{}, ctx.Err()
}

func TestContextSnapshotConcurrentUpdates(t *testing.T) {
	capture := &pressureCapture{}
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for i := range 10000 {
			event := contextFixture()
			event.Step = i
			event.Pressure.Buckets.Pinned = i
			_ = capture.OnPressure(t.Context(), event)
		}
	}()
	close(start)
	for range 10000 {
		event, seen := capture.snapshot()
		if seen && event.Step != event.Pressure.Buckets.Pinned {
			t.Errorf("snapshot torn: %+v", event)
			break
		}
	}
	<-done
	event, seen := capture.snapshot()
	if !seen || event.Step != 9999 || event.Pressure.Buckets.Pinned != 9999 {
		t.Fatalf("latest concurrent snapshot = %+v, %v; want step and pinned 9999", event, seen)
	}
}

func TestContextFailureBeforeRuntimePreservesSample(t *testing.T) {
	sess := newCanarySession(t, &captureCaller{answer: "ok"})
	capture := &pressureCapture{runID: "prior", seen: true, latest: contextFixture()}
	sess.pressure = capture
	sess.canary.burn()
	sess.canary.entropy = errorCanaryReader{}
	if _, err := runOnce(t.Context(), io.Discard, nil, sess, "goal", nil); !errors.Is(err, errCanaryUnavailable) {
		t.Fatalf("runOnce before runtime = %v; want canary unavailable", err)
	}
	if sess.pressure != capture {
		t.Fatal("failure before Runtime discarded preceding sample")
	}
}

func TestContextValidation(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		sess          *replSession
		want          string
	}{
		{"arguments", "/context extra", &replSession{}, "usage: /context\n"},
		{"runtime absent", "/context", &replSession{}, "context: runtime unavailable\n"},
		{"empty", "/context", &replSession{runtime: &golemruntime.Runtime{}, budget: agent.Budget{InputCeiling: 11024, OutputReserve: 1024}}, "context: no pressure sample for the current session\nconfigured input ceiling: 11024 tokens; explicit output reserve: 1024 tokens\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			forced, exit := dispatchSlash(context.Background(), &out, tc.sess, tc.command)
			if out.String() != tc.want || forced != "" || exit {
				t.Fatalf("dispatchSlash(%q) = %q, %q, %v; want %q, empty, false", tc.command, out.String(), forced, exit, tc.want)
			}
		})
	}
}

func TestContextReadOnly(t *testing.T) {
	summarizerCalls := 0
	sess, caller := newCompactSession(t, 5, func(context.Context, string, []conversation.Message) (string, error) {
		summarizerCalls++
		return "SUM", nil
	})
	// Promoted nil interfaces panic on every operation, including operations the
	// usual counting fixtures do not override. Only construction precedes them.
	store := &struct{ conversation.Store }{}
	sess.session.store = store
	sess.thinkModels = &struct{ capChecker }{}
	sess.orch = agent.New(&struct{ agent.ModelCaller }{}, agent.ContextManager{})
	installCompactRuntime(t, sess, golemruntime.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
		summarizerCalls++
		return "SUM", nil
	}})
	sess.pressure = &pressureCapture{runID: "run-1", seen: true, latest: contextFixture()}
	for range 2 {
		var out strings.Builder
		dispatchSlash(t.Context(), &out, sess, "/context")
		if !strings.HasPrefix(out.String(), "context: last assembled request run-1, step 1\n") {
			t.Fatalf("read-only context = %q", out.String())
		}
	}
	if summarizerCalls != 0 || len(caller.requests) != 0 {
		t.Fatalf("/context operations = summaries %d, model %d; want zero", summarizerCalls, len(caller.requests))
	}
}

func TestContextCompactionKeepsHistoricalSample(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "automatic"}[automatic], func(t *testing.T) {
			calls := 0
			sess, caller := newCompactSession(t, 5, nil)
			// Deliberately different estimators: assembly includes system/schemas,
			// while compaction independently estimates stored history with len/4.
			sess.orch = agent.New(caller, agent.ContextManager{Estimate: func(s string) int {
				if s == "" {
					return 0
				}
				return 1
			}})
			budget := agent.Budget{InputCeiling: 180}
			if !automatic {
				budget.InputCeiling = 10000
			}
			sess.budget = budget
			installCompactRuntime(t, sess, golemruntime.Options{Budget: budget, Summarizer: func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil }})
			if _, err := runOnce(t.Context(), io.Discard, nil, sess, "goal", nil); err != nil {
				t.Fatal(err)
			}
			before, seen := sess.pressure.snapshot()
			if !seen {
				t.Fatal("turn missing sample")
			}
			var out strings.Builder
			if !automatic {
				dispatchSlash(t.Context(), &out, sess, "/compact")
				if want := "compact: history token estimate 103 -> 104 (changed)\n"; out.String() != want {
					t.Errorf("manual compaction = %q; want %q", out.String(), want)
				}
			}
			if calls != 1 || sess.session.historySummary() != "SUM" {
				t.Fatalf("compaction = calls %d, summary %q", calls, sess.session.historySummary())
			}
			if automatic {
				dispatchSlash(t.Context(), &out, sess, "/compact")
				if want := "compact: history token estimate 104 -> 104 (unchanged)\n"; out.String() != want {
					t.Errorf("stored history after automatic compaction = %q; want %q", out.String(), want)
				}
			}
			if before.Pressure.InputTokens != 13 || before.Pressure.Buckets != (agent.PressureBuckets{Available: true, Pinned: 2, ToolSchema: 1, History: 10}) {
				t.Errorf("full assembled estimate = %+v; want input 13, buckets pinned 2, schema 1, history 10", before.Pressure)
			}
			if after, ok := sess.pressure.snapshot(); !ok || after != before {
				t.Fatalf("compaction changed historical pressure = %+v; want %+v", after, before)
			}
			old := sess.pressure
			if _, err := runOnce(t.Context(), io.Discard, nil, sess, "next", nil); err != nil {
				t.Fatal(err)
			}
			after, _ := sess.pressure.snapshot()
			if sess.pressure == old || sess.pressure.runID == old.runID || after.Pressure.InputTokens != 12 || after.Pressure.Buckets != (agent.PressureBuckets{Available: true, Pinned: 3, ToolSchema: 1, History: 8}) {
				t.Errorf("next assembled estimate = %+v; want new run input 12, buckets pinned 3, schema 1, history 8", after.Pressure)
			}
		})
	}
}
