package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

func newRun(ics ...Interceptor) *interceptorRun { return &interceptorRun{chain: ics} }

func inputScope(msgs []InspectedMessage, hasSystem, hasSummary bool) hookScope {
	return hookScope{hook: HookInput, messages: msgs, hasSystem: hasSystem, hasSummary: hasSummary}
}

func inputInvoke(in InputInspection) func(Interceptor) ([]Finding, error) {
	return func(ic Interceptor) ([]Finding, error) { return ic.InspectInput(context.Background(), in) }
}

func TestRunHookRunsEveryInterceptorAfterBlockAndError(t *testing.T) {
	bad := &stubInterceptor{name: "bad", err: errors.New("boom")}
	blocker := blockAll("blocker")
	tail := &stubInterceptor{name: "tail", input: func(InputInspection) []Finding {
		return []Finding{{Rule: "seen", Verdict: VerdictTag, Risk: 5}}
	}}
	rec := &interceptRecorder{}
	r := newRun(bad, blocker, tail)
	findings, verdict, err := r.runHook(context.Background(), rec, 3, inputScope(nil, false, false), inputInvoke(InputInspection{Step: 3}))
	if err == nil || err.Error() != "agent: interceptor bad input: boom" {
		t.Fatalf("err = %v", err)
	}
	if len(tail.inputs) != 1 {
		t.Fatalf("tail ran %d times after an error and a block, want 1 (spec D1)", len(tail.inputs))
	}
	if verdict != VerdictBlock {
		t.Fatalf("verdict = %s, want block", verdict)
	}
	want := []Finding{
		{Interceptor: "blocker", Rule: "deny", Verdict: VerdictBlock, Risk: 100, Hook: HookInput, Step: 3, StateIndex: -1, Group: -1, Alternative: -1},
		{Interceptor: "tail", Rule: "seen", Verdict: VerdictTag, Risk: 5, Hook: HookInput, Step: 3, StateIndex: -1, Group: -1, Alternative: -1},
	}
	if len(findings) != 2 || findings[0] != want[0] || findings[1] != want[1] {
		t.Fatalf("findings = %+v, want %+v", findings, want)
	}
	if r.risk.Score != 105 || len(rec.interceptions) != 1 || rec.interceptions[0].Risk.Score != 105 {
		t.Fatalf("score = %d, events = %+v; findings and telemetry must survive the error", r.risk.Score, rec.interceptions)
	}
}

func TestRunHookJoinsEveryError(t *testing.T) {
	r := newRun(&stubInterceptor{name: "a", err: errors.New("boom")}, &stubInterceptor{name: "b", err: errors.New("bang")})
	_, _, err := r.runHook(context.Background(), normalizeObserver(nil), 0, hookScope{hook: HookOutput},
		func(ic Interceptor) ([]Finding, error) {
			return ic.InspectOutput(context.Background(), OutputInspection{})
		})
	if err == nil || err.Error() != "agent: interceptor a output: boom\nagent: interceptor b output: bang" {
		t.Fatalf("err = %q", err)
	}
}

func TestTerminalAtJoinsBlockedErrorWithHookErrors(t *testing.T) {
	block := []Finding{{Interceptor: "b", Rule: "deny", Verdict: VerdictBlock}}
	err := terminalAt(VerdictBlock, HookInput, 4, block, VerdictBlock, errors.New("boom"))
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Step != 4 || err.Error() != "agent: input blocked by interceptor b (deny)\nboom" {
		t.Fatalf("err = %v (%T)", err, err)
	}
	if err := terminalAt(VerdictBlock, HookInput, 0, nil, VerdictTag, errors.New("boom")); errors.As(err, &blocked) || err.Error() != "boom" {
		t.Fatalf("tag plus error must be the error alone, got %v", err)
	}
	if err := terminalAt(VerdictBlock, HookOutput, 1, block, VerdictBlock, nil); !errors.As(err, &blocked) || blocked.Hook != HookOutput {
		t.Fatalf("block without error must be the BlockedError alone, got %v", err)
	}
	if err := terminalAt(VerdictAbort, HookToolCall, 1, block, VerdictBlock, nil); err != nil {
		t.Fatalf("a recoverable block below the threshold must return nil, got %v", err)
	}
	if err := terminalAt(VerdictAbort, HookToolCall, 1, block, VerdictBlock, errors.New("boom")); !errors.As(err, &blocked) {
		t.Fatalf("a recoverable block WITH an error must still carry the BlockedError, got %v", err)
	}
}

