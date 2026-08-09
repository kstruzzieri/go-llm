package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

type dispatchCaller struct {
	mu   sync.Mutex
	reqs []provider.ChatRequest
}

type cancelDispatchCaller struct {
	started chan string
	exited  chan string
}

func (c *cancelDispatchCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	task := req.Messages[len(req.Messages)-1].Content
	c.started <- task
	<-ctx.Done()
	c.exited <- task
	return agent.ModelResult{}, ctx.Err()
}

func TestDispatchCancellationStopsActiveChildrenAndSkipsQueuedChildren(t *testing.T) {
	caller := &cancelDispatchCaller{started: make(chan string, 4), exited: make(chan string, 4)}
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps: 2, Budget: agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 10_000},
		MaxTasks: 4, MaxConcurrent: 2, MaxSummaryBytes: 1024, MaxResultBytes: 4096, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := tool.Invoke(ctx, json.RawMessage(`{"tasks":["one","two","three","four"]}`))
		done <- err
	}()
	for range 2 {
		select {
		case <-caller.started:
		case <-time.After(2 * time.Second):
			t.Fatal("active child did not start")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after cancellation")
	}
	if len(caller.exited) != 2 {
		t.Fatalf("exited children = %d, want 2", len(caller.exited))
	}
	select {
	case task := <-caller.started:
		t.Fatalf("queued child %q started after cancellation", task)
	default:
	}
}

func (c *dispatchCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, req)
	c.mu.Unlock()
	task := req.Messages[len(req.Messages)-1].Content
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	return agent.ModelResult{
		Response:     provider.ChatResponse{Content: "summary: " + task, Done: true},
		RouteOutcome: outcome,
	}, nil
}

func (c *dispatchCaller) requests() []provider.ChatRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]provider.ChatRequest(nil), c.reqs...)
}

type dispatchNamedTool struct {
	name   string
	effect agent.Effect
}

func (t dispatchNamedTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: t.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t dispatchNamedTool) Effect() agent.Effect { return t.effect }
func (dispatchNamedTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "unused"}, nil
}

func TestDispatchRunsBoundedReadOnlyChildren(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "memory_search", effect: read},
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "run_command", effect: agent.Effect{Class: agent.Exec}},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
		dispatchNamedTool{name: "retrieve", effect: read},
		dispatchNamedTool{name: "dispatch", effect: agent.Effect{Class: agent.Read | agent.Network}},
	}
	caller := &dispatchCaller{}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps:        2,
		Budget:          agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 10_000},
		MaxTasks:        2,
		MaxConcurrent:   1,
		MaxSummaryBytes: 1024,
		MaxResultBytes:  4096,
		Timeout:         time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["first","second"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected error result: %s", out.Content)
	}
	if tool.Spec().Name != DispatchToolName {
		t.Fatalf("tool name = %q, want %q", tool.Spec().Name, DispatchToolName)
	}
	effect := tool.Effect()
	if effect.Class != agent.Read|agent.Network || effect.Approval != agent.ApprovalNever || effect.Timeout != time.Minute || effect.OutputCap != 4096 {
		t.Fatalf("effect = %+v", effect)
	}

	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out.Content)
	}
	if len(envelope.Results) != 2 {
		t.Fatalf("results = %+v, want 2", envelope.Results)
	}
	for i, task := range []string{"first", "second"} {
		got := envelope.Results[i]
		if got.Summary != "summary: "+task || got.StopReason != agent.Completed.String() || got.Model != "local/fast" || got.Error != "" {
			t.Fatalf("result %d = %+v", i, got)
		}
	}

	wantTools := []string{"read_file", "search", "glob", "list", "retrieve"}
	reqs := caller.requests()
	if len(reqs) != 2 {
		t.Fatalf("child requests = %d, want 2", len(reqs))
	}
	for i, req := range reqs {
		gotTools := make([]string, len(req.Tools))
		for j, spec := range req.Tools {
			gotTools[j] = spec.Function.Name
		}
		if !slices.Equal(gotTools, wantTools) {
			t.Fatalf("child %d tools = %v, want %v", i, gotTools, wantTools)
		}
		if req.Options.NumPredict != 123 {
			t.Fatalf("child %d NumPredict = %d, want 123", i, req.Options.NumPredict)
		}
		if req.Messages[0].Role != "system" || req.Messages[0].Content != dispatchSystemPrompt || req.Messages[len(req.Messages)-1].Role != "user" {
			t.Fatalf("child %d messages = %+v", i, req.Messages)
		}
	}
}

