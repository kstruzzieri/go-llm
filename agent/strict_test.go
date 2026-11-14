package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// grantApprover answers like a host holding a stored grant for every keyed
// action: approved, with ViaGrant when the request carries a key. It records
// every reusable-approval method it receives.
type grantApprover struct{ calls []string }

func (g *grantApprover) Approve(_ context.Context, call provider.ToolCall, _ string) (bool, error) {
	g.calls = append(g.calls, "Approve "+call.Function.Name)
	return true, nil
}

func (g *grantApprover) ApproveKeyed(_ context.Context, call provider.ToolCall, _, key string) (ApprovalDecision, error) {
	g.calls = append(g.calls, "ApproveKeyed "+call.Function.Name)
	return ApprovalDecision{Approved: true, ViaGrant: key != ""}, nil
}

func (g *grantApprover) ApproveWithRisk(_ context.Context, call provider.ToolCall, _, key string, _ RiskReport) (ApprovalDecision, error) {
	g.calls = append(g.calls, "ApproveWithRisk "+call.Function.Name)
	return ApprovalDecision{Approved: true, ViaGrant: key != ""}, nil
}

// freshRequest is one ApproveFresh call's arguments.
type freshRequest struct {
	call     provider.ToolCall
	preview  string
	keyed    bool
	risk     RiskReport
	exposure Exposure
}

// freshGrantApprover is a grantApprover that can also ask a person: every
// ApproveFresh is answered yes and recorded.
type freshGrantApprover struct {
	grantApprover
	fresh []freshRequest
}

func (f *freshGrantApprover) ApproveFresh(_ context.Context, call provider.ToolCall, preview string, keyed bool, risk RiskReport, exp Exposure) (bool, error) {
	f.fresh = append(f.fresh, freshRequest{call, preview, keyed, risk, exp})
	return true, nil
}

// stubTool is a workspace-origin tool with a fixed static effect.
type stubTool struct {
	name   string
	effect Effect
}

func (s stubTool) Spec() ToolSpec {
	return ToolSpec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s stubTool) Effect() Effect { return s.effect }
func (stubTool) Origin() Origin   { return OriginWorkspace }
func (stubTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "done"}, nil
}

// planningStub is a stubTool whose Plan returns plan.
type planningStub struct {
	stubTool
	plan ToolPlan
}

func (p planningStub) Plan(context.Context, json.RawMessage) (ToolPlan, error) { return p.plan, nil }

// grantedWrite is a Write tool whose plan carries an approval key, so a
// grantApprover answers it from a grant.
func grantedWrite(name string) Tool {
	return planningStub{stubTool{name, Effect{Class: Write}},
		ToolPlan{Effect: Effect{Class: Write}, Preview: "P", ApprovalKey: "key:" + name}}
}

// neverWrite is a Write tool that declares ApprovalNever.
func neverWrite(name string) Tool {
	return stubTool{name, Effect{Class: Write, Approval: ApprovalNever}}
}

func foreignFetch(name string) Tool { return admitTool{name: name, origin: OriginForeign}.tool() }

func exposedPrior() *Exposure { return &Exposure{Recorded: true, Foreign: true, First: "tool:x"} }

