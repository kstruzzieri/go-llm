package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// requestShape is what a turn observably received: the system message and
// the tool names of the provider request the orchestrator built.
type requestShape struct {
	system string
	tools  []string
}

func shapeOf(req provider.ChatRequest) requestShape {
	var s requestShape
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		s.system = req.Messages[0].Content
	}
	for _, t := range req.Tools {
		s.tools = append(s.tools, t.Function.Name)
	}
	return s
}

func hasTool(s requestShape, name string) bool {
	for _, n := range s.tools {
		if n == name {
			return true
		}
	}
	return false
}

// blockingSpecTool parks validation inside Tool.Spec until released, so a
// test can complete Close while Replace is validating.
type blockingSpecTool struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSpecTool) Spec() agent.ToolSpec {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return agent.ToolSpec{Name: "blocking", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (b *blockingSpecTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (b *blockingSpecTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ok"}, nil
}

func newReplaceRuntime(t *testing.T, caller agent.ModelCaller, system string, tools ...agent.Tool) *golem.Runtime {
	t.Helper()
	rt, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		System:       system,
		Tools:        tools,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func runTurn(t *testing.T, rt *golem.Runtime, runID string) {
	t.Helper()
	if _, err := rt.Run(context.Background(), golem.Turn{RunID: runID, Message: "go"}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("Run %s: %v", runID, err)
	}
}

// A turn reserved before Replace completes with its original System and
// Tools; the next turn observes the replacement. The gate is run.started:
// it is the first event emitted after reservation and before the request is
// built, and the sink is synchronous, so blocking there parks run A exactly
// where a lazily-read snapshot would still be replaceable.
func TestReplaceFixesSnapshotAtReservation(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "OLD SYSTEM", namedTool("old_tool"))

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	gate := func(ev golem.Event) error {
		if ev.Type == "run.started" {
			once.Do(func() { close(entered) })
			<-release
		}
		return nil
	}
	runErr := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), golem.Turn{RunID: "a", Message: "go"}, gate)
		runErr <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("run.started gate never fired; the event name or sink ordering changed")
	}
	if err := rt.Replace("NEW SYSTEM", []agent.Tool{namedTool("new_tool")}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	close(release)
	if err := <-runErr; err != nil {
		t.Fatalf("Run a: %v", err)
	}
	runTurn(t, rt, "b")

	if len(caller.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(caller.requests))
	}
	a, b := shapeOf(caller.requests[0]), shapeOf(caller.requests[1])
	if a.system != "OLD SYSTEM" || !hasTool(a, "old_tool") || hasTool(a, "new_tool") {
		t.Fatalf("turn reserved before Replace saw %+v, want the old snapshot", a)
	}
	if b.system != "NEW SYSTEM" || !hasTool(b, "new_tool") || hasTool(b, "old_tool") {
		t.Fatalf("turn reserved after Replace saw %+v, want the new snapshot", b)
	}
	if !hasTool(a, "read_file") || !hasTool(b, "read_file") {
		t.Fatalf("runtime file tools must precede host tools in both snapshots: a=%v b=%v", a.tools, b.tools)
	}
}

func TestReplaceValidatesAndInstallsNothing(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "OLD SYSTEM", namedTool("old_tool"))
	for _, tc := range []struct {
		name  string
		tools []agent.Tool
		want  string
	}{
		{"duplicate of a runtime file tool", []agent.Tool{namedTool("read_file")}, "duplicate tool name"},
		{"duplicate within the list", []agent.Tool{namedTool("t"), namedTool("t")}, "duplicate tool name"},
		{"nil tool", []agent.Tool{nil}, "nil tool"},
		{"empty name", []agent.Tool{namedTool("")}, "empty name"},
	} {
		err := rt.Replace("X", tc.tools)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	runTurn(t, rt, "after")
	got := shapeOf(caller.requests[0])
	if got.system != "OLD SYSTEM" || !hasTool(got, "old_tool") || hasTool(got, "t") {
		t.Fatalf("a rejected Replace changed the snapshot: %+v", got)
	}
}

func TestReplaceErrClosedDominatesValidation(t *testing.T) {
	rt := newReplaceRuntime(t, &captureCaller{answer: "ok"}, "S")
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rt.Replace("S", []agent.Tool{namedTool("read_file")}); !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed to dominate the validation error", err)
	}
}

// Close completes while Replace is still validating (parked inside
// Tool.Spec); the list is also invalid. ErrClosed must win: the validation
// error is retained, the lock reacquired, and closed rechecked before any
// error is returned or anything is published.
func TestReplaceErrClosedWhenCloseCompletesDuringValidation(t *testing.T) {
	rt := newReplaceRuntime(t, &captureCaller{answer: "ok"}, "S")
	tool := &blockingSpecTool{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- rt.Replace("X", []agent.Tool{tool, namedTool("read_file")}) }()
	<-tool.entered
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(tool.release)
	if err := <-done; !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed after Close completed during validation", err)
	}
}

// Replace("", nil) after a richer replacement leaves the New default prompt
// and exactly the runtime's own file tools: a replacement can shrink.
func TestReplaceDefaultsEmptySystemLikeNew(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "OLD SYSTEM")
	if err := rt.Replace("RICH", []agent.Tool{namedTool("extra")}); err != nil {
		t.Fatalf("Replace rich: %v", err)
	}
	if err := rt.Replace("", nil); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	runTurn(t, rt, "r")
	got := shapeOf(caller.requests[0])
	if got.system != golem.SystemPrompt(false, false) {
		t.Fatalf("system = %q, want the New default", got.system)
	}
	if hasTool(got, "extra") || !hasTool(got, "read_file") {
		t.Fatalf("tools = %v, want the file tools only", got.tools)
	}
}

func TestReplaceCopiesTheToolSlice(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "S")
	tools := []agent.Tool{namedTool("kept")}
	if err := rt.Replace("S", tools); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	tools[0] = namedTool("swapped")
	runTurn(t, rt, "r")
	got := shapeOf(caller.requests[0])
	if !hasTool(got, "kept") || hasTool(got, "swapped") {
		t.Fatalf("Replace aliased the caller's slice: %+v", got)
	}
}

// Run under -race: Replace against concurrent Runs must never race or
// deadlock. No ordering assertion — that is the barrier test's job.
func TestReplaceConcurrentWithRuns(t *testing.T) {
	rt := newReplaceRuntime(t, scriptedCaller{result: agent.ModelResult{Response: provider.ChatResponse{Content: "ok", Done: true}}}, "S")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if err := rt.Replace(fmt.Sprintf("S%d", i), []agent.Tool{namedTool(fmt.Sprintf("t%d", i))}); err != nil {
				t.Errorf("Replace %d: %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := rt.Run(context.Background(), golem.Turn{RunID: fmt.Sprintf("r%d", i), Message: "go"}, func(golem.Event) error { return nil }); err != nil {
				t.Errorf("Run %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
