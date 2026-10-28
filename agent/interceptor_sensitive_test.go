package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
	"github.com/kstruzzieri/go-llm/provider"
)

// exactFake stands in for the ConfiguredSecrets guard (#424): it blocks a
// call whose arguments carry fixture, decoding JSON exactly as the real
// guard does. A plain substring check over the all-escaped arguments the
// tests send would never fire, and every test here would pass vacuously.
type exactFake struct {
	name                 string
	fixture              string
	atOutput, atToolCall bool
	tag                  bool // report a match as a Tag, which marks nothing
}

func (e exactFake) Name() string                { return e.name }
func (e exactFake) SensitiveText(s string) bool { return strings.Contains(s, e.fixture) }
func (e exactFake) InspectInput(context.Context, InputInspection) ([]Finding, error) {
	return nil, nil
}

func (e exactFake) InspectOutput(_ context.Context, out OutputInspection) ([]Finding, error) {
	if e.atOutput && slices.ContainsFunc(out.ToolCalls, e.hit) {
		return e.finding(), nil
	}
	return nil, nil
}

func (e exactFake) InspectToolCall(_ context.Context, in ToolCallInspection) ([]Finding, error) {
	if e.atToolCall && e.hit(in.Call) {
		return e.finding(), nil
	}
	return nil, nil
}

// finding targets the output content; the tool-call hook retargets it.
func (e exactFake) finding() []Finding {
	v := VerdictBlock
	if e.tag {
		v = VerdictTag
	}
	return []Finding{{Rule: "configured", Verdict: v, Risk: 100, Target: TargetOutputContent}}
}

func (e exactFake) hit(c provider.ToolCall) bool {
	return secretscan.MatchJSON(c.Function.Arguments, e.SensitiveText)
}

// echoFake reports the raw argument document of the first output tool call
// as both its finding's Detail and its error: the diagnostics a sensitive
// block must drop, whatever their form.
type echoFake struct{ verdict Verdict }

func (echoFake) Name() string { return "echo" }
func (echoFake) InspectInput(context.Context, InputInspection) ([]Finding, error) {
	return nil, nil
}
func (e echoFake) InspectOutput(_ context.Context, out OutputInspection) ([]Finding, error) {
	if len(out.ToolCalls) == 0 {
		return nil, nil
	}
	c := out.ToolCalls[0]
	doc := string(c.Function.Arguments)
	return []Finding{{Rule: "echo", Verdict: e.verdict, Risk: 10, Detail: doc, Target: TargetOutputToolCall, ToolCallID: c.ID}}, errors.New(doc)
}
func (echoFake) InspectToolCall(context.Context, ToolCallInspection) ([]Finding, error) {
	return nil, nil
}

// scopedExact is a RunScopedInterceptor that does NOT implement
// SensitiveTextInterceptor itself; only its per-run instance does.
type scopedExact struct{ run exactFake }