func TestNewDispatchAppliesConservativeDefaults(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	got := tool.limits
	if got.MaxSteps != 6 || got.Budget.InputCeiling != agent.DefaultInputCeiling || got.Budget.OutputReserve != 1024 || got.Budget.TotalTokens != 32_768 {
		t.Fatalf("default child budget = %+v", got)
	}
	if got.MaxTasks != 4 || got.MaxConcurrent != 1 || got.MaxSummaryBytes != 8*1024 || got.MaxResultBytes != 64*1024 || got.Timeout != 5*time.Minute {
		t.Fatalf("default dispatch limits = %+v", got)
	}
}

func TestNewDispatchRejectsUnusableResultCap(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	for _, limit := range []int{1, 110} {
		_, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 1, MaxResultBytes: limit})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("max result bytes %d cannot hold required metadata", limit)) {
			t.Fatalf("NewDispatch(%d) error = %v", limit, err)
		}
	}
}

func TestNewDispatchRejectsUnusableSummaryCap(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	_, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{MaxSummaryBytes: 1})
	if err == nil || !strings.Contains(err.Error(), "max summary bytes 1 cannot hold truncation marker") {
		t.Fatalf("NewDispatch error = %v", err)
	}
}

func TestNewDispatchReportsNegativeLimitFieldAndValue(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tests := map[string]DispatchLimits{
		"max steps":         {MaxSteps: -1},
		"input ceiling":     {Budget: agent.Budget{InputCeiling: -1}},
		"output reserve":    {Budget: agent.Budget{OutputReserve: -1}},
		"total tokens":      {Budget: agent.Budget{TotalTokens: -1}},
		"max tasks":         {MaxTasks: -1},
		"max concurrent":    {MaxConcurrent: -1},
		"max summary bytes": {MaxSummaryBytes: -1},
		"max result bytes":  {MaxResultBytes: -1},
		"timeout":           {Timeout: -1},
	}
	for field, limits := range tests {
		t.Run(field, func(t *testing.T) {
			_, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, limits)
			if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "-1") {
				t.Fatalf("normalizeDispatchLimits error = %v", err)
			}
		})
	}
}

type repeatingParentDispatchCaller struct {
	calls atomic.Int32
}

func (c *repeatingParentDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	n := c.calls.Add(1)
	if n > DefaultDispatchCallsPerRun+1 {
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
	}
	task := "task-" + strconv.Itoa(int(n))
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "dispatch-" + task,
		Function: provider.ToolCallFunction{
			Name: DispatchToolName, Arguments: json.RawMessage(`{"tasks":["` + task + `"]}`),
		},
	}}}}, nil
}

func TestParentRunCapsRepeatedDispatchFanout(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	childCaller := &dispatchCaller{}
	dispatch, err := NewDispatch(childCaller, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	parent := agent.New(&repeatingParentDispatchCaller{}, agent.ContextManager{}, agent.WithToolInvocationLimit(agent.ToolInvocationLimit{
		Tool: DispatchToolName, Max: DefaultDispatchCallsPerRun,
	}))
	result, err := parent.Run(context.Background(), agent.Request{
		Goal:     "investigate",
		Tools:    []agent.Tool{dispatch},
		MaxSteps: DefaultDispatchCallsPerRun + 2,
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.StopReason != agent.Completed {
		t.Fatalf("stop reason = %v, want completed", result.StopReason)
	}
	if len(childCaller.requests()) != DefaultDispatchCallsPerRun {
		t.Fatalf("child runs = %d, want %d", len(childCaller.requests()), DefaultDispatchCallsPerRun)
	}
	if len(result.ToolCalls) != DefaultDispatchCallsPerRun+1 {
		t.Fatalf("dispatch calls = %d, want %d", len(result.ToolCalls), DefaultDispatchCallsPerRun+1)
	}
	last := result.ToolCalls[len(result.ToolCalls)-1]
	if last.Invoked || !last.IsError {
		t.Fatalf("capped call = %+v, want synthetic error without invocation", last)
	}
}

func TestNewDispatchRejectsMutatingCanonicalTool(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	if _, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("NewDispatch error = %v", err)
	}
}

func TestDispatchRejectsOversizedTaskBeforeModelCall(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	caller := &dispatchCaller{}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	raw, _ := json.Marshal(dispatchArgs{Tasks: []string{strings.Repeat("x", maxDispatchTaskBytes+1)}})
	out, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError || !strings.Contains(out.Content, "at most 8192 bytes") {
		t.Fatalf("result = %+v", out)
	}
	if len(caller.requests()) != 0 {
		t.Fatal("oversized task reached child model")
	}
}

func TestDispatchRejectsOversizedArgumentsBeforeModelCall(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	caller := &dispatchCaller{}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	raw := json.RawMessage(`{"padding":"` + strings.Repeat("x", maxDispatchArgsBytes) + `","tasks":["one"]}`)
	out, err := tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError || !strings.Contains(out.Content, "arguments must be at most") {
		t.Fatalf("result = %+v", out)
	}
	if len(caller.requests()) != 0 {
		t.Fatal("oversized arguments reached child model")
	}
}

func TestDispatchAllowsWorstCaseEscapedTaskWithinDecodedLimit(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	caller := &dispatchCaller{}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	raw := json.RawMessage(`{"tasks":["` + strings.Repeat(`\u0000`, maxDispatchTaskBytes) + `"]}`)
	out, err := tool.Invoke(context.Background(), raw)
	if err != nil || out.IsError {
		t.Fatalf("Invoke = %+v, %v", out, err)
	}
	if len(caller.requests()) != 1 {
		t.Fatalf("child model calls = %d, want 1", len(caller.requests()))
	}
}

type staticDispatchCaller struct {
	content string
}

func (c staticDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		Response: provider.ChatResponse{Content: c.content, Done: true},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "local", Model: "fast"},
		},
	}, nil
}