// strictRun runs one model step per batch in steps, then a final answer.
func strictRun(t *testing.T, req Request, steps ...[]provider.ToolCall) Result {
	t.Helper()
	var script []ModelResult
	for _, s := range steps {
		script = append(script, toolCallResponse(s...))
	}
	req.Goal = "g"
	res, err := newTestOrchestrator(&scriptedCaller{responses: append(script, finalAnswer("done"))}).
		Run(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// records returns the run's records for name, in order.
func records(res Result, name string) []ToolCallRecord {
	var out []ToolCallRecord
	for _, r := range res.ToolCalls {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// onlyRecord returns the run's single record for name.
func onlyRecord(t *testing.T, res Result, name string) ToolCallRecord {
	t.Helper()
	rs := records(res, name)
	if len(rs) != 1 {
		t.Fatalf("%d records for %s, want 1: %+v", len(rs), name, res.ToolCalls)
	}
	return rs[0]
}

// wantFreshOnly fails unless ap was asked fresh exactly n times and never
// consulted a reusable-approval method.
func wantFreshOnly(t *testing.T, ap *freshGrantApprover, n int) {
	t.Helper()
	if len(ap.fresh) != n {
		t.Fatalf("ApproveFresh calls = %d, want %d", len(ap.fresh), n)
	}
	if len(ap.calls) != 0 {
		t.Fatalf("reusable-approval methods consulted: %v", ap.calls)
	}
}

func steps(names ...string) [][]provider.ToolCall {
	out := make([][]provider.ToolCall, len(names))
	for i, n := range names {
		out[i] = []provider.ToolCall{tc(n, `{}`)}
	}
	return out
}

func TestApprovalsSuspendedOutsideARun(t *testing.T) {
	if ApprovalsSuspended(context.Background()) {
		t.Fatal("approvals suspended outside any run")
	}
}

func TestApprovalsSuspendedNeedsStrictAndExposure(t *testing.T) {
	for _, tt := range []struct {
		name string
		req  Request
		want bool
	}{
		{"strict and clean", Request{StrictApproval: true}, false},
		{"strict and exposed", Request{StrictApproval: true, PriorExposure: exposedPrior()}, true},
		{"not strict and exposed", Request{PriorExposure: exposedPrior()}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, _ := newExposureRun(context.Background(), tt.req)
			if got := ApprovalsSuspended(ctx); got != tt.want {
				t.Fatalf("ApprovalsSuspended = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStrictCleanRunUsesGrant(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap, Tools: []Tool{grantedWrite(WriteFileToolName)}},
		steps(WriteFileToolName)...)
	if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || !r.AutoApproved {
		t.Fatalf("record = %+v, want invoked through the grant", r)
	}
	if len(ap.fresh) != 0 {
		t.Fatalf("ApproveFresh called on a clean run: %+v", ap.fresh)
	}
}

func TestStrictExposedAsksFresh(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap,
		Tools: []Tool{foreignFetch("fetch"), grantedWrite(WriteFileToolName)}},
		steps("fetch", WriteFileToolName)...)
	if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || r.AutoApproved {
		t.Fatalf("record = %+v, want invoked on a one-time approval", r)
	}
	wantFreshOnly(t, ap, 1)
	f := ap.fresh[0]
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:fetch"}); f.exposure != want {
		t.Fatalf("exposure = %+v, want %+v", f.exposure, want)
	}
	if f.call.Function.Name != WriteFileToolName || f.preview != "P" || !f.keyed {
		t.Fatalf("fresh request = %+v, want the keyed write with its preview", f)
	}
}

func TestStrictDeniesApproverWithoutFresh(t *testing.T) {
	ap := &grantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap,
		Tools: []Tool{foreignFetch("fetch"), grantedWrite(WriteFileToolName)}},
		steps("fetch", WriteFileToolName)...)
	if r := onlyRecord(t, res, WriteFileToolName); !r.Denied || r.Invoked || r.AutoApproved {
		t.Fatalf("record = %+v, want denied and not invoked", r)
	}
	if len(ap.calls) != 0 {
		t.Fatalf("reusable-approval methods consulted: %v", ap.calls)
	}
}

func TestStrictOffIgnoresExposure(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{Approver: ap, Tools: []Tool{foreignFetch("fetch"), grantedWrite(WriteFileToolName)}},
		steps("fetch", WriteFileToolName)...)
	if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || !r.AutoApproved {
		t.Fatalf("record = %+v, want invoked through the grant", r)
	}
	if len(ap.fresh) != 0 {
		t.Fatalf("ApproveFresh called with strict off: %+v", ap.fresh)
	}
}

func TestStrictSerialBatchExposesLaterCall(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap,
		Tools: []Tool{foreignFetch("fetch"), grantedWrite(EditFileToolName)}},
		[]provider.ToolCall{tc("fetch", `{}`), tc(EditFileToolName, `{}`)})
	if r := onlyRecord(t, res, EditFileToolName); !r.Invoked || r.AutoApproved {
		t.Fatalf("record = %+v, want invoked on a one-time approval", r)
	}
	wantFreshOnly(t, ap, 1)
}

