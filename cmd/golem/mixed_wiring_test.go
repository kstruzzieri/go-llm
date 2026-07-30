package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// The two anchor payloads differ in every traced dimension (depth, rank, byte
// count) so a trace row built from the wrong group cannot pass the content pin.
const (
	mixedCardContent     = "CARD ONE"
	mixedEvidenceContent = "EVIDENCE TWO LINES"
)

// mixedWiringSet is the structured payload mixedWiringTool attaches to its
// result — the anchor that makes the next assembly a MIXED one. Without it every
// assembly short-circuits to the legacy path and emits nothing.
func mixedWiringSet() *agent.ContextSet {
	return &agent.ContextSet{Groups: []agent.ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			Rank:    7,
		},
		Alternatives: []agent.ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: mixedCardContent,
		}},
	}, {
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "two.go"},
			Rank:    3,
		},
		Alternatives: []agent.ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim},
			}},
			Content: mixedEvidenceContent,
		}},
	}}}
}

// mixedWiringTool is a read-only tool whose result carries a structured set.
type mixedWiringTool struct{}

func (mixedWiringTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "ctxtool", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (mixedWiringTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (mixedWiringTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ctx:fallback", Context: mixedWiringSet()}, nil
}

// assemblyRecorder implements Observer plus the OPTIONAL
// ContextAssemblyObserver. err, when set, is returned from OnContextAssembly.
type assemblyRecorder struct {
	events []agent.ContextAssemblyEvent
	err    error
}

func (*assemblyRecorder) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*assemblyRecorder) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (*assemblyRecorder) OnToken(context.Context, agent.TokenEvent) error       { return nil }
func (r *assemblyRecorder) OnContextAssembly(_ context.Context, e agent.ContextAssemblyEvent) error {
	r.events = append(r.events, e)
	return r.err
}

// mixedAssemblyRow finds one RAG subject's trace row.
func mixedAssemblyRow(tr agent.ContextAssemblyTrace, id string) (agent.ContextSubjectTrace, bool) {
	for _, s := range tr.Subjects {
		if s.Subject.Domain == contextdepth.DomainRAG && s.Subject.ID == id {
			return s, true
		}
	}
	return agent.ContextSubjectTrace{}, false
}

// assertMixedAssembly drives a factory-built orchestrator through one structured
// tool step and asserts whether that step's assembly took the MIXED path. Mixed
// also changes the prompt actually sent; the emitted trace is just the
// consequence this test observes.
func assertMixedAssembly(t *testing.T, wantMixed bool) {
	t.Helper()
	caller := &scriptCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "c0", Type: "function",
			Function: provider.ToolCallFunction{Name: "ctxtool", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
	rec := &assemblyRecorder{}
	orch := newOrchestratorFactory(caller, flags{progressive: wantMixed})()
	if _, err := orch.Run(context.Background(), agent.Request{
		Goal:  "GOAL",
		Tools: []agent.Tool{mixedWiringTool{}},
	}, rec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both scripted responses consumed proves the tool step and the assembly
	// under test really happened, so zero events is an absence rather than a run
	// that never assembled anything.
	if caller.i != 2 {
		t.Fatalf("model calls = %d, want 2 (one tool step, one final)", caller.i)
	}
	want := 0
	if wantMixed {
		want = 1
	}
	if len(rec.events) != want {
		t.Fatalf("assembly events = %d, want %d: %+v", len(rec.events), want, rec.events)
	}
	if want == 0 {
		return
	}
	ev := rec.events[0]
	if ev.Step != 1 {
		t.Errorf("event Step = %d, want 1 (the first assembly that sees the anchor)", ev.Step)
	}
	if ev.Trace.Subjects == nil {
		t.Fatal("mixed event carries a zero trace (nil Subjects)")
	}
	for _, w := range []struct {
		id    string
		depth contextdepth.Depth
		rank  int
		bytes int
	}{
		{"one.go", contextdepth.DepthL0, 7, len(mixedCardContent)},
		{"two.go", contextdepth.DepthL2, 3, len(mixedEvidenceContent)},
	} {
		got, ok := mixedAssemblyRow(ev.Trace, w.id)
		if !ok {
			t.Errorf("trace has no row for %s: %+v", w.id, ev.Trace.Subjects)
			continue
		}
		if got.EffectiveDepth != w.depth || got.Rank != w.rank || got.Bytes != w.bytes {
			t.Errorf("%s row = depth %v rank %d bytes %d, want %v/%d/%d",
				w.id, got.EffectiveDepth, got.Rank, got.Bytes, w.depth, w.rank, w.bytes)
		}
		if got.ToolCallID != "c0" {
			t.Errorf("%s row ToolCallID = %q, want %q", w.id, got.ToolCallID, "c0")
		}
	}
}

// TestMultiObserverOnContextAssemblyFanout: the trace reaches every child that
// implements ContextAssemblyObserver, carrying its content, and a child that
// does not implement it is skipped rather than panicking.
func TestMultiObserverOnContextAssemblyFanout(t *testing.T) {
	first, second := &assemblyRecorder{}, &assemblyRecorder{}
	plain := nonPressureObs{} // implements NOTHING optional
	// Without this guard the skip path stops being covered the moment someone
	// gives nonPressureObs an OnContextAssembly, and the test still passes.
	if _, ok := agent.Observer(plain).(agent.ContextAssemblyObserver); ok {
		t.Fatal("nonPressureObs must not satisfy ContextAssemblyObserver")
	}
	m := &multiObserver{children: []agent.Observer{plain, first, second}}
	event := agent.ContextAssemblyEvent{Step: 3, Trace: agent.ContextAssemblyTrace{
		MaxTokens:        4096,
		SelectedSubjects: 2,
		Subjects: []agent.ContextSubjectTrace{{
			Subject:    contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			ToolCallID: "c0",
			Rank:       7,
		}},
	}}
	if err := m.OnContextAssembly(context.Background(), event); err != nil {
		t.Fatalf("OnContextAssembly: %v", err)
	}
	for i, child := range []*assemblyRecorder{first, second} {
		if len(child.events) != 1 {
			t.Fatalf("child %d got %d event(s), want 1", i, len(child.events))
		}
		got := child.events[0]
		if got.Step != 3 || got.Trace.MaxTokens != 4096 || len(got.Trace.Subjects) != 1 {
			t.Errorf("child %d event = %+v, want the step-3 trace unchanged", i, got)
		}
		if row := got.Trace.Subjects[0]; row.Subject.ID != "one.go" || row.ToolCallID != "c0" || row.Rank != 7 {
			t.Errorf("child %d row = %+v, want the one.go/c0/rank-7 row", i, row)
		}
	}
}

// TestMultiObserverOnContextAssemblyPropagatesError: the first child's error
// stops the fan-out and travels out UNCHANGED, like the other callbacks.
func TestMultiObserverOnContextAssemblyPropagatesError(t *testing.T) {
	sentinel := errors.New("assembly child refused")
	failing := &assemblyRecorder{err: sentinel}
	later := &assemblyRecorder{}
	m := &multiObserver{children: []agent.Observer{failing, later}}
	// Identity, not errors.Is: the pin is that nothing wraps or replaces it.
	if err := m.OnContextAssembly(context.Background(), agent.ContextAssemblyEvent{Step: 1}); err != sentinel {
		t.Fatalf("error = %v, want the sentinel unchanged", err)
	}
	if len(later.events) != 0 {
		t.Fatalf("fan-out continued past the failing child: %+v", later.events)
	}
}
