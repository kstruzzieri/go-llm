package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// dispatchTestEnvelope mirrors the library's result envelope JSON. Field names
// are pinned to dispatch.go's JSON tags; a rename there breaks these tests
// loudly, which is correct.
type dispatchTestEnvelope struct {
	Results []struct {
		Summary    string `json:"summary"`
		StopReason string `json:"stop_reason"`
		Model      string `json:"model"`
		Error      string `json:"error,omitempty"`
	} `json:"results"`
}

// countingDispatchStub stands in for the real dispatch tool: same name and
// read-only effect, counts actual Invokes so the test observes the per-run cap
// rather than re-deriving it from the option we installed.
type countingDispatchStub struct{ invokes int }

func (s *countingDispatchStub) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: agenttools.DispatchToolName, Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (s *countingDispatchStub) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (s *countingDispatchStub) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	s.invokes++
	return agent.ToolResult{Content: "ok"}, nil
}

// TestOrchestratorFactory_DispatchPerRunInvocationCap drives a factory-built
// orchestrator through DefaultDispatchCallsPerRun+1 dispatch tool calls. With
// -dispatch the cap must synthetically fail the last call before Invoke;
// without it the control run proves the counter can actually see all calls
// (the assertion is not blind to the behavior it tests).
func TestOrchestratorFactory_DispatchPerRunInvocationCap(t *testing.T) {
	calls := agenttools.DefaultDispatchCallsPerRun + 1
	for _, tc := range []struct {
		name        string
		dispatch    bool
		wantInvokes int
	}{
		{"cap installed with -dispatch", true, agenttools.DefaultDispatchCallsPerRun},
		{"control: no cap without -dispatch", false, calls},
	} {
		t.Run(tc.name, func(t *testing.T) {
			responses := make([]agent.ModelResult, 0, calls+1)
			for i := range calls {
				// Distinct arguments per call so the repeat-call governor does
				// not collapse them before the invocation limit is exercised.
				responses = append(responses, agent.ModelResult{Response: provider.ChatResponse{
					ToolCalls: []provider.ToolCall{{
						ID: fmt.Sprintf("c%d", i), Type: "function",
						Function: provider.ToolCallFunction{
							Name:      agenttools.DispatchToolName,
							Arguments: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
						},
					}},
				}})
			}
			responses = append(responses, agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}})
			stub := &countingDispatchStub{}
			caller := &scriptCaller{responses: responses}
			orch := newOrchestratorFactory(caller, flags{dispatch: tc.dispatch})()
			res, err := orch.Run(context.Background(), agent.Request{
				Goal:  "explore",
				Tools: []agent.Tool{stub},
			}, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if stub.invokes != tc.wantInvokes {
				t.Fatalf("dispatch invokes = %d, want %d", stub.invokes, tc.wantInvokes)
			}
			// All scripted responses consumed: every tool step really happened,
			// so the count above measures the cap, not an early stop.
			if caller.i != calls+1 {
				t.Fatalf("model calls = %d, want %d", caller.i, calls+1)
			}
			var dispatchRecords []agent.ToolCallRecord
			for _, rec := range res.ToolCalls {
				if rec.Name == agenttools.DispatchToolName {
					dispatchRecords = append(dispatchRecords, rec)
				}
			}
			if len(dispatchRecords) != calls {
				t.Fatalf("dispatch call records = %d, want %d", len(dispatchRecords), calls)
			}
			last := dispatchRecords[len(dispatchRecords)-1]
			if tc.dispatch {
				if last.Invoked || !last.IsError {
					t.Fatalf("capped call must be a synthetic pre-invoke failure: %+v", last)
				}
			} else if !last.Invoked {
				t.Fatalf("uncapped final call must actually invoke: %+v", last)
			}
		})
	}
}

// specRecordingCaller records the tool specs of every child chat request and
// answers immediately, ending each child run in one step.
type specRecordingCaller struct {
	mu    sync.Mutex
	tools [][]string
}

func (c *specRecordingCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	c.mu.Lock()
	c.tools = append(c.tools, names)
	c.mu.Unlock()
	resp := provider.ChatResponse{Content: "child done", Done: true}
	if onToken != nil {
		_ = onToken(resp)
	}
	return agent.ModelResult{Response: resp}, nil
}

// sentinelRetrieve is a stand-in for the shared readyRetrieve instance; its
// content surfacing in a child summary proves the INSTANCE golem passed in is
// the one children invoke.
type sentinelRetrieve struct{ invoked atomic.Bool }

func (r *sentinelRetrieve) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "retrieve", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (r *sentinelRetrieve) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (r *sentinelRetrieve) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	r.invoked.Store(true)
	return agent.ToolResult{Content: "SENTINEL-RETRIEVE-CHUNK"}, nil
}

func invokeDispatch(t *testing.T, tool agent.Tool, tasks []string) dispatchTestEnvelope {
	t.Helper()
	raw, err := json.Marshal(map[string][]string{"tasks": tasks})
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("dispatch Invoke: %v", err)
	}
	var envelope dispatchTestEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode envelope: %v (content %q)", err, out.Content)
	}
	return envelope
}