func TestStrictParallelReadsExposeNextStep(t *testing.T) {
	// Each read waits for the other to start, so a serial batch fails both.
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	meet := func(context.Context) error {
		started.Done()
		select {
		case <-both:
			return nil
		case <-time.After(5 * time.Second):
			return errors.New("reads did not run in parallel")
		}
	}
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap, Tools: []Tool{
		admitTool{name: "a", origin: OriginForeign, run: meet}.tool(),
		admitTool{name: "b", origin: OriginForeign, run: meet}.tool(),
		grantedWrite(WriteFileToolName),
	}}, []provider.ToolCall{tc("a", `{}`), tc("b", `{}`)}, []provider.ToolCall{tc(WriteFileToolName, `{}`)})
	for _, n := range []string{"a", "b"} {
		if r := onlyRecord(t, res, n); r.IsError {
			t.Fatalf("%s = %+v, want a successful parallel read", n, r)
		}
	}
	if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || r.AutoApproved {
		t.Fatalf("record = %+v, want invoked on a one-time approval", r)
	}
	wantFreshOnly(t, ap, 1)
}

func TestStrictAcceptedAdviceSuspendsFirstCall(t *testing.T) {
	// containment: EXPOSURE-2
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap, Advisory: testAdvisory(),
		Tools: []Tool{grantedWrite(EditFileToolName)}}, steps(EditFileToolName)...)
	if r := onlyRecord(t, res, EditFileToolName); !r.Invoked || r.AutoApproved {
		t.Fatalf("record = %+v, want invoked on a one-time approval", r)
	}
	wantFreshOnly(t, ap, 1)
	if want := (Exposure{Recorded: true, Foreign: true, First: "consult"}); ap.fresh[0].exposure != want {
		t.Fatalf("exposure = %+v, want %+v", ap.fresh[0].exposure, want)
	}
}

func TestStrictBlockedAdviceLeavesGrantUsable(t *testing.T) {
	blockAdvice := &stubInterceptor{name: "blk", input: func(in InputInspection) []Finding {
		for _, m := range in.Messages {
			if strings.HasPrefix(m.ToolName, "consult/") {
				return []Finding{{Rule: "r", Verdict: VerdictBlock, Risk: 1, Target: TargetMessage, StateIndex: m.StateIndex}}
			}
		}
		return nil
	}}
	mc := &scriptedCaller{responses: []ModelResult{toolCallResponse(tc(WriteFileToolName, `{}`)), finalAnswer("done")}}
	o := newTestOrchestrator(mc, WithInterceptors(blockAdvice))
	ap := &freshGrantApprover{}
	tools := []Tool{grantedWrite(WriteFileToolName)}
	blocked, err := o.Run(context.Background(), Request{Goal: "g", StrictApproval: true, Approver: ap,
		Advisory: testAdvisory(), Tools: tools}, nil)
	if !errors.Is(err, ErrAdvisoryBlocked) {
		t.Fatalf("err = %v, want ErrAdvisoryBlocked", err)
	}
	res, err := o.Run(context.Background(), Request{Goal: "g", StrictApproval: true, Approver: ap,
		PriorExposure: &blocked.Exposure, Tools: tools}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || !r.AutoApproved {
		t.Fatalf("record = %+v, want invoked through the grant", r)
	}
	if len(ap.fresh) != 0 {
		t.Fatalf("ApproveFresh called after blocked advice: %+v", ap.fresh)
	}
}

func TestStrictOneTimeApprovalDoesNotClear(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, Approver: ap,
		Tools: []Tool{foreignFetch("fetch"), grantedWrite(WriteFileToolName)}},
		[]provider.ToolCall{tc("fetch", `{}`)},
		[]provider.ToolCall{tc(WriteFileToolName, `{"n":1}`)},
		[]provider.ToolCall{tc(WriteFileToolName, `{"n":2}`)})
	rs := records(res, WriteFileToolName)
	if len(rs) != 2 {
		t.Fatalf("records = %+v, want two writes", res.ToolCalls)
	}
	for _, r := range rs {
		if !r.Invoked || r.AutoApproved {
			t.Fatalf("record = %+v, want invoked on a one-time approval", r)
		}
	}
	wantFreshOnly(t, ap, 2)
}

