package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeWriteTool stands in for write_file / edit_file: a Write-class tool whose
// name is what the #347 predicate keys on. approval and fail select the
// dispatch outcome each test needs.
type fakeWriteTool struct {
	name     string
	approval ApprovalPolicy
	fail     bool
}

func (f fakeWriteTool) Spec() ToolSpec {
	return ToolSpec{Name: f.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (f fakeWriteTool) Effect() Effect {
	return Effect{Class: Write, Approval: f.approval}
}

func (f fakeWriteTool) Invoke(_ context.Context, _ json.RawMessage) (ToolResult, error) {
	if f.fail {
		return ToolResult{IsError: true, Content: "write refused"}, nil
	}
	return ToolResult{Content: "wrote by " + f.name}, nil
}

// fakeVerifier records how often the Orchestrator ran the post-batch check and
// returns a scripted outcome. hook runs inside Verify so a test can cancel the
// parent context mid-check.
type fakeVerifier struct {
	calls int
	out   string
	err   error
	hook  func()
}

func (f *fakeVerifier) Verify(_ context.Context, _ Approver) (string, error) {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	return f.out, f.err
}

type denyApprover struct{}

func (denyApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return false, nil
}

// batchThenAnswer scripts one tool-call batch followed by a final answer.
func batchThenAnswer(calls ...provider.ToolCall) *scriptedCaller {
	return &scriptedCaller{responses: []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: calls}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
}

// observationFor returns the tool observation the model would read for id.
func observationFor(t *testing.T, msgs []provider.ChatMessage, id string) string {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	t.Fatalf("no tool observation for call %q in %+v", id, msgs)
	return ""
}

const verifyObs = "\n--- post-batch verification ---\nstatus: failed\n"

func TestVerifyRunsOnceOnLastSuccessfulWrite(t *testing.T) {
	v := &fakeVerifier{out: verifyObs}
	o := newTestOrchestrator(batchThenAnswer(
		toolCall("w1", "write_file", "{}"),
		toolCall("r1", "echo", "{}"),
		toolCall("e1", "edit_file", "{}"),
	), WithVerifier(v))

	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
		fakeWriteTool{name: "edit_file", approval: ApprovalNever},
		echoTool{name: "echo"},
	}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.calls != 1 {
		t.Fatalf("expected exactly one verification for the batch, got %d", v.calls)
	}
	if got := observationFor(t, res.Messages, "e1"); !strings.HasSuffix(got, verifyObs) {
		t.Fatalf("edit_file observation must carry the verification, got %q", got)
	}
	for _, id := range []string{"w1", "r1"} {
		if got := observationFor(t, res.Messages, id); strings.Contains(got, "post-batch verification") {
			t.Fatalf("call %q must not carry the verification, got %q", id, got)
		}
	}
}

func TestVerifyAnchorsOnLastWriteWhenALaterCallFails(t *testing.T) {
	v := &fakeVerifier{out: verifyObs}
	o := newTestOrchestrator(batchThenAnswer(
		toolCall("w1", "write_file", "{}"),
		toolCall("x1", "nope", "{}"), // unknown tool: synthetic failure after the write
	), WithVerifier(v))

	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if v.calls != 1 {
		t.Fatalf("a batch that mutated the tree verifies once, got %d", v.calls)
	}
	if got := observationFor(t, res.Messages, "w1"); !strings.HasSuffix(got, verifyObs) {
		t.Fatalf("verification must anchor on the write, got %q", got)
	}
	if got := observationFor(t, res.Messages, "x1"); strings.Contains(got, "post-batch verification") {
		t.Fatalf("failed call must not carry the verification, got %q", got)
	}
}

func TestVerifySkippedCases(t *testing.T) {
	cases := []struct {
		name     string
		tools    []Tool
		calls    []provider.ToolCall
		approver Approver
	}{
		{
			name:  "read only batch",
			tools: []Tool{echoTool{name: "echo"}},
			calls: []provider.ToolCall{toolCall("r1", "echo", "{}"), toolCall("r2", "echo", "{}")},
		},
		{
			name:  "write returns IsError",
			tools: []Tool{fakeWriteTool{name: "write_file", approval: ApprovalNever, fail: true}},
			calls: []provider.ToolCall{toolCall("w1", "write_file", "{}")},
		},
		{
			name:     "write denied by approver",
			tools:    []Tool{fakeWriteTool{name: "write_file", approval: ApprovalAlways}},
			calls:    []provider.ToolCall{toolCall("w1", "write_file", "{}")},
			approver: denyApprover{},
		},
		{
			name:  "unknown tool only",
			calls: []provider.ToolCall{toolCall("x1", "nope", "{}")},
		},
		{
			name:  "malformed arguments",
			tools: []Tool{fakeWriteTool{name: "write_file", approval: ApprovalNever}},
			calls: []provider.ToolCall{{
				ID: "w1", Type: "function",
				Function: provider.ToolCallFunction{Name: "write_file", Arguments: json.RawMessage(`{`)},
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeVerifier{out: verifyObs}
			o := newTestOrchestrator(batchThenAnswer(tc.calls...), WithVerifier(v))
			if _, err := o.Run(context.Background(), Request{
				Goal: "q", Tools: tc.tools, Approver: tc.approver,
			}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if v.calls != 0 {
				t.Fatalf("expected no verification, got %d", v.calls)
			}
		})
	}
}

func TestVerifySkippedWhenGovernorStopsTheBatch(t *testing.T) {
	v := &fakeVerifier{out: verifyObs}
	// defaultToolErrorCap identical SUCCESSFUL writes trip the governor's
	// no-progress repeat detector (identical name+args+result). They must
	// succeed: with failing writes the anchor would stay -1 and the skip could
	// not be attributed to the StopReason guard.
	calls := make([]provider.ToolCall, defaultToolErrorCap)
	for i := range calls {
		calls[i] = toolCall("w"+string(rune('1'+i)), "write_file", "{}")
	}
	o := newTestOrchestrator(batchThenAnswer(calls...), WithVerifier(v))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.StopReason != RepeatLimitReached {
		t.Fatalf("fixture must trip the repeat governor; got StopReason=%v", res.StopReason)
	}
	for _, rec := range res.ToolCalls {
		if rec.IsError || !rec.Invoked {
			t.Fatalf("fixture must record successful writes, got %+v", rec)
		}
	}
	if v.calls != 0 {
		t.Fatalf("a stopped batch must not verify, got %d", v.calls)
	}
}

func TestVerifyUnsetLeavesTranscriptUnchanged(t *testing.T) {
	run := func(opts ...Option) []provider.ChatMessage {
		o := newTestOrchestrator(batchThenAnswer(toolCall("w1", "write_file", "{}")), opts...)
		res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
			fakeWriteTool{name: "write_file", approval: ApprovalNever},
		}}, nil)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res.Messages
	}
	base := run()
	withEmpty := run(WithVerifier(&fakeVerifier{}))
	if len(base) != len(withEmpty) {
		t.Fatalf("message count changed: %d vs %d", len(base), len(withEmpty))
	}
	for i := range base {
		if base[i].Content != withEmpty[i].Content || base[i].Role != withEmpty[i].Role {
			t.Fatalf("message %d differs: %+v vs %+v", i, base[i], withEmpty[i])
		}
	}
}

