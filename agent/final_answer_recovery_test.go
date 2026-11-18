package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// #767. The nudge is written out, not read from finalAnswerNudge, so wording
// drift fails here.
const (
	recoveryGoal  = "What is the code word?"
	recoveryNudge = "Your last reply had reasoning but no answer. Reply now with your final answer only."
)

// recoveryStep is one scripted Chat call. chunks, when nil, streams the
// response content as one chunk (nothing for empty content). cancel cancels
// the run's context inside the call, before it returns.
type recoveryStep struct {
	result ModelResult
	err    error
	chunks []provider.ChatResponse
	cancel bool
}

// recoveryCaller records every wire request: what the orchestrator sends is
// where the recovery tail must appear, and the only place it may.
type recoveryCaller struct {
	script []recoveryStep
	reqs   []provider.ChatRequest
	cancel context.CancelFunc
}

func (c *recoveryCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
	i := len(c.reqs)
	req.Messages = slices.Clone(req.Messages)
	c.reqs = append(c.reqs, req)
	if i >= len(c.script) {
		return ModelResult{}, errors.New("recoveryCaller: unscripted call")
	}
	s := c.script[i]
	if s.cancel {
		c.cancel()
	}
	chunks := s.chunks
	if chunks == nil && s.result.Response.Content != "" {
		chunks = []provider.ChatResponse{{Content: s.result.Response.Content}}
	}
	for _, ch := range chunks {
		if err := onToken(ch); err != nil {
			return ModelResult{}, err
		}
	}
	return s.result, s.err
}

func thinkingOnlyStep(thinking string) recoveryStep {
	return recoveryStep{result: ModelResult{Response: provider.ChatResponse{Thinking: thinking, Done: true}}}
}

func answerStep(content string) recoveryStep {
	return recoveryStep{result: ModelResult{Response: provider.ChatResponse{Content: content, Done: true}}}
}

func spyToolCall() provider.ToolCall {
	return provider.ToolCall{ID: "s1", Type: "function", Function: provider.ToolCallFunction{Name: "spy", Arguments: json.RawMessage(`{}`)}}
}

func spyStep(content string) recoveryStep {
	return recoveryStep{result: ModelResult{Response: provider.ChatResponse{Content: content, ToolCalls: []provider.ToolCall{spyToolCall()}, Done: true}}}
}

// spyTool counts its invocations. Its schema is deliberately cheap, so a
// setup request that advertises it still fits under the tail-fit ceiling.
type spyTool struct {
	invoked *int
	set     *ContextSet
}

func (spyTool) Spec() ToolSpec { return ToolSpec{Name: "spy", Parameters: json.RawMessage(`{}`)} }
func (spyTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (s spyTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	*s.invoked++
	return ToolResult{Content: "spy-result", Context: s.set}, nil
}

// recoveryObs records step indexes and the mixed traces, and fails OnStep or
// OnPressure at one chosen step (-1 never).
type recoveryObs struct {
	steps     []int
	traces    map[int]ContextAssemblyTrace
	pressures map[int]Pressure
	failStep  int
	failPress int
	failWith  error
}

func newRecoveryObs() *recoveryObs {
	return &recoveryObs{failStep: -1, failPress: -1, traces: map[int]ContextAssemblyTrace{}, pressures: map[int]Pressure{}}
}

func (o *recoveryObs) OnStep(_ context.Context, e StepEvent) error {
	o.steps = append(o.steps, e.Index)
	if e.Index == o.failStep {
		return o.failWith
	}
	return nil
}
func (o *recoveryObs) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (o *recoveryObs) OnToken(context.Context, TokenEvent) error       { return nil }
func (o *recoveryObs) OnPressure(_ context.Context, e PressureEvent) error {
	o.pressures[e.Step] = e.Pressure
	if e.Step == o.failPress {
		return o.failWith
	}
	return nil
}
func (o *recoveryObs) OnContextAssembly(_ context.Context, e ContextAssemblyEvent) error {
	o.traces[e.Step] = e.Trace
	return nil
}

func countKind(events []EventRecord, kind string) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func eventLog(events []EventRecord) string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind + "(" + strconv.Itoa(e.Step) + ")"
	}
	return strings.Join(out, ",")
}

