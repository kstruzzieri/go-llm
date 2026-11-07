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

// admitTool is a read-only tool with a configurable static origin (none when
// noOrigin), an optional per-invocation origin, and an optional Plan failure.
type admitTool struct {
	name     string
	noOrigin bool
	origin   Origin
	invoked  Origin // per-invocation origin; OriginUnknown defers to the static one
	planErr  bool
	run      func(context.Context) error
	set      *ContextSet // returned as the structured payload when non-nil
}

func (a admitTool) Spec() ToolSpec {
	return ToolSpec{Name: a.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (admitTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (a admitTool) Invoke(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
	if a.run != nil {
		if err := a.run(ctx); err != nil {
			return ToolResult{}, err
		}
	}
	return ToolResult{Content: "out", Origin: a.invoked, Context: a.set}, nil
}

// admitOriginTool adds OriginTool; a noOrigin admitTool is used bare and
// does not implement it.
type admitOriginTool struct{ admitTool }

func (a admitOriginTool) Origin() Origin { return a.origin }

type admitPlanTool struct{ admitOriginTool }

func (admitPlanTool) Plan(context.Context, json.RawMessage) (ToolPlan, error) {
	return ToolPlan{}, errors.New("plan exploded")
}

func (a admitTool) tool() Tool {
	switch {
	case a.noOrigin:
		return a
	case a.planErr:
		return admitPlanTool{admitOriginTool{a}}
	}
	return admitOriginTool{a}
}

func foreignStub() Tool {
	return admitTool{name: "stub", origin: OriginForeign}.tool()
}

func runAdmit(t *testing.T, mc ModelCaller, req Request, cm *ContextManager, opts ...Option) (Result, error) {
	t.Helper()
	m := ContextManager{Compactor: RecencyCompactor{Estimate: runeEstimator}, Estimate: runeEstimator}
	if cm != nil {
		m = *cm
	}
	req.Goal = "g"
	return New(mc, m, opts...).Run(context.Background(), req, nil)
}

func admitRun(t *testing.T, calls []provider.ToolCall, tools ...Tool) Exposure {
	t.Helper()
	res, err := runAdmit(t, scriptedToolCalls(calls...), Request{Tools: tools}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Exposure
}

func scriptedToolCalls(calls ...provider.ToolCall) *scriptedCaller {
	return &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
}

func TestAdmitForeignToolResult(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("stub", `{}`)}, foreignStub())
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:stub"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitToolWithNoOrigin(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("bare", `{}`)}, admitTool{name: "bare", noOrigin: true}.tool())
	if want := (Exposure{Recorded: true, Unknown: true, First: "tool:bare"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitWorkspaceAndModelToolsAdmitNothing(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("ws", `{}`), tc("mdl", `{}`)},
		admitTool{name: "ws", origin: OriginWorkspace}.tool(),
		admitTool{name: "mdl", origin: OriginModel}.tool())
	if want := (Exposure{Recorded: true}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitWorkspaceToolLoweringItsOrigin(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("ws", `{}`)},
		admitTool{name: "ws", origin: OriginWorkspace, invoked: OriginForeign}.tool())
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:ws"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitForeignPlanFailure(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("pf", `{}`)}, admitTool{name: "pf", origin: OriginForeign, planErr: true}.tool())
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:pf"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitSkipsBlockedObservation(t *testing.T) {
	blockTool := &stubInterceptor{name: "blk", input: func(in InputInspection) []Finding {
		var fs []Finding
		for _, m := range in.Messages {
			if m.Role == "tool" {
				fs = append(fs, Finding{Rule: "r", Verdict: VerdictBlock, Risk: 1, Target: TargetMessage, StateIndex: m.StateIndex})
			}
		}
		return fs
	}}
	res, err := runAdmit(t, scriptedToolCalls(tc("stub", `{}`)), Request{Tools: []Tool{foreignStub()}}, nil, WithInterceptors(blockTool))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].Blocked {
		t.Fatalf("observation was not blocked: %+v", res.ToolCalls)
	}
	if want := (Exposure{Recorded: true}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestAdmitSkipsDenialUnknownToolAndMalformedArgs(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("nosuch", `{}`), tc("stub", `{not json`)}, foreignStub())
	if want := (Exposure{Recorded: true}); got != want {
		t.Fatalf("unknown tool / malformed args: Exposure = %+v, want %+v", got, want)
	}

	res, err := runAdmit(t, scriptedToolCalls(tc("writer", `{}`)), Request{Tools: []Tool{foreignWriter{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].Denied {
		t.Fatalf("call was not denied: %+v", res.ToolCalls)
	}
	if want := (Exposure{Recorded: true}); res.Exposure != want {
		t.Fatalf("denial: Exposure = %+v, want %+v", res.Exposure, want)
	}
}

// foreignWriter is a foreign-origin mutating tool; with no approver the call is denied.
type foreignWriter struct{}

func (foreignWriter) Spec() ToolSpec {
	return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (foreignWriter) Effect() Effect { return Effect{Class: Write, Approval: ApprovalAlways} }
func (foreignWriter) Origin() Origin { return OriginForeign }
func (foreignWriter) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "wrote"}, nil
}

func TestAdmitParallelReadBatch(t *testing.T) {
	got := admitRun(t, []provider.ToolCall{tc("a", `{}`), tc("b", `{}`)},
		admitTool{name: "a", origin: OriginForeign}.tool(),
		admitTool{name: "b", origin: OriginForeign}.tool())
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:a"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

// childRun runs a child orchestrator over a foreign stub with ctx.
func childRun(ctx context.Context) error {
	_, err := New(scriptedToolCalls(tc("stub", `{}`)), ContextManager{Estimate: runeEstimator}).
		Run(ctx, Request{Goal: "child", Tools: []Tool{foreignStub()}}, nil)
	return err
}

func TestAdmitNestedRunReachesParent(t *testing.T) {
	host := admitTool{name: "host", origin: OriginWorkspace, run: childRun}.tool()
	got := admitRun(t, []provider.ToolCall{tc("host", `{}`)}, host)
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:stub"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitParallelChildren(t *testing.T) {
	ctx, run := newExposureRun(context.Background(), Request{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- childRun(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:stub"}); run.snapshot() != want {
		t.Fatalf("Exposure = %+v, want %+v", run.snapshot(), want)
	}
}

func TestAdmitMixedAssembly(t *testing.T) {
	cm := ContextManager{Mixed: true, Estimate: runeEstimator}
	res, err := runAdmit(t, scriptedToolCalls(tc("stub", `{}`)), Request{Tools: []Tool{foreignStub()}}, &cm)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:stub"}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestAdmitAcceptedAdvice(t *testing.T) {
	res, err := runAdmit(t, exposureAnswer(), Request{Advisory: testAdvisory()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "consult"}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestAdmitBlockedAdviceAdmitsNothing(t *testing.T) {
	mc := exposureAnswer()
	res, err := runAdmit(t, mc, Request{Advisory: testAdvisory()}, nil, WithInterceptors(blockAll("blk")))
	if !errors.Is(err, ErrAdvisoryBlocked) {
		t.Fatalf("err = %v, want ErrAdvisoryBlocked", err)
	}
	if mc.calls != 0 {
		t.Fatalf("model calls = %d, want 0", mc.calls)
	}
	if want := (Exposure{Recorded: true}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestAdmitInvokeErrorIsForeign(t *testing.T) {
	boom := func(context.Context) error { return errors.New("boom") }
	got := admitRun(t, []provider.ToolCall{tc("bad", `{}`)}, admitTool{name: "bad", origin: OriginForeign, run: boom}.tool())
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:bad"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

func TestAdmitMixedAlternativesWithContextSet(t *testing.T) {
	cm := ContextManager{Mixed: true, Estimate: runeEstimator}
	tool := admitTool{name: "ctx", origin: OriginForeign, set: validSet()}.tool()
	res, err := runAdmit(t, scriptedToolCalls(tc("ctx", `{}`)), Request{Tools: []Tool{tool}}, &cm)
	if err != nil {
		t.Fatal(err)
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:ctx"}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestAdmitForeignTrustedOriginClaim(t *testing.T) {
	tool := admitTool{name: "liar", origin: OriginForeign, invoked: OriginWorkspace}.tool()
	got := admitRun(t, []provider.ToolCall{tc("liar", `{}`)}, tool)
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:liar"}); got != want {
		t.Fatalf("Exposure = %+v, want %+v", got, want)
	}
}

// bulkAdmitTool returns a large result so the next step's assembly evicts it.
type bulkAdmitTool struct {
	name   string
	origin Origin
}

func (b bulkAdmitTool) Spec() ToolSpec {
	return ToolSpec{Name: b.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (bulkAdmitTool) Effect() Effect   { return Effect{Class: Read} }
func (b bulkAdmitTool) Origin() Origin { return b.origin }
func (b bulkAdmitTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: strings.Repeat(b.name+" ", 100)}, nil
}

func TestAdmitCompactionEvictingForeignObservation(t *testing.T) {
	mc := &capturingScriptedCaller{scriptedCaller: scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{tc("far", `{}`)}}},
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{tc("near", `{}`)}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}}
	res, err := runAdmit(t, mc, Request{
		Budget: Budget{InputCeiling: 700 + len([]rune(ToolTrustContract))},
		Tools:  []Tool{bulkAdmitTool{"far", OriginForeign}, bulkAdmitTool{"near", OriginWorkspace}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, e := range res.Events {
		saw = saw || e.Kind == "compaction"
	}
	if !saw {
		t.Fatalf("no compaction event: %+v", res.Events)
	}
	// The premise: the foreign result reached the model, then was evicted
	// before the final call, so the record is the only trace left of it.
	sent := func(i int) string {
		b, err := json.Marshal(mc.reqs[i])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const farText = "far far far"
	if len(mc.reqs) != 3 || !strings.Contains(sent(1), farText) {
		t.Fatalf("foreign result never reached the model (%d requests)", len(mc.reqs))
	}
	if strings.Contains(sent(2), farText) {
		t.Fatal("foreign result was not evicted before the final call")
	}
	if want := (Exposure{Recorded: true, Foreign: true, First: "tool:far"}); res.Exposure != want {
		t.Fatalf("Exposure = %+v, want %+v", res.Exposure, want)
	}
}
