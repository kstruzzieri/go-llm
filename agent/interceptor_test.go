package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// stubInterceptor records every inspection and answers with the configured
// functions. A nil function returns no findings; err, when set, is returned
// by every hook after recording.
type stubInterceptor struct {
	name     string
	err      error
	input    func(InputInspection) []Finding
	output   func(OutputInspection) []Finding
	toolCall func(ToolCallInspection) []Finding
	inputs   []InputInspection
	outputs  []OutputInspection
	calls    []ToolCallInspection
}

func (s *stubInterceptor) Name() string { return s.name }

func (s *stubInterceptor) InspectInput(_ context.Context, in InputInspection) ([]Finding, error) {
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		return nil, s.err
	}
	if s.input == nil {
		return nil, nil
	}
	return s.input(in), nil
}

func (s *stubInterceptor) InspectOutput(_ context.Context, out OutputInspection) ([]Finding, error) {
	s.outputs = append(s.outputs, out)
	if s.err != nil {
		return nil, s.err
	}
	if s.output == nil {
		return nil, nil
	}
	return s.output(out), nil
}

func (s *stubInterceptor) InspectToolCall(_ context.Context, call ToolCallInspection) ([]Finding, error) {
	s.calls = append(s.calls, call)
	if s.err != nil {
		return nil, s.err
	}
	if s.toolCall == nil {
		return nil, nil
	}
	return s.toolCall(call), nil
}

// verdictAll answers every hook with one finding of the given verdict.
func verdictAll(name string, v Verdict) *stubInterceptor {
	f := []Finding{{Rule: "deny", Verdict: v, Risk: 100}}
	return &stubInterceptor{
		name:     name,
		input:    func(InputInspection) []Finding { return f },
		output:   func(OutputInspection) []Finding { return f },
		toolCall: func(ToolCallInspection) []Finding { return f },
	}
}

func blockAll(name string) *stubInterceptor { return verdictAll(name, VerdictBlock) }

// scopedStub is a RunScopedInterceptor: ForRun hands out a fresh stub and an
// addendum carrying the ForRun sequence number.
type scopedStub struct {
	mu      sync.Mutex
	name    string
	forRuns int
	nilRun  bool
	rename  bool
	scopes  []RunScope
	runs    []*stubInterceptor
}

