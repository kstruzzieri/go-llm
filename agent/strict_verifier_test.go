package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// The strict gate through the real VerifierSlot (#584). The verify command
// is armed but every approver here refuses it, so nothing is executed.

// armedVerifierSlot returns a slot armed with a host command over a temp
// workspace.
func armedVerifierSlot(t *testing.T) *tools.VerifierSlot {
	t.Helper()
	dir := t.TempDir()
	ws, err := tools.NewWorkspace(dir)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	slot, err := tools.NewVerifierSlot(dir, tools.SandboxConfig{})
	if err != nil {
		t.Fatalf("NewVerifierSlot: %v", err)
	}
	cmd, err := tools.NewVerifyCommand(ws, []string{"go", "version"}, "", time.Minute)
	if err != nil {
		t.Fatalf("NewVerifyCommand: %v", err)
	}
	if err := slot.Set(cmd); err != nil {
		t.Fatalf("Set: %v", err)
	}
	return slot
}

// strictWriteFile is a workspace-origin tool named write_file, so a
// successful call reaches post-batch verification. It plans its static
// effect, a preview and key.
type strictWriteFile struct {
	effect agent.Effect
	key    string
}

func (strictWriteFile) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: agent.WriteFileToolName, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s strictWriteFile) Effect() agent.Effect { return s.effect }
func (strictWriteFile) Origin() agent.Origin   { return agent.OriginWorkspace }
func (s strictWriteFile) Plan(context.Context, json.RawMessage) (agent.ToolPlan, error) {
	return agent.ToolPlan{Effect: s.effect, Preview: "P", ApprovalKey: s.key}, nil
}
func (strictWriteFile) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "wrote"}, nil
}

