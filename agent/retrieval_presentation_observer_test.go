package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// presentationCaller captures each request while still running the real
// orchestrator loop. The second request is the independently observable final
// presentation that the observer event must describe.
type presentationCaller struct {
	responses []ModelResult
	requests  []provider.ChatRequest
	calls     int
}

func (c *presentationCaller) Chat(_ context.Context, req provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {
	c.requests = append(c.requests, req)
	r := c.responses[c.calls]
	c.calls++
	if onToken != nil && r.Response.Content != "" {
		if err := onToken(provider.ChatResponse{Content: r.Response.Content}); err != nil {
			return ModelResult{}, err
		}
	}
	return r, nil
}

type presentationTool struct {
	set       *ContextSet
	attrib    *RetrievalAttribution
	outputCap int
}

func (presentationTool) Spec() ToolSpec {
	return ToolSpec{Name: "retrieve", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (p presentationTool) Effect() Effect {
	return Effect{Class: Read, Approval: ApprovalNever, OutputCap: p.outputCap}
}

func (p presentationTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "PRESENTED-RETRIEVAL", Attrib: p.attrib, Context: p.set}, nil
}

type presentationRec struct {
	events []RetrievalPresentationEvent
	log    []string
	err    error
}

func (r *presentationRec) OnStep(context.Context, StepEvent) error         { return nil }
func (r *presentationRec) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (r *presentationRec) OnToken(context.Context, TokenEvent) error       { return nil }
func (r *presentationRec) OnPressure(_ context.Context, e PressureEvent) error {
	r.log = append(r.log, fmt.Sprintf("pressure:%d", e.Step))
	return nil
}
func (r *presentationRec) OnContextAssembly(_ context.Context, e ContextAssemblyEvent) error {
	r.log = append(r.log, fmt.Sprintf("assembly:%d", e.Step))
	return nil
}
func (r *presentationRec) OnRetrievalPresentation(_ context.Context, e RetrievalPresentationEvent) error {
	r.events = append(r.events, e)
	r.log = append(r.log, fmt.Sprintf("retrieval:%d", e.Step))
	return r.err
}

var _ RetrievalPresentationObserver = (*presentationRec)(nil)

func presentationRun(t *testing.T, obs Observer, mixed bool, set *ContextSet,
	attrib *RetrievalAttribution, outputCap, calls int, budget Budget) (*presentationCaller, error) {
	t.Helper()
	responses := make([]ModelResult, 0, calls+1)
	for i := range calls {
		responses = append(responses, ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: fmt.Sprintf("present-%d", i), Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{}`)},
		}}}})
	}
	responses = append(responses, ModelResult{Response: provider.ChatResponse{Content: "final", Done: true}})
	mc := &presentationCaller{responses: responses}
	o := New(mc, ContextManager{Mixed: mixed, Estimate: runeEstimator})
	_, err := o.Run(context.Background(), Request{
		Goal: "GOAL", Budget: budget,
		Tools: []Tool{presentationTool{set: set, attrib: attrib, outputCap: outputCap}},
	}, obs)
	return mc, err
}

func TestRetrievalPresentationFlatFinalMessage(t *testing.T) {
	attrib := &RetrievalAttribution{Sources: []RetrievedSource{{
		StableKey: "flat-key", Source: "flat.go", StartLine: 7, EndLine: 11, Score: 0.9,
	}}}
	rec := &presentationRec{}
	mc := &presentationCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "flat-call", Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
	o := New(mc, ContextManager{Estimate: runeEstimator})
	if _, err := o.Run(context.Background(), Request{
		Goal: "GOAL", Tools: []Tool{presentationTool{attrib: attrib}},
	}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 2 || len(mc.requests) != 2 {
		t.Fatalf("model calls/requests = %d/%d, want 2/2", mc.calls, len(mc.requests))
	}
	var presented *provider.ChatMessage
	for i := range mc.requests[1].Messages {
		msg := &mc.requests[1].Messages[i]
		if msg.Role == "tool" && msg.ToolCallID == "flat-call" {
			presented = msg
			break
		}
	}
	if presented == nil || presented.Content != "PRESENTED-RETRIEVAL" {
		t.Fatalf("final model request tool message = %+v, want flat-call/PRESENTED-RETRIEVAL", presented)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %+v, want one final presentation", rec.events)
	}
	e := rec.events[0]
	if e.Step != 1 || e.ToolCallID != "flat-call" {
		t.Fatalf("event step/call = %d/%q, want 1/flat-call", e.Step, e.ToolCallID)
	}
	if len(e.Attribution.Sources) != 1 || e.Attribution.Sources[0] != (RetrievedSource{
		StableKey: "flat-key", Source: "flat.go", StartLine: 7, EndLine: 11, Score: 0.9,
	}) {
		t.Fatalf("event sources = %+v, want literal flat source", e.Attribution.Sources)
	}
	attrib.Sources[0].StableKey = "mutated-after-run"
	if e.Attribution.Sources[0].StableKey != "flat-key" {
		t.Fatalf("event aliases tool attribution: %+v", e.Attribution.Sources)
	}
}

func TestRetrievalPresentationUsesMixedChosenAlternative(t *testing.T) {
	rec := &presentationRec{}
	mc, err := presentationRun(t, rec, true, mixedTraceSet(), nil, 0, 1, Budget{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 2 || len(rec.events) != 1 {
		t.Fatalf("calls/events = %d/%d, want 2/1", mc.calls, len(rec.events))
	}
	e := rec.events[0]
	if e.Step != 1 || e.ToolCallID != "present-0" || len(e.Attribution.Sources) != 1 ||
		e.Attribution.Sources[0].StableKey != "k2" {
		t.Fatalf("mixed event = %+v, want step 1 present-0 with only k2", e)
	}
}

func TestRetrievalPresentationSkipsEvictedAndAllOmittedMessages(t *testing.T) {
	cases := []struct {
		name      string
		mixed     bool
		set       *ContextSet
		attrib    *RetrievalAttribution
		outputCap int
		budget    Budget
	}{
		{
			name:   "legacy chain evicted",
			attrib: &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "legacy-key"}}},
			budget: Budget{InputCeiling: 60},
		},
		{
			name:      "all mixed subjects omitted",
			mixed:     true,
			set:       mixedTraceSet(),
			attrib:    &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "fallback-key"}}},
			outputCap: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &presentationRec{}
			mc, err := presentationRun(t, rec, tc.mixed, tc.set, tc.attrib, tc.outputCap, 1, tc.budget)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if mc.calls != 2 {
				t.Fatalf("model calls = %d, want 2", mc.calls)
			}
			if len(rec.events) != 0 {
				t.Fatalf("events = %+v, want no presentation for an absent final message", rec.events)
			}
		})
	}
}

func TestRetrievalPresentationRepeatedHistoryRemainsObservable(t *testing.T) {
	rec := &presentationRec{}
	mc, err := presentationRun(t, rec, true, mixedTraceSet(), nil, 0, 2, Budget{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 3 {
		t.Fatalf("model calls = %d, want 3", mc.calls)
	}
	got := make([]string, 0, len(rec.events))
	for _, e := range rec.events {
		if len(e.Attribution.Sources) != 1 {
			t.Fatalf("event = %+v, want one source", e)
		}
		got = append(got, fmt.Sprintf("%d:%s:%s", e.Step, e.ToolCallID, e.Attribution.Sources[0].StableKey))
	}
	want := []string{"1:present-0:k2", "2:present-0:k2", "2:present-1:k2"}
	if !slices.Equal(got, want) {
		t.Fatalf("presentations = %q, want %q", got, want)
	}
}

func TestRetrievalPresentationFollowsAssemblyCallbacks(t *testing.T) {
	rec := &presentationRec{}
	if _, err := presentationRun(t, rec, true, mixedTraceSet(), nil, 0, 1, Budget{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"pressure:0", "pressure:1", "assembly:1", "retrieval:1"}
	if !slices.Equal(rec.log, want) {
		t.Fatalf("callback order = %q, want %q", rec.log, want)
	}
}

func TestRetrievalPresentationErrorAbortsBeforeModelCall(t *testing.T) {
	sentinel := errors.New("retrieval presentation abort")
	rec := &presentationRec{err: sentinel}
	mc, err := presentationRun(t, rec, false, nil,
		&RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "flat-key"}}}, 0, 1, Budget{})
	if err != sentinel {
		t.Fatalf("Run error = %v, want sentinel unchanged", err)
	}
	if len(rec.events) != 1 || mc.calls != 1 {
		t.Fatalf("events/model calls = %d/%d, want 1/1", len(rec.events), mc.calls)
	}
}

func TestRetrievalPresentationNilAndPlainObserversRemainOptional(t *testing.T) {
	attrib := &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "flat-key"}}}
	plain := &plainObs{}
	if _, ok := Observer(plain).(RetrievalPresentationObserver); ok {
		t.Fatal("plain observer must not satisfy RetrievalPresentationObserver")
	}
	for _, obs := range []Observer{nil, plain} {
		mc, err := presentationRun(t, obs, false, nil, attrib, 0, 1, Budget{})
		if err != nil || mc.calls != 2 {
			t.Fatalf("Run/calls = %v/%d, want nil/2", err, mc.calls)
		}
	}
	if _, ok := Observer(nopObserver{}).(RetrievalPresentationObserver); ok {
		t.Fatal("nopObserver must not satisfy RetrievalPresentationObserver")
	}
}