func TestVerifyErrorAbortsRunWithLiveParentContext(t *testing.T) {
	// The exact defect a post-hoc ctx.Err() check would hide: the consumer's
	// approval boundary normalizes an interrupted prompt to context.Canceled
	// WITHOUT cancelling the parent context.
	sentinel := context.Canceled
	v := &fakeVerifier{out: verifyObs, err: sentinel}
	o := newTestOrchestrator(batchThenAnswer(toolCall("w1", "write_file", "{}")), WithVerifier(v))

	ctx := context.Background()
	_, err := o.Run(ctx, Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if ctx.Err() != nil {
		t.Fatalf("fixture invalid: parent context must stay live")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("verifier error must abort the run, got err=%v", err)
	}
	if v.calls != 1 {
		t.Fatalf("the check must have run once, got %d", v.calls)
	}
	// Result.Messages is nil on an abort (resultMessages never runs), so
	// asserting the observation's absence here would assert nothing. That pin
	// lives in TestVerifyBatchNeverAppendsWhenTheVerifierErrors.
}

func TestVerifyNonCancellationErrorAlsoAborts(t *testing.T) {
	sentinel := errors.New("approval prompt failed")
	v := &fakeVerifier{out: verifyObs, err: sentinel}
	o := newTestOrchestrator(batchThenAnswer(toolCall("w1", "write_file", "{}")), WithVerifier(v))
	_, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("approver/control-plane failure must abort, got %v", err)
	}
}

