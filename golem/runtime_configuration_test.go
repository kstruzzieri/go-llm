package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// The tuples below are written out as independent literals rather than derived
// from each other: Budget.OutputReserve reaches the wire as Options.NumPredict,
// so "wire" values pin what the model caller actually observed for a turn.
func oldConfigOptions() provider.ModelOptions {
	return provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096}
}

func oldConfigWireOptions() provider.ModelOptions {
	return provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096, NumPredict: 321}
}

func oldConfigBudget() agent.Budget {
	return agent.Budget{InputCeiling: 8192, OutputReserve: 321}
}

func newConfigOptions() provider.ModelOptions {
	return provider.ModelOptions{ThinkEffort: "high", NumCtx: 2048}
}

func newConfigWireOptions() provider.ModelOptions {
	return provider.ModelOptions{ThinkEffort: "high", NumCtx: 2048, NumPredict: 654}
}

func newConfigBudget() agent.Budget {
	return agent.Budget{InputCeiling: 4096, OutputReserve: 654}
}

func oldConfigToolNames() []string {
	return []string{"read_file", "search", "glob", "list", "old_tool"}
}

func newConfigToolNames() []string {
	return []string{"read_file", "search", "glob", "list", "new_tool"}
}

// newConfigurationRuntime builds the OLD tuple every test in this file replaces.
func newConfigurationRuntime(t *testing.T, caller agent.ModelCaller, tools ...agent.Tool) *golem.Runtime {
	t.Helper()
	if len(tools) == 0 {
		tools = []agent.Tool{namedTool("old_tool")}
	}
	rt, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		System:       "OLD SYSTEM",
		Tools:        tools,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
		ModelOptions: oldConfigOptions(),
		Budget:       oldConfigBudget(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan error, 1)
		go func() { done <- rt.Close() }()
		_ = waitReplace(t, done)
	})
	return rt
}

// newConfiguration is the complete NEW tuple, including its own orchestrator.
func newConfiguration(caller agent.ModelCaller) golem.Configuration {
	return golem.Configuration{
		System:       "NEW SYSTEM",
		Tools:        []agent.Tool{namedTool("new_tool")},
		ModelOptions: newConfigOptions(),
		Orchestrator: agent.New(caller, agent.ContextManager{}),
		Budget:       newConfigBudget(),
	}
}

func assertBudget(t *testing.T, got, want agent.Budget) {
	t.Helper()
	if got != want {
		t.Errorf("budget = %+v, want %+v", got, want)
	}
}

// assertOldTupleIntact runs one turn and proves the complete OLD tuple —
// orchestrator, system, tools, options, and budget — is still installed.
func assertOldTupleIntact(t *testing.T, rt *golem.Runtime, old *captureCaller, runID string) {
	t.Helper()
	before := len(old.requests)
	runTurn(t, rt, runID)
	if len(old.requests) != before+1 {
		t.Fatalf("old orchestrator requests = %d, want %d", len(old.requests), before+1)
	}
	assertSnapshot(t, old.requests[before], "OLD SYSTEM", oldConfigToolNames(), oldConfigWireOptions())
	assertModelOptions(t, rt.ModelOptions(), oldConfigOptions())
	assertBudget(t, rt.Budget(), oldConfigBudget())
}

// specCountingTool records how often validation asked for its spec, so a test
// can prove an already-canceled publication never reaches Tool.Spec at all.
type specCountingTool struct{ calls *atomic.Int32 }

