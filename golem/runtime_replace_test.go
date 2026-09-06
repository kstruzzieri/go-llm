package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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
		// agent.Run appends the #430 base contract to every effective prompt;
		// these tests are about the APPLICATION prompt Replace installs, so
		// the constant suffix is stripped here and asserted once explicitly
		// in TestReplaceDefaultsEmptySystemLikeNew.
		s.system = strings.TrimSuffix(req.Messages[0].Content, "\n\n"+agent.ToolTrustContract)
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
	t       *testing.T
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSpecTool) Spec() agent.ToolSpec {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-time.After(10 * time.Second):
		b.t.Error("timed out waiting to release Tool.Spec")
	}
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
		ModelOptions: provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096},
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

func waitReplace[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for runtime barrier")
		var zero T
		return zero
	}
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
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var once sync.Once
	gate := func(ev golem.Event) error {
		if ev.Type == "run.started" {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return errors.New("timed out waiting to release run.started")
			}
		}
		return nil
	}
	runErr := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), golem.Turn{RunID: "a", Message: "go"}, gate)
		runErr <- err
	}()
	waitReplace(t, entered)
	if err := rt.Replace("NEW SYSTEM", []agent.Tool{namedTool("new_tool")}, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096}); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	unblock()
	if err := waitReplace(t, runErr); err != nil {
		t.Fatalf("Run a: %v", err)
	}
	runTurn(t, rt, "b")

	if len(caller.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(caller.requests))
	}
	assertSnapshot(t, caller.requests[0], "OLD SYSTEM", []string{"read_file", "search", "glob", "list", "old_tool"}, provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
	assertSnapshot(t, caller.requests[1], "NEW SYSTEM", []string{"read_file", "search", "glob", "list", "new_tool"}, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096})
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
		err := rt.Replace("X", tc.tools, provider.ModelOptions{ThinkEffort: "high"})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		runTurn(t, rt, tc.name)
		assertSnapshot(t, caller.requests[len(caller.requests)-1], "OLD SYSTEM", []string{"read_file", "search", "glob", "list", "old_tool"}, provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
	}
}

func TestReplaceErrClosedDominatesValidation(t *testing.T) {
	rt := newReplaceRuntime(t, &captureCaller{answer: "ok"}, "S")
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rt.Replace("X", []agent.Tool{namedTool("read_file")}, provider.ModelOptions{ThinkEffort: "high"}); !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed to dominate validation", err)
	}
	assertModelOptions(t, rt.ModelOptions(), provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
}

// Close wins whether the validation parked inside Tool.Spec later succeeds
// or fails. The last published options remain readable after Close.
func TestReplaceErrClosedWhenCloseCompletesDuringValidation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			rt := newReplaceRuntime(t, &captureCaller{answer: "ok"}, "S")
			tool := &blockingSpecTool{t: t, entered: make(chan struct{}), release: make(chan struct{})}
			unblock := sync.OnceFunc(func() { close(tool.release) })
			t.Cleanup(unblock)
			tools := []agent.Tool{tool}
			if invalid {
				tools = append(tools, namedTool("read_file"))
			}
			done := make(chan error, 1)
			go func() { done <- rt.Replace("X", tools, provider.ModelOptions{ThinkEffort: "high"}) }()
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
			assertModelOptions(t, rt.ModelOptions(), provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
		})
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
	if raw := caller.requests[0].Messages[0].Content; !strings.HasSuffix(raw, "\n\n"+agent.ToolTrustContract) {
		t.Fatalf("effective system = %q, want the #430 base contract appended by agent.Run", raw)
	}
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
			if err := rt.Replace(fmt.Sprintf("S%d", i), []agent.Tool{namedTool(fmt.Sprintf("t%d", i))}, provider.ModelOptions{Think: provider.Ptr(i%2 == 0), Stop: []string{"STOP"}}); err != nil {
				t.Errorf("Replace %d: %v", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			_ = rt.ModelOptions()
			if _, err := rt.Run(context.Background(), golem.Turn{RunID: fmt.Sprintf("r%d", i), Message: "go"}, func(golem.Event) error { return nil }); err != nil {
				t.Errorf("Run %d: %v", i, err)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitReplace(t, done)
}

func assertSnapshot(t *testing.T, req provider.ChatRequest, system string, tools []string, options provider.ModelOptions) {
	t.Helper()
	got := shapeOf(req)
	if got.system != system || !reflect.DeepEqual(got.tools, tools) {
		t.Errorf("snapshot = %+v, want system %q tools %v", got, system, tools)
	}
	assertModelOptions(t, req.Options, options)
}

func assertModelOptions(t *testing.T, got, want provider.ModelOptions) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("options = %+v, want %+v", got, want)
	}
}