func tailMessages() []Message {
	return []Message{
		{ChatMessage: provider.ChatMessage{Role: "assistant"}},
		{ChatMessage: provider.ChatMessage{Role: "user", Content: recoveryNudge}},
	}
}

// prefixCompactor returns the prefix of the State it was given that ends at
// the pinned goal: a slice sharing the canonical backing array, with spare
// capacity behind it once a tool step has appended.
type prefixCompactor struct{}

func (prefixCompactor) Compact(_ context.Context, st State, _ TokenBudget) (State, CompactionReport, error) {
	for i, m := range st.Messages {
		if m.Segment == Pinned && m.Role == "user" {
			out := st
			out.Messages = st.Messages[:i+1]
			return out, CompactionReport{Strategy: "prefix", DroppedCount: len(st.Messages) - i - 1}, nil
		}
	}
	return st, CompactionReport{}, nil
}

func TestFinalAnswerRecovery(t *testing.T) {
	// Independent prices under runeEstimator: the step-0 prompt advertises the
	// spy schema, the recovery prompt carries the tail and no tools.
	cm := ContextManager{Estimate: runeEstimator, frameToolResults: true}
	sys := cm.estimate(withToolTrustContract(""))
	goalCost := cm.messageCost(Message{ChatMessage: provider.ChatMessage{Role: "user", Content: recoveryGoal}, Segment: Pinned})
	schema := cm.estimate(toolSchemaString([]provider.Tool{{Type: "function", Function: provider.ToolFunction{Name: "spy", Parameters: json.RawMessage(`{}`)}}}))
	tail := 0
	for _, m := range tailMessages() {
		tail += cm.messageCost(m)
	}
	if tail < 1 {
		t.Fatalf("tail costs %d; the budget rows need it to cost something", tail)
	}
	const gen = 16
	firstPrompt := sys + goalCost + schema
	recoveryPrompt := sys + goalCost + tail
	first, second := firstPrompt+gen, recoveryPrompt+gen

	callerErr := errors.New("stream broke")
	sentinel := errors.New("observer failed")
	const fenceKey = "REASONINGBUTNOANSWER" // folded from the nudge's "reasoning but no answer"

	pelicanThinking := thinkingOnlyStep("The code word is PELICAN.")
	isBlocked := func(err error) bool {
		var b *BlockedError
		return errors.As(err, &b)
	}
	goalOnly := func(t *testing.T, msgs []provider.ChatMessage) {
		t.Helper()
		if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != recoveryGoal {
			t.Fatalf("Result.Messages = %+v, want only the goal", msgs)
		}
	}
	endsWith := func(t *testing.T, msgs []provider.ChatMessage, role, content string) {
		t.Helper()
		if len(msgs) == 0 {
			t.Fatal("Result.Messages is empty")
		}
		last := msgs[len(msgs)-1]
		if last.Role != role || last.Content != content || len(last.ToolCalls) != 0 {
			t.Fatalf("last message = %+v, want {%s %q} with no tool calls", last, role, content)
		}
	}

	type check func(t *testing.T, res Result, c *recoveryCaller, obs *recoveryObs, invoked int)
	rows := []struct {
		name         string
		flagOff      bool
		maxSteps     int
		budget       Budget
		system       string
		systemKeys   []string
		compactor    Compactor
		interceptors []Interceptor
		script       []recoveryStep
		obs          func(*recoveryObs)
		wantCalls    int
		wantAnswer   string
		wantStop     StopReason
		wantErr      func(error) bool
		wantRetries  int
		check        check
	}{
		{
			name:      "recovers",
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 2, wantAnswer: "PELICAN", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, c *recoveryCaller, _ *recoveryObs, _ int) {
				if got, want := eventLog(res.Events), "step(0),final_answer_retry(1),token(1),step(1),stop(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				if n := len(c.reqs[0].Tools); n != 1 {
					t.Errorf("request 1 tools = %d, want 1", n)
				}
				if n := len(c.reqs[1].Tools); n != 0 {
					t.Errorf("request 2 tools = %d, want 0", n)
				}
				msgs := c.reqs[1].Messages
				if len(msgs) < 2 {
					t.Fatalf("request 2 messages = %+v", msgs)
				}
				want := []provider.ChatMessage{{Role: "assistant"}, {Role: "user", Content: recoveryNudge}}
				for i, w := range want {
					got := msgs[len(msgs)-2+i]
					if got.Role != w.Role || got.Content != w.Content || len(got.ToolCalls) != 0 {
						t.Errorf("request 2 tail[%d] = %+v, want %+v", i, got, w)
					}
				}
				for _, m := range res.Messages {
					if m.Content == recoveryNudge || (m.Role == "assistant" && m.Content == "" && len(m.ToolCalls) == 0) {
						t.Errorf("Result.Messages carries the tail: %+v", res.Messages)
					}
				}
				endsWith(t, res.Messages, "assistant", "PELICAN")
			},
		},
		{
			name:      "second empty",
			script:    []recoveryStep{pelicanThinking, pelicanThinking},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				goalOnly(t, res.Messages)
				if n := countKind(res.Events, "stop"); n != 1 {
					t.Errorf("stop events = %d, want 1", n)
				}
			},
		},
		{
			name: "flag off", flagOff: true,
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: Completed,
		},
		{
			name: "no step left", maxSteps: 1,
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: Completed,
		},
		{
			name:      "whitespace thinking",
			script:    []recoveryStep{thinkingOnlyStep(" \n"), answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: Completed,
		},
		{
			name: "stream-only thinking",
			script: []recoveryStep{
				{result: ModelResult{Response: provider.ChatResponse{Done: true}}, chunks: []provider.ChatResponse{{Thinking: "The code word is PELICAN."}}},
				answerStep("PELICAN"),
			},
			wantCalls: 1, wantAnswer: "", wantStop: Completed,
		},
		{
			name:      "recovery returns a tool call",
			script:    []recoveryStep{pelicanThinking, spyStep("PELICAN")},
			wantCalls: 2, wantAnswer: "PELICAN", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, invoked int) {
				if invoked != 0 || len(res.ToolCalls) != 0 {
					t.Errorf("spy invoked %d times, %d tool call records; want none", invoked, len(res.ToolCalls))
				}
				for _, m := range res.Messages {
					if m.Role == "tool" {
						t.Errorf("Result.Messages has a tool message: %+v", res.Messages)
					}
				}
				endsWith(t, res.Messages, "assistant", "PELICAN")
			},
		},
		{
			name:      "budget exhausted by the first call",
			budget:    Budget{OutputReserve: gen, TotalTokens: first},
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: BudgetReached,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				if got := res.Steps[0].Pressure.InputTokens; got != firstPrompt {
					t.Fatalf("step 0 prompt = %d, test priced %d", got, firstPrompt)
				}
				if got, want := eventLog(res.Events), "step(0),stop(0)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
			},
		},
		{
			// One token short of the tail-inclusive recovery, but enough for
			// the same prompt without the tail.
			name:      "budget refuses the recovery",
			budget:    Budget{OutputReserve: gen, TotalTokens: first + second - 1},
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: BudgetReached,
			check: func(t *testing.T, res Result, _ *recoveryCaller, obs *recoveryObs, _ int) {
				if got := res.Steps[0].Pressure.InputTokens; got != firstPrompt {
					t.Fatalf("step 0 prompt = %d, test priced %d", got, firstPrompt)
				}
				if got := obs.pressures[1].InputTokens; got != recoveryPrompt {
					t.Errorf("recovery pressure = %d, test priced %d (tail %d)", got, recoveryPrompt, tail)
				}
				if got, want := eventLog(res.Events), "step(0),stop(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				goalOnly(t, res.Messages)
			},
		},
		{
			name:      "budget exhausted by the recovery",
			budget:    Budget{OutputReserve: gen, TotalTokens: first + second},
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 2, wantAnswer: "PELICAN", wantStop: BudgetReached, wantRetries: 1,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				endsWith(t, res.Messages, "assistant", "PELICAN")
				if n := countKind(res.Events, "stop"); n != 1 {
					t.Errorf("stop events = %d, want 1", n)
				}
			},
		},
		{
			name:      "budget exhausted by a whitespace recovery",
			budget:    Budget{OutputReserve: gen, TotalTokens: first + second},
			script:    []recoveryStep{pelicanThinking, answerStep(" \n")},
			wantCalls: 2, wantAnswer: "", wantStop: BudgetReached, wantRetries: 1,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				goalOnly(t, res.Messages)
			},
		},
		{
			name:      "prefix-aliasing compactor",
			compactor: prefixCompactor{},
			script:    []recoveryStep{spyStep(""), pelicanThinking, answerStep("PELICAN")},
			wantCalls: 3, wantAnswer: "PELICAN", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, c *recoveryCaller, _ *recoveryObs, invoked int) {
				if invoked != 1 {
					t.Errorf("spy invoked %d times, want 1", invoked)
				}
				if len(res.Messages) != 4 {
					t.Fatalf("Result.Messages = %+v, want goal, tool call, tool result, answer", res.Messages)
				}
				call, result := res.Messages[1], res.Messages[2]
				if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != "s1" || call.ToolCalls[0].Function.Name != "spy" {
					t.Errorf("step-0 tool call = %+v, want it unchanged", call)
				}
				if result.Role != "tool" || result.ToolCallID != "s1" || result.Content != "spy-result" {
					t.Errorf("step-0 tool result = %+v, want it unchanged", result)
				}
				endsWith(t, res.Messages, "assistant", "PELICAN")
				if got := c.reqs[2].Messages[len(c.reqs[2].Messages)-1].Content; got != recoveryNudge {
					t.Errorf("recovery request ends with %q, want the nudge", got)
				}
			},
		},
		{
			name: "recovery is tool-only on the last step", maxSteps: 2,
			script:    []recoveryStep{pelicanThinking, spyStep("")},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, invoked int) {
				if invoked != 0 {
					t.Errorf("spy invoked %d times, want 0", invoked)
				}
				if got, want := eventLog(res.Events), "step(0),final_answer_retry(1),step(1),stop(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				goalOnly(t, res.Messages)
			},
		},
		{
			name: "caller error on the recovery with partial content",
			script: []recoveryStep{pelicanThinking, {
				result: ModelResult{Response: provider.ChatResponse{Content: "PEL"}}, err: callerErr,
			}},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: func(err error) bool { return errors.Is(err, callerErr) },
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				if len(res.Messages) != 2 {
					t.Fatalf("Result.Messages = %+v, want goal and the partial answer", res.Messages)
				}
				endsWith(t, res.Messages, "assistant", "PEL")
			},
		},
		{
			name:      "caller error on the recovery without content",
			script:    []recoveryStep{pelicanThinking, {err: callerErr}},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: func(err error) bool { return errors.Is(err, callerErr) },
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				goalOnly(t, res.Messages)
			},
		},
		{
			name: "caller error on the recovery with blocked partial content",
			interceptors: []Interceptor{&stubInterceptor{name: "guard", output: func(out OutputInspection) []Finding {
				if strings.Contains(out.Content, "PEL") {
					return []Finding{{Rule: "pel", Verdict: VerdictBlock, Risk: 90}}
				}
				return nil
			}}},
			script: []recoveryStep{pelicanThinking, {
				result: ModelResult{Response: provider.ChatResponse{Content: "PEL"}}, err: callerErr,
			}},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: func(err error) bool { return errors.Is(err, callerErr) && isBlocked(err) },
			check: func(t *testing.T, res Result, _ *recoveryCaller, obs *recoveryObs, _ int) {
				if got, want := eventLog(res.Events), "step(0),final_answer_retry(1),token(1),blocked(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				if len(res.Steps) != 1 || !slices.Equal(obs.steps, []int{0}) {
					t.Errorf("steps = %d records, OnStep %v; want only step 0", len(res.Steps), obs.steps)
				}
			},
		},
		{
			name: "output guard blocks the thinking",
			interceptors: []Interceptor{&stubInterceptor{name: "guard", output: func(out OutputInspection) []Finding {
				if strings.Contains(out.Content+out.Thinking, "PELICAN") {
					return []Finding{{Rule: "pelican", Verdict: VerdictBlock, Risk: 90}}
				}
				return nil
			}}},
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 1, wantAnswer: "", wantStop: Completed,
			wantErr: isBlocked,
		},
		{
			name: "output guard blocks the recovered answer",
			interceptors: []Interceptor{&stubInterceptor{name: "guard", output: func(out OutputInspection) []Finding {
				if strings.Contains(out.Content, "PELICAN") {
					return []Finding{{Rule: "pelican", Verdict: VerdictBlock, Risk: 90}}
				}
				return nil
			}}},
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: isBlocked,
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				if got, want := eventLog(res.Events), "step(0),final_answer_retry(1),token(1),step(1),blocked(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				if len(res.Steps) != 2 || res.Steps[1].Response.Content != "" {
					t.Errorf("steps = %+v, want a redacted record for step 1", res.Steps)
				}
			},
		},
		{
			name:      "caller cancels and returns success",
			script:    []recoveryStep{pelicanThinking, {result: ModelResult{Response: provider.ChatResponse{Content: "PELICAN", Done: true}}, cancel: true}},
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: func(err error) bool { return errors.Is(err, context.Canceled) },
			check: func(t *testing.T, res Result, _ *recoveryCaller, _ *recoveryObs, _ int) {
				goalOnly(t, res.Messages)
			},
		},
		{
			name:      "OnStep error on the recovery",
			script:    []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			obs:       func(o *recoveryObs) { o.failStep, o.failWith = 1, sentinel },
			wantCalls: 2, wantAnswer: "", wantStop: Completed, wantRetries: 1,
			wantErr: func(err error) bool { return errors.Is(err, sentinel) },
		},
		{
			name:       "fence key in the nudge",
			system:     "Snapshot fenced by " + fenceKey + ".",
			systemKeys: []string{fenceKey},
			script:     []recoveryStep{pelicanThinking, answerStep("PELICAN")},
			wantCalls:  2, wantAnswer: "PELICAN", wantStop: Completed, wantRetries: 1,
			check: func(t *testing.T, res Result, c *recoveryCaller, _ *recoveryObs, _ int) {
				if got, want := eventLog(res.Events), "step(0),fence_rotation(1),final_answer_retry(1),token(1),step(1),stop(1)"; got != want {
					t.Errorf("events = %s, want %s", got, want)
				}
				if !strings.Contains(c.reqs[0].Messages[0].Content, fenceKey) {
					t.Fatalf("request 1 system = %q, want the key", c.reqs[0].Messages[0].Content)
				}
				if sys := c.reqs[1].Messages[0]; sys.Role != "system" || strings.Contains(sys.Content, fenceKey) {
					t.Errorf("request 2 system = %+v, want the key rotated", sys)
				}
				if len(res.SystemKeys) != 1 || res.SystemKeys[0] == fenceKey {
					t.Errorf("SystemKeys = %v, want the key rotated", res.SystemKeys)
				}
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &recoveryCaller{script: row.script, cancel: cancel}
			cmgr := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
			if row.compactor != nil {
				cmgr.Compactor = row.compactor
			}
			var opts []Option
			if len(row.interceptors) > 0 {
				opts = append(opts, WithInterceptors(row.interceptors...))
			}
			o := New(c, cmgr, opts...)
			obs := newRecoveryObs()
			if row.obs != nil {
				row.obs(obs)
			}
			invoked := 0
			maxSteps := row.maxSteps
			if maxSteps == 0 {
				maxSteps = 5
			}
			res, err := o.Run(ctx, Request{
				Goal: recoveryGoal, System: row.system, SystemKeys: row.systemKeys,
				Tools: []Tool{spyTool{invoked: &invoked}}, MaxSteps: maxSteps, Budget: row.budget,
				RecoverThinkingOnlyAnswer: !row.flagOff,
			}, obs)
			if row.wantErr == nil && err != nil {
				t.Fatalf("Run: %v", err)
			}
			if row.wantErr != nil && !row.wantErr(err) {
				t.Fatalf("Run error = %v, not the one wanted", err)
			}
			if len(c.reqs) != row.wantCalls {
				t.Errorf("calls = %d, want %d", len(c.reqs), row.wantCalls)
			}
			if res.Answer != row.wantAnswer {
				t.Errorf("Answer = %q, want %q", res.Answer, row.wantAnswer)
			}
			if res.StopReason != row.wantStop {
				t.Errorf("StopReason = %v, want %v", res.StopReason, row.wantStop)
			}
			if n := countKind(res.Events, "final_answer_retry"); n != row.wantRetries {
				t.Errorf("final_answer_retry events = %d, want %d (%s)", n, row.wantRetries, eventLog(res.Events))
			}
			// A check indexes the recorded requests, so it runs only on the
			// expected call count.
			if row.check != nil && len(c.reqs) == row.wantCalls {
				row.check(t, res, c, obs, invoked)
			}
		})
	}
}