func (s specCountingTool) Spec() agent.ToolSpec {
	s.calls.Add(1)
	return agent.ToolSpec{Name: "counting", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (specCountingTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (specCountingTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

// cancelingSpecTool cancels the caller's context from inside Tool.Spec, so a
// publication racing its own validation installs nothing.
type cancelingSpecTool struct{ cancel context.CancelFunc }

func (c cancelingSpecTool) Spec() agent.ToolSpec {
	c.cancel()
	return agent.ToolSpec{Name: "canceling", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (cancelingSpecTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (cancelingSpecTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

// A complete replacement swaps the whole tuple at once: the new orchestrator
// serves the next turn with the new system, tools, options, and budget.
func TestReplaceConfigurationReplacesTheWholeTuple(t *testing.T) {
	old := &captureCaller{answer: "old answer"}
	rt := newConfigurationRuntime(t, old)
	next := &captureCaller{answer: "new answer"}
	if err := rt.ReplaceConfiguration(context.Background(), newConfiguration(next)); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	runTurn(t, rt, "after")
	if len(old.requests) != 0 {
		t.Fatalf("old orchestrator served %d requests after replacement, want 0", len(old.requests))
	}
	if len(next.requests) != 1 {
		t.Fatalf("new orchestrator requests = %d, want 1", len(next.requests))
	}
	assertSnapshot(t, next.requests[0], "NEW SYSTEM", newConfigToolNames(), newConfigWireOptions())
	assertModelOptions(t, rt.ModelOptions(), newConfigOptions())
	assertBudget(t, rt.Budget(), newConfigBudget())
}

// An empty System defaults exactly like New and Replace.
func TestReplaceConfigurationDefaultsEmptySystemLikeNew(t *testing.T) {
	next := &captureCaller{answer: "new answer"}
	rt := newConfigurationRuntime(t, &captureCaller{answer: "old answer"})
	cfg := newConfiguration(next)
	cfg.System = ""
	if err := rt.ReplaceConfiguration(context.Background(), cfg); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	runTurn(t, rt, "default")
	if got := shapeOf(next.requests[0]).system; got != golem.SystemPrompt(false, false) {
		t.Fatalf("system = %q, want the New default", got)
	}
}

// Mutating the caller's slices after publication changes nothing.
func TestReplaceConfigurationCopiesInputs(t *testing.T) {
	next := &captureCaller{answer: "new answer"}
	rt := newConfigurationRuntime(t, &captureCaller{answer: "old answer"})
	cfg := newConfiguration(next)
	cfg.ModelOptions = literalModelOptions()
	if err := rt.ReplaceConfiguration(context.Background(), cfg); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	cfg.Tools[0] = namedTool("swapped")
	mutateModelOptions(cfg.ModelOptions)
	want := literalModelOptions()
	assertModelOptions(t, rt.ModelOptions(), want)
	runTurn(t, rt, "copied")
	got := shapeOf(next.requests[0])
	if !hasTool(got, "new_tool") || hasTool(got, "swapped") {
		t.Fatalf("ReplaceConfiguration aliased the caller's tool slice: %+v", got)
	}
	want.NumPredict = 654
	assertModelOptions(t, next.requests[0].Options, want)
}

// Existing two- and three-argument Replace calls keep compiling and preserve
// the orchestrator and budget the last configuration published.
func TestReplacePreservesOrchestratorAndBudget(t *testing.T) {
	old := &captureCaller{answer: "old answer"}
	next := &captureCaller{answer: "new answer"}
	rt := newConfigurationRuntime(t, old)
	if err := rt.ReplaceConfiguration(context.Background(), newConfiguration(next)); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	replace := rt.Replace // inferred method values still support two arguments
	if err := replace("TWO ARG SYSTEM", []agent.Tool{namedTool("two_arg")}); err != nil {
		t.Fatalf("Replace(system, tools): %v", err)
	}
	runTurn(t, rt, "two-arg")
	if err := rt.Replace("THREE ARG SYSTEM", []agent.Tool{namedTool("three_arg")}, provider.ModelOptions{NumCtx: 999}); err != nil {
		t.Fatalf("Replace(system, tools, options): %v", err)
	}
	runTurn(t, rt, "three-arg")
	if len(old.requests) != 0 {
		t.Fatalf("old orchestrator served %d requests, want 0", len(old.requests))
	}
	if len(next.requests) != 2 {
		t.Fatalf("new orchestrator requests = %d, want 2", len(next.requests))
	}
	assertSnapshot(t, next.requests[0], "TWO ARG SYSTEM", []string{"read_file", "search", "glob", "list", "two_arg"}, newConfigWireOptions())
	assertSnapshot(t, next.requests[1], "THREE ARG SYSTEM", []string{"read_file", "search", "glob", "list", "three_arg"}, provider.ModelOptions{NumCtx: 999, NumPredict: 654})
	assertBudget(t, rt.Budget(), newConfigBudget())
}

// Budget() and ModelOptions() keep reporting the last published values after Close.
func TestReplaceConfigurationBudgetAndOptionsAfterClose(t *testing.T) {
	rt := newConfigurationRuntime(t, &captureCaller{answer: "old answer"})
	if err := rt.ReplaceConfiguration(context.Background(), newConfiguration(&captureCaller{answer: "new answer"})); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	assertBudget(t, rt.Budget(), newConfigBudget())
	assertModelOptions(t, rt.ModelOptions(), newConfigOptions())
}

// Every rejection leaves the complete old tuple installed.
func TestReplaceConfigurationValidatesAndInstallsNothing(t *testing.T) {
	old := &captureCaller{answer: "old answer"}
	rt := newConfigurationRuntime(t, old)
	next := &captureCaller{answer: "new answer"}
	for _, tc := range []struct {
		name         string
		tools        []agent.Tool
		nilOrchestra bool
		want         string
	}{
		{name: "duplicate of a runtime file tool", tools: []agent.Tool{namedTool("read_file")}, want: "duplicate tool name"},
		{name: "duplicate within the list", tools: []agent.Tool{namedTool("t"), namedTool("t")}, want: "duplicate tool name"},
		{name: "nil tool", tools: []agent.Tool{nil}, want: "nil tool"},
		{name: "empty name", tools: []agent.Tool{namedTool("")}, want: "empty name"},
		{name: "nil orchestrator", nilOrchestra: true, want: "golem: invalid request: orchestrator is required"},
	} {
		cfg := newConfiguration(next)
		if tc.tools != nil {
			cfg.Tools = tc.tools
		}
		if tc.nilOrchestra {
			cfg.Orchestrator = nil
		}
		err := rt.ReplaceConfiguration(context.Background(), cfg)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if tc.nilOrchestra && !errors.Is(err, golem.ErrInvalidRequest) {
			t.Fatalf("nil orchestrator err = %v, want ErrInvalidRequest", err)
		}
		assertOldTupleIntact(t, rt, old, tc.name)
	}
	if len(next.requests) != 0 {
		t.Fatalf("rejected orchestrator served %d requests, want 0", len(next.requests))
	}
}

// ErrClosed dominates validation errors, including a Close that completes
// while Tool.Spec is still running.
func TestReplaceConfigurationErrClosedDominatesValidation(t *testing.T) {
	rt := newConfigurationRuntime(t, &captureCaller{answer: "old answer"})
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := newConfiguration(&captureCaller{answer: "new answer"})
	cfg.Tools = []agent.Tool{namedTool("read_file")}
	cfg.Orchestrator = nil
	if err := rt.ReplaceConfiguration(context.Background(), cfg); !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed to dominate validation", err)
	}
	assertBudget(t, rt.Budget(), oldConfigBudget())
	assertModelOptions(t, rt.ModelOptions(), oldConfigOptions())
}

func TestReplaceConfigurationErrClosedWhenCloseCompletesDuringValidation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			rt := newConfigurationRuntime(t, &captureCaller{answer: "old answer"})
			tool := &blockingSpecTool{t: t, entered: make(chan struct{}), release: make(chan struct{})}
			unblock := sync.OnceFunc(func() { close(tool.release) })
			t.Cleanup(unblock)
			cfg := newConfiguration(&captureCaller{answer: "new answer"})
			cfg.Tools = []agent.Tool{tool}
			if invalid {
				cfg.Tools = append(cfg.Tools, namedTool("read_file"))
			}
			done := make(chan error, 1)
			go func() { done <- rt.ReplaceConfiguration(context.Background(), cfg) }()
			waitReplace(t, tool.entered)
			closed := make(chan error, 1)
			go func() { closed <- rt.Close() }()
			if err := waitReplace(t, closed); err != nil {
				t.Fatal(err)
			}
			unblock()
			if err := waitReplace(t, done); !errors.Is(err, golem.ErrClosed) {
				t.Errorf("err = %v, want ErrClosed after Close completed during validation", err)
			}
			assertBudget(t, rt.Budget(), oldConfigBudget())
			assertModelOptions(t, rt.ModelOptions(), oldConfigOptions())
		})
	}
}

// Cancellation at entry and cancellation raised while Tool.Spec runs both
// install nothing.
func TestReplaceConfigurationCancellationInstallsNothing(t *testing.T) {
	for _, when := range []string{"entry", "during validation"} {
		t.Run(when, func(t *testing.T) {
			old := &captureCaller{answer: "old answer"}
			rt := newConfigurationRuntime(t, old)
			next := &captureCaller{answer: "new answer"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := newConfiguration(next)
			var specCalls atomic.Int32
			if when == "entry" {
				cfg.Tools = []agent.Tool{specCountingTool{calls: &specCalls}}
				cancel()
			} else {
				cfg.Tools = []agent.Tool{cancelingSpecTool{cancel: cancel}}
			}
			err := rt.ReplaceConfiguration(ctx, cfg)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if when == "entry" && specCalls.Load() != 0 {
				t.Errorf("Tool.Spec calls = %d, want an already-canceled publication to validate nothing", specCalls.Load())
			}
			assertOldTupleIntact(t, rt, old, "after "+when)
			if len(next.requests) != 0 {
				t.Fatalf("canceled orchestrator served %d requests, want 0", len(next.requests))
			}
		})
	}
}

// parkingCaller answers a tool call on its first step and a final message on
// its second, parking the first step so a test can publish a new configuration
// while an old turn is mid-flight.
type parkingCaller struct {
	requests []provider.ChatRequest
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (c *parkingCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if len(c.requests) == 1 {
		c.once.Do(func() { close(c.entered) })
		<-c.release
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "call-1", Type: "function",
			Function: provider.ToolCallFunction{Name: "lookup", Arguments: json.RawMessage(`{"path":"file.txt"}`)},
		}}}}, nil
	}
	if err := onToken(provider.ChatResponse{Content: "old answer"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "old answer", Done: true}}, nil
}

// A turn reserved before publication keeps the whole old tuple for every one of
// its steps; the next reservation receives the whole new tuple. Two barriers:
// run.started is the first event after reservation and before the request or
// the orchestrator is read, so a publication there catches any read taken later
// than reservation; parking inside the first model call then proves the second
// step of the same turn still runs on the old tuple.
func TestReplaceConfigurationFixesTheWholeTupleAtReservation(t *testing.T) {
	for _, barrier := range []string{"run.started", "first model call"} {
		t.Run(barrier, func(t *testing.T) {
			old := &parkingCaller{entered: make(chan struct{}), release: make(chan struct{})}
			rt := newConfigurationRuntime(t, old, previewTool{})
			unblockCaller := sync.OnceFunc(func() { close(old.release) })
			t.Cleanup(unblockCaller)
			next := &captureCaller{answer: "new answer"}

			started, gateRelease := make(chan struct{}), make(chan struct{})
			unblockGate := sync.OnceFunc(func() { close(gateRelease) })
			t.Cleanup(unblockGate)
			var once sync.Once
			gate := func(ev golem.Event) error {
				if barrier != "run.started" || ev.Type != "run.started" {
					return nil
				}
				once.Do(func() { close(started) })
				select {
				case <-gateRelease:
				case <-time.After(10 * time.Second):
					return errors.New("timed out waiting to release run.started")
				}
				return nil
			}
			parked, release := old.entered, unblockCaller
			if barrier == "run.started" {
				unblockCaller() // the sink gate parks this turn; its model steps must not
				parked, release = started, unblockGate
			}
			runErr := make(chan error, 1)
			go func() {
				_, err := rt.Run(context.Background(), golem.Turn{RunID: "parked", Message: "go"}, gate)
				runErr <- err
			}()
			waitReplace(t, parked)
			if err := rt.ReplaceConfiguration(context.Background(), newConfiguration(next)); err != nil {
				t.Fatalf("ReplaceConfiguration: %v", err)
			}
			release()
			if err := waitReplace(t, runErr); err != nil {
				t.Fatalf("parked run: %v", err)
			}
			runTurn(t, rt, "after")

			if len(old.requests) != 2 {
				t.Fatalf("parked turn steps = %d, want 2", len(old.requests))
			}
			oldTools := []string{"read_file", "search", "glob", "list", "lookup"}
			for i := range old.requests {
				assertSnapshot(t, old.requests[i], "OLD SYSTEM", oldTools, oldConfigWireOptions())
			}
			if len(next.requests) != 1 {
				t.Fatalf("new orchestrator requests = %d, want 1", len(next.requests))
			}
			assertSnapshot(t, next.requests[0], "NEW SYSTEM", newConfigToolNames(), newConfigWireOptions())
		})
	}
}

// Legacy Replace parked in its own validation still preserves the orchestrator
// and budget a ReplaceConfiguration published meanwhile, and installs only its
// own system, tools, and options.
func TestReplacePreservesConfigurationPublishedDuringItsValidation(t *testing.T) {
	old := &captureCaller{answer: "old answer"}
	rt := newConfigurationRuntime(t, old)
	next := &captureCaller{answer: "new answer"}
	tool := &blockingSpecTool{t: t, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(tool.release) })
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() { done <- rt.Replace("LAST SYSTEM", []agent.Tool{tool}) }()
	waitReplace(t, tool.entered)
	if err := rt.ReplaceConfiguration(context.Background(), newConfiguration(next)); err != nil {
		t.Fatalf("ReplaceConfiguration: %v", err)
	}
	unblock()
	if err := waitReplace(t, done); err != nil {
		t.Fatal(err)
	}
	runTurn(t, rt, "after")
	if len(old.requests) != 0 {
		t.Fatalf("old orchestrator served %d requests, want 0", len(old.requests))
	}
	if len(next.requests) != 1 {
		t.Fatalf("new orchestrator requests = %d, want 1", len(next.requests))
	}
	assertSnapshot(t, next.requests[0], "LAST SYSTEM", []string{"read_file", "search", "glob", "list", "blocking"}, newConfigWireOptions())
	assertBudget(t, rt.Budget(), newConfigBudget())
}