func TestStrictRaisesApprovalNever(t *testing.T) {
	// containment: STRICT-1
	lowering := planningStub{stubTool{"lowering", Effect{Class: Write, Approval: ApprovalAlways}},
		ToolPlan{Effect: Effect{Class: Write, Approval: ApprovalNever}}}
	for _, tt := range []struct {
		name        string
		strict      bool
		prior       *Exposure
		ap          Approver
		tool        Tool
		wantAsked   bool
		wantInvoked bool
	}{
		{"exposed, fresh approver", true, exposedPrior(), &freshGrantApprover{}, neverWrite("w"), true, true},
		{"exposed, plain approver", true, exposedPrior(), &grantApprover{}, neverWrite("w"), false, false},
		{"exposed, nil approver", true, exposedPrior(), nil, neverWrite("w"), false, false},
		{"exposed, plan lowers approval", true, exposedPrior(), &freshGrantApprover{}, lowering, true, true},
		{"clean", true, nil, &freshGrantApprover{}, neverWrite("w"), false, true},
		{"strict off", false, exposedPrior(), &freshGrantApprover{}, neverWrite("w"), false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name := tt.tool.Spec().Name
			res := strictRun(t, Request{StrictApproval: tt.strict, PriorExposure: tt.prior, Approver: tt.ap,
				Tools: []Tool{tt.tool}}, steps(name)...)
			r := onlyRecord(t, res, name)
			if r.Invoked != tt.wantInvoked || r.Denied == tt.wantInvoked {
				t.Fatalf("record = %+v, want invoked=%v", r, tt.wantInvoked)
			}
			var asked int
			var consulted []string
			switch a := tt.ap.(type) {
			case *freshGrantApprover:
				asked, consulted = len(a.fresh), a.calls
			case *grantApprover:
				consulted = a.calls
			}
			if (asked == 1) != tt.wantAsked || asked > 1 {
				t.Fatalf("ApproveFresh calls = %d, want asked=%v", asked, tt.wantAsked)
			}
			if len(consulted) != 0 {
				t.Fatalf("reusable-approval methods consulted: %v", consulted)
			}
		})
	}
}

func TestStrictExemptStaysUnprompted(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, PriorExposure: exposedPrior(), Approver: ap,
		StrictExempt: []string{"memo"}, Tools: []Tool{neverWrite("memo"), neverWrite("other")}},
		[]provider.ToolCall{tc("memo", `{}`), tc("other", `{}`)})
	for _, n := range []string{"memo", "other"} {
		if r := onlyRecord(t, res, n); !r.Invoked {
			t.Fatalf("%s = %+v, want invoked", n, r)
		}
	}
	wantFreshOnly(t, ap, 1)
	if got := ap.fresh[0].call.Function.Name; got != "other" {
		t.Fatalf("asked about %q, want only the unnamed tool", got)
	}
}

func TestStrictPlainReadStaysUnprompted(t *testing.T) {
	ap := &freshGrantApprover{}
	res := strictRun(t, Request{StrictApproval: true, PriorExposure: exposedPrior(), Approver: ap,
		Tools: []Tool{stubTool{"never", Effect{Class: Read, Approval: ApprovalNever}}, stubTool{"default", Effect{Class: Read}}}},
		steps("never", "default")...)
	for _, n := range []string{"never", "default"} {
		if r := onlyRecord(t, res, n); !r.Invoked {
			t.Fatalf("%s = %+v, want invoked", n, r)
		}
	}
	wantFreshOnly(t, ap, 0)
}

// childRunner is a workspace-origin read tool that runs a child orchestrator
// on the context it receives, with its own approver and no strict flag.
func childRunner(childAp Approver, tool Tool, out *Result) Tool {
	return admitTool{name: "host", origin: OriginWorkspace, run: func(ctx context.Context) error {
		name := tool.Spec().Name
		res, err := newTestOrchestrator(scriptedToolCalls(tc(name, `{}`))).
			Run(ctx, Request{Goal: "child", Approver: childAp, Tools: []Tool{tool}}, nil)
		*out = res
		return err
	}}.tool()
}

func TestStrictNestedRunInheritsStrictness(t *testing.T) {
	childAp := &freshGrantApprover{}
	var child Result
	strictRun(t, Request{StrictApproval: true, Approver: &freshGrantApprover{},
		Tools: []Tool{foreignFetch("fetch"), childRunner(childAp, grantedWrite(WriteFileToolName), &child)}},
		steps("fetch", "host")...)
	if r := onlyRecord(t, child, WriteFileToolName); !r.Invoked || r.AutoApproved {
		t.Fatalf("child record = %+v, want invoked on a one-time approval", r)
	}
	wantFreshOnly(t, childAp, 1)
}