type malformedModelDispatchCaller struct{}

func (malformedModelDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		Response: provider.ChatResponse{Content: "summary", Done: true},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "local", Model: string([]byte{'m', 0xff})},
		},
	}, nil
}

func TestDispatchMarksInvalidModelUTF8AsTruncated(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(malformedModelDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !out.Truncated || !envelope.Results[0].Truncated || !utf8.ValidString(envelope.Results[0].Model) {
		t.Fatalf("result = %+v; tool truncated = %v", envelope.Results[0], out.Truncated)
	}
}

type emptyDispatchCaller struct{}

func (emptyDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		Response: provider.ChatResponse{Content: " \t", Done: true},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "local", Model: "fast"},
		},
	}, nil
}

func TestDispatchTreatsMissingChildSummaryAsError(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(emptyDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := envelope.Results[0]
	if !out.IsError || got.Summary != "" || got.Error != "child produced no summary before completed" {
		t.Fatalf("result = %+v; tool error = %v", got, out.IsError)
	}
}

type loopingDispatchCaller struct {
	usage provider.Usage
	calls atomic.Int32
}

type partialErrorDispatchCaller struct {
	calls atomic.Int32
}

type firstCallErrorDispatchCaller struct{}

func (firstCallErrorDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}},
	}, errors.New("child stream failed")
}

type partialStreamErrorDispatchCaller struct{}

func (partialStreamErrorDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		Response:     provider.ChatResponse{Content: "partial synthesis"},
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}},
	}, errors.New("child stream failed")
}

func TestDispatchPreservesPartialStreamSummaryOnError(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(partialStreamErrorDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := envelope.Results[0]
	if !out.IsError || got.Summary != "Partial result before error: partial synthesis" || got.StopReason != "error" || got.Model != "local/fast" || !strings.Contains(got.Error, "child stream failed") {
		t.Fatalf("result = %+v; tool error = %v", got, out.IsError)
	}
}

type blankErrorDispatchCaller struct{}

func (blankErrorDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}},
	}, errors.New("")
}

func TestDispatchSurfacesBlankChildError(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(blankErrorDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got := envelope.Results[0]; !out.IsError || got.Error != "child failed without an error message" {
		t.Fatalf("result = %+v; tool error = %v", got, out.IsError)
	}
}

type largeErrorDispatchCaller struct{}

func (largeErrorDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}},
	}, errors.New(string(bytes.Repeat([]byte{0xff}, 10_000)))
}

func TestDispatchBoundsErrorWithoutErasingModel(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(largeErrorDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 1, MaxResultBytes: 256})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := envelope.Results[0]
	if len(out.Content) > 256 || got.Model != "local/fast" || got.Error == "" || !strings.HasSuffix(got.Error, "… [error details truncated]") || !utf8.ValidString(got.Error) || !got.Truncated || !out.Truncated {
		t.Fatalf("result = %+v; tool truncated = %v", got, out.Truncated)
	}
}

