package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type writeTool struct {
	planPreview string
	planKey     string
}

func (writeTool) Spec() ToolSpec { return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{}`)} }
func (writeTool) Effect() Effect {
	return Effect{Class: Write, Approval: ApprovalOnWrite}
}
func (w writeTool) Plan(context.Context, json.RawMessage) (ToolPlan, error) {
	return ToolPlan{Effect: Effect{Class: Write, Approval: ApprovalOnWrite}, Preview: w.planPreview, ApprovalKey: w.planKey}, nil
}
func (writeTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "wrote"}, nil
}

type defaultWriteTool struct{}

func (defaultWriteTool) Spec() ToolSpec {
	return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{}`)}
}
func (defaultWriteTool) Effect() Effect { return Effect{Class: Write} }
func (defaultWriteTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "wrote"}, nil
}

func writeCallSeq() []ModelResult {
	return []ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "writer", Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "ok", Done: true}},
	}
}

func TestNilApproverDeniesWriteTool(t *testing.T) {
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{writeTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ToolCalls[0].Denied {
		t.Fatalf("nil approver must deny a Write tool, got %+v", res.ToolCalls[0])
	}
}

func TestNilApproverDeniesDefaultWriteTool(t *testing.T) {
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{defaultWriteTool{}}}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ToolCalls[0].Denied {
		t.Fatalf("nil approver must deny a zero-policy Write tool, got %+v", res.ToolCalls[0])
	}
}

type capturingApprover struct {
	gotPreview string
	allow      bool
}

func (c *capturingApprover) Approve(_ context.Context, _ provider.ToolCall, preview string) (bool, error) {
	c.gotPreview = preview
	return c.allow, nil
}

func TestApproverReceivesPlanPreview(t *testing.T) {
	ap := &capturingApprover{allow: true}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planPreview: "DIFF: +1 -0"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.gotPreview != "DIFF: +1 -0" {
		t.Fatalf("approver preview = %q, want plan preview", ap.gotPreview)
	}
}

// keyedCapturingApprover implements Approver AND KeyedApprover, recording the
// key and preview it receives so tests can pin the plan->approver delivery.
type keyedCapturingApprover struct {
	gotKey     string
	gotPreview string
	keyedCalls int
	plainCalls int
	decision   ApprovalDecision
}

func (c *keyedCapturingApprover) Approve(_ context.Context, _ provider.ToolCall, preview string) (bool, error) {
	c.plainCalls++
	c.gotPreview = preview
	return c.decision.Approved, nil
}

func (c *keyedCapturingApprover) ApproveKeyed(_ context.Context, _ provider.ToolCall, preview, key string) (ApprovalDecision, error) {
	c.keyedCalls++
	c.gotPreview, c.gotKey = preview, key
	return c.decision, nil
}

func TestKeyedApproverReceivesPlanApprovalKey(t *testing.T) {
	ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planPreview: "P", planKey: "grant-key-1"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.keyedCalls != 1 || ap.plainCalls != 0 {
		t.Fatalf("keyed=%d plain=%d, want the keyed path exactly once", ap.keyedCalls, ap.plainCalls)
	}
	if ap.gotKey != "grant-key-1" {
		t.Fatalf("key = %q, want the plan's ApprovalKey", ap.gotKey)
	}
	if ap.gotPreview != "P" {
		t.Fatalf("preview = %q: keyed path must still carry the preview", ap.gotPreview)
	}
}

func TestKeyedApproverGetsEmptyKeyForNonPlanningTool(t *testing.T) {
	ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}
	ap.gotKey = "sentinel-not-overwritten"
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{defaultWriteTool{}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ap.keyedCalls != 1 || ap.gotKey != "" {
		t.Fatalf("keyed=%d key=%q, want keyed call with empty key", ap.keyedCalls, ap.gotKey)
	}
}

func TestPlainApproverFallbackWhenNotKeyed(t *testing.T) {
	ap := &capturingApprover{allow: true}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planPreview: "P", planKey: "grant-key-1"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ToolCalls[0].Denied {
		t.Fatal("plain approver returning true must still approve a keyed plan")
	}
	if ap.gotPreview != "P" {
		t.Fatalf("preview = %q, want fallback to Approve with preview", ap.gotPreview)
	}
}

func TestAutoApprovedRecordedOnToolCallRecord(t *testing.T) {
	ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true, ViaGrant: true}}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planKey: "k"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.ToolCalls[0].AutoApproved {
		t.Fatal("a ViaGrant approval must be recorded as AutoApproved")
	}
}

func TestManualApprovalNotRecordedAsAuto(t *testing.T) {
	ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true}} // prompted "y"
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	res, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planKey: "k"}}, Approver: ap,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ToolCalls[0].AutoApproved {
		t.Fatal("an explicit approval must not be recorded as AutoApproved")
	}
}

func TestAutoApprovedOmittedFromRecordJSONWhenFalse(t *testing.T) {
	// Same byte-identity discipline as RouteOutcome (types.go): pre-#341 run
	// traces must marshal identically.
	b, err := json.Marshal(ToolCallRecord{Name: "writer"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "AutoApproved") {
		t.Fatalf("false AutoApproved must be omitted from marshaled records: %s", b)
	}
}

func TestToolResultEventCarriesAutoApproved(t *testing.T) {
	// Request has NO Observer field; observers are Run's third argument.
	// resultRec is the existing Observer+ToolResultObserver fixture.
	ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true, ViaGrant: true}}
	mc := &scriptedCaller{responses: writeCallSeq()}
	o := newTestOrchestrator(mc)
	rec := &resultRec{}
	_, err := o.Run(context.Background(), Request{
		Goal: "q", Tools: []Tool{writeTool{planKey: "k"}}, Approver: ap,
	}, rec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rec.results) != 1 || !rec.results[0].AutoApproved {
		t.Fatalf("results = %+v, want one event with AutoApproved", rec.results)
	}
}