func TestStrictParentExemptDoesNotExemptChild(t *testing.T) {
	parentAp, childAp := &freshGrantApprover{}, &freshGrantApprover{}
	var child Result
	res := strictRun(t, Request{StrictApproval: true, PriorExposure: exposedPrior(), Approver: parentAp,
		StrictExempt: []string{"memo"}, Tools: []Tool{neverWrite("memo"), childRunner(childAp, neverWrite("memo"), &child)}},
		steps("memo", "host")...)
	if r := onlyRecord(t, res, "memo"); !r.Invoked {
		t.Fatalf("parent memo = %+v, want invoked", r)
	}
	wantFreshOnly(t, parentAp, 0)
	if r := onlyRecord(t, child, "memo"); !r.Invoked {
		t.Fatalf("child memo = %+v, want invoked after asking", r)
	}
	wantFreshOnly(t, childAp, 1)
}

func TestStrictEmptyPreviewReachesFresh(t *testing.T) {
	ap := &freshGrantApprover{}
	const args = `{"path":"a.txt"}`
	strictRun(t, Request{StrictApproval: true, PriorExposure: exposedPrior(), Approver: ap,
		Tools: []Tool{stubTool{"writer", Effect{Class: Write}}}}, []provider.ToolCall{tc("writer", args)})
	wantFreshOnly(t, ap, 1)
	f := ap.fresh[0]
	if f.preview != "" || f.keyed || f.call.ID != "writer" || f.call.Function.Name != "writer" ||
		string(f.call.Function.Arguments) != args {
		t.Fatalf("fresh request = %+v, want the bare call with an empty preview and no key", f)
	}
}

func TestStrictUnknownExposureAsksFresh(t *testing.T) {
	hist := []provider.ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}}
	bare := admitTool{name: "bare", noOrigin: true}.tool()
	for _, tt := range []struct {
		name      string
		req       Request
		steps     [][]provider.ToolCall
		wantFirst string
	}{
		{"history", Request{History: hist}, steps(WriteFileToolName), "history"},
		{"summary", Request{HistorySummary: "s"}, steps(WriteFileToolName), "history"},
		{"unrecorded prior", Request{PriorExposure: &Exposure{}}, steps(WriteFileToolName), "history"},
		{"tool with no origin", Request{Tools: []Tool{bare}}, steps("bare", WriteFileToolName), "tool:bare"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ap := &freshGrantApprover{}
			req := tt.req
			req.StrictApproval, req.Approver = true, ap
			req.Tools = append(slices.Clone(req.Tools), grantedWrite(WriteFileToolName))
			res := strictRun(t, req, tt.steps...)
			if r := onlyRecord(t, res, WriteFileToolName); !r.Invoked || r.AutoApproved {
				t.Fatalf("record = %+v, want invoked on a one-time approval", r)
			}
			wantFreshOnly(t, ap, 1)
			if want := (Exposure{Recorded: true, Unknown: true, First: tt.wantFirst}); ap.fresh[0].exposure != want {
				t.Fatalf("exposure = %+v, want %+v", ap.fresh[0].exposure, want)
			}
		})
	}
}

func TestStrictWireBytesIdentical(t *testing.T) {
	wire := func(strict bool) ([]string, *freshGrantApprover) {
		mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: []ModelResult{
			toolCallResponse(tc("fetch", `{}`)),
			toolCallResponse(tc(WriteFileToolName, `{}`)),
			finalAnswer("done"),
		}}}
		ap := &freshGrantApprover{}
		if _, err := newTestOrchestrator(mc).Run(context.Background(), Request{Goal: "g", StrictApproval: strict,
			Approver: ap, Tools: []Tool{foreignFetch("fetch"), grantedWrite(WriteFileToolName)}}, nil); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range mc.reqs {
			b, err := json.Marshal(normalizedRequest(t, r))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(b))
		}
		return out, ap
	}
	off, _ := wire(false)
	on, ap := wire(true)
	wantFreshOnly(t, ap, 1) // the strict run took the fresh path
	if len(off) != 3 || !slices.Equal(off, on) {
		t.Fatalf("wire requests differ:\noff %q\non  %q", off, on)
	}
}
