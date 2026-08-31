package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// asmRec implements Observer, ContextAssemblyObserver AND PressureObserver,
// recording every OnContextAssembly plus a single interleaved log of the two
// per-step callbacks so their ordering contract is falsifiable. err, when set,
// is returned from OnContextAssembly.
type asmRec struct {
	events []ContextAssemblyEvent
	log    []string // "pressure:<step>" / "assembly:<step>" in call order
	err    error
}

func (r *asmRec) OnStep(context.Context, StepEvent) error         { return nil }
func (r *asmRec) OnToolCall(context.Context, ToolCallEvent) error { return nil }
func (r *asmRec) OnToken(context.Context, TokenEvent) error       { return nil }
func (r *asmRec) OnPressure(_ context.Context, e PressureEvent) error {
	r.log = append(r.log, fmt.Sprintf("pressure:%d", e.Step))
	return nil
}
func (r *asmRec) OnContextAssembly(_ context.Context, e ContextAssemblyEvent) error {
	r.events = append(r.events, e)
	r.log = append(r.log, fmt.Sprintf("assembly:%d", e.Step))
	return r.err
}

// assemblyRun drives an orchestrator through nCalls structured tool-call steps
// followed by a final answer, returning the caller so a test can count the model
// calls that actually happened (which is how "aborted before the model call" is
// distinguished from "ran anyway").
//
// Step 0's assembly runs BEFORE the first tool result exists, so it carries no
// structured anchor and takes the legacy path however Mixed is set: a run with
// nCalls tool steps has nCalls MIXED assemblies, at steps 1..nCalls. That offset
// is deliberate — a hardcoded event Step would be invisible at step 0.
func assemblyRun(t *testing.T, obs Observer, mixed bool, set *ContextSet, outputCap, nCalls int) (*scriptedCaller, error) {
	t.Helper()
	responses := make([]ModelResult, 0, nCalls+1)
	for i := 0; i < nCalls; i++ {
		responses = append(responses, ModelResult{Response: provider.ChatResponse{
			ToolCalls: []provider.ToolCall{{
				ID: fmt.Sprintf("c%d", i), Type: "function",
				Function: provider.ToolCallFunction{Name: "ctxtool", Arguments: json.RawMessage(`{}`)},
			}},
		}})
	}
	responses = append(responses, ModelResult{Response: provider.ChatResponse{Content: "final", Done: true}})
	mc := &scriptedCaller{responses: responses}
	// No Compactor: Mixed with a custom one is ErrMixedCompactor, and New keeps
	// the manager unnormalized so nil stays nil here.
	o := New(mc, ContextManager{Mixed: mixed, Estimate: runeEstimator})
	_, err := o.Run(context.Background(), Request{
		Goal:  "GOAL",
		Tools: []Tool{&ctxTool{name: "ctxtool", set: set, class: Read, outputCap: outputCap}},
	}, obs)
	return mc, err
}

