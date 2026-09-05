package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// tagCurl tags a tool call whose arguments mention curl as network and one
// that mentions sudo as privileged.
func tagCurl() *stubInterceptor {
	return &stubInterceptor{name: "cls", toolCall: func(c ToolCallInspection) []Finding {
		args := string(c.Call.Function.Arguments)
		switch {
		case strings.Contains(args, "curl"):
			return []Finding{{Rule: "network", Verdict: VerdictTag, Risk: 20, Detail: "curl"}}
		case strings.Contains(args, "sudo"):
			return []Finding{{Rule: "privileged", Verdict: VerdictTag, Risk: 20, Detail: "sudo"}}
		}
		return nil
	}}
}

// tagCurlAndWrote is tagCurl plus a tag on a tool observation that says
// "wrote", so a later hook fires an observer event while the current-call
// carrier still holds the call's finding.
func tagCurlAndWrote() *stubInterceptor {
	s := tagCurl()
	s.input = func(in InputInspection) []Finding {
		for _, m := range in.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "wrote") {
				return []Finding{{Rule: "wrote", Verdict: VerdictTag, Risk: 1, Target: TargetMessage, StateIndex: m.StateIndex}}
			}
		}
		return nil
	}
	return s
}

// currentCallApprover records, per approval, the current-call findings and
// the cumulative report it was handed, and can scribble on both afterwards.
type currentCallApprover struct {
	current    [][]Finding
	cumulative []RiskReport
	mutate     bool
}

func (a *currentCallApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return true, nil
}

func (a *currentCallApprover) ApproveWithRisk(_ context.Context, _ provider.ToolCall, _, _ string, risk RiskReport) (ApprovalDecision, error) {
	a.current = append(a.current, append([]Finding(nil), risk.CurrentToolCallFindings...))
	a.cumulative = append(a.cumulative, RiskReport{Score: risk.Score, Findings: append([]Finding(nil), risk.Findings...)})
	if a.mutate {
		for i := range risk.Findings {
			risk.Findings[i].Rule = "clobbered"
		}
		for i := range risk.CurrentToolCallFindings {
			risk.CurrentToolCallFindings[i].Rule = "clobbered"
		}
		risk.Findings = append(risk.Findings, Finding{Rule: "injected"})
		risk.CurrentToolCallFindings = append(risk.CurrentToolCallFindings, Finding{Rule: "injected"})
	}
	return ApprovalDecision{Approved: true}, nil
}

// writerCallWith is a call to the writer stub with the given provider ID.
func writerCallWith(id, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: "writer", Arguments: json.RawMessage(args)}}
}

// stepsOf scripts one model step per call batch, then a final answer.
func stepsOf(batches ...[]provider.ToolCall) []ModelResult {
	var out []ModelResult
	for _, b := range batches {
		out = append(out, ModelResult{Response: provider.ChatResponse{ToolCalls: b}})
	}
	return append(out, ModelResult{Response: provider.ChatResponse{Content: "ok", Done: true}})
}

func ruleNames(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

// TestCurrentFindingsFollowTheCallNotTheID: three calls sharing the empty
// provider ID each get exactly their own inspection's findings at approval,
// a clean call between two classified ones gets none, and the cumulative
// report keeps growing underneath.
func TestCurrentFindingsFollowTheCallNotTheID(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf(
		[]provider.ToolCall{writerCallWith("", `{"cmd":"curl"}`)},
		[]provider.ToolCall{writerCallWith("", `{"cmd":"ls"}`)},
		[]provider.ToolCall{writerCallWith("", `{"cmd":"curl"}`)},
	)}
	ap := &currentCallApprover{}
	o := newTestOrchestrator(mc, WithInterceptors(tagCurl()))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantCurrent := [][]string{{"network"}, {}, {"network"}}
	if len(ap.current) != 3 {
		t.Fatalf("approvals = %d, want 3", len(ap.current))
	}
	for i, got := range ap.current {
		if !reflect.DeepEqual(ruleNames(got), wantCurrent[i]) {
			t.Fatalf("approval %d current = %v, want %v", i, ruleNames(got), wantCurrent[i])
		}
		for _, f := range got {
			if f.Interceptor != "cls" || f.Hook != HookToolCall || f.Target != TargetToolCall || f.ToolCallID != "" {
				t.Fatalf("approval %d current finding not normalized for this call: %+v", i, f)
			}
		}
	}
	wantCumulative := []int{1, 1, 2}
	wantScore := []int{20, 20, 40}
	for i, c := range ap.cumulative {
		if len(c.Findings) != wantCumulative[i] || c.Score != wantScore[i] {
			t.Fatalf("approval %d cumulative = %d findings score %d, want %d and %d", i, len(c.Findings), c.Score, wantCumulative[i], wantScore[i])
		}
	}
	if res.Risk == nil || len(res.Risk.Findings) != 2 || res.Risk.Score != 40 || res.Risk.CurrentToolCallFindings != nil {
		t.Fatalf("result risk = %+v, want two cumulative findings, score 40, no current carrier", res.Risk)
	}
}

// TestCurrentFindingsReusedIDWithinOneStep: two calls with the same ID in
// one model step are approved in order with their own findings.
func TestCurrentFindingsReusedIDWithinOneStep(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf(
		[]provider.ToolCall{writerCallWith("x", `{"cmd":"curl"}`), writerCallWith("x", `{"cmd":"ls"}`)},
	)}
	ap := &currentCallApprover{}
	o := newTestOrchestrator(mc, WithInterceptors(tagCurl()))
	if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := [][]string{}
	for _, c := range ap.current {
		got = append(got, ruleNames(c))
	}
	if want := [][]string{{"network"}, {}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("current per approval = %v, want %v", got, want)
	}
}