func (s *scopedStub) Name() string { return s.name }
func (s *scopedStub) InspectInput(context.Context, InputInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (s *scopedStub) InspectOutput(context.Context, OutputInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (s *scopedStub) InspectToolCall(context.Context, ToolCallInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (s *scopedStub) ForRun(_ context.Context, scope RunScope) (Interceptor, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forRuns++
	s.scopes = append(s.scopes, scope)
	if s.nilRun {
		return nil, "", nil
	}
	name := s.name
	if s.rename {
		name = "other"
	}
	run := &stubInterceptor{name: name}
	s.runs = append(s.runs, run)
	return run, " [canary:" + strings.Repeat("x", s.forRuns) + "]", nil
}

// interceptRecorder records observer callbacks in loop order.
type interceptRecorder struct {
	kinds         []string
	interceptions []InterceptionEvent
	toolResults   []ToolResultEvent
	err           error // returned by OnInterception when set
	onToolResult  func(*ToolResultEvent)
}

func (r *interceptRecorder) OnStep(context.Context, StepEvent) error {
	r.kinds = append(r.kinds, "step")
	return nil
}
func (r *interceptRecorder) OnToolCall(context.Context, ToolCallEvent) error {
	r.kinds = append(r.kinds, "tool_call")
	return nil
}
func (r *interceptRecorder) OnToken(context.Context, TokenEvent) error {
	r.kinds = append(r.kinds, "token")
	return nil
}
func (r *interceptRecorder) OnPressure(context.Context, PressureEvent) error {
	r.kinds = append(r.kinds, "pressure")
	return nil
}
func (r *interceptRecorder) OnToolResult(_ context.Context, e ToolResultEvent) error {
	r.kinds = append(r.kinds, "tool_result")
	if r.onToolResult != nil {
		r.onToolResult(&e)
	}
	r.toolResults = append(r.toolResults, e)
	return nil
}
func (r *interceptRecorder) OnInterception(_ context.Context, e InterceptionEvent) error {
	r.kinds = append(r.kinds, "interception")
	r.interceptions = append(r.interceptions, e)
	return r.err
}

// capturingScriptedCaller is scriptedCaller plus every request sent.
type capturingScriptedCaller struct {
	scriptedCaller
	reqs []provider.ChatRequest
}

func (c *capturingScriptedCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (ModelResult, error) {
	c.reqs = append(c.reqs, req)
	return c.scriptedCaller.Chat(ctx, req, onToken)
}

func finalAnswer(s string) ModelResult {
	return ModelResult{Response: provider.ChatResponse{Content: s, Done: true}}
}

func kinds(events []EventRecord) string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return strings.Join(out, ",")
}

func TestRunRejectsInvalidInterceptors(t *testing.T) {
	cases := []struct {
		name string
		ics  []Interceptor
		want string
	}{
		{"nil", []Interceptor{nil}, "agent: nil interceptor at index 0"},
		{"empty name", []Interceptor{&stubInterceptor{name: ""}}, "agent: interceptor at index 0 has an empty name"},
		{"duplicate", []Interceptor{&stubInterceptor{name: "dup"}, &stubInterceptor{name: "dup"}}, `agent: duplicate interceptor name "dup"`},
		{"scoped nil", []Interceptor{&scopedStub{name: "sc", nilRun: true}}, "agent: interceptor sc returned a nil run instance"},
		{"scoped renamed", []Interceptor{&scopedStub{name: "sc", rename: true}}, `agent: interceptor sc returned a run instance named "other"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &scriptedCaller{responses: []ModelResult{finalAnswer("x")}}
			o := newTestOrchestrator(mc, WithInterceptors(tc.ics...))
			_, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if mc.calls != 0 {
				t.Fatalf("model called %d times with an invalid interceptor set", mc.calls)
			}
		})
	}
}

func TestRunScopedInterceptorResolvedPerRunWithAddendum(t *testing.T) {
	sc := &scopedStub{name: "canary"}
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: []ModelResult{finalAnswer("a"), finalAnswer("b")}}}
	o := newTestOrchestrator(mc, WithInterceptors(sc))
	for i := 0; i < 2; i++ {
		if _, err := o.Run(context.Background(), Request{Goal: "q", System: "sys"}, nil); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if sc.forRuns != 2 || len(sc.runs) != 2 || sc.scopes[0] != (RunScope{System: "sys"}) {
		t.Fatalf("ForRun calls = %d, instances = %d, scopes = %+v", sc.forRuns, len(sc.runs), sc.scopes)
	}
	if got := mc.reqs[0].Messages[0].Content; got != "sys [canary:x]" {
		t.Fatalf("run 0 system = %q", got)
	}
	if got := mc.reqs[1].Messages[0].Content; got != "sys [canary:xx]" {
		t.Fatalf("run 1 system = %q, want its own addendum only", got)
	}
}

func TestPreChangeEncodingsAreByteIdentical(t *testing.T) {
	res := Result{
		Answer:    "done",
		Steps:     []StepRecord{{Index: 0, Response: provider.ChatResponse{Content: "done", Done: true}}},
		Events:    []EventRecord{{Step: 0, Kind: "step"}, {Step: 0, Kind: "stop"}},
		ToolCalls: []ToolCallRecord{{Step: 0, Name: "read_file", Invoked: true}},
	}
	cases := []struct {
		name string
		v    any
		want string
	}{
		{"Result", res, `{"Answer":"done","Messages":null,"Steps":[{"Index":0,"Response":{"model":"","provider":"","content":"done","done":true,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"latency":{"load_duration":0,"prompt_eval_duration":0,"generation_duration":0}},"RouteOutcome":null,"Pressure":{"UsedPct":0,"Evicted":0,"Compactions":0,"AnchorOmissions":0,"InputTokens":0,"InputBudget":0,"Level":0,"Cause":0,"Mitigation":0},"Latency":0}],"Events":[{"Step":0,"Kind":"step"},{"Step":0,"Kind":"stop"}],"Usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"ToolCalls":[{"Step":0,"Name":"read_file","IsError":false,"Denied":false,"Invoked":true,"Latency":0}],"StopReason":0}`},
		{"ToolResult", ToolResult{Content: "x"}, `{"Content":"x","IsError":false,"Preview":"","Truncated":false,"Attrib":null,"Context":null,"RouteOutcome":null}`},
		{"ToolCallRecord", ToolCallRecord{Step: 1, Name: "n", Invoked: true}, `{"Step":1,"Name":"n","IsError":false,"Denied":false,"Invoked":true,"Latency":0}`},
	}
	for _, tc := range cases {
		b, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Fatalf("%s encoding changed:\n got %s\nwant %s", tc.name, b, tc.want)
		}
	}
}

func TestBlockedErrorNamesTheFirstFindingAtTheMaximumVerdict(t *testing.T) {
	err := &BlockedError{Hook: HookToolCall, Step: 2, Findings: []Finding{
		{Interceptor: "zw", Rule: "zero_width", Verdict: VerdictTag},
		{Interceptor: "path", Rule: "escape", Verdict: VerdictBlock},
		{Interceptor: "canary", Rule: "nonce_in_args", Verdict: VerdictAbort},
		{Interceptor: "second", Rule: "also_abort", Verdict: VerdictAbort},
	}}
	if got := err.Error(); got != "agent: tool_call blocked by interceptor canary (nonce_in_args)" {
		t.Fatalf("Error() = %q", got)
	}
	var target *BlockedError
	if !errors.As(error(err), &target) {
		t.Fatal("errors.As must find *BlockedError")
	}
}

func TestEnumStrings(t *testing.T) {
	if s := strings.Join([]string{HookInput.String(), HookOutput.String(), HookToolCall.String(), Hook(0).String()}, ","); s != "input,output,tool_call,unknown" {
		t.Fatalf("hooks = %s", s)
	}
	if s := strings.Join([]string{VerdictAllow.String(), VerdictTag.String(), VerdictBlock.String(), VerdictAbort.String(), Verdict(9).String()}, ","); s != "allow,tag,block,abort,unknown" {
		t.Fatalf("verdicts = %s", s)
	}
	if s := strings.Join([]string{OriginUnknown.String(), OriginUser.String(), OriginSystem.String(), OriginModel.String(), OriginWorkspace.String(), OriginForeign.String(), Origin(99).String()}, ","); s != "unknown,user,system,model,workspace,foreign,unknown" {
		t.Fatalf("origins = %s", s)
	}
	if s := strings.Join([]string{TargetNone.String(), TargetSystem.String(), TargetSummary.String(), TargetMessage.String(), TargetOutputContent.String(), TargetOutputToolCall.String(), TargetToolCall.String(), TargetKind(9).String()}, ","); s != "none,system,summary,message,output_content,output_tool_call,tool_call,unknown" {
		t.Fatalf("targets = %s", s)
	}
	if normalizeOrigin(Origin(99)) != OriginUnknown || normalizeOrigin(OriginForeign) != OriginForeign {
		t.Fatal("normalizeOrigin must map unknown values to OriginUnknown and keep known ones")
	}
}