func TestReplacePreservesOptionsAtPublication(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "OLD SYSTEM")
	tool := &blockingSpecTool{t: t, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(tool.release) })
	t.Cleanup(unblock)
	done := make(chan error, 1)
	replace := rt.Replace // inferred method values still support two arguments
	go func() { done <- replace("LAST SYSTEM", []agent.Tool{tool}) }()
	waitReplace(t, tool.entered)
	if err := rt.Replace("MIDDLE SYSTEM", nil, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096}); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := waitReplace(t, done); err != nil {
		t.Fatal(err)
	}
	runTurn(t, rt, "after")
	assertSnapshot(t, caller.requests[0], "LAST SYSTEM", []string{"read_file", "search", "glob", "list", "blocking"}, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096})
}

func TestReplaceModelOptionsValidationAndReset(t *testing.T) {
	caller := &captureCaller{answer: "ok"}
	rt := newReplaceRuntime(t, caller, "OLD SYSTEM", namedTool("old_tool"))
	err := rt.Replace("BAD SYSTEM", []agent.Tool{namedTool("bad_tool")}, provider.ModelOptions{ThinkEffort: "high"}, provider.ModelOptions{})
	if !errors.Is(err, golem.ErrInvalidRequest) || err.Error() != "golem: invalid request: Replace accepts at most one model options value" {
		t.Fatalf("error = %v", err)
	}
	runTurn(t, rt, "unchanged")
	assertSnapshot(t, caller.requests[0], "OLD SYSTEM", []string{"read_file", "search", "glob", "list", "old_tool"}, provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
	if err := rt.Replace("RESET SYSTEM", nil, provider.ModelOptions{}); err != nil {
		t.Fatal(err)
	}
	assertModelOptions(t, rt.ModelOptions(), provider.ModelOptions{})
	runTurn(t, rt, "reset")
	assertSnapshot(t, caller.requests[1], "RESET SYSTEM", []string{"read_file", "search", "glob", "list"}, provider.ModelOptions{})
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	err = rt.Replace("BAD SYSTEM", nil, provider.ModelOptions{}, provider.ModelOptions{})
	if !errors.Is(err, golem.ErrClosed) {
		t.Fatalf("error = %v, want ErrClosed", err)
	}
	assertModelOptions(t, rt.ModelOptions(), provider.ModelOptions{})
}

// This caller exercises both final-response-only and token-callback paths,
// and replaces the runtime while the first model step is completing.
type replacingCaller struct {
	toolWireCaller
	replace func() error
	tokens  bool
}

func (c *replacingCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if !c.tokens {
		onToken = func(provider.ChatResponse) error { return nil }
	}
	result, err := c.toolWireCaller.Chat(ctx, req, onToken)
	if len(c.requests) == 1 && err == nil {
		err = c.replace()
	}
	return result, err
}

func TestReplaceKeepsOptionsAcrossModelSteps(t *testing.T) {
	for _, tokens := range []bool{false, true} {
		t.Run(fmt.Sprintf("tokens=%t", tokens), func(t *testing.T) {
			caller := &replacingCaller{tokens: tokens}
			rt := newReplaceRuntime(t, caller, "OLD SYSTEM", previewTool{})
			caller.replace = func() error {
				return rt.Replace("NEW SYSTEM", []agent.Tool{previewTool{}}, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096})
			}
			runTurn(t, rt, "a")
			runTurn(t, rt, "b")
			if len(caller.requests) != 4 {
				t.Fatalf("requests = %d, want 4", len(caller.requests))
			}
			for _, i := range []int{0, 1} {
				assertSnapshot(t, caller.requests[i], "OLD SYSTEM", []string{"read_file", "search", "glob", "list", "lookup"}, provider.ModelOptions{Think: provider.Ptr(false), NumCtx: 4096})
			}
			for _, i := range []int{2, 3} {
				assertSnapshot(t, caller.requests[i], "NEW SYSTEM", []string{"read_file", "search", "glob", "list", "lookup"}, provider.ModelOptions{ThinkEffort: "high", NumCtx: 4096})
			}
		})
	}
}

