package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
)

type fakeCaller struct {
	gotName string
	gotArgs any
	res     *gomcp.CallToolResult
	err     error
}

func (f *fakeCaller) CallTool(_ context.Context, p *gomcp.CallToolParams) (*gomcp.CallToolResult, error) {
	f.gotName, f.gotArgs = p.Name, p.Arguments
	return f.res, f.err
}

func newAdapter(c toolCaller) *toolAdapter {
	return &toolAdapter{caller: c, remoteName: "read", prefixedName: "mcp__fs__read", description: "d", schema: json.RawMessage(`{"type":"object"}`)}
}

func TestAdapterSpecAndEffect(t *testing.T) {
	a := newAdapter(&fakeCaller{})
	if a.Spec().Name != "mcp__fs__read" {
		t.Fatalf("name %q", a.Spec().Name)
	}
	e := a.Effect()
	if e.Approval != agent.ApprovalAlways {
		t.Fatal("imported tools must require approval (ApprovalAlways)")
	}
	if !e.Class.Has(agent.Network) || !e.Class.IsMutating() {
		t.Fatal("effect must be a conservative upper bound (network + mutating)")
	}
	if e.Timeout <= 0 || e.OutputCap <= 0 {
		t.Fatal("effect must declare a positive timeout and output cap")
	}
}

func TestAdapterPlanShowsMCPCall(t *testing.T) {
	a := newAdapter(&fakeCaller{})
	pt, ok := any(a).(agent.PlanningTool)
	if !ok {
		t.Fatal("mcp adapter must provide an approval preview")
	}
	plan, err := pt.Plan(context.Background(), json.RawMessage(`{"path":"x y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Effect.Approval != agent.ApprovalAlways {
		t.Fatalf("approval = %v, want ApprovalAlways", plan.Effect.Approval)
	}
	for _, want := range []string{`tool: mcp__fs__read`, `remote: read`, `args: {"path":"x y"}`} {
		if !strings.Contains(plan.Preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, plan.Preview)
		}
	}
}

func TestAdapterInvokeMapsResult(t *testing.T) {
	fc := &fakeCaller{res: &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "ok"}}}}
	out, err := newAdapter(fc).Invoke(context.Background(), json.RawMessage(`{"path":"x"}`))
	if err != nil {
		t.Fatalf("Invoke returned a hard error: %v", err)
	}
	if out.IsError || out.Content != "ok" {
		t.Fatalf("got %+v", out)
	}
	if fc.gotName != "read" {
		t.Fatalf("called remote name %q, want unprefixed 'read'", fc.gotName)
	}
}

func TestAdapterInvokeServerError(t *testing.T) {
	fc := &fakeCaller{res: &gomcp.CallToolResult{IsError: true, Content: []gomcp.Content{&gomcp.TextContent{Text: "boom"}}}}
	out, _ := newAdapter(fc).Invoke(context.Background(), nil)
	if !out.IsError {
		t.Fatal("CallToolResult.IsError must map to ToolResult.IsError")
	}
}

func TestAdapterInvokeTransportError(t *testing.T) {
	fc := &fakeCaller{err: errors.New("session dropped")}
	out, err := newAdapter(fc).Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("transport error must NOT be a hard error (would abort the loop): %v", err)
	}
	if !out.IsError {
		t.Fatal("transport error must become ToolResult{IsError:true}")
	}
}

func TestAdapterInvokeBadArgs(t *testing.T) {
	out, err := newAdapter(&fakeCaller{}).Invoke(context.Background(), json.RawMessage(`{not json`))
	if err != nil || !out.IsError {
		t.Fatalf("malformed args -> IsError, no hard error; got (%+v,%v)", out, err)
	}
}
