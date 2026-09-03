package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

type echoTool struct{ name string }

func (e echoTool) Spec() ToolSpec {
	return ToolSpec{Name: e.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (echoTool) Effect() Effect { return Effect{Class: Read, Approval: ApprovalNever} }
func (echoTool) Invoke(_ context.Context, args json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "tool-said:" + string(args)}, nil
}

func TestRunDispatchesToolThenAnswers(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "echo", Arguments: json.RawMessage(`{"x":1}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{echoTool{name: "echo"}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "done" || res.StopReason != Completed {
		t.Fatalf("got answer=%q stop=%v", res.Answer, res.StopReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "echo" || res.ToolCalls[0].IsError {
		t.Fatalf("expected one successful echo call, got %+v", res.ToolCalls)
	}
	if len(res.Events) < 4 || res.Events[1].Kind != "tool_call" || res.Events[2].Kind != "tool_result" {
		t.Fatalf("expected ordered tool_call/tool_result events, got %+v", res.Events)
	}
}

func TestUnknownToolBecomesObservation(t *testing.T) {
	mc := &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "nope", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "recovered", Done: true}},
	}}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "recovered" || !res.ToolCalls[0].IsError {
		t.Fatalf("unknown tool must yield IsError observation, got %+v", res.ToolCalls)
	}
}

func TestRecordResult_CopiesRouteOutcome(t *testing.T) {
	var res Result
	var state State
	ro := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "p", Model: "coder"}}
	call := provider.ToolCall{Function: provider.ToolCallFunction{Name: "delegate_code"}}
	out := ToolResult{Content: "code", RouteOutcome: ro}

	o := New(nil, ContextManager{})
	b := newBatch()
	stop, err := o.recordResult(context.Background(), &res, &state, nil, &restraintGovernor{}, 0, call, Effect{Class: Read | Network}, ToolCallRecord{Step: 0, Name: "delegate_code"}, out, &b, o.newInterceptorRun())
	if err != nil || stop {
		t.Fatalf("recordResult: stop=%v err=%v", stop, err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].RouteOutcome != ro {
		t.Fatalf("RouteOutcome not copied into ToolCallRecord: %+v", res.ToolCalls)
	}
}

// ctxTool returns a caller-owned ContextSet so a test can mutate it AFTER
// dispatch and see whether State kept a copy or an alias. class selects the
// dispatch path (exactly Read is parallel-eligible); outputCap of 0 means the
// anchor's cap can only have come from normalizeEffect.
type ctxTool struct {
	name      string
	set       *ContextSet
	class     EffectClass
	outputCap int
}

func (c *ctxTool) Spec() ToolSpec {
	return ToolSpec{Name: c.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (c *ctxTool) Effect() Effect {
	return Effect{Class: c.class, Approval: ApprovalNever, OutputCap: c.outputCap}
}

func (c *ctxTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "ctx:" + c.name, Context: c.set}, nil
}

// dispatchPaths pairs the two dispatch entry points with the effect class that
// routes to each: recordResult is shared, so both must show identical structure
// handling. dispatchBatch asserts from the event order that the intended path
// actually ran.
var dispatchPaths = []struct {
	name     string
	parallel bool
}{
	{"serial", false},
	{"parallel", true},
}

// dispatchBatch drives a two-call batch through runToolCalls — the real entry
// point that chooses the parallel or serial path — and returns the State
// dispatch built. State is the only place the runtime-only anchor fields exist;
// Run does not expose it. The second tool carries no ContextSet, so every
// caller also observes the legacy nil path on the same dispatch.
func dispatchBatch(t *testing.T, mixed, parallel bool, set *ContextSet, outputCap int) State {
	t.Helper()
	// Exactly Read is parallel-eligible; Read|Network forces the serial path
	// without changing anything else about the call.
	second := EffectClass(Read | Network)
	if parallel {
		second = Read
	}
	tools := []Tool{
		&ctxTool{name: "with-set", set: set, class: Read, outputCap: outputCap},
		&ctxTool{name: "no-set", class: second, outputCap: outputCap},
	}
	reg, err := newToolRegistry(tools)
	if err != nil {
		t.Fatalf("newToolRegistry: %v", err)
	}
	calls := make([]provider.ToolCall, len(tools))
	for i, tl := range tools {
		calls[i] = provider.ToolCall{
			ID: fmt.Sprintf("call-%d", i), Type: "function",
			Function: provider.ToolCallFunction{Name: tl.Spec().Name, Arguments: json.RawMessage(`{}`)},
		}
	}
	o := New(nil, ContextManager{Mixed: mixed})
	var res Result
	var state State
	if err := o.runToolCalls(context.Background(), &res, &state, reg, calls, nil, normalizeObserver(nil), 0, &restraintGovernor{}, o.newInterceptorRun()); err != nil {
		t.Fatalf("runToolCalls: %v", err)
	}
	// Assert the path actually TAKEN, not the predicate that selects it: the
	// serial path interleaves call/result per tool, the parallel path emits all
	// calls (phase 1) before all results (phase 3). Checking canRunParallel here
	// would stay green even if runToolCalls stopped routing to the parallel path.
	wantKinds := []string{"tool_call", "tool_result", "tool_call", "tool_result"}
	if parallel {
		wantKinds = []string{"tool_call", "tool_call", "tool_result", "tool_result"}
	}
	if len(res.Events) != len(wantKinds) {
		t.Fatalf("events = %+v, want %d of %v", res.Events, len(wantKinds), wantKinds)
	}
	for i, e := range res.Events {
		if e.Kind != wantKinds[i] {
			t.Fatalf("event kinds = %+v, want %v: the intended dispatch path was not taken", res.Events, wantKinds)
		}
	}
	if len(state.Messages) != len(tools) {
		t.Fatalf("state.Messages = %d, want %d", len(state.Messages), len(tools))
	}
	return state
}

// attributedSet is validSet plus a verbatim component and attribution, so the
// clone has a nested pointer and two nested slices to get wrong.
func attributedSet() *ContextSet {
	s := validSet()
	a := &s.Groups[0].Alternatives[0]
	a.Desc.Representations = append(a.Desc.Representations, testVerbatimRep)
	a.Attrib = &RetrievalAttribution{Sources: []RetrievedSource{
		{Source: "pkg/doc.go", StableKey: "k1", StartLine: 1, EndLine: 9, Score: 0.5},
	}}
	return s
}

func TestToolObservationDeepCopiesContext(t *testing.T) {
	for _, path := range dispatchPaths {
		t.Run(path.name, func(t *testing.T) {
			set := attributedSet()
			state := dispatchBatch(t, true, path.parallel, set, 0)

			got := state.Messages[0].Context
			if got == nil {
				t.Fatal("mixed dispatch dropped the structured payload")
			}
			if got == set {
				t.Fatal("anchor aliases the tool-owned set: shallow copy")
			}
			if c := state.Messages[1].Context; c != nil {
				t.Errorf("tool that returned no set got Context %+v, want nil", c)
			}

			// Mutate every level the tool still owns. A shared slice, descriptor
			// slice, or attribution pointer shows up as a changed expectation below.
			set.MinVerbatim = 7
			set.Groups[0].Desc.Subject.ID = "mutated-subject"
			set.Groups[0].Alternatives[0].Content = "mutated content"
			set.Groups[0].Alternatives[0].Desc.Representations[0].Kind = contextdepth.RepresentationGenerated
			set.Groups[0].Alternatives[0].Attrib.Sources[0].Source = "mutated.go"
			set.Groups[0].Alternatives[0].Attrib.Sources[0].Score = 9.5
			set.Groups = append(set.Groups, set.Groups[0])

			// Expectations are the literals validSet/attributedSet wrote, never
			// values re-read from set: an aliased anchor would agree with set either way.
			if n := got.MinVerbatim; n != 0 {
				t.Errorf("anchor MinVerbatim = %d, want 0", n)
			}
			if n := len(got.Groups); n != 1 {
				t.Fatalf("anchor groups = %d, want 1", n)
			}
			if id := got.Groups[0].Desc.Subject.ID; id != "pkg/doc.go" {
				t.Errorf("anchor subject ID = %q, want %q", id, "pkg/doc.go")
			}
			alt := got.Groups[0].Alternatives[0]
			if alt.Content != "source pkg/doc.go (no summary)" {
				t.Errorf("anchor alternative content = %q, want the original content", alt.Content)
			}
			if k := alt.Desc.Representations[0].Kind; k != contextdepth.RepresentationMetadata {
				t.Errorf("anchor representation[0].Kind = %v, want metadata", k)
			}
			if alt.Attrib == nil {
				t.Fatal("anchor dropped attribution")
			}
			if src := alt.Attrib.Sources[0]; src.Source != "pkg/doc.go" || src.Score != 0.5 {
				t.Errorf("anchor attribution source = %+v, want the original source", src)
			}
		})
	}
}

// TestRecordResultRejectsOversizedContextBeforeCloning pins the admission
// boundary: the completed result is recorded and observed first, but its
// oversized carrier never reaches State for cloning.
func TestRecordResultRejectsOversizedContextBeforeCloning(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  *ContextSet
		want string
	}{
		{"groups", groupsSet(maxContextGroups + 1), "257 groups exceeds limit 256"},
		{"alternatives", altsSet(maxContextAlternatives + 1), "65 alternatives exceeds limit 64"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var res Result
			var state State
			obs := &resultRec{}
			o := New(nil, ContextManager{Mixed: true})
			b := newBatch()
			_, err := o.recordResult(context.Background(), &res, &state, obs, &restraintGovernor{}, 0,
				provider.ToolCall{ID: "call-1", Function: provider.ToolCallFunction{Name: "ctx"}},
				Effect{}, ToolCallRecord{}, ToolResult{Content: "result", Context: tt.set}, &b, o.newInterceptorRun())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("recordResult error = %v, want %q", err, tt.want)
			}
			if len(res.ToolCalls) != 1 {
				t.Fatalf("completed result was not recorded: %+v", res.ToolCalls)
			}
			if len(res.Events) != 1 || res.Events[0].Kind != "tool_result" {
				t.Fatalf("result event = %+v, want one tool_result", res.Events)
			}
			if len(obs.results) != 1 || obs.results[0].Result.Context != tt.set {
				t.Fatalf("ToolResultObserver did not receive the completed result: %+v", obs.results)
			}
			if len(state.Messages) != 0 {
				t.Fatalf("oversized ContextSet reached State: %+v", state.Messages)
			}
		})
	}
}

// TestToolObservationSkipsContextWhenNotMixed pins the legacy default: nothing
// reads the set with Mixed off, so cloning it would make State retain a
// guaranteed-dead copy of every alternative for the whole run.
func TestToolObservationSkipsContextWhenNotMixed(t *testing.T) {
	for _, path := range dispatchPaths {
		t.Run(path.name, func(t *testing.T) {
			state := dispatchBatch(t, false, path.parallel, attributedSet(), 0)

			if c := state.Messages[0].Context; c != nil {
				t.Errorf("Mixed off: anchor Context = %+v, want nil", c)
			}
			// Byte-identical to the pre-#331 anchor, field by field
			// (ChatMessage carries a slice, so it is not comparable whole).
			got := state.Messages[0]
			want := provider.ChatMessage{
				Role: "tool", Content: "ctx:with-set", ToolName: "with-set", ToolCallID: "call-0",
			}
			if got.Role != want.Role || got.Content != want.Content ||
				got.ToolName != want.ToolName || got.ToolCallID != want.ToolCallID ||
				got.ToolCalls != nil {
				t.Errorf("anchor ChatMessage = %+v, want %+v", got.ChatMessage, want)
			}
			if got.Segment != Elastic || got.Attrib != nil {
				t.Errorf("anchor Segment=%v Attrib=%+v, want Elastic and nil", got.Segment, got.Attrib)
			}
		})
	}
}

// TestRecordResultCopiesOutputCap: the anchor must carry the NORMALIZED cap so
// mixed assembly can enforce the same tool-output boundary capOutput enforces
// on fallback Content. Only the serial row is needed — recordResult is the
// shared tail, and TestToolObservationDeepCopiesContext proves the parallel
// path reaches it.
func TestRecordResultCopiesOutputCap(t *testing.T) {
	tests := []struct {
		name     string
		declared int
		want     int
	}{
		// Expected values are literals/constants, not normalizeEffect output: an
		// assertion derived from the function under test cannot see it gutted.
		{"unset cap normalized to the default", 0, defaultOutputCap},
		{"negative cap normalized to the default", -1, defaultOutputCap},
		{"declared cap preserved", 7, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := dispatchBatch(t, false, false, nil, tt.declared)
			for i, msg := range state.Messages {
				if msg.OutputCap != tt.want {
					t.Errorf("state.Messages[%d].OutputCap = %d, want %d", i, msg.OutputCap, tt.want)
				}
			}
		})
	}

	// Unresolved effect: unknown tool, malformed arguments, and plan failure all
	// return from prepareCall BEFORE normalizeEffect, so recordResult receives a
	// zero Effect while the anchor still carries model-visible Content. It must
	// record 0 — quietly substituting a default the call was never dispatched
	// under would hand mixed assembly a cap that bounds nothing.
	t.Run("unresolved effect stays zero", func(t *testing.T) {
		var res Result
		var state State
		o := New(nil, ContextManager{})
		b := newBatch()
		if _, err := o.recordResult(context.Background(), &res, &state, nil, &restraintGovernor{}, 0,
			provider.ToolCall{Function: provider.ToolCallFunction{Name: "nope"}},
			Effect{}, ToolCallRecord{}, ToolResult{IsError: true, Content: "unknown tool: nope"}, &b, o.newInterceptorRun()); err != nil {
			t.Fatalf("recordResult: %v", err)
		}
		if got := state.Messages[0].OutputCap; got != 0 {
			t.Errorf("unresolved-effect anchor OutputCap = %d, want 0", got)
		}
	})
}

// TestMessageSerializationExcludesRuntimeFields closes the persistence leak:
// Context and OutputCap are runtime-only and must never reach a transcript.
func TestMessageSerializationExcludesRuntimeFields(t *testing.T) {
	const altSentinel = "SER-SENTINEL-ALT"
	const capSentinel = 424242

	set := validSet()
	set.Groups[0].Alternatives[0].Content = altSentinel
	msg := Message{
		ChatMessage: provider.ChatMessage{Role: "tool", Content: "model-visible", ToolName: "ctx", ToolCallID: "call-0"},
		Segment:     Elastic,
		Context:     set,
		OutputCap:   capSentinel,
	}

	tests := []struct {
		name string
		v    any
	}{
		{"message", msg},
		{"state", State{System: "sys", Messages: []Message{msg}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.v)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got := string(b)
			for _, forbidden := range []string{altSentinel, strconv.Itoa(capSentinel), "Context", "OutputCap"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("marshaled %s leaks %q: %s", tt.name, forbidden, got)
				}
			}
			// Guards against a vacuous pass: the message itself must be marshaled.
			if !strings.Contains(got, "model-visible") {
				t.Fatalf("marshaled %s lost the model-visible content: %s", tt.name, got)
			}
		})
	}
}

func TestToolCallRecord_NilRouteOutcome_OmittedFromJSON(t *testing.T) {
	b, err := json.Marshal(ToolCallRecord{Step: 1, Name: "read_file"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "RouteOutcome") {
		t.Fatalf("nil RouteOutcome should be omitted, got %s", b)
	}
}