// TestCurrentFindingsReusedIDDifferentClassification: the same provider ID
// across steps with different classifications yields each call's own class
// at its approval, never the earlier one.
func TestCurrentFindingsReusedIDDifferentClassification(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf(
		[]provider.ToolCall{writerCallWith("same", `{"cmd":"curl"}`)},
		[]provider.ToolCall{writerCallWith("same", `{"cmd":"sudo"}`)},
	)}
	ap := &currentCallApprover{}
	o := newTestOrchestrator(mc, WithInterceptors(tagCurl()))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := [][]string{}
	for _, c := range ap.current {
		got = append(got, ruleNames(c))
	}
	if want := [][]string{{"network"}, {"privileged"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("current per approval = %v, want %v", got, want)
	}
	if got := ruleNames(res.Risk.Findings); !reflect.DeepEqual(got, []string{"network", "privileged"}) {
		t.Fatalf("cumulative = %v", got)
	}
}

// TestCurrentFindingsAreOwnedCopies: an approver that scribbles on both
// slices of its report changes neither the next approval nor the result.
func TestCurrentFindingsAreOwnedCopies(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf(
		[]provider.ToolCall{writerCallWith("1", `{"cmd":"curl"}`)},
		[]provider.ToolCall{writerCallWith("2", `{"cmd":"curl"}`)},
	)}
	ap := &currentCallApprover{mutate: true}
	o := newTestOrchestrator(mc, WithInterceptors(tagCurl()))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ruleNames(ap.current[1]); !reflect.DeepEqual(got, []string{"network"}) {
		t.Fatalf("second approval current = %v, want the call's own finding only", got)
	}
	if got := ruleNames(ap.cumulative[1].Findings); !reflect.DeepEqual(got, []string{"network", "network"}) {
		t.Fatalf("second approval cumulative = %v, want two untouched findings", got)
	}
	if got := ruleNames(res.Risk.Findings); !reflect.DeepEqual(got, []string{"network", "network"}) {
		t.Fatalf("result findings = %v, want untouched", got)
	}
}

// TestCurrentFindingsAbsentFromCumulativeViews: observer events, the
// result, and JSON carry the cumulative contract only.
func TestCurrentFindingsAbsentFromCumulativeViews(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf([]provider.ToolCall{writerCallWith("1", `{"cmd":"curl"}`)})}
	rec := &interceptRecorder{}
	ap := &currentCallApprover{}
	o := newTestOrchestrator(mc, WithInterceptors(tagCurlAndWrote()))
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, rec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Two events: the tool-call hook (before the carrier is set) and the
	// tool-result hook (after it, when the carrier still holds the call's
	// finding). Neither may expose the carrier.
	if len(rec.interceptions) != 2 || rec.interceptions[0].Hook != HookToolCall || rec.interceptions[1].Hook != HookInput {
		t.Fatalf("interception events = %+v, want tool-call then tool-result", rec.interceptions)
	}
	for i, e := range rec.interceptions {
		if e.Risk.CurrentToolCallFindings != nil || len(e.Risk.Findings) != i+1 {
			t.Fatalf("event %d risk = %+v, want cumulative only", i, e.Risk)
		}
	}
	if len(ap.current) != 1 || len(ap.current[0]) != 1 {
		t.Fatalf("approver current = %v, want the one finding", ap.current)
	}
	if res.Risk.CurrentToolCallFindings != nil {
		t.Fatalf("result carries current findings: %+v", res.Risk)
	}
	raw, err := json.Marshal(RiskReport{Score: 1, CurrentToolCallFindings: []Finding{{Rule: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"Score":1,"Findings":null}` {
		t.Fatalf("json = %s, want the cumulative fields only", got)
	}
	raw, err = json.Marshal(res.Risk)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CurrentToolCallFindings") {
		t.Fatalf("result json leaks the carrier: %s", raw)
	}
}

// TestCurrentFindingsDoNotLeakAcrossRuns: a second run on the same
// orchestrator starts with no current findings and no cumulative report.
func TestCurrentFindingsDoNotLeakAcrossRuns(t *testing.T) {
	o := newTestOrchestrator(nil, WithInterceptors(tagCurl()))
	first := &scriptedCaller{responses: stepsOf([]provider.ToolCall{writerCallWith("1", `{"cmd":"curl"}`)})}
	o.model = first
	ap1 := &currentCallApprover{}
	if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap1}, nil); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(ap1.current) != 1 || len(ap1.current[0]) != 1 {
		t.Fatalf("first run current = %v", ap1.current)
	}
	o.model = &scriptedCaller{responses: stepsOf([]provider.ToolCall{writerCallWith("1", `{"cmd":"ls"}`)})}
	ap2 := &currentCallApprover{}
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap2}, nil)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(ap2.current) != 1 || len(ap2.current[0]) != 0 || len(ap2.cumulative[0].Findings) != 0 {
		t.Fatalf("second run leaked state: current=%v cumulative=%+v", ap2.current, ap2.cumulative)
	}
	if res.Risk != nil {
		t.Fatalf("second run risk = %+v, want nil", res.Risk)
	}
}

// TestCurrentFindingsNilWithoutChain: without interceptors the approval
// report is empty on both axes.
func TestCurrentFindingsNilWithoutChain(t *testing.T) {
	mc := &scriptedCaller{responses: stepsOf([]provider.ToolCall{writerCallWith("1", `{"cmd":"curl"}`)})}
	ap := &currentCallApprover{}
	if _, err := newTestOrchestrator(mc).Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}, Approver: ap}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ap.current) != 1 || ap.current[0] != nil || ap.cumulative[0].Findings != nil || ap.cumulative[0].Score != 0 {
		t.Fatalf("report without a chain = current %v cumulative %+v, want empty", ap.current, ap.cumulative)
	}
}