// The inverse race: a ReplaceConfiguration parked in its own validation
// publishes its complete tuple, discarding the legacy Replace that landed
// meanwhile. A complete replacement is not a field merge.
func TestReplaceConfigurationOverwritesReplaceLandingDuringItsValidation(t *testing.T) {
	old := &captureCaller{answer: "old answer"}
	rt := newConfigurationRuntime(t, old)
	next := &captureCaller{answer: "new answer"}
	tool := &blockingSpecTool{t: t, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(tool.release) })
	t.Cleanup(unblock)
	cfg := newConfiguration(next)
	cfg.Tools = []agent.Tool{tool}
	done := make(chan error, 1)
	go func() { done <- rt.ReplaceConfiguration(context.Background(), cfg) }()
	waitReplace(t, tool.entered)
	if err := rt.Replace("MIDDLE SYSTEM", nil, provider.ModelOptions{NumCtx: 111}); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := waitReplace(t, done); err != nil {
		t.Fatal(err)
	}
	runTurn(t, rt, "after")
	if len(old.requests) != 0 {
		t.Fatalf("old orchestrator served %d requests, want 0", len(old.requests))
	}
	assertSnapshot(t, next.requests[0], "NEW SYSTEM", []string{"read_file", "search", "glob", "list", "blocking"}, newConfigWireOptions())
	assertBudget(t, rt.Budget(), newConfigBudget())
}