// runWithSlot runs one write_file call, then a final answer, verified by slot.
func runWithSlot(t *testing.T, req agent.Request, slot agent.Verifier) agent.Result {
	t.Helper()
	mc := &scripted{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: agent.WriteFileToolName, Arguments: json.RawMessage(`{}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
	req.Goal = "g"
	res, err := agent.New(mc, agent.ContextManager{}).RunWithVerifier(context.Background(), req, nil, slot)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// onlyToolMessage returns the run's single tool observation.
func onlyToolMessage(t *testing.T, res agent.Result) string {
	t.Helper()
	var out []string
	for _, m := range res.Messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d tool messages, want 1", len(out))
	}
	return out[0]
}

func exposed() *agent.Exposure {
	return &agent.Exposure{Recorded: true, Foreign: true, First: "tool:x"}
}

// allowAllButVerify approves every call except the verify command.
func allowAllButVerify(call provider.ToolCall) bool {
	return call.Function.Name != tools.VerifyToolName
}

// Approver shapes, each recording the methods it receives into log.
type plainShape struct{ log *[]string }

func (p plainShape) Approve(_ context.Context, call provider.ToolCall, preview string) (bool, error) {
	*p.log = append(*p.log, fmt.Sprintf("Approve %s %q", call.Function.Name, preview))
	return allowAllButVerify(call), nil
}

type keyedShape struct{ plainShape }

func (k keyedShape) ApproveKeyed(_ context.Context, call provider.ToolCall, preview, key string) (agent.ApprovalDecision, error) {
	*k.log = append(*k.log, fmt.Sprintf("ApproveKeyed %s %q %q", call.Function.Name, preview, key))
	return agent.ApprovalDecision{Approved: allowAllButVerify(call), ViaGrant: key != ""}, nil
}

type riskShape struct{ plainShape }

func (r riskShape) ApproveWithRisk(_ context.Context, call provider.ToolCall, preview, key string, _ agent.RiskReport) (agent.ApprovalDecision, error) {
	*r.log = append(*r.log, fmt.Sprintf("ApproveWithRisk %s %q %q", call.Function.Name, preview, key))
	return agent.ApprovalDecision{Approved: allowAllButVerify(call), ViaGrant: key != ""}, nil
}

type riskKeyedShape struct{ keyedShape }

func (r riskKeyedShape) ApproveWithRisk(ctx context.Context, call provider.ToolCall, preview, key string, risk agent.RiskReport) (agent.ApprovalDecision, error) {
	return riskShape{r.plainShape}.ApproveWithRisk(ctx, call, preview, key, risk)
}

// freshShape is a risk-and-keyed approver that can also ask a person.
type freshShape struct {
	riskKeyedShape
	fresh []string
}

func (f *freshShape) ApproveFresh(_ context.Context, call provider.ToolCall, _ string, keyed bool,
	risk agent.RiskReport, exp agent.Exposure) (bool, error) {
	f.fresh = append(f.fresh, fmt.Sprintf("%s keyed=%v score=%d findings=%d %+v",
		call.Function.Name, keyed, risk.Score, len(risk.Findings), exp))
	return allowAllButVerify(call), nil
}

func TestStrictVerifierAsksFresh(t *testing.T) {
	var log []string
	ap := &freshShape{riskKeyedShape: riskKeyedShape{keyedShape{plainShape{&log}}}}
	res := runWithSlot(t, agent.Request{StrictApproval: true, PriorExposure: exposed(), Approver: ap,
		Tools: []agent.Tool{strictWriteFile{effect: agent.Effect{Class: agent.Write}, key: "k"}}}, armedVerifierSlot(t))
	exp := fmt.Sprintf("%+v", *exposed())
	want := []string{
		agent.WriteFileToolName + " keyed=true score=0 findings=0 " + exp,
		tools.VerifyToolName + " keyed=true score=0 findings=0 " + exp,
	}
	if !slices.Equal(ap.fresh, want) {
		t.Fatalf("ApproveFresh calls =\n%q\nwant\n%q", ap.fresh, want)
	}
	if len(log) != 0 {
		t.Fatalf("reusable-approval methods consulted: %q", log)
	}
	if out := onlyToolMessage(t, res); !strings.Contains(out, "reason: not approved") {
		t.Fatalf("observation does not carry the refused verification:\n%s", out)
	}
}

func TestStrictCleanPathPreservesApproverMethods(t *testing.T) {
	slot := armedVerifierSlot(t)
	for _, tt := range []struct {
		name  string
		shape func(*[]string) agent.Approver
	}{
		{"plain", func(l *[]string) agent.Approver { return plainShape{l} }},
		{"keyed", func(l *[]string) agent.Approver { return keyedShape{plainShape{l}} }},
		{"risk", func(l *[]string) agent.Approver { return riskShape{plainShape{l}} }},
		{"risk and keyed", func(l *[]string) agent.Approver { return riskKeyedShape{keyedShape{plainShape{l}}} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := map[bool][]string{}
			for _, strict := range []bool{false, true} {
				var log []string
				res := runWithSlot(t, agent.Request{StrictApproval: strict, Approver: tt.shape(&log),
					Tools: []agent.Tool{strictWriteFile{effect: agent.Effect{Class: agent.Write}, key: "k"}}}, slot)
				if out := onlyToolMessage(t, res); !strings.Contains(out, "reason: not approved") {
					t.Fatalf("strict=%v: verification was not reached:\n%s", strict, out)
				}
				logs[strict] = log
			}
			if len(logs[false]) != 2 || !slices.Equal(logs[false], logs[true]) {
				t.Fatalf("approver calls differ:\nstrict off %q\nstrict on  %q", logs[false], logs[true])
			}
			if tt.name == "risk and keyed" {
				if !strings.HasPrefix(logs[true][0], "ApproveWithRisk "+agent.WriteFileToolName) ||
					!strings.HasPrefix(logs[true][1], "ApproveKeyed "+tools.VerifyToolName) {
					t.Fatalf("calls = %q, want the tool call on ApproveWithRisk and the verifier on ApproveKeyed", logs[true])
				}
			}
		})
	}
}

func TestStrictNilApproverVerifierStillSkips(t *testing.T) {
	res := runWithSlot(t, agent.Request{StrictApproval: true, PriorExposure: exposed(),
		StrictExempt: []string{agent.WriteFileToolName},
		Tools:        []agent.Tool{strictWriteFile{effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}}}},
		armedVerifierSlot(t))
	if len(res.ToolCalls) != 1 || !res.ToolCalls[0].Invoked {
		t.Fatalf("records = %+v, want the exempt write invoked", res.ToolCalls)
	}
	if out := onlyToolMessage(t, res); !strings.Contains(out, "reason: no approver available") {
		t.Fatalf("observation does not report the missing approver:\n%s", out)
	}
}