type longModelDispatchCaller struct{}

func (longModelDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{
		Response: provider.ChatResponse{Content: "summary", Done: true},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "local", Model: strings.Repeat("m", 300)},
		},
	}, nil
}

func TestDispatchRejectsResultCapThatWouldTruncateModel(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(longModelDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 1, MaxResultBytes: 256})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	_, err = tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err == nil || !strings.Contains(err.Error(), "child 1: model identity exceeds 256-byte cap") {
		t.Fatalf("Invoke error = %v", err)
	}
}

func TestDispatchPreservesModelIdentityOnFirstCallError(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(firstCallErrorDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := envelope.Results[0]
	if !out.IsError || got.Model != "local/fast" || got.StopReason != "error" || !strings.Contains(got.Error, "child stream failed") || strings.Contains(got.Error, "identity unavailable") {
		t.Fatalf("result = %+v; tool error = %v", got, out.IsError)
	}
}

func (c *partialErrorDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.calls.Add(1) > 1 {
		return agent.ModelResult{}, errors.New("child model failed")
	}
	return agent.ModelResult{
		Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "read-1", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{}`)},
		}}},
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}},
	}, nil
}

func TestDispatchPreservesKnownModelIdentityOnChildError(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(&partialErrorDispatchCaller{}, agent.ContextManager{}, available, DispatchLimits{})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError {
		t.Fatalf("ToolResult.IsError = false: %s", out.Content)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	got := envelope.Results[0]
	if got.Model != "local/fast" || got.StopReason != "error" || got.Summary != "Partial result before error: unused" || !strings.Contains(got.Error, "child model failed") {
		t.Fatalf("result = %+v", got)
	}
}

func (c *loopingDispatchCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	n := c.calls.Add(1)
	return agent.ModelResult{
		Response: provider.ChatResponse{
			ToolCalls: []provider.ToolCall{{
				ID:       "read-" + strconv.Itoa(int(n)),
				Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{}`)},
			}},
			Usage: c.usage,
		},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "local", Model: "fast"},
		},
	}, nil
}

func TestDispatchReportsPerChildBudgetStopAndModel(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	caller := &loopingDispatchCaller{usage: provider.Usage{TotalTokens: 10}}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps: 3, Budget: agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 10},
		MaxTasks: 2, MaxConcurrent: 2, MaxSummaryBytes: 1024, MaxResultBytes: 4096, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one","two"]}`))
	if err != nil || out.IsError {
		t.Fatalf("Invoke = %+v, %v", out, err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	for i, result := range envelope.Results {
		if result.StopReason != agent.BudgetReached.String() || result.Model != "local/fast" || result.Summary != "Partial result before budget_reached: unused" || result.Error != "" {
			t.Fatalf("result %d = %+v", i, result)
		}
	}
	if caller.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want one independently budgeted call per child", caller.calls.Load())
	}
}

func TestDispatchReportsChildStepCap(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	caller := &loopingDispatchCaller{usage: provider.Usage{TotalTokens: 1}}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps: 2, Budget: agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 100},
		MaxTasks: 1, MaxConcurrent: 1, MaxSummaryBytes: 1024, MaxResultBytes: 4096, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
	if err != nil || out.IsError {
		t.Fatalf("Invoke = %+v, %v", out, err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got := envelope.Results[0]; got.StopReason != agent.StepCapReached.String() || got.Model != "local/fast" || got.Summary != "Partial result before step_cap_reached: unused" || got.Error != "" {
		t.Fatalf("result = %+v", got)
	}
	if caller.calls.Load() != 2 {
		t.Fatalf("model calls = %d, want max steps 2", caller.calls.Load())
	}
}

func TestDispatchReturnsValidJSONWithinConfiguredByteCaps(t *testing.T) {
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(staticDispatchCaller{content: strings.Repeat("界", 200)}, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps: 2, Budget: agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 10_000},
		MaxTasks: 2, MaxConcurrent: 2, MaxSummaryBytes: 100, MaxResultBytes: 360, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one","two"]}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(out.Content) > 360 {
		t.Fatalf("result bytes = %d, want <= 360", len(out.Content))
	}
	if !out.Truncated {
		t.Fatal("ToolResult.Truncated = false, want true")
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, out.Content)
	}
	for i, result := range envelope.Results {
		if result.Summary == "" || len(result.Summary) > 100 || !utf8.ValidString(result.Summary) || !result.Truncated || result.Model != "local/fast" || result.StopReason != agent.Completed.String() {
			t.Fatalf("result %d = %+v", i, result)
		}
	}
}