func literalModelOptions() provider.ModelOptions {
	return provider.ModelOptions{Temperature: provider.Ptr(0.25), TopP: provider.Ptr(0.75), TopK: provider.Ptr(17), NumPredict: 123, NumCtx: 4567, Stop: []string{"STOP"}, RepeatPenalty: provider.Ptr(1.2), Think: provider.Ptr(true), ThinkEffort: "high"}
}

func mutateModelOptions(o provider.ModelOptions) {
	if o.Temperature != nil {
		*o.Temperature = 9
	}
	if o.TopP != nil {
		*o.TopP = 8
	}
	if o.TopK != nil {
		*o.TopK = 7
	}
	if o.RepeatPenalty != nil {
		*o.RepeatPenalty = 6
	}
	if o.Think != nil {
		*o.Think = !*o.Think
	}
	if len(o.Stop) > 0 {
		o.Stop[0] = "changed"
	}
}

type mutatingOptionsCaller struct{ captureCaller }

func (c *mutatingOptionsCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	result, err := c.captureCaller.Chat(ctx, req, onToken)
	if len(c.requests) == 1 {
		mutateModelOptions(req.Options)
	}
	return result, err
}

func TestModelOptionsOwnership(t *testing.T) {
	for _, boundary := range []string{"New", "Replace", "getter", "caller"} {
		for _, variant := range []string{"values", "zero", "nil", "empty stop"} {
			t.Run(boundary+"/"+variant, func(t *testing.T) {
				literal := func() provider.ModelOptions {
					switch variant {
					case "zero":
						return provider.ModelOptions{Temperature: provider.Ptr(0.0), TopP: provider.Ptr(0.0), TopK: provider.Ptr(0), RepeatPenalty: provider.Ptr(0.0), Think: provider.Ptr(false)}
					case "nil":
						return provider.ModelOptions{}
					case "empty stop":
						return provider.ModelOptions{Stop: []string{}}
					default:
						return literalModelOptions()
					}
				}
				source := literal()
				capture := &captureCaller{answer: "ok"}
				var caller agent.ModelCaller = capture
				if boundary == "caller" {
					mutating := &mutatingOptionsCaller{captureCaller: captureCaller{answer: "ok"}}
					caller = mutating
					capture = &mutating.captureCaller
				}
				rt, err := golem.New(context.Background(), golem.Options{Root: t.TempDir(), Orchestrator: agent.New(caller, agent.ContextManager{}), ModelOptions: source})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = rt.Close() })
				if boundary == "Replace" || boundary == "getter" {
					source = literal()
					if err := rt.Replace("S", nil, source); err != nil {
						t.Fatal(err)
					}
				}
				switch boundary {
				case "New", "Replace":
					mutateModelOptions(source)
				case "getter":
					mutateModelOptions(rt.ModelOptions())
				case "caller":
					runTurn(t, rt, "mutate")
				}
				assertModelOptions(t, rt.ModelOptions(), literal())
				runTurn(t, rt, "after")
				assertModelOptions(t, capture.requests[len(capture.requests)-1].Options, literal())
			})
		}
	}
}

func TestModelOptionsAfterClose(t *testing.T) {
	rt := newReplaceRuntime(t, &captureCaller{answer: "ok"}, "S")
	if err := rt.Replace("S", nil, provider.ModelOptions{ThinkEffort: "medium", Think: provider.Ptr(true), Stop: []string{"CLOSED STOP"}}); err != nil {
		t.Fatal(err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	got := rt.ModelOptions()
	assertModelOptions(t, got, provider.ModelOptions{ThinkEffort: "medium", Think: provider.Ptr(true), Stop: []string{"CLOSED STOP"}})
	mutateModelOptions(got)
	assertModelOptions(t, rt.ModelOptions(), provider.ModelOptions{ThinkEffort: "medium", Think: provider.Ptr(true), Stop: []string{"CLOSED STOP"}})
}
