package golem

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// seqCaller issues one tool call per name in order, then answers.
type seqCaller struct {
	names []string
	step  int
}

func (c *seqCaller) Chat(_ context.Context, _ provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.step < len(c.names) {
		call := provider.ToolCall{ID: "c" + c.names[c.step], Type: "function", Function: provider.ToolCallFunction{Name: c.names[c.step], Arguments: json.RawMessage(`{"content":"n"}`)}}
		c.step++
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{call}}}, nil
	}
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

type yesApprover struct{}

func (yesApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return true, nil
}

type strictForeign struct{ *floorStub }

func (strictForeign) Origin() agent.Origin { return agent.OriginForeign }

func memCreate() agent.Tool {
	return agenttools.AgentMemoryCreate{S: &floorMemStore{}, SessionID: func() string { return "s" }}
}

func runStrict(t *testing.T, strict bool, caller agent.ModelCaller, tools []agent.Tool) agent.Result {
	t.Helper()
	rt, err := New(context.Background(), Options{Root: t.TempDir(), Profile: ProfileTrusted, Tools: tools, StrictApproval: strict, Orchestrator: agent.New(caller, agent.ContextManager{})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })
	res, err := rt.Run(context.Background(), Turn{RunID: "strict", Message: "go", Approver: yesApprover{}}, func(Event) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStrictExemptNamesMatchFloor(t *testing.T) {
	fake := &floorStub{name: "dispatch", effect: agent.Effect{Class: agent.Read}}
	tools := []agent.Tool{
		memCreate(), agenttools.AgentMemoryPromote{}, &agenttools.DelegateCode{}, &agenttools.Dispatch{},
		fake, &floorStub{name: "other", effect: agent.Effect{Class: agent.Read}},
	}
	got := strictExemptNames(floorAll(tools))
	want := []string{"agent_memory_create", "agent_memory_promote", "delegate_code", "dispatch"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	// Exactly one "dispatch": the library type, not the fake.
	if n := len(strictExemptNames([]agent.Tool{floorWrap(fake)})); n != 0 {
		t.Fatalf("a tool merely named dispatch listed: %d", n)
	}
}

func floorAll(tools []agent.Tool) []agent.Tool {
	out := make([]agent.Tool, len(tools))
	for i, t := range tools {
		out[i] = floorWrap(t)
	}
	return out
}

func TestEffectivePermissionsReportsStrict(t *testing.T) {
	for _, strict := range []bool{false, true} {
		rt, err := New(context.Background(), Options{Root: t.TempDir(), StrictApproval: strict, Orchestrator: agent.New(&seqCaller{}, agent.ContextManager{})})
		if err != nil {
			t.Fatal(err)
		}
		if got := rt.EffectivePermissions().StrictApproval; got != strict {
			t.Fatalf("StrictApproval = %v, want %v", got, strict)
		}
		closeTestRuntime(t, rt)
	}
}

// TestStrictOptionSetsRequest drives the request through a real run: the
// option decides suspension, and the exempt names are present either way.
func TestStrictOptionSetsRequest(t *testing.T) {
	for _, strict := range []bool{true, false} {
		foreign := strictForeign{&floorStub{name: "fetch", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}}
		put := &floorStub{name: "put_note", effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}}
		res := runStrict(t, strict, &seqCaller{names: []string{"fetch", "agent_memory_create", "put_note"}}, []agent.Tool{foreign, memCreate(), put})
		if len(res.ToolCalls) != 3 {
			t.Fatalf("strict=%v calls = %+v", strict, res.ToolCalls)
		}
		if res.ToolCalls[1].Denied || !res.ToolCalls[1].Invoked {
			t.Fatalf("strict=%v exempt tool = %+v, want invoked", strict, res.ToolCalls[1])
		}
		if denied := res.ToolCalls[2].Denied; denied != strict {
			t.Fatalf("strict=%v floored tool denied = %v", strict, denied)
		}
	}
}

func TestStrictRuntimeSuspendsFlooredTool(t *testing.T) {
	foreign := strictForeign{&floorStub{name: "fetch", effect: agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}}}
	put := &floorStub{name: "put_note", effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}}
	res := runStrict(t, true, &seqCaller{names: []string{"put_note", "fetch", "put_note"}}, []agent.Tool{foreign, put})
	if res.ToolCalls[0].Denied || !res.ToolCalls[0].Invoked {
		t.Fatalf("clean call = %+v, want approved", res.ToolCalls[0])
	}
	if !res.ToolCalls[2].Denied || put.invokes.Load() != 1 {
		t.Fatalf("exposed call = %+v invokes=%d, want denied", res.ToolCalls[2], put.invokes.Load())
	}
}

// hostTool runs a strict-off runtime turn on the context its parent run gave it.
type hostTool struct {
	run func(ctx context.Context) error
}

func (hostTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "host", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (hostTool) Effect() agent.Effect { return agent.Effect{Class: agent.Read} }
func (h hostTool) Invoke(ctx context.Context, _ json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, h.run(ctx)
}

func TestStrictRuntimeInsideStrictParentKeepsExemptions(t *testing.T) {
	put := &floorStub{name: "put_note", effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}}
	inner, err := New(context.Background(), Options{Root: t.TempDir(), Profile: ProfileTrusted, Tools: []agent.Tool{memCreate(), put},
		Orchestrator: agent.New(&seqCaller{names: []string{"agent_memory_create", "put_note"}}, agent.ContextManager{})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, inner) })
	var innerRes agent.Result
	host := hostTool{run: func(ctx context.Context) error {
		var err error
		innerRes, err = inner.Run(ctx, Turn{RunID: "inner", Message: "go", Approver: yesApprover{}}, func(Event) error { return nil })
		return err
	}}
	parent := agent.New(&seqCaller{names: []string{"host"}}, agent.ContextManager{})
	_, err = parent.Run(context.Background(), agent.Request{
		Goal: "go", Tools: []agent.Tool{host}, MaxSteps: 4, StrictApproval: true,
		PriorExposure: &agent.Exposure{Recorded: true, Foreign: true, First: "tool:x"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(innerRes.ToolCalls) != 2 {
		t.Fatalf("inner calls = %+v", innerRes.ToolCalls)
	}
	if c := innerRes.ToolCalls[0]; c.Denied || !c.Invoked {
		t.Fatalf("exempt tool = %+v, want invoked unprompted", c)
	}
	if c := innerRes.ToolCalls[1]; !c.Denied || put.invokes.Load() != 0 {
		t.Fatalf("floored tool = %+v, want denied", c)
	}
}