// TestOnContextAssemblyEmitsPerMixedStep is the CONTENT pin on the emitted
// trace: it asserts the distinctive per-subject values only the real allocation
// could produce (each fixture group's own depth, rank, byte count and estimated
// cost, plus the producing call ID), not merely that some trace arrived. A
// re-derived or zero trace passes a `Subjects != nil` shape check and fails
// here.
func TestOnContextAssemblyEmitsPerMixedStep(t *testing.T) {
	rec := &asmRec{}
	mc, err := assemblyRun(t, rec, true, mixedTraceSet(), 0, 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 2 {
		t.Fatalf("model calls = %d, want 2 (one tool step, one final)", mc.calls)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want exactly one (step 0 has no anchor yet): %+v", len(rec.events), rec.events)
	}
	ev := rec.events[0]
	if ev.Step != 1 {
		t.Errorf("event Step = %d, want 1 (the first assembly that sees the anchor)", ev.Step)
	}
	tr := ev.Trace
	if tr.Subjects == nil {
		t.Fatal("mixed event carries a zero trace (nil Subjects)")
	}
	// State at step 1 is: pinned goal (must-fit, no row) + one completed chain
	// (span row) + the anchor's two group rows.
	if tr.SelectedSubjects != 3 || tr.RenderedSubjects != 3 || tr.OmittedSubjects != 0 {
		t.Errorf("counters = selected %d rendered %d omitted %d, want 3/3/0",
			tr.SelectedSubjects, tr.RenderedSubjects, tr.OmittedSubjects)
	}
	if len(tr.Subjects) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(tr.Subjects), tr.Subjects)
	}
	if tr.MaxTokens <= 0 || tr.EstimatedTokensUsed <= 0 {
		t.Errorf("MaxTokens = %d, EstimatedTokensUsed = %d, want both > 0", tr.MaxTokens, tr.EstimatedTokensUsed)
	}
	if tr.EstimatedTokensUsed+tr.EstimatedTokensFree != tr.MaxTokens {
		t.Errorf("used %d + free %d != MaxTokens %d", tr.EstimatedTokensUsed, tr.EstimatedTokensFree, tr.MaxTokens)
	}
	// The chain span row: assembler-owned, so rank 0 and no producing call.
	span := traceRow(t, tr, contextdepth.DomainConversation, "1")
	if span.Omitted || span.Lane != laneTool || span.ToolCallID != "" {
		t.Errorf("chain span row = %+v, want retained lane %d with no tool call ID", span, laneTool)
	}
	// Anchor rows: every traced dimension differs between the two fixture groups,
	// so a row built from the wrong group cannot pass.
	anchors := []struct {
		id     string
		depth  contextdepth.Depth
		rank   int
		bytes  int
		tokens int
	}{
		{"one.go", contextdepth.DepthL0, 7, len(traceCardContent), len([]rune(traceCardContent))},
		{"two.go", contextdepth.DepthL2, 3, len(traceEvidenceContent), len([]rune(traceEvidenceContent))},
	}
	for _, want := range anchors {
		got := traceRow(t, tr, contextdepth.DomainRAG, want.id)
		if got.Omitted || got.Decision != DecisionBase || got.OmissionReason != "" {
			t.Errorf("%s row = %+v, want rendered with decision %q", want.id, got, DecisionBase)
		}
		if got.EffectiveDepth != want.depth || got.Rank != want.rank {
			t.Errorf("%s row depth/rank = %v/%d, want %v/%d", want.id, got.EffectiveDepth, got.Rank, want.depth, want.rank)
		}
		if got.Bytes != want.bytes || got.EstimatedTokens != want.tokens {
			t.Errorf("%s row bytes/tokens = %d/%d, want %d/%d", want.id, got.Bytes, got.EstimatedTokens, want.bytes, want.tokens)
		}
		if got.ToolCallID != "c0" {
			t.Errorf("%s row ToolCallID = %q, want %q", want.id, got.ToolCallID, "c0")
		}
		if got.Lane != laneTool {
			t.Errorf("%s row Lane = %d, want %d", want.id, got.Lane, laneTool)
		}
	}
}

// TestOnContextAssemblyZeroEventsOnLegacyPath: legacy assembly emits NOTHING.
// Both sub-cases run two assemblies each, and the model-call count proves they
// really happened, so zero events is an absence rather than a run that never
// assembled anything.
func TestOnContextAssemblyZeroEventsOnLegacyPath(t *testing.T) {
	cases := []struct {
		name  string
		mixed bool
		set   *ContextSet
	}{
		// The structured payload is present but Mixed is off, so dispatch never
		// even clones it onto the anchor.
		{"mixed_off_with_payload", false, mixedTraceSet()},
		// Mixed is on but no tool ever attaches a payload, so every assembly
		// short-circuits to the legacy path.
		{"mixed_on_no_anchors", true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &asmRec{}
			mc, err := assemblyRun(t, rec, c.mixed, c.set, 0, 1)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if mc.calls != 2 {
				t.Fatalf("model calls = %d, want 2: the two assemblies under test did not run", mc.calls)
			}
			if len(rec.events) != 0 {
				t.Fatalf("legacy assembly emitted %d event(s): %+v", len(rec.events), rec.events)
			}
		})
	}
}

// TestOnContextAssemblyZeroEventsOnErrorPath: a mixed assembly that fails emits
// nothing. The malformed set (an assembler-owned domain) is rejected inside the
// mixed pipeline, so the failure is reached only after the anchor exists.
func TestOnContextAssemblyZeroEventsOnErrorPath(t *testing.T) {
	bad := &ContextSet{Groups: []ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: "hijack"},
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: traceCardContent,
		}},
	}}}
	rec := &asmRec{}
	mc, err := assemblyRun(t, rec, true, bad, 0, 1)
	if err == nil {
		t.Fatal("malformed context set must abort the run")
	}
	if !strings.Contains(err.Error(), "assembler-owned") {
		t.Fatalf("error = %v, want the mixed-assembly validation failure", err)
	}
	if mc.calls != 1 {
		t.Fatalf("model calls = %d, want 1: the error must precede step 1's model call", mc.calls)
	}
	if len(rec.events) != 0 {
		t.Fatalf("error path emitted %d event(s): %+v", len(rec.events), rec.events)
	}
}