func TestVerifyCancelledDuringCheckSkipsAppend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Verify succeeds, but the parent is cancelled while it runs: the ordering
	// pin is that State is never mutated after a cancellation.
	v := &fakeVerifier{out: verifyObs, hook: cancel}
	o := newTestOrchestrator(batchThenAnswer(toolCall("w1", "write_file", "{}")), WithVerifier(v))
	_, err := o.Run(ctx, Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during the check must abort, got %v", err)
	}
}

func TestVerifyIsInvisibleToTheModelAndTheRecord(t *testing.T) {
	v := &fakeVerifier{out: verifyObs}
	o := newTestOrchestrator(batchThenAnswer(toolCall("w1", "write_file", "{}")), WithVerifier(v))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
		fakeWriteTool{name: "write_file", approval: ApprovalNever},
	}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "write_file" {
		t.Fatalf("verification must not appear as a tool call: %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].IsError {
		t.Fatalf("a failing verification must not mark the write as failed: %+v", res.ToolCalls[0])
	}
	var results int
	for _, e := range res.Events {
		if e.Kind == "tool_result" {
			results++
		}
	}
	if results != 1 {
		t.Fatalf("verification must emit no tool_result event, got %d", results)
	}
}

// mustAnchorState is a one-message transcript standing in for a batch whose
// only observation is the write the verification anchors on.
func mustAnchorState() State {
	return State{Messages: []Message{{ChatMessage: provider.ChatMessage{
		Role: "tool", Content: "wrote", ToolCallID: "w1", ToolName: WriteFileToolName,
	}}}}
}

func TestBatchNotePredicate(t *testing.T) {
	cases := []struct {
		name    string
		tool    string
		invoked bool
		isError bool
		want    int
	}{
		{"invoked successful write", WriteFileToolName, true, false, 0},
		{"invoked successful edit", EditFileToolName, true, false, 0},
		{"not invoked but no error", WriteFileToolName, false, false, -1},
		{"invoked with error", WriteFileToolName, true, true, -1},
		{"read tool", "read_file", true, false, -1},
		{"exec tool", "run_command", true, false, -1},
		{"background exec tool", "start_command", true, false, -1},
		{"agent memory write", "agent_memory_write", true, false, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := mustAnchorState()
			b := newBatch()
			b.note(&st,
				provider.ToolCall{Function: provider.ToolCallFunction{Name: tc.tool}},
				ToolCallRecord{Invoked: tc.invoked},
				ToolResult{IsError: tc.isError})
			if b.verifyAnchor != tc.want {
				t.Fatalf("verifyAnchor = %d, want %d", b.verifyAnchor, tc.want)
			}
		})
	}
}

func TestVerifyBatchNeverAppendsWhenTheVerifierErrors(t *testing.T) {
	sentinel := errors.New("approval prompt failed")
	st := mustAnchorState()
	b := batch{verifyAnchor: 0}
	o := New(nil, ContextManager{}, WithVerifier(&fakeVerifier{out: verifyObs, err: sentinel}))

	if err := o.verifyBatch(context.Background(), &st, nil, &b); !errors.Is(err, sentinel) {
		t.Fatalf("verifyBatch err = %v, want %v", err, sentinel)
	}
	if st.Messages[0].Content != "wrote" {
		t.Fatalf("observation appended despite the error: %q", st.Messages[0].Content)
	}
}

func TestVerifyBatchNeverAppendsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st := mustAnchorState()
	b := batch{verifyAnchor: 0}
	o := New(nil, ContextManager{}, WithVerifier(&fakeVerifier{out: verifyObs, hook: cancel}))

	if err := o.verifyBatch(ctx, &st, nil, &b); !errors.Is(err, context.Canceled) {
		t.Fatalf("verifyBatch err = %v, want context.Canceled", err)
	}
	if st.Messages[0].Content != "wrote" {
		t.Fatalf("observation appended after cancellation: %q", st.Messages[0].Content)
	}
}

func TestVerifyBatchAppendsToTheAnchorOnSuccess(t *testing.T) {
	st := mustAnchorState()
	b := batch{verifyAnchor: 0}
	o := New(nil, ContextManager{}, WithVerifier(&fakeVerifier{out: verifyObs}))

	if err := o.verifyBatch(context.Background(), &st, nil, &b); err != nil {
		t.Fatalf("verifyBatch: %v", err)
	}
	if want := "wrote" + verifyObs; st.Messages[0].Content != want {
		t.Fatalf("Content = %q, want %q", st.Messages[0].Content, want)
	}
}

// recordingCaller captures every assembled request so a test can assert on what
// the MODEL actually reads, rather than on State — the two differ under mixed
// assembly, which is the whole point of this test.
type recordingCaller struct {
	responses []ModelResult
	calls     int
	requests  []provider.ChatRequest
}

func (r *recordingCaller) Chat(_ context.Context, req provider.ChatRequest,
	_ func(provider.ChatResponse) error) (ModelResult, error) {
	r.requests = append(r.requests, req)
	out := r.responses[r.calls]
	r.calls++
	return out, nil
}

func verifySurvivalSet() *ContextSet {
	return &ContextSet{Groups: []ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			Rank:    1,
		},
		Alternatives: []ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: "structured-alternative",
		}},
	}}}
}

// TestVerifyObservationReachesTheModelUnderBothAssemblies pins the reason the
// anchor is the last successful WRITE and not the batch's last result: the
// batch ends with a structured retrieval anchor, whose Content mixed assembly
// replaces at materialization. Anchoring on the last message would put the
// verification somewhere the model never reads under -progressive.
func TestVerifyObservationReachesTheModelUnderBothAssemblies(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		name := "legacy"
		if mixed {
			name = "mixed"
		}
		t.Run(name, func(t *testing.T) {
			mc := &recordingCaller{responses: []ModelResult{
				{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
					toolCall("w1", "write_file", "{}"),
					toolCall("c1", "retrieve", "{}"),
				}}},
				{Response: provider.ChatResponse{Content: "done", Done: true}},
			}}
			o := New(mc, ContextManager{Mixed: mixed, Estimate: runeEstimator},
				WithVerifier(&fakeVerifier{out: verifyObs}))

			if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{
				fakeWriteTool{name: "write_file", approval: ApprovalNever},
				&ctxTool{name: "retrieve", set: verifySurvivalSet(), class: Read},
			}}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(mc.requests) != 2 {
				t.Fatalf("expected a second model call after the batch, got %d", len(mc.requests))
			}

			var write, retrieve string
			var sawWrite, sawRetrieve bool
			for _, m := range mc.requests[1].Messages {
				switch m.ToolCallID {
				case "w1":
					write, sawWrite = m.Content, true
				case "c1":
					retrieve, sawRetrieve = m.Content, true
				}
			}
			if !sawWrite || !sawRetrieve {
				t.Fatalf("both observations must reach the model: write=%v retrieve=%v", sawWrite, sawRetrieve)
			}
			if !strings.HasSuffix(write, verifyObs) {
				t.Fatalf("model-visible write observation lost the verification: %q", write)
			}
			if strings.Contains(retrieve, "post-batch verification") {
				t.Fatalf("verification must not ride on the retrieval anchor: %q", retrieve)
			}
			if mixed && retrieve != "structured-alternative" {
				t.Fatalf("fixture invalid: mixed assembly must have rewritten the anchor, got %q", retrieve)
			}
		})
	}
}
