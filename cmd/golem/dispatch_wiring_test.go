package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// dispatchOneShotHarness spins up a fake openai-compat backend that captures
// every /v1/chat/completions request body and answers a one-chunk stream, plus
// a models.json pointing golem at it. Modeled on writeRunLifecycleConfig.
func dispatchOneShotHarness(t *testing.T) (configPath, root string, chatBodies func() []string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"},{"id":"weak-model"}]}`)
		case "/v1/chat/completions":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read chat body: %v", err)
			}
			mu.Lock()
			bodies = append(bodies, string(body))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{"content":"final answer"}}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	root = t.TempDir()
	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"test": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}},
  "models": {
    "agent": {"name": "agent-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "weak": {"name": "weak-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream"]}
  },
  "defaults": {"agent": "agent"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// TestRunOneShot_DispatchRegistration proves the actual main.go wiring: with
// -dispatch the model-facing request carries the dispatch tool schema and the
// system fragment, the startup notice names the parent-chain head, and the run
// completes (so the invocation-limit option did not fail fast); without the
// flag none of that appears.
func TestRunOneShot_DispatchRegistration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dispatch bool
	}{
		{"enabled", true},
		{"disabled control", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, root, chatBodies := dispatchOneShotHarness(t)
			args := []string{"-config", configPath, "-root", root, "-p", "say done",
				"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context"}
			if tc.dispatch {
				args = append(args, "-dispatch")
			}
			stdin, stdout, stderr := runTestFiles(t)
			if err := run(args, stdin, stdout, stderr); err != nil {
				t.Fatalf("run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			bodies := chatBodies()
			if len(bodies) != 1 {
				t.Fatalf("chat requests = %d, want exactly 1 (the parent turn)", len(bodies))
			}
			wireHasTool := strings.Contains(bodies[0], `"name":"dispatch"`)
			wireHasFragment := strings.Contains(bodies[0], "you may call dispatch")
			notice := strings.Contains(readRunTestFile(t, stderr), "dispatch: enabled -> test/agent-model")
			if tc.dispatch {
				if !wireHasTool {
					t.Fatalf("dispatch tool schema missing from the model request:\n%s", bodies[0])
				}
				if !wireHasFragment {
					t.Fatalf("dispatch system fragment missing from the model request:\n%s", bodies[0])
				}
				if !notice {
					t.Fatalf("startup notice missing:\n%s", readRunTestFile(t, stderr))
				}
			} else {
				if wireHasTool || wireHasFragment || notice {
					t.Fatalf("dispatch artifacts leaked into a default run: tool=%v fragment=%v notice=%v",
						wireHasTool, wireHasFragment, notice)
				}
			}
			if !strings.Contains(readRunTestFile(t, stdout), "final answer") {
				t.Fatalf("one-shot answer missing from stdout")
			}
		})
	}
}

// TestRunOneShot_DispatchUsesConfiguredSlots covers the production composition
// in main: the config-derived Router capacity must reach dispatch's governor.
// The first two child HTTP requests stay blocked while the test proves a third
// cannot start; a static-serial wiring regression instead times out waiting for
// the second child.
func TestRunOneShot_DispatchUsesConfiguredSlots(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	tasks := []string{"alpha", "beta", "gamma", "delta"}
	started := make(chan string, len(tasks))
	release := make(chan struct{})
	var releaseOnce sync.Once

	writeSSE := func(w http.ResponseWriter, chunks ...string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
		case "/v1/chat/completions":
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			child, toolResult, task := false, false, ""
			for _, message := range req.Messages {
				child = child || message.Role == "system" && strings.Contains(message.Content, "read-only exploration subagent")
				toolResult = toolResult || message.Role == "tool"
				if message.Role == "user" {
					task = message.Content
				}
			}
			switch {
			case child:
				started <- task
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				writeSSE(w,
					fmt.Sprintf(`{"model":"agent-model","choices":[{"delta":{"content":%q}}]}`, "done "+task),
					`{"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				)
			case toolResult:
				writeSSE(w,
					`{"model":"agent-model","choices":[{"delta":{"content":"parent done"}}]}`,
					`{"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				)
			default:
				writeSSE(w,
					`{"model":"agent-model","choices":[{"delta":{"tool_calls":[{"index":0,"id":"dispatch-1","type":"function","function":{"name":"dispatch","arguments":"{\"tasks\":[\"alpha\",\"beta\",\"gamma\",\"delta\"]}"}}]}}]}`,
					`{"model":"agent-model","choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
				)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {"test": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s", "slot_discovery": true}},
  "models": {
    "agent": {"name": "agent-model", "provider": "test", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"], "slots": 2}
  },
  "defaults": {"agent": "agent"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	done := make(chan error, 1)
	go func() {
		done <- run([]string{"-config", configPath, "-root", root, "-p", "fan out",
			"-dispatch", "-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context"}, stdin, stdout, stderr)
	}()

	startedCount := 0
	for range 2 {
		select {
		case <-started:
			startedCount++
		case err := <-done:
			t.Fatalf("run ended before governed children started: %v\nstdout:\n%s\nstderr:\n%s", err,
				readRunTestFile(t, stdout), readRunTestFile(t, stderr))
		case <-time.After(5 * time.Second):
			t.Fatalf("two governed children did not start concurrently through run wiring: started=%d", startedCount)
		}
	}
	select {
	case task := <-started:
		t.Fatalf("child %q exceeded configured slot fan-out 2", task)
	case <-time.After(200 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for startedCount < len(tasks) {
		select {
		case <-started:
			startedCount++
		case <-time.After(5 * time.Second):
			t.Fatalf("started children = %d, want %d", startedCount, len(tasks))
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("one-shot dispatch run did not finish")
	}
	if !strings.Contains(readRunTestFile(t, stdout), "parent done") {
		t.Fatalf("one-shot answer missing from stdout: %q", readRunTestFile(t, stdout))
	}
}

// TestRunOneShot_DispatchRolePreflightGate proves an explicit -dispatch-role
// whose chain has no tool-capable model fails at STARTUP, before any model
// call: children always carry the file tools, so a chain without tool_call can
// only ever fail at invocation time otherwise.
func TestRunOneShot_DispatchRolePreflightGate(t *testing.T) {
	configPath, root, chatBodies := dispatchOneShotHarness(t)
	stdin, stdout, stderr := runTestFiles(t)
	err := run([]string{"-config", configPath, "-root", root, "-p", "say done",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context",
		"-dispatch", "-dispatch-role", "weak"}, stdin, stdout, stderr)
	if err == nil {
		t.Fatalf("run must fail startup preflight for a non-tool-capable dispatch chain; stdout:\n%s", readRunTestFile(t, stdout))
	}
	if !strings.Contains(err.Error(), "tool-capability preflight") || !strings.Contains(err.Error(), "weak") {
		t.Fatalf("error must name the capability preflight and the failing chain entry: %v", err)
	}
	if got := chatBodies(); len(got) != 0 {
		t.Fatalf("no model call may happen after a failed dispatch preflight, got %d", len(got))
	}
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
			orch := newOrchestratorFactory(caller, flags{dispatch: tc.dispatch}, nil)()
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

// specRecordingCaller records the tool specs and generation cap of every child
// chat request and answers immediately, ending each child run in one step.
type specRecordingCaller struct {
	mu          sync.Mutex
	tools       [][]string
	numPredicts []int
}

func (c *specRecordingCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}
	c.mu.Lock()
	c.tools = append(c.tools, names)
	c.numPredicts = append(c.numPredicts, req.Options.NumPredict)
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

// TestNewDispatchTool_TimeoutCoversAllSequentialTasks pins the golem-side
// dispatch invocation ceiling: the library's 5m default bounds the WHOLE
// invocation, and a live two-task smoke on gemma4:31b measured task 2 starving
// behind task 1 (single model calls ran 76-347s). Golem therefore budgets the
// library's per-task ceiling times the 4-task maximum.
func TestNewDispatchTool_TimeoutCoversAllSequentialTasks(t *testing.T) {
	tool, err := newDispatchTool(&specRecordingCaller{}, false, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, validDispatchAvailable(t))
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	if got := tool.Effect().Timeout; got != 20*time.Minute {
		t.Fatalf("dispatch invocation timeout = %v, want 20m (5m per task x 4 max tasks)", got)
	}
}

// TestNewDispatchTool_TinyCeilingFailsLoudly pins the fail-fast for degenerate
// budgets: a resolved ceiling at or below the library's default 1024-token
// child output reserve cannot host a child run, and dispatch must refuse at
// STARTUP with the library's reserve-vs-ceiling error rather than let every
// child fail at invocation time. A mutation that stops threading the budget
// also flips this test (the library default ceiling would pass validation).
func TestNewDispatchTool_TinyCeilingFailsLoudly(t *testing.T) {
	_, err := newDispatchTool(&specRecordingCaller{}, false, agent.Budget{InputCeiling: 500}, dispatchFanout{maxConcurrent: 1}, nil, validDispatchAvailable(t))
	if err == nil {
		t.Fatal("a 500-token ceiling cannot hold the default child output reserve; construction must fail loudly")
	}
	if !strings.Contains(err.Error(), "output reserve") {
		t.Fatalf("wrong failure category: %v", err)
	}
}

// TestNewDispatchTool_ChildBudgetThreadsThroughRuns proves the budget golem
// passes reaches child model calls: Budget.OutputReserve becomes the child
// request's NumPredict (the library's documented override), so a mutation that
// drops the Budget from the dispatch limits regresses to the library default
// reserve and fails this test.
func TestNewDispatchTool_ChildBudgetThreadsThroughRuns(t *testing.T) {
	caller := &specRecordingCaller{}
	tool, err := newDispatchTool(caller, false, agent.Budget{InputCeiling: 9000, OutputReserve: 777}, dispatchFanout{maxConcurrent: 1}, nil, validDispatchAvailable(t))
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	invokeDispatch(t, tool, []string{"inspect"})
	if len(caller.numPredicts) != 1 || caller.numPredicts[0] != 777 {
		t.Fatalf("child NumPredict = %v, want [777] (Budget.OutputReserve must reach child calls)", caller.numPredicts)
	}
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
			tool, err := newDispatchTool(caller, false, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, available)
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
	tool, err := newDispatchTool(&retrieveCallingCaller{}, false, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, append(validDispatchAvailable(t), sentinel))
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
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}
	return agent.ModelResult{Response: resp, RouteOutcome: outcome}, nil
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
	tool, err := newDispatchTool(caller, false, agent.Budget{}, dispatchFanout{maxConcurrent: 1}, nil, validDispatchAvailable(t))
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	// Fatalf is only legal on the test goroutine, so the Invoke goroutine
	// reports back over a channel instead of using invokeDispatch.
	done := make(chan error, 1)
	go func() {
		raw, err := json.Marshal(map[string][]string{"tasks": tasks})
		if err != nil {
			done <- err
			return
		}
		_, err = tool.Invoke(context.Background(), raw)
		done <- err
	}()
	var first string
	select {
	case first = <-caller.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first child never started")
	}
	// Absence proof with a bounded observation window, not an instant default:
	// while the first child is still gated inside its model call, a second
	// start under a MaxConcurrent>1 mutation lands here well within the
	// window, so the mutation is caught before the gate release below.
	select {
	case second := <-caller.started:
		t.Fatalf("child %q started while %q was still executing (children must be sequential)", second, first)
	case <-time.After(200 * time.Millisecond):
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
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch Invoke: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	if got := caller.max.Load(); got != 1 {
		t.Fatalf("concurrent children high-water mark = %d, want 1", got)
	}
}

// TestNewDispatchTool_ChildrenUseGovernedFanout proves newDispatchTool threads
// the fan-out policy into DispatchLimits: with a governor returning 2 under a
// ceiling of 4, exactly two children run per wave and the governor is read
// once for the whole invocation.
func TestNewDispatchTool_ChildrenUseGovernedFanout(t *testing.T) {
	tasks := []string{"alpha", "beta", "gamma", "delta"}
	caller := &gatedSerialCaller{
		started: make(chan string, len(tasks)),
		gates:   map[string]chan struct{}{},
	}
	for _, task := range tasks {
		caller.gates[task] = make(chan struct{})
	}
	var governorReads atomic.Int32
	tool, err := newDispatchTool(
		caller, false, agent.Budget{},
		dispatchFanout{
			maxConcurrent: 4,
			governor: func() int {
				governorReads.Add(1)
				return 2
			},
		}, nil,
		validDispatchAvailable(t),
	)
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	type invokeResult struct {
		out agent.ToolResult
		err error
	}
	done := make(chan invokeResult, 1)
	go func() {
		raw, err := json.Marshal(map[string][]string{"tasks": tasks})
		if err != nil {
			done <- invokeResult{err: err}
			return
		}
		out, err := tool.Invoke(context.Background(), raw)
		done <- invokeResult{out: out, err: err}
	}()
	recvWave := func() []string {
		t.Helper()
		wave := make([]string, 0, 2)
		for range 2 {
			select {
			case task := <-caller.started:
				wave = append(wave, task)
			case <-time.After(5 * time.Second):
				t.Fatal("governed child wave did not start")
			}
		}
		return wave
	}
	firstWave := recvWave()
	// This is an absence/liveness watchdog, not a performance assertion: a
	// dropped governor admits the third child while the first wave is gated.
	select {
	case task := <-caller.started:
		t.Fatalf("child %q exceeded governed fan-out 2", task)
	case <-time.After(200 * time.Millisecond):
	}
	for _, task := range firstWave {
		close(caller.gates[task])
	}
	secondWave := recvWave()
	for _, task := range secondWave {
		close(caller.gates[task])
	}
	select {
	case got := <-done:
		if got.err != nil || got.out.IsError {
			t.Fatalf("dispatch Invoke = %+v, %v", got.out, got.err)
		}
		var envelope dispatchTestEnvelope
		if err := json.Unmarshal([]byte(got.out.Content), &envelope); err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if len(envelope.Results) != len(tasks) {
			t.Fatalf("result count = %d, want %d", len(envelope.Results), len(tasks))
		}
		for i, result := range envelope.Results {
			if result.Model != "local/fast" {
				t.Fatalf("result %d model = %q, want local/fast", i, result.Model)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
	if got := caller.max.Load(); got != 2 {
		t.Fatalf("concurrent children = %d, want 2", got)
	}
	if got := governorReads.Load(); got != 1 {
		t.Fatalf("governor reads = %d, want 1", got)
	}
}

// TestNewDispatchTool_OutOfOrderCompletionNoticeNamesTask proves the notice
// number identifies the input task instead of masquerading as a completion
// count when parallel children finish out of order.
func TestNewDispatchTool_OutOfOrderCompletionNoticeNamesTask(t *testing.T) {
	oneGate := make(chan struct{})
	twoGate := make(chan struct{})
	close(twoGate)
	var releaseOne sync.Once
	t.Cleanup(func() { releaseOne.Do(func() { close(oneGate) }) })
	caller := &gatedSerialCaller{
		started: make(chan string, 2),
		gates:   map[string]chan struct{}{"one": oneGate, "two": twoGate},
	}
	notices := make(chan string, 2)
	tool, err := newDispatchTool(
		caller, false, agent.Budget{}, dispatchFanout{maxConcurrent: 2},
		func(line string) { notices <- line }, validDispatchAvailable(t),
	)
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		out, err := tool.Invoke(context.Background(), json.RawMessage(`{"tasks":["one","two"]}`))
		if err == nil && out.IsError {
			err = fmt.Errorf("dispatch returned an error result: %s", out.Content)
		}
		done <- err
	}()
	select {
	case got := <-notices:
		if want := "dispatch: task #2 finished (2 total)"; got != want {
			t.Fatalf("first completion notice = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second input task did not finish")
	}
	releaseOne.Do(func() { close(oneGate) })
	select {
	case got := <-notices:
		if want := "dispatch: task #1 finished (2 total)"; got != want {
			t.Fatalf("second completion notice = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first input task did not finish")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch Invoke: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not finish")
	}
}

// TestNewDispatchTool_CompletionNoticeUsesReboundSink proves per-child
// completion notices flow through the late-bound feedbackNotifier into
// replControl's mid-turn stderr path -- the same rebind main performs -- and
// never the stale initial sink or the renderer's stdout stream.
func TestNewDispatchTool_CompletionNoticeUsesReboundSink(t *testing.T) {
	var fallback atomic.Int32
	notifier := newFeedbackNotifier(func(string) { fallback.Add(1) })
	tool, err := newDispatchTool(
		&specRecordingCaller{}, false, agent.Budget{},
		dispatchFanout{maxConcurrent: 1}, notifier.notify,
		validDispatchAvailable(t),
	)
	if err != nil {
		t.Fatalf("newDispatchTool: %v", err)
	}
	var stdout, stderr bytes.Buffer
	ctrl := newReplControl(&stdout, &stderr, make(chan struct{}, 1), func() {})
	ctrl.enterTurn()
	notifier.set(ctrl.notice) // same late bind main performs before any turn
	invokeDispatch(t, tool, []string{"one", "two"})
	if fallback.Load() != 0 {
		t.Fatalf("completion used stale fallback sink %d time(s)", fallback.Load())
	}
	if stdout.Len() != 0 {
		t.Fatalf("mid-turn completion touched stdout: %q", stdout.String())
	}
	got := stderr.String()
	for _, want := range []string{
		"dispatch: task #1 finished (2 total)",
		"dispatch: task #2 finished (2 total)",
	} {
		if strings.Count(got, want) != 1 {
			t.Fatalf("completion notice %q count != 1 in %q", want, got)
		}
	}
}