type gatedDispatchCaller struct {
	started chan string
	gates   map[string]chan struct{}
	active  atomic.Int32
	max     atomic.Int32
}

func TestDispatchDeadlineSalvagesCompletedChildEvidence(t *testing.T) {
	caller := &gatedDispatchCaller{
		started: make(chan string, 2),
		gates: map[string]chan struct{}{
			"one": make(chan struct{}),
			"two": make(chan struct{}),
		},
	}
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 2, MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type invokeResult struct {
		out agent.ToolResult
		err error
	}
	done := make(chan invokeResult, 1)
	go func() {
		out, err := tool.Invoke(ctx, json.RawMessage(`{"tasks":["one","two"]}`))
		done <- invokeResult{out: out, err: err}
	}()
	for range 2 {
		select {
		case <-caller.started:
		case <-time.After(2 * time.Second):
			t.Fatal("children did not start")
		}
	}
	close(caller.gates["one"]) // "one" completes; "two" blocks until the deadline

	var got invokeResult
	select {
	case got = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not return after its deadline")
	}
	if got.err != nil {
		t.Fatalf("Invoke error = %v, want salvaged envelope", got.err)
	}
	if !got.out.IsError {
		t.Fatalf("ToolResult.IsError = false: %s", got.out.Content)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(got.out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	first := envelope.Results[0]
	if first.Summary != "summary: one" || first.StopReason != agent.Completed.String() || first.Model != "local/fast" || first.Error != "" {
		t.Fatalf("completed child result = %+v", first)
	}
	second := envelope.Results[1]
	if second.StopReason != "error" || !strings.Contains(second.Error, "context deadline exceeded") || !strings.Contains(second.Error, "child model identity unavailable") {
		t.Fatalf("timed-out child result = %+v", second)
	}
}

func TestDispatchDeadlineReportsQueuedChildNotStarted(t *testing.T) {
	caller := &gatedDispatchCaller{
		started: make(chan string, 2),
		gates: map[string]chan struct{}{
			"one": make(chan struct{}),
			"two": make(chan struct{}),
		},
	}
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 2, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := tool.Invoke(ctx, json.RawMessage(`{"tasks":["one","two"]}`))
	if err != nil {
		t.Fatalf("Invoke error = %v, want salvaged envelope", err)
	}
	if !out.IsError {
		t.Fatalf("ToolResult.IsError = false: %s", out.Content)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	// MaxConcurrent 1: either child may win the single slot, so assert set-wise.
	var timedOut, notStarted int
	for _, result := range envelope.Results {
		switch {
		case strings.Contains(result.Error, "child not started before dispatch timeout"):
			notStarted++
		case strings.Contains(result.Error, "context deadline exceeded"):
			timedOut++
		default:
			t.Fatalf("unexpected result = %+v", result)
		}
		if result.StopReason != "error" {
			t.Fatalf("stop reason = %q, want error", result.StopReason)
		}
	}
	if timedOut != 1 || notStarted != 1 {
		t.Fatalf("timedOut = %d, notStarted = %d, want 1 and 1\n%s", timedOut, notStarted, out.Content)
	}
}

func TestMarshalDispatchEnvelopeShrinksLongestFieldAcrossSummariesAndErrors(t *testing.T) {
	envelope := &dispatchEnvelope{Results: []dispatchResult{
		{Summary: "short but useful summary", StopReason: "completed", Model: "local/fast"},
		{StopReason: "error", Model: "local/fast", Error: strings.Repeat("e", 2000)},
	}}
	content, truncated, err := marshalDispatchEnvelope(envelope, 512)
	if err != nil {
		t.Fatalf("marshalDispatchEnvelope: %v", err)
	}
	if !truncated || len(content) > 512 {
		t.Fatalf("truncated = %v, bytes = %d, want truncated within 512", truncated, len(content))
	}
	var got dispatchEnvelope
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Results[0].Summary != "short but useful summary" || got.Results[0].Truncated {
		t.Fatalf("healthy summary was sacrificed to a bloated error: %+v", got.Results[0])
	}
	if !strings.HasSuffix(got.Results[1].Error, dispatchErrorTruncated) || !got.Results[1].Truncated {
		t.Fatalf("bloated error was not truncated: %+v", got.Results[1])
	}
}

func TestDispatchSharesConcurrencyLimitAcrossInvocations(t *testing.T) {
	caller := &gatedDispatchCaller{
		started: make(chan string, 2),
		gates: map[string]chan struct{}{
			"one": make(chan struct{}),
			"two": make(chan struct{}),
		},
	}
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{MaxTasks: 1, MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one"]}`))
		firstDone <- err
	}()
	select {
	case task := <-caller.started:
		if task != "one" {
			t.Fatalf("first task = %q", task)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first child did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	type invokeResult struct {
		out agent.ToolResult
		err error
	}
	secondDone := make(chan invokeResult, 1)
	go func() {
		out, err := tool.Invoke(ctx, json.RawMessage(`{"tasks":["two"]}`))
		secondDone <- invokeResult{out: out, err: err}
	}()
	select {
	case task := <-caller.started:
		close(caller.gates["one"])
		<-firstDone
		t.Fatalf("child %q bypassed shared concurrency limit", task)
	case got := <-secondDone:
		if got.err != nil || !got.out.IsError || !strings.Contains(got.out.Content, "child not started before dispatch timeout") {
			t.Fatalf("second Invoke = %+v, %v, want salvaged not-started envelope", got.out, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Invoke did not honor its deadline while queued")
	}
	close(caller.gates["one"])
	if err := <-firstDone; err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	if caller.max.Load() != 1 {
		t.Fatalf("max active children = %d, want 1", caller.max.Load())
	}
}

func (c *gatedDispatchCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	task := req.Messages[len(req.Messages)-1].Content
	active := c.active.Add(1)
	defer c.active.Add(-1)
	for {
		old := c.max.Load()
		if active <= old || c.max.CompareAndSwap(old, active) {
			break
		}
	}
	c.started <- task
	select {
	case <-c.gates[task]:
		outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
		return agent.ModelResult{Response: provider.ChatResponse{Content: "summary: " + task, Done: true}, RouteOutcome: outcome}, nil
	case <-ctx.Done():
		return agent.ModelResult{}, ctx.Err()
	}
}

func TestDispatchBoundsConcurrencyAndPreservesResultOrder(t *testing.T) {
	tasks := []string{"one", "two", "three", "four"}
	caller := &gatedDispatchCaller{started: make(chan string, len(tasks)), gates: make(map[string]chan struct{}, len(tasks))}
	for _, task := range tasks {
		caller.gates[task] = make(chan struct{})
	}
	read := agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
	available := []agent.Tool{
		dispatchNamedTool{name: "read_file", effect: read},
		dispatchNamedTool{name: "search", effect: read},
		dispatchNamedTool{name: "glob", effect: read},
		dispatchNamedTool{name: "list", effect: read},
	}
	tool, err := NewDispatch(caller, agent.ContextManager{}, available, DispatchLimits{
		MaxSteps: 2, Budget: agent.Budget{InputCeiling: 4096, OutputReserve: 123, TotalTokens: 10_000},
		MaxTasks: 4, MaxConcurrent: 2, MaxSummaryBytes: 1024, MaxResultBytes: 4096, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewDispatch: %v", err)
	}
	raw, _ := json.Marshal(dispatchArgs{Tasks: tasks})
	type invokeResult struct {
		out agent.ToolResult
		err error
	}
	done := make(chan invokeResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		out, err := tool.Invoke(ctx, raw)
		done <- invokeResult{out: out, err: err}
	}()

	wave := make([]string, 0, 2)
	for range 2 {
		select {
		case task := <-caller.started:
			wave = append(wave, task)
		case <-time.After(2 * time.Second):
			t.Fatal("two children did not start concurrently")
		}
	}
	select {
	case task := <-caller.started:
		t.Fatalf("child %q started above concurrency limit", task)
	default:
	}
	close(caller.gates[wave[1]])
	close(caller.gates[wave[0]])
	for range 2 {
		select {
		case task := <-caller.started:
			close(caller.gates[task])
		case <-time.After(2 * time.Second):
			t.Fatal("queued child did not start after capacity became available")
		}
	}

	var got invokeResult
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	if got.err != nil || got.out.IsError {
		t.Fatalf("Invoke = %+v, %v", got.out, got.err)
	}
	if caller.max.Load() != 2 {
		t.Fatalf("maximum active children = %d, want 2", caller.max.Load())
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(got.out.Content), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	for i, task := range tasks {
		if envelope.Results[i].Summary != "summary: "+task {
			t.Fatalf("result %d = %+v, want task %q", i, envelope.Results[i], task)
		}
	}
}