func TestNewDispatchTool_ChildrenSeeExactlyTheReadToolset(t *testing.T) {
	for _, tc := range []struct {
		name         string
		withRetrieve bool
		want         []string
	}{
		{"with retrieve", true, []string{"read_file", "search", "glob", "list", "retrieve"}},
		{"without retrieve", false, []string{"read_file", "search", "glob", "list"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			available := validDispatchAvailable(t)
			if tc.withRetrieve {
				available = append(available, &sentinelRetrieve{})
			}
			caller := &specRecordingCaller{}
			tool, err := newDispatchTool(caller, false, available)
			if err != nil {
				t.Fatalf("newDispatchTool: %v", err)
			}
			invokeDispatch(t, tool, []string{"inspect"})
			if len(caller.tools) != 1 {
				t.Fatalf("child chat requests = %d, want 1", len(caller.tools))
			}
			if got := strings.Join(caller.tools[0], ","); got != strings.Join(tc.want, ",") {
				t.Fatalf("child toolset = %v, want %v", caller.tools[0], tc.want)
			}
		})
	}
}

// retrieveCallingCaller scripts one child: call retrieve, then answer with the
// tool result content so the summary carries the sentinel.
type retrieveCallingCaller struct{ step atomic.Int32 }

func (c *retrieveCallingCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	var resp provider.ChatResponse
	if c.step.Add(1) == 1 {
		resp = provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "r1", Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{}`)},
		}}}
	} else {
		content := "no retrieval result seen"
		for _, m := range req.Messages {
			if strings.Contains(m.Content, "SENTINEL-RETRIEVE-CHUNK") {
				content = "used SENTINEL-RETRIEVE-CHUNK"
			}
		}
		resp = provider.ChatResponse{Content: content, Done: true}
	}
	if onToken != nil {
		_ = onToken(resp)
	}
	return agent.ModelResult{Response: resp}, nil
}

func TestNewDispatchTool_ChildInvokesTheSharedRetrieveInstance(t *testing.T) {
	sentinel := &sentinelRetrieve{}
	tool, err := newDispatchTool(&retrieveCallingCaller{}, false, append(validDispatchAvailable(t), sentinel))
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	envelope := invokeDispatch(t, tool, []string{"find the sentinel"})
	if !sentinel.invoked.Load() {
		t.Fatal("child never invoked the retrieve instance golem passed in")
	}
	if len(envelope.Results) != 1 || !strings.Contains(envelope.Results[0].Summary, "SENTINEL-RETRIEVE-CHUNK") {
		t.Fatalf("summary must carry the sentinel retrieval: %+v", envelope.Results)
	}
}

// gatedSerialCaller blocks each child's chat on a per-task gate and records the
// concurrent-children high-water mark. Modeled on the library's
// TestDispatchBoundsConcurrencyAndPreservesResultOrder gate choreography.
type gatedSerialCaller struct {
	started chan string
	gates   map[string]chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func (c *gatedSerialCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		observed := c.max.Load()
		if active <= observed || c.max.CompareAndSwap(observed, active) {
			break
		}
	}
	task := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			task = m.Content
		}
	}
	c.started <- task
	select {
	case <-c.gates[task]:
	case <-ctx.Done():
		return agent.ModelResult{}, ctx.Err()
	}
	resp := provider.ChatResponse{Content: "done " + task, Done: true}
	if onToken != nil {
		_ = onToken(resp)
	}
	return agent.ModelResult{Response: resp}, nil
}

// TestNewDispatchTool_ChildrenRunSequentially proves golem keeps the library's
// MaxConcurrent=1 default: while the first child is gated inside its model
// call, no second child may start. Mutating golem to pass MaxConcurrent>1 makes
// the second child start into the buffered channel and drives max above 1.
func TestNewDispatchTool_ChildrenRunSequentially(t *testing.T) {
	tasks := []string{"alpha", "beta"}
	caller := &gatedSerialCaller{started: make(chan string, len(tasks)), gates: map[string]chan struct{}{}}
	for _, task := range tasks {
		caller.gates[task] = make(chan struct{})
	}
	tool, err := newDispatchTool(caller, false, validDispatchAvailable(t))
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		invokeDispatch(t, tool, tasks)
	}()
	var first string
	select {
	case first = <-caller.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first child never started")
	}
	select {
	case second := <-caller.started:
		t.Fatalf("child %q started while %q was still executing (children must be sequential)", second, first)
	default:
	}
	close(caller.gates[first])
	var second string
	select {
	case second = <-caller.started:
	case <-time.After(5 * time.Second):
		t.Fatal("second child never started after the first finished")
	}
	close(caller.gates[second])
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	if got := caller.max.Load(); got != 1 {
		t.Fatalf("concurrent children high-water mark = %d, want 1", got)
	}
}