// TestFinalAnswerRecoveryTailFit pins the recovery's admission at the exact
// must-fit ceiling and one below it, in legacy and Mixed assembly, with and
// without an advisory. The raw estimator prices the nudge at 0 and empty text
// at 2, so its raw and guarded (Mixed) tail prices differ: pricing the tail
// with the other assembly's estimator fails one of the two ceilings.
func TestFinalAnswerRecoveryTailFit(t *testing.T) {
	est := func(s string) int {
		switch s {
		case recoveryNudge:
			return 0
		case "":
			return 2
		}
		return len([]rune(s))
	}
	sentinel := errors.New("pressure observer failed")
	type variant struct {
		name    string
		mixed   bool
		advised bool
	}
	variants := []variant{
		{name: "legacy"},
		{name: "legacy advisory", advised: true},
		{name: "mixed", mixed: true},
		{name: "mixed advisory", mixed: true, advised: true},
	}
	for _, v := range variants {
		price := ContextManager{Estimate: est, frameToolResults: true}
		if v.mixed {
			price.Estimate = guardedEstimate(est)
		}
		mustFit := price.estimate(withToolTrustContract("")) +
			price.messageCost(Message{ChatMessage: provider.ChatMessage{Role: "user", Content: recoveryGoal}, Segment: Pinned})
		for _, m := range tailMessages() {
			mustFit += price.messageCost(m)
		}
		if v.advised {
			adv := testAdvisory()
			mustFit += price.estimate(renderAdvisoryLines(advisoryPlaceholderOpen, advisoryPlaceholderClose, recoveryGoal, *adv)) - price.estimate(recoveryGoal)
		}
		cases := []struct {
			suffix    string
			ceiling   int
			failPress bool
		}{
			{"exact fit", mustFit, false},
			{"minus 1", mustFit - 1, false},
		}
		if !v.mixed && !v.advised {
			cases = append(cases, struct {
				suffix    string
				ceiling   int
				failPress bool
			}{"minus 1 pressure observer fails", mustFit - 1, true})
		}
		for _, tc := range cases {
			t.Run(v.name+" "+tc.suffix, func(t *testing.T) {
				c := &recoveryCaller{script: []recoveryStep{spyStep(""), thinkingOnlyStep("The code word is PELICAN."), answerStep("PELICAN")}}
				o := New(c, ContextManager{Mixed: v.mixed, Estimate: est})
				obs := newRecoveryObs()
				if tc.failPress {
					obs.failPress, obs.failWith = 2, sentinel
				}
				invoked := 0
				req := Request{
					Goal: recoveryGoal, Tools: []Tool{spyTool{invoked: &invoked, set: mixedTraceSet()}},
					MaxSteps: 5, Budget: Budget{InputCeiling: tc.ceiling}, RecoverThinkingOnlyAnswer: true,
				}
				if v.advised {
					req.Advisory = testAdvisory()
				}
				res, err := o.Run(context.Background(), req, obs)
				if tc.failPress {
					if !errors.Is(err, sentinel) {
						t.Fatalf("Run error = %v, want the pressure observer's", err)
					}
					if n := countKind(res.Events, "final_answer_retry") + countKind(res.Events, "stop"); n != 0 {
						t.Errorf("events = %s, want no retry and no stop", eventLog(res.Events))
					}
					return
				}
				if err != nil {
					t.Fatalf("Run: %v (events %s)", err, eventLog(res.Events))
				}
				if invoked != 1 {
					t.Fatalf("spy invoked %d times, want 1", invoked)
				}
				if v.mixed {
					if _, ok := obs.traces[1]; !ok {
						t.Fatal("the thinking-only step did not take the mixed path")
					}
				}
				if tc.ceiling == mustFit {
					if len(c.reqs) != 3 || countKind(res.Events, "final_answer_retry") != 1 {
						t.Fatalf("calls = %d, events %s; want 3 calls and one retry", len(c.reqs), eventLog(res.Events))
					}
					if res.Answer != "PELICAN" || res.StopReason != Completed {
						t.Errorf("Answer = %q, StopReason = %v", res.Answer, res.StopReason)
					}
					if got := res.Steps[2].Pressure.InputTokens; got != mustFit {
						t.Errorf("recovery prompt = %d, want the must-fit %d", got, mustFit)
					}
					if v.mixed {
						tr, ok := obs.traces[2]
						if !ok {
							t.Fatal("no mixed trace for the recovery step")
						}
						if tr.EstimatedTokensUsed != res.Steps[2].Pressure.InputTokens {
							t.Errorf("trace used %d, recovery pressure %d; want equal", tr.EstimatedTokensUsed, res.Steps[2].Pressure.InputTokens)
						}
						for _, s := range tr.Subjects {
							if s.Subject.Domain != contextdepth.DomainConversation {
								continue
							}
							if i, err := strconv.Atoi(s.Subject.ID); err != nil || i >= 3 {
								t.Errorf("trace subject %+v is not a State message", s.Subject)
							}
						}
					}
					return
				}
				if len(c.reqs) != 2 {
					t.Fatalf("calls = %d, want 2 (the recovery does not fit)", len(c.reqs))
				}
				if res.Answer != "" || res.StopReason != Completed {
					t.Errorf("Answer = %q, StopReason = %v; want empty and Completed", res.Answer, res.StopReason)
				}
				if n := countKind(res.Events, "final_answer_retry"); n != 0 {
					t.Errorf("events = %s, want no retry", eventLog(res.Events))
				}
				if n := countKind(res.Events, "stop"); n != 1 {
					t.Errorf("events = %s, want one stop", eventLog(res.Events))
				}
				m := res.Messages
				if len(m) != 3 || m[0].Content != recoveryGoal || m[1].Role != "assistant" || len(m[1].ToolCalls) != 1 ||
					m[2].Role != "tool" || m[2].Content != "spy-result" {
					t.Errorf("Result.Messages = %+v, want the canonical goal, tool call and tool result", m)
				}
			})
		}
	}
}