// TestOnContextAssemblyEmitsWhenEverySubjectOmitted is the fixture the
// zero-trace guard's SHAPE matters for: an output cap too small to hold even the
// omission placeholder evicts the whole chain, so the trace has rows and not one
// of them is rendered. It must still emit — a guard keyed on rendered subjects
// (or on any "did anything survive" test) would swallow exactly the assembly an
// operator most wants to see.
func TestOnContextAssemblyEmitsWhenEverySubjectOmitted(t *testing.T) {
	rec := &asmRec{}
	// 1 byte < len(omittedObservation): no legal rendering, so the chain is evicted.
	mc, err := assemblyRun(t, rec, true, mixedTraceSet(), 1, 1)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 2 {
		t.Fatalf("model calls = %d, want 2", mc.calls)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want exactly one: %+v", len(rec.events), rec.events)
	}
	tr := rec.events[0].Trace
	if tr.Subjects == nil {
		t.Fatal("all-omitted assembly emitted a zero trace (nil Subjects)")
	}
	if len(tr.Subjects) == 0 {
		t.Fatal("all-omitted assembly must still carry its rows")
	}
	if tr.RenderedSubjects != 0 || tr.OmittedSubjects != len(tr.Subjects) {
		t.Fatalf("counters = rendered %d omitted %d over %d rows, want 0 rendered",
			tr.RenderedSubjects, tr.OmittedSubjects, len(tr.Subjects))
	}
	for i, s := range tr.Subjects {
		if !s.Omitted || s.OmissionReason != OmitChainEvicted {
			t.Errorf("row %d (%s/%s) = omitted %v reason %q, want omitted with %q",
				i, s.Subject.Domain, s.Subject.ID, s.Omitted, s.OmissionReason, OmitChainEvicted)
		}
	}
}

// TestOnContextAssemblyFollowsOnPressure pins the cross-callback order with one
// interleaved log: for a given step, OnPressure comes first. The legacy step 0
// contributes pressure ALONE, which is the shape the two callbacks' different
// gates produce — pressure also fires on the exhaustion error path, the trace
// only on mixed success.
func TestOnContextAssemblyFollowsOnPressure(t *testing.T) {
	rec := &asmRec{}
	if _, err := assemblyRun(t, rec, true, mixedTraceSet(), 0, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"pressure:0", "pressure:1", "assembly:1", "pressure:2", "assembly:2"}
	if !slices.Equal(rec.log, want) {
		t.Fatalf("callback log = %q, want %q", rec.log, want)
	}
}

// TestOnContextAssemblyMultiStepStepIndices: every mixed assembly emits its own
// event, tagged with its own step. The two traces also differ in row count (one
// chain, then two), so re-sending the first event's trace fails here too.
func TestOnContextAssemblyMultiStepStepIndices(t *testing.T) {
	rec := &asmRec{}
	mc, err := assemblyRun(t, rec, true, mixedTraceSet(), 0, 2)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if mc.calls != 3 {
		t.Fatalf("model calls = %d, want 3 (two tool steps, one final)", mc.calls)
	}
	if len(rec.events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(rec.events), rec.events)
	}
	// Rows per step: chain span + two anchor groups, once per completed chain.
	wantRows := []int{3, 6}
	for i, ev := range rec.events {
		if ev.Step != i+1 {
			t.Errorf("event %d Step = %d, want %d", i, ev.Step, i+1)
		}
		if len(ev.Trace.Subjects) != wantRows[i] {
			t.Errorf("event %d rows = %d, want %d: %+v", i, len(ev.Trace.Subjects), wantRows[i], ev.Trace.Subjects)
		}
	}
}

// TestOnContextAssemblyErrorAbortsRun: the callback's error propagates
// UNCHANGED and stops the run before the step's model call, like OnPressure.
func TestOnContextAssemblyErrorAbortsRun(t *testing.T) {
	sentinel := errors.New("assembly observer abort")
	rec := &asmRec{err: sentinel}
	mc, err := assemblyRun(t, rec, true, mixedTraceSet(), 0, 1)
	// Identity, not errors.Is: the pin is that nothing wraps or replaces it.
	if err != sentinel {
		t.Fatalf("err = %v, want the sentinel unchanged", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("events = %d, want the one that failed: %+v", len(rec.events), rec.events)
	}
	if mc.calls != 1 {
		t.Fatalf("model calls = %d, want 1: the abort must precede step 1's model call", mc.calls)
	}
}

// TestPlainObserverUnaffectedByContextAssemblySeam: an Observer that does not
// implement the optional interface is untouched by the seam.
func TestPlainObserverUnaffectedByContextAssemblySeam(t *testing.T) {
	var obs Observer = &plainObs{}
	if _, ok := obs.(ContextAssemblyObserver); ok {
		t.Fatal("plainObs must not satisfy ContextAssemblyObserver")
	}
	if _, ok := Observer(nopObserver{}).(ContextAssemblyObserver); ok {
		t.Fatal("nopObserver must not satisfy ContextAssemblyObserver (regression guard)")
	}
	mc, err := assemblyRun(t, obs, true, mixedTraceSet(), 0, 1)
	if err != nil {
		t.Fatalf("plain observer run failed: %v", err)
	}
	if mc.calls != 2 {
		t.Fatalf("model calls = %d, want 2", mc.calls)
	}
}