func TestNormalizeFindingValidatesTargetsPerHook(t *testing.T) {
	msgs := []InspectedMessage{
		{StateIndex: 4, Role: "user", Origin: OriginUser},
		{StateIndex: 7, Role: "tool", Origin: OriginWorkspace, ToolCallID: "c7", Alternatives: []InspectedAlternative{{Group: 0, Alternative: 0}, {Group: 0, Alternative: 1}}},
	}
	in := inputScope(msgs, true, false)
	out := hookScope{hook: HookOutput, callIDs: []string{"o1"}}
	tc := hookScope{hook: HookToolCall, toolCallID: "t9"}
	none := func(f Finding) Finding {
		f.Target, f.StateIndex, f.Group, f.Alternative, f.ToolCallID = TargetNone, -1, -1, -1, ""
		return f
	}
	base := Finding{Interceptor: "ic", Rule: "r", Hook: HookInput, Step: 2}
	cases := []struct {
		name  string
		scope hookScope
		in    Finding
		want  Finding
	}{
		{"system present", in, Finding{Target: TargetSystem, StateIndex: 3, ToolCallID: "x", Origin: OriginForeign},
			func() Finding { f := none(base); f.Target, f.Origin = TargetSystem, OriginSystem; return f }()},
		{"summary absent", in, Finding{Target: TargetSummary, Origin: OriginForeign},
			func() Finding { f := none(base); f.Origin = OriginForeign; return f }()},
		{"message valid alternative", in, Finding{Target: TargetMessage, StateIndex: 7, Group: 0, Alternative: 1, Origin: OriginForeign},
			func() Finding {
				f := base
				f.Target, f.StateIndex, f.Group, f.Alternative, f.ToolCallID, f.Origin = TargetMessage, 7, 0, 1, "c7", OriginWorkspace
				return f
			}()},
		{"message invalid alternative", in, Finding{Target: TargetMessage, StateIndex: 7, Group: 3, Alternative: 0},
			func() Finding {
				f := base
				f.Target, f.StateIndex, f.Group, f.Alternative, f.ToolCallID, f.Origin = TargetMessage, 7, -1, -1, "c7", OriginWorkspace
				return f
			}()},
		{"message unknown index", in, Finding{Target: TargetMessage, StateIndex: 9, Origin: OriginForeign},
			func() Finding { f := none(base); f.Origin = OriginForeign; return f }()},
		{"output kind in input hook", in, Finding{Target: TargetOutputContent, Origin: Origin(99)},
			func() Finding { f := none(base); f.Origin = OriginUnknown; return f }()},
		{"output content", out, Finding{Target: TargetOutputContent, Origin: OriginForeign, StateIndex: 5},
			func() Finding {
				f := none(base)
				f.Hook, f.Target, f.Origin = HookOutput, TargetOutputContent, OriginModel
				return f
			}()},
		{"output tool call valid", out, Finding{Target: TargetOutputToolCall, ToolCallID: "o1"},
			func() Finding {
				f := none(base)
				f.Hook, f.Target, f.ToolCallID, f.Origin = HookOutput, TargetOutputToolCall, "o1", OriginModel
				return f
			}()},
		{"output tool call unknown id", out, Finding{Target: TargetOutputToolCall, ToolCallID: "zz"},
			func() Finding { f := none(base); f.Hook, f.Origin = HookOutput, OriginModel; return f }()},
		{"tool call hook forces kind", tc, Finding{Target: TargetMessage, StateIndex: 1, ToolCallID: "spoof", Origin: OriginForeign},
			func() Finding {
				f := none(base)
				f.Hook, f.Target, f.ToolCallID, f.Origin = HookToolCall, TargetToolCall, "t9", OriginModel
				return f
			}()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Rule = "r"
			got := normalizeFinding(c.in, "ic", 2, c.scope)
			if got != c.want {
				t.Fatalf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestRunHookNormalizesValues(t *testing.T) {
	long := strings.Repeat("x", 300)
	ic := &stubInterceptor{name: "real", input: func(InputInspection) []Finding {
		return []Finding{
			{Interceptor: "spoofed", Verdict: Verdict(9), Risk: 500, Detail: "line one\nline two"},
			{Rule: "neg", Risk: -5, Detail: long},
		}
	}}
	r := newRun(ic)
	got, verdict, err := r.runHook(context.Background(), normalizeObserver(nil), 2, inputScope(nil, false, false), inputInvoke(InputInspection{}))
	if err != nil {
		t.Fatal(err)
	}
	want0 := Finding{Interceptor: "real", Rule: "unspecified", Verdict: VerdictAbort, Risk: 100, Detail: "line one line two",
		Hook: HookInput, Step: 2, StateIndex: -1, Group: -1, Alternative: -1}
	if got[0] != want0 {
		t.Fatalf("finding 0 = %+v, want %+v", got[0], want0)
	}
	if len(got[1].Detail) != 256 || got[1].Risk != 0 || verdict != VerdictAbort {
		t.Fatalf("finding 1 = %+v, verdict %s", got[1], verdict)
	}
	if r.risk.Score != 100 {
		t.Fatalf("score = %d, want 100", r.risk.Score)
	}
}

func TestRunHookScoreSaturates(t *testing.T) {
	r := newRun(&stubInterceptor{name: "ic", input: func(InputInspection) []Finding {
		return []Finding{{Rule: "r", Risk: 100}}
	}})
	r.risk.Score = math.MaxInt - 10
	if _, _, err := r.runHook(context.Background(), normalizeObserver(nil), 0, inputScope(nil, false, false), inputInvoke(InputInspection{})); err != nil {
		t.Fatal(err)
	}
	if r.risk.Score != math.MaxInt {
		t.Fatalf("score = %d, want MaxInt", r.risk.Score)
	}
}

func TestRunHookSnapshotsAndEvents(t *testing.T) {
	ic := &stubInterceptor{name: "ic",
		input:    func(InputInspection) []Finding { return []Finding{{Rule: "a", Verdict: VerdictTag, Risk: 30}} },
		toolCall: func(ToolCallInspection) []Finding { return []Finding{{Rule: "b", Verdict: VerdictTag, Risk: 50}} },
	}
	rec := &interceptRecorder{}
	r := newRun(ic)
	if _, _, err := r.runHook(context.Background(), rec, 0, inputScope(nil, false, false), inputInvoke(InputInspection{})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.runHook(context.Background(), rec, 1, hookScope{hook: HookToolCall, toolCallID: "call-7"},
		func(ic Interceptor) ([]Finding, error) {
			return ic.InspectToolCall(context.Background(), ToolCallInspection{})
		}); err != nil {
		t.Fatal(err)
	}
	e1 := rec.interceptions[1]
	if e1.Hook != HookToolCall || e1.ToolCallID != "call-7" || e1.Risk.Score != 80 || len(e1.Risk.Findings) != 2 {
		t.Fatalf("event 1 = %+v", e1)
	}
	if f := e1.Findings[0]; f.Target != TargetToolCall || f.ToolCallID != "call-7" || f.Origin != OriginModel {
		t.Fatalf("tool-call finding = %+v", f)
	}
	e1.Risk.Findings[0].Risk = 999
	e1.Findings[0].Risk = 999
	if r.risk.Findings[0].Risk != 30 {
		t.Fatal("observer wrote through to the run's report: snapshot missing")
	}
	snap := r.snapshot()
	snap.Findings[1].Risk = 999
	if r.risk.Findings[1].Risk != 50 {
		t.Fatal("snapshot aliases the run's findings")
	}
}

func TestRunHookNoFindingsNoEventAndObserverErrorJoined(t *testing.T) {
	rec := &interceptRecorder{}
	r := newRun(&stubInterceptor{name: "quiet"})
	findings, verdict, err := r.runHook(context.Background(), rec, 0, hookScope{hook: HookOutput},
		func(ic Interceptor) ([]Finding, error) {
			return ic.InspectOutput(context.Background(), OutputInspection{})
		})
	if err != nil || findings != nil || verdict != VerdictAllow || len(rec.interceptions) != 0 {
		t.Fatalf("got findings=%v verdict=%s events=%d err=%v; want none", findings, verdict, len(rec.interceptions), err)
	}
	rec = &interceptRecorder{err: errors.New("sink down")}
	r = newRun(blockAll("b"))
	_, _, err = r.runHook(context.Background(), rec, 0, inputScope(nil, false, false), inputInvoke(InputInspection{}))
	if err == nil || err.Error() != "sink down" {
		t.Fatalf("err = %v, want the observer's error", err)
	}
	if r.result() == nil || r.result().Score != 100 {
		t.Fatalf("result = %+v, want the finding retained", r.result())
	}
}