func (s scopedExact) Name() string { return s.run.name }
func (scopedExact) InspectInput(context.Context, InputInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (scopedExact) InspectOutput(context.Context, OutputInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (scopedExact) InspectToolCall(context.Context, ToolCallInspection) ([]Finding, error) {
	panic("shared instance used")
}
func (s scopedExact) ForRun(context.Context, RunScope) (Interceptor, string, error) {
	return s.run, "", nil
}

// sensitiveFixture is a synthetic value generated per test. It holds bytes
// that json.Marshal escapes (<, >, &) so that only a decoding check finds it
// in encoded output, and none that Go quoting escapes, so %#v renders it
// verbatim. Its escaped form writes every byte as \u00XX.
type sensitiveFixture struct {
	value, escaped string
}

func newSensitiveFixture(t *testing.T) sensitiveFixture {
	t.Helper()
	v := "mule<orbit>&" + rand.Text()
	var b strings.Builder
	for i := 0; i < len(v); i++ {
		_, _ = fmt.Fprintf(&b, `\u%04x`, v[i])
	}
	return sensitiveFixture{value: v, escaped: b.String()}
}

// args is an argument document carrying the fixture only as \u escapes. The
// guard fixtures must see through the escapes; a raw substring check must not.
func (f sensitiveFixture) args(t *testing.T) string {
	t.Helper()
	doc := `{"q":"` + f.escaped + `"}`
	if strings.Contains(doc, f.value) {
		t.Fatalf("argument document %q carries the literal fixture", doc)
	}
	if !secretscan.MatchJSON([]byte(doc), func(s string) bool { return s == f.value }) {
		t.Fatalf("argument document %q does not decode to the fixture", doc)
	}
	return doc
}

// forms are the renderings of the fixture a leak can take: the literal, its
// escaped form, and the escaped form as Go quoting (%#v) doubles it.
func (f sensitiveFixture) forms() []string {
	return []string{f.value, f.escaped, strings.ReplaceAll(f.escaped, `\`, `\\`)}
}

func (f sensitiveFixture) assertAbsent(t *testing.T, label, text string) {
	t.Helper()
	for _, form := range f.forms() {
		if strings.Contains(text, form) {
			t.Errorf("%s carries the fixture (form %q): %s", label, form, text)
		}
	}
}

// assertErrorClean renders every node of the error tree with %v, %+v and
// %#v, and JSON-encodes every node.
func (f sensitiveFixture) assertErrorClean(t *testing.T, err error) {
	t.Helper()
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		for _, verb := range []string{"%v", "%+v", "%#v"} {
			f.assertAbsent(t, fmt.Sprintf("error node %T rendered %s", e, verb), fmt.Sprintf(verb, e))
		}
		f.assertJSONClean(t, fmt.Sprintf("error node %T", e), e)
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		}
	}
	walk(err)
}

// assertJSONClean encodes v and checks the encoded text and every decoded
// JSON key and string, independently of the production walker.
func (f sensitiveFixture) assertJSONClean(t *testing.T, label string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: encode: %v", label, err)
	}
	f.assertAbsent(t, label+" JSON", string(b))
	var doc any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("%s: decode: %v", label, err)
	}
	var walk func(any)
	walk = func(n any) {
		switch n := n.(type) {
		case string:
			f.assertAbsent(t, label+" decoded JSON string", n)
		case map[string]any:
			for k, c := range n {
				f.assertAbsent(t, label+" decoded JSON key", k)
				walk(c)
			}
		case []any:
			for _, c := range n {
				walk(c)
			}
		}
	}
	walk(doc)
}

func oneCall(id, name, args string) ModelResult {
	return ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{toolCall(id, name, args)}}}
}

func requireBlocked(t *testing.T, err error, hook Hook) *BlockedError {
	t.Helper()
	var blocked *BlockedError
	if !errors.As(err, &blocked) || blocked.Hook != hook {
		t.Fatalf("err = %v, want a *BlockedError at %s", err, hook)
	}
	return blocked
}

// sensitiveChain installs the guard and an echoing Tag interceptor in either
// order: the guard may sit anywhere in a host's chain, so findings recorded
// before it fires must lose their text too. names is the resulting order.
func sensitiveChain(exact exactFake, echoFirst bool) (chain []Interceptor, names []string) {
	if echoFirst {
		return []Interceptor{echoFake{verdict: VerdictTag}, exact}, []string{"echo", exact.name}
	}
	return []Interceptor{exact, echoFake{verdict: VerdictTag}}, []string{exact.name, "echo"}
}

func TestSensitiveBlockDropsAggregateFreeText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		echoFirst bool
	}{{"guard first", false}, {"guard after echo", true}} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSensitiveFixture(t)
			chain, names := sensitiveChain(exactFake{name: "exact", fixture: fx.value, atOutput: true}, tc.echoFirst)
			mc := &scriptedCaller{responses: []ModelResult{oneCall("id-"+fx.value, "echo", fx.args(t)), finalAnswer("never")}}
			rec := &interceptRecorder{}
			o := newTestOrchestrator(mc, WithInterceptors(chain...))
			res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec)
			blocked := requireBlocked(t, err, HookOutput)

			// Every finding keeps its bounded metadata, so dropping findings
			// cannot pass for dropping their text.
			if len(blocked.Findings) != 2 || blocked.Findings[0].Interceptor != names[0] || blocked.Findings[1].Interceptor != names[1] {
				t.Fatalf("findings = %+v, want %v in chain order", blocked.Findings, names)
			}
			echo := blocked.Findings[slices.Index(names, "echo")]
			if echo.Rule != "echo" || echo.Verdict != VerdictTag || echo.Target != TargetOutputToolCall {
				t.Fatalf("echo finding = %+v, want its rule, verdict and target kept", echo)
			}
			if res.Risk == nil || len(rec.interceptions) != 1 {
				t.Fatalf("risk = %+v, interceptions = %d; want the block published once", res.Risk, len(rec.interceptions))
			}
			sets := map[string][]Finding{"BlockedError": blocked.Findings, "Result.Risk": res.Risk.Findings}
			for i, e := range rec.interceptions {
				sets[fmt.Sprintf("event %d", i)] = e.Findings
				sets[fmt.Sprintf("event %d risk", i)] = e.Risk.Findings
				if e.ToolCallID != "" {
					t.Errorf("event %d ToolCallID = %q, want empty", i, e.ToolCallID)
				}
			}
			for label, fs := range sets {
				for _, f := range fs {
					if f.Detail != "" || f.ToolCallID != "" {
						t.Errorf("%s finding %s/%s keeps Detail %q, ToolCallID %q", label, f.Interceptor, f.Rule, f.Detail, f.ToolCallID)
					}
				}
			}
			fx.assertErrorClean(t, err)
			fx.assertJSONClean(t, "events", rec.interceptions)
			fx.assertJSONClean(t, "risk", res.Risk)
			fx.assertJSONClean(t, "result", res)
		})
	}
}

func TestSensitiveToolCallHookClearsEventID(t *testing.T) {
	fx := newSensitiveFixture(t)
	exact := exactFake{name: "exact", fixture: fx.value, atToolCall: true}
	mc := &scriptedCaller{responses: []ModelResult{oneCall("id-"+fx.value, "echo", fx.args(t)), finalAnswer("done")}}
	rec := &interceptRecorder{}
	o := newTestOrchestrator(mc, WithInterceptors(exact))
	if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var hooked []InterceptionEvent
	for _, e := range rec.interceptions {
		if e.Hook == HookToolCall {
			hooked = append(hooked, e)
		}
	}
	if len(hooked) != 1 || len(hooked[0].Findings) != 1 {
		t.Fatalf("tool-call events = %+v, want one with one finding", hooked)
	}
	e := hooked[0]
	if e.ToolCallID != "" || e.Findings[0].ToolCallID != "" || e.Findings[0].Target != TargetToolCall {
		t.Fatalf("event ToolCallID = %q, finding ToolCallID = %q, target %s; want both IDs empty", e.ToolCallID, e.Findings[0].ToolCallID, e.Findings[0].Target)
	}
	fx.assertJSONClean(t, "tool-call event", e)
}

func TestSensitiveToolCallHookBlockStaysRecoverable(t *testing.T) {
	fx := newSensitiveFixture(t)
	exact := exactFake{name: "exact", fixture: fx.value, atToolCall: true}
	tool := &countingWriteTool{writeTool: writeTool{planPreview: "P", planKey: "k"}}
	ap := &riskCapturingApprover{keyedCapturingApprover: keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}}
	mc := &scriptedCaller{responses: []ModelResult{oneCall("1", "writer", fx.args(t)), finalAnswer("done")}}
	o := newTestOrchestrator(mc, WithInterceptors(exact))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{tool}, Approver: ap}, &interceptRecorder{})
	if err != nil {
		t.Fatalf("Run: %v; a tool-call block with no hook error is a model-visible observation", err)
	}
	if tool.planned != 0 || tool.invoked != 0 || ap.riskCalls+ap.keyedCalls+ap.plainCalls != 0 {
		t.Fatalf("planned=%d invoked=%d approvals=%d, want 0/0/0", tool.planned, tool.invoked, ap.riskCalls+ap.keyedCalls+ap.plainCalls)
	}
	if mc.calls != 2 || res.Answer != "done" || res.StopReason != Completed {
		t.Fatalf("model calls = %d, answer %q, stop %s; want a further turn that completes", mc.calls, res.Answer, res.StopReason)
	}
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].IsError || !res.ToolCalls[0].Blocked {
		t.Fatalf("records = %+v, want one blocked error", res.ToolCalls)
	}
	const want = "tool call blocked by interceptor exact (configured)"
	if len(res.Messages) != 4 || res.Messages[2].Role != "tool" || res.Messages[2].Content != want {
		t.Fatalf("messages = %+v, want the synthetic %q observation", res.Messages, want)
	}
}

func TestSensitiveBlockBoundsHookAndObserverErrors(t *testing.T) {
	plain := func(doc string) error { return errors.New(doc) }
	cases := []struct {
		name      string
		wrap      func(doc string) error
		is        []error
		echoFirst bool
	}{
		{"plain", plain, nil, false},
		{"canceled", func(doc string) error { return fmt.Errorf("observer saw %s: %w", doc, context.Canceled) }, []error{context.Canceled}, false},
		{"deadline", func(doc string) error { return fmt.Errorf("observer saw %s: %w", doc, context.DeadlineExceeded) }, []error{context.DeadlineExceeded}, false},
		{"plain, guard after echo", plain, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSensitiveFixture(t)
			doc := fx.args(t)
			chain, _ := sensitiveChain(exactFake{name: "exact", fixture: fx.value, atOutput: true}, tc.echoFirst)
			mc := &scriptedCaller{responses: []ModelResult{oneCall("1", "echo", doc), finalAnswer("never")}}
			rec := &interceptRecorder{err: tc.wrap(doc)}
			o := newTestOrchestrator(mc, WithInterceptors(chain...))
			_, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec)
			_ = requireBlocked(t, err, HookOutput)
			if !errors.Is(err, ErrDiagnosticsSuppressed) {
				t.Fatalf("err = %v, want ErrDiagnosticsSuppressed", err)
			}
			for _, target := range []error{context.Canceled, context.DeadlineExceeded} {
				want := slices.Contains(tc.is, target)
				if errors.Is(err, target) != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v", target, !want, want)
				}
			}
			fx.assertErrorClean(t, err)
		})
	}
}

func TestSensitiveBlockStopsPublication(t *testing.T) {
	cases := []struct {
		name string
		// calls builds the batch; bad is either the fixture document or a
		// clean one for the control run.
		calls      func(bad string) []provider.ToolCall
		writers    int    // planning, approval-gated calls in the batch
		wantEvents string // the control run's events, which pin the dispatch path
	}{
		{"single call", func(bad string) []provider.ToolCall {
			return []provider.ToolCall{toolCall("1", "writer", bad)}
		}, 1, "step,tool_call,tool_result,token,step,stop"},
		{"clean+bad serial", func(bad string) []provider.ToolCall {
			return []provider.ToolCall{toolCall("1", "writer", `{"q":"clean"}`), toolCall("2", "writer", bad)}
		}, 2, "step,tool_call,tool_result,tool_call,tool_result,token,step,stop"},
		{"clean+bad parallel", func(bad string) []provider.ToolCall {
			return []provider.ToolCall{toolCall("1", "a", `{"q":"clean"}`), toolCall("2", "b", bad)}
		}, 0, "step,tool_call,tool_call,tool_result,tool_result,token,step,stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newSensitiveFixture(t)
			type counts struct{ steps, toolCalls, plans, approvals, invokes, modelCalls int }
			run := func(bad string) (counts, Result, error) {
				writer := &countingWriteTool{writeTool: writeTool{planPreview: "P", planKey: "k"}}
				a := &invokeCountingTool{echoTool: echoTool{name: "a"}}
				b := &invokeCountingTool{echoTool: echoTool{name: "b"}}
				ap := &riskCapturingApprover{keyedCapturingApprover: keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}}
				batch := ModelResult{Response: provider.ChatResponse{ToolCalls: tc.calls(bad)}}
				mc := &scriptedCaller{responses: []ModelResult{batch, finalAnswer("done")}}
				rec := &interceptRecorder{}
				exact := exactFake{name: "exact", fixture: fx.value, atOutput: true, atToolCall: true}
				o := newTestOrchestrator(mc, WithInterceptors(exact))
				res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writer, a, b}, Approver: ap}, rec)
				c := counts{
					plans: writer.planned, approvals: ap.riskCalls + ap.keyedCalls + ap.plainCalls,
					invokes: writer.invoked + a.invoked + b.invoked, modelCalls: mc.calls,
				}
				for _, k := range rec.kinds {
					switch k {
					case "step":
						c.steps++
					case "tool_call":
						c.toolCalls++
					}
				}
				return c, res, err
			}

			// Control: with clean arguments the same batch publishes, plans,
			// prompts and invokes, so the zero counts below are not vacuous.
			n := len(tc.calls(""))
			got, res, err := run(`{"q":"also clean"}`)
			if err != nil || kinds(res.Events) != tc.wantEvents {
				t.Fatalf("control: err = %v, events = %s; want %s", err, kinds(res.Events), tc.wantEvents)
			}
			if want := (counts{steps: 2, toolCalls: n, plans: tc.writers, approvals: tc.writers, invokes: n, modelCalls: 2}); got != want {
				t.Fatalf("control counts = %+v, want %+v", got, want)
			}

			got, res, err = run(fx.args(t))
			_ = requireBlocked(t, err, HookOutput)
			if got != (counts{modelCalls: 1}) {
				t.Fatalf("counts = %+v; want no publication, plan, approval, invocation or further model call", got)
			}
			if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
				t.Fatalf("messages = %+v, want only the goal", res.Messages)
			}
		})
	}
}

func TestSensitiveCapabilitySurvivesRunScopedResolution(t *testing.T) {
	fx := newSensitiveFixture(t)
	scoped := scopedExact{run: exactFake{name: "exact", fixture: fx.value, atOutput: true}}
	if _, ok := Interceptor(scoped).(SensitiveTextInterceptor); ok {
		t.Fatal("the installed instance must not carry the capability itself")
	}
	mc := &scriptedCaller{responses: []ModelResult{oneCall("1", "echo", fx.args(t)), finalAnswer("never")}}
	o := newTestOrchestrator(mc, WithInterceptors(scoped, echoFake{verdict: VerdictTag}))
	_, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, nil)
	blocked := requireBlocked(t, err, HookOutput)
	if len(blocked.Findings) != 2 || blocked.Findings[1].Detail != "" || !errors.Is(err, ErrDiagnosticsSuppressed) {
		t.Fatalf("findings = %+v, err = %v; want the per-run instance's block to suppress diagnostics", blocked.Findings, err)
	}
}

func TestNonSensitiveBlockKeepsDetail(t *testing.T) {
	const doc = `{"q":"benign"}`
	cases := []struct {
		name     string
		chain    []Interceptor
		findings int
	}{
		{"echo alone", []Interceptor{echoFake{verdict: VerdictBlock}}, 1},
		// A sensitive interceptor that is installed but silent marks nothing.
		{"exact installed but clean", []Interceptor{exactFake{name: "exact", fixture: newSensitiveFixture(t).value, atOutput: true, atToolCall: true}, echoFake{verdict: VerdictBlock}}, 1},
		// Only a finding at Block or above means protected text was present.
		{"exact tags", []Interceptor{exactFake{name: "exact", fixture: "benign", atOutput: true, tag: true}, echoFake{verdict: VerdictBlock}}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &scriptedCaller{responses: []ModelResult{oneCall("1", "echo", doc), finalAnswer("never")}}
			rec := &interceptRecorder{}
			o := newTestOrchestrator(mc, WithInterceptors(tc.chain...))
			res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, rec)
			blocked := requireBlocked(t, err, HookOutput)
			want := Finding{Interceptor: "echo", Rule: "echo", Verdict: VerdictBlock, Risk: 10, Detail: doc, Origin: OriginModel,
				Hook: HookOutput, Target: TargetOutputToolCall, StateIndex: -1, ToolCallID: "1", Group: -1, Alternative: -1}
			// The echo finding is the last of the hook's findings everywhere.
			last := tc.findings - 1
			if len(blocked.Findings) != tc.findings || blocked.Findings[last] != want {
				t.Fatalf("findings = %+v, want %d ending in %+v", blocked.Findings, tc.findings, want)
			}
			if len(rec.interceptions) != 1 || len(rec.interceptions[0].Findings) != tc.findings ||
				rec.interceptions[0].Findings[last] != want || res.Risk.Findings[last] != want {
				t.Fatalf("events = %+v, risk = %+v; want the finding unchanged", rec.interceptions, res.Risk)
			}
			if errors.Is(err, ErrDiagnosticsSuppressed) || !strings.Contains(err.Error(), "agent: interceptor echo output: "+doc) {
				t.Fatalf("err = %q, want the interceptor error kept", err)
			}
		})
	}
}

func TestSensitiveBlockKeepsAbortVerdict(t *testing.T) {
	fx := newSensitiveFixture(t)
	exact := exactFake{name: "exact", fixture: fx.value, atOutput: true}
	canary := &stubInterceptor{name: "canary", output: func(out OutputInspection) []Finding {
		if len(out.ToolCalls) == 0 {
			return nil
		}
		return []Finding{{Rule: "canary_leak", Verdict: VerdictAbort, Risk: 100, Detail: "nonce seen"}}
	}}
	mc := &scriptedCaller{responses: []ModelResult{oneCall("1", "echo", fx.args(t)), finalAnswer("never")}}
	o := newTestOrchestrator(mc, WithInterceptors(exact, canary))
	_, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, nil)
	blocked := requireBlocked(t, err, HookOutput)
	if c := cause(blocked.Findings); c.Verdict != VerdictAbort || c.Interceptor != "canary" || c.Detail != "" {
		t.Fatalf("cause = %+v, want the canary abort with its Detail dropped", c)
	}
	if len(blocked.Findings) != 2 || blocked.Findings[0].Verdict != VerdictBlock {
		t.Fatalf("findings = %+v, want the exact block kept beside the abort", blocked.Findings)
	}
	if err.Error() != "agent: output blocked by interceptor canary (canary_leak)" {
		t.Fatalf("err = %q", err)
	}
}

func TestSensitiveBlockDropsMatchingRouteOutcome(t *testing.T) {
	fx := newSensitiveFixture(t)
	key := provider.ModelKey{Provider: "local", Model: "m"}
	cases := []struct {
		name      string
		route     provider.RouteOutcome
		sensitive bool // whether the blocking chain carries the exact guard
		keep      bool
	}{
		{"planned model", provider.RouteOutcome{PlannedModel: provider.ModelKey{Provider: "local", Model: fx.value}, ActualModel: key}, true, false},
		{"actual model", provider.RouteOutcome{PlannedModel: key, ActualModel: provider.ModelKey{Provider: fx.value, Model: "m"}}, true, false},
		{"reason", provider.RouteOutcome{PlannedModel: key, ActualModel: key, Reason: "picked " + fx.value}, true, false},
		{"attempt", provider.RouteOutcome{PlannedModel: key, ActualModel: key, Attempts: []provider.RouteAttempt{{Key: key, ErrorClass: fx.value}}}, true, false},
		// An outcome that cannot be encoded cannot be checked, so a chain that
		// protects text drops it; a chain that protects nothing keeps it.
		{"unencodable", provider.RouteOutcome{PlannedModel: key, ActualModel: key, Score: math.NaN()}, true, false},
		{"unencodable without the guard", provider.RouteOutcome{PlannedModel: key, ActualModel: key, Score: math.NaN()}, false, true},
		{"clean", provider.RouteOutcome{PlannedModel: key, ActualModel: key, Reason: "best score", Attempts: []provider.RouteAttempt{{Key: key, Status: provider.AttemptStatusSucceeded}}}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route := tc.route
			resp := oneCall("1", "echo", fx.args(t))
			resp.RouteOutcome = &route
			chain := []Interceptor{exactFake{name: "exact", fixture: fx.value, atOutput: true}}
			if !tc.sensitive {
				chain = []Interceptor{echoFake{verdict: VerdictBlock}}
			}
			mc := &scriptedCaller{responses: []ModelResult{resp, finalAnswer("never")}}
			o := newTestOrchestrator(mc, WithInterceptors(chain...))
			res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, nil)
			_ = requireBlocked(t, err, HookOutput)
			if len(res.Steps) != 1 {
				t.Fatalf("steps = %+v, want the one blocked record", res.Steps)
			}
			got := res.Steps[0].RouteOutcome
			// Kept means today's behavior: the caller's own outcome, by identity.
			want := &route
			if !tc.keep {
				want = nil
			}
			if got != want {
				t.Fatalf("RouteOutcome = %p %+v, want %p", got, got, want)
			}
		})
	}
}
