//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestDispatchScopedInvokeAcceptsMixedTasks(t *testing.T) {
	parent := scopedFixture(t)
	caller := &dispatchCaller{}
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(parent), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":["legacy",{"task":"scoped","scope":"a"}]}`))
	if err != nil || out.IsError {
		t.Fatalf("scoped Invoke = %+v, %v", out, err)
	}
	var envelope dispatchEnvelope
	if err := json.Unmarshal([]byte(out.Content), &envelope); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"summary: legacy", "summary: scoped"} {
		if envelope.Results[i].Summary != want || envelope.Results[i].Model != "local/fast" {
			t.Fatalf("result %d = %+v", i, envelope.Results[i])
		}
	}
	if len(caller.requests()) != 2 {
		t.Fatalf("model requests = %d", len(caller.requests()))
	}
}

func TestDispatchScopedInputValidation(t *testing.T) {
	tests := []struct {
		name, item string
		valid      bool
	}{
		{"string", `"legacy"`, true},
		{"object", `{"task":"scoped","scope":"a"}`, true},
		{"casing", `{"TASK":"scoped","SCOPE":"a"}`, true},
		{"duplicate", `{"task":null,"task":"scoped","scope":null,"scope":"a"}`, true},
		{"last task null", `{"task":"old","task":null,"scope":"a"}`, false},
		{"last scope null", `{"task":"scoped","scope":"a","scope":null}`, false},
		{"null", `null`, false}, {"array", `[]`, false}, {"number", `1`, false}, {"bool", `true`, false},
		{"empty object", `{}`, false}, {"missing task", `{"scope":"a"}`, false}, {"missing scope", `{"task":"x"}`, false},
		{"empty task", `{"task":"","scope":"a"}`, false}, {"blank task", `{"task":" \t","scope":"a"}`, false},
		{"empty scope", `{"task":"x","scope":""}`, false}, {"unknown", `{"task":"x","scope":"a","extra":1}`, false},
		{"task type", `{"task":1,"scope":"a"}`, false}, {"scope type", `{"task":"x","scope":[]}`, false},
		{"decoded task limit", fmt.Sprintf(`{"task":%q,"scope":"a"}`, strings.Repeat("x", 8192)), true},
		{"decoded task too long", fmt.Sprintf(`{"task":%q,"scope":"a"}`, strings.Repeat("x", 8193)), false},
		{"decoded scope limit", fmt.Sprintf(`{"task":"x","scope":%q}`, strings.Repeat("./", 2047)+"a/"), true},
		{"decoded scope too long", fmt.Sprintf(`{"task":"x","scope":%q}`, strings.Repeat("./", 2048)+"a"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			caller := &dispatchCaller{}
			d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			out, err := d.Invoke(context.Background(), json.RawMessage(`{"tasks":["valid first",`+tt.item+`]}`))
			if err != nil {
				t.Fatal(err)
			}
			if out.IsError == tt.valid {
				t.Fatalf("valid=%v, result=%+v", tt.valid, out)
			}
			want := 0
			if tt.valid {
				want = 2
			}
			if len(caller.requests()) != want {
				t.Fatalf("model calls = %d, want %d", len(caller.requests()), want)
			}
		})
	}
}

func TestDispatchScopedPreflightRejectsLaterScope(t *testing.T) {
	for _, scope := range []string{"../outside", ".", "missing", "a/visible.txt"} {
		t.Run(scope, func(t *testing.T) {
			caller := &dispatchCaller{}
			d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"tasks": []any{"valid legacy", map[string]string{"task": "valid scoped", "scope": "a"}, map[string]string{"task": "invalid", "scope": scope}}})
			out, err := d.Invoke(context.Background(), raw)
			if err != nil || !out.IsError {
				t.Fatalf("invalid scope = %+v, %v", out, err)
			}
			if len(caller.requests()) != 0 {
				t.Fatalf("preflight started %d children", len(caller.requests()))
			}
			if (scope == "." || scope == "../outside") && out.Content != "path denied by workspace policy" {
				t.Fatalf("policy error = %q", out.Content)
			}
		})
	}
}

func TestDispatchScopedAggregateArgumentLimit(t *testing.T) {
	if maxDispatchArgsBytes != 197632 {
		t.Fatalf("raw argument cap = %d", maxDispatchArgsBytes)
	}
	caller := &dispatchCaller{}
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"tasks":[{"task":"x","scope":"a"}]}`
	raw += strings.Repeat(" ", 197632-len(raw))
	out, err := d.Invoke(context.Background(), json.RawMessage(raw))
	if err != nil || out.IsError {
		t.Fatalf("exact cap = %+v, %v", out, err)
	}
	out, err = d.Invoke(context.Background(), json.RawMessage(raw+" "))
	if err != nil || !out.IsError || out.Content != "dispatch arguments must be at most 197632 bytes" {
		t.Fatalf("above cap = %+v, %v", out, err)
	}
	if len(caller.requests()) != 1 {
		t.Fatalf("model calls = %d", len(caller.requests()))
	}
}

func TestDispatchScopedMinimumCapBeforeLaunch(t *testing.T) {
	for _, scoped := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(scoped), func(t *testing.T) {
			minimum := dispatchEnvelope{Results: make([]dispatchResult, 4)}
			for i := range minimum.Results {
				minimum.Results[i] = dispatchResult{Summary: dispatchSummaryTruncated, StopReason: agent.ToolErrorCapReached.String(), Model: "?", Error: dispatchErrorTruncated, Truncated: true}
			}
			encoded, _ := json.Marshal(minimum)
			for _, delta := range []int{-1, 0} {
				caller := &dispatchCaller{}
				d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{MaxResultBytes: len(encoded) + 36*scoped + delta})
				if err != nil {
					t.Fatal(err)
				}
				tasks := []any{"legacy", "legacy", "legacy", "legacy"}
				for i := range scoped {
					tasks[i] = map[string]string{"task": "scoped", "scope": "a"}
				}
				raw, _ := json.Marshal(map[string]any{"tasks": tasks})
				out, err := d.Invoke(context.Background(), raw)
				if err != nil {
					t.Fatal(err)
				}
				if delta < 0 {
					if !out.IsError || !strings.Contains(out.Content, "required metadata") || len(caller.requests()) != 0 {
						t.Fatalf("insufficient cap started child: %+v calls=%d", out, len(caller.requests()))
					}
				} else if out.IsError || len(caller.requests()) != 4 {
					t.Fatalf("exact minimum = %+v calls=%d", out, len(caller.requests()))
				}
			}
		})
	}
}

type dispatchModelFunc func(context.Context, provider.ChatRequest) (agent.ModelResult, error)

func (f dispatchModelFunc) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return f(ctx, req)
}

func TestDispatchScopedRealToolObservations(t *testing.T) {
	parent := scopedFixture(t)
	backend := progressiveFixture()
	retrieval := &Retrieve{R: backend, Progressive: true}
	caller := dispatchModelFunc(func(_ context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
		task := ""
		var observations []string
		for _, m := range req.Messages {
			if m.Role == "user" {
				task = m.Content
			}
			if m.Role == "tool" {
				first, last := strings.IndexByte(m.Content, '\n'), strings.LastIndexByte(m.Content, '\n')
				if first < 0 || last <= first {
					t.Errorf("unframed observation: %q", m.Content)
					continue
				}
				observations = append(observations, m.Content[first+1:last])
			}
		}
		scoped := task != "legacy"
		names := []string{}
		for _, tool := range req.Tools {
			names = append(names, tool.Function.Name)
		}
		wantNames := []string{"read_file", "search", "glob", "list"}
		system := scopedDispatchSystemPrompt
		if !scoped {
			wantNames = append(wantNames, "retrieve")
			system = dispatchSystemPrompt
		}
		if !slices.Equal(names, wantNames) {
			t.Errorf("%s advertised tools=%q", task, names)
		}
		if req.Messages[0].Content != system+"\n\n"+agent.ToolTrustContract {
			t.Errorf("%s system=%q", task, req.Messages[0].Content)
		}
		result := agent.ModelResult{RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: task}}}
		if len(observations) > 0 {
			want := []string{"B_ONLY\n"}
			if scoped {
				word := "B_ONLY"
				if task == "a" {
					word = "A_ONLY"
				}
				want = []string{word + "\n", "visible.txt:1: " + word, "visible.txt", "visible.txt", "unknown tool: retrieve"}
			}
			if !slices.Equal(observations, want) {
				t.Errorf("%s observations=%q, want %q", task, observations, want)
			}
			result.Response = provider.ChatResponse{Content: "summary: " + task, Done: true}
			return result, nil
		}
		path := "visible.txt"
		if !scoped {
			path = "b/visible.txt"
		}
		calls := []provider.ToolCall{{ID: "read", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, path))}}}
		if scoped {
			for _, item := range []struct{ name, args string }{{"search", `{"pattern":"ONLY"}`}, {"glob", `{"pattern":"**"}`}, {"list", `{}`}, {"retrieve", `{"query":"private"}`}} {
				calls = append(calls, provider.ToolCall{ID: item.name, Function: provider.ToolCallFunction{Name: item.name, Arguments: json.RawMessage(item.args)}})
			}
		}
		result.Response = provider.ChatResponse{ToolCalls: calls}
		return result, nil
	})
	d, err := NewDispatch(caller, agent.ContextManager{}, append(NewFileToolsForWorkspace(parent), retrieval), DispatchLimits{MaxConcurrent: 3}, &childProbe{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":[{"task":"a","scope":"a"},"legacy",{"task":"b","scope":"b"}]}`))
	if err != nil || out.IsError {
		t.Fatalf("Invoke=%+v, %v", out, err)
	}
	var decoded struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(out.Content), &decoded); err != nil {
		t.Fatal(err)
	}
	for i, task := range []string{"a", "legacy", "b"} {
		got := decoded.Results[i]
		if got["summary"] != "summary: "+task || got["model"] != "local/"+task || got["risk_score"] != float64(7) {
			t.Errorf("result %d=%v", i, got)
		}
		if i == 0 {
			if got["scope_denials"] != float64(3) {
				t.Errorf("a count=%v, want 3", got)
			}
		} else if _, ok := got["scope_denials"]; ok {
			t.Errorf("zero/unscoped count published: %v", got)
		}
	}
	if backend.retrieveCalls != 0 || !reflect.ValueOf(backend.gotReq).IsZero() {
		t.Errorf("retrieve backend=%d render=%+v", backend.retrieveCalls, backend.gotReq)
	}
	if got, err := parent.readAll("b/visible.txt"); err != nil || string(got) != "B_ONLY\n" {
		t.Errorf("parent after Invoke=%q, %v", got, err)
	}
}

// Capture real acquired handles; production still performs every operation.
func captureDispatchRoots(d *Dispatch) (func() []*os.File, <-chan struct{}) {
	var mu sync.Mutex
	var roots []*os.File
	acquired := make(chan struct{}, 16)
	d.prepareChildTools = func(scope *string) ([]agent.Tool, *atomic.Int64, func(), error) {
		readers, count, cleanup, err := d.childTools(scope)
		if err == nil && scope != nil {
			mu.Lock()
			roots = append(roots, readers[0].(*ReadFile).ws.pinnedRoot)
			mu.Unlock()
			acquired <- struct{}{}
		}
		return readers, count, cleanup, err
	}
	return func() []*os.File { mu.Lock(); defer mu.Unlock(); return append([]*os.File(nil), roots...) }, acquired
}

func checkDispatchRoots(t *testing.T, roots []*os.File, closed bool) {
	t.Helper()
	for i, root := range roots {
		_, err := root.Stat()
		if closed && !errors.Is(err, os.ErrClosed) {
			t.Errorf("root %d remains open after return: %v", i, err)
		}
		if !closed && err != nil {
			t.Errorf("root %d closed before workers exit: %v", i, err)
		}
	}
}

func awaitDispatchSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch signal timed out")
	}
}

func TestDispatchScopedPartialCleanup(t *testing.T) {
	caller := &dispatchCaller{}
	parent := scopedFixture(t)
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(parent), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	roots, _ := captureDispatchRoots(d)
	out, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":[{"task":"one","scope":"a"},{"task":"two","scope":"b"},{"task":"bad","scope":"missing"}]}`))
	if err != nil || !out.IsError || out.Content != "path not found" {
		t.Fatalf("setup failure=%+v, %v", out, err)
	}
	if len(roots()) != 2 {
		t.Fatalf("acquired roots=%d, want 2", len(roots()))
	}
	checkDispatchRoots(t, roots(), true)
	if len(caller.requests()) != 0 {
		t.Fatalf("preflight started %d models", len(caller.requests()))
	}
}

func TestDispatchScopedWorkerLifetime(t *testing.T) {
	for _, mode := range []string{"complete", "cancel", "deadline", "model-error"} {
		t.Run(mode, func(t *testing.T) {
			parent := scopedFixture(t)
			entered := make(chan struct{}, 4)
			release := make(chan struct{})
			var calls, callbacks atomic.Int32
			var roots func() []*os.File
			caller := dispatchModelFunc(func(ctx context.Context, _ provider.ChatRequest) (agent.ModelResult, error) {
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				checkDispatchRoots(t, roots(), false)
				if err := ctx.Err(); err != nil {
					return agent.ModelResult{}, err
				}
				if mode == "model-error" {
					return agent.ModelResult{}, errors.New("model failure")
				}
				return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}, RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}}, nil
			})
			d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(parent), DispatchLimits{MaxConcurrent: 1, OnChildComplete: func(int, int) { callbacks.Add(1); checkDispatchRoots(t, roots(), false) }})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ = captureDispatchRoots(d)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				defer cancel()
			}
			type outcome struct {
				out agent.ToolResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				out, err := d.Invoke(ctx, json.RawMessage(`{"tasks":[{"task":"one","scope":"a"},{"task":"two","scope":"b"},{"task":"three","scope":"a"},{"task":"four","scope":"b"}]}`))
				done <- outcome{out, err}
			}()
			awaitDispatchSignal(t, entered)
			if len(roots()) != 4 {
				t.Errorf("preflight roots=%d, want 4", len(roots()))
			}
			checkDispatchRoots(t, roots(), false)
			if mode == "cancel" {
				cancel()
			} else if mode != "deadline" {
				close(release)
			}
			var result outcome
			select {
			case result = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Invoke stuck")
			}
			checkDispatchRoots(t, roots(), true)
			if mode == "cancel" {
				if !errors.Is(result.err, context.Canceled) {
					t.Errorf("cancel=%v", result.err)
				}
			} else if result.err != nil {
				t.Errorf("Invoke=%v", result.err)
			}
			want := int32(4)
			if mode == "cancel" || mode == "deadline" {
				want = 1
			}
			if calls.Load() != want || callbacks.Load() != want {
				t.Errorf("calls=%d callbacks=%d, want %d", calls.Load(), callbacks.Load(), want)
			}
			if mode == "deadline" && (!result.out.IsError || !strings.Contains(result.out.Content, "child not started before dispatch timeout")) {
				t.Errorf("queued deadline=%+v", result.out)
			}
			if _, err := parent.readAll("b/visible.txt"); err != nil {
				t.Errorf("parent closed: %v", err)
			}
		})
	}
}

func TestDispatchScopedOverlappingLifetimes(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	caller := dispatchModelFunc(func(ctx context.Context, _ provider.ChatRequest) (agent.ModelResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return agent.ModelResult{}, ctx.Err()
		}
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}, RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "fast"}}}, nil
	})
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	roots, acquired := captureDispatchRoots(d)
	done := make(chan error, 2)
	invoke := func() {
		out, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":[{"task":"one","scope":"a"}]}`))
		if out.IsError {
			err = fmt.Errorf("%s", out.Content)
		}
		done <- err
	}
	go invoke()
	awaitDispatchSignal(t, acquired)
	awaitDispatchSignal(t, entered)
	go invoke()
	awaitDispatchSignal(t, acquired)
	if len(roots()) != 2 || roots()[0] == roots()[1] {
		t.Fatalf("independent roots=%v", roots())
	}
	checkDispatchRoots(t, roots(), false)
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("overlap hung")
		}
	}
	checkDispatchRoots(t, roots(), true)
}

func TestDispatchScopedMaximumCountSurvivesTruncation(t *testing.T) {
	d, err := NewDispatch(staticDispatchCaller{content: strings.Repeat("x", 4096)}, agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{MaxResultBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	d.prepareChildTools = func(scope *string) ([]agent.Tool, *atomic.Int64, func(), error) {
		tools, count, cleanup, err := d.childTools(scope)
		if count != nil {
			count.Store(math.MaxInt64)
		}
		return tools, count, cleanup, err
	}
	out, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":[{"task":"a","scope":"a"},{"task":"b","scope":"b"},{"task":"a2","scope":"a"},{"task":"b2","scope":"b"}]}`))
	if err != nil || out.IsError || !out.Truncated || len(out.Content) > 1024 {
		t.Fatalf("bounded count=%+v, %v", out, err)
	}
	var result struct {
		Results []struct {
			ScopeDenials int64 `json:"scope_denials"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	for i, r := range result.Results {
		if r.ScopeDenials != math.MaxInt64 {
			t.Errorf("result %d count=%d", i, r.ScopeDenials)
		}
	}
}

func TestDispatchScopedSchema(t *testing.T) {
	d, err := NewDispatch(&dispatchCaller{}, agent.ContextManager{}, childTools(), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Tasks struct {
				Items struct {
					OneOf []struct {
						Type                 string         `json:"type"`
						Required             []string       `json:"required"`
						AdditionalProperties *bool          `json:"additionalProperties"`
						Properties           map[string]any `json:"properties"`
					} `json:"oneOf"`
				} `json:"items"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(d.Spec().Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	variants := schema.Properties.Tasks.Items.OneOf
	if len(variants) != 2 || variants[0].Type != "string" || variants[1].Type != "object" {
		t.Fatalf("task schema=%s", d.Spec().Parameters)
	}
	object := variants[1]
	if !slices.Equal(object.Required, []string{"task", "scope"}) || object.AdditionalProperties == nil || *object.AdditionalProperties || len(object.Properties) != 2 {
		t.Fatalf("scoped schema=%s", d.Spec().Parameters)
	}
}

func TestDispatchScopedInvokeSnapshotsCurrentGuard(t *testing.T) {
	parent := scopedFixture(t)
	caller := &dispatchCaller{}
	d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(parent), DispatchLimits{})
	if err != nil {
		t.Fatal(err)
	}
	parent.SetScopeGuard(func(string, bool) error { return errors.New("private host detail") })
	out, err := d.Invoke(t.Context(), json.RawMessage(`{"tasks":["legacy",{"task":"scoped","scope":"a"}]}`))
	if err != nil || !out.IsError || out.Content != "path denied by workspace policy" || len(caller.requests()) != 0 {
		t.Fatalf("late guard=%+v, %v calls=%d", out, err, len(caller.requests()))
	}
}

func TestDispatchScopedSymlinkPreflightCleanup(t *testing.T) {
	for _, scope := range []string{"scope-link", "scope-link/nested"} {
		t.Run(scope, func(t *testing.T) {
			parent := scopedFixture(t)
			if err := os.Mkdir(filepath.Join(parent.root, "a", "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("a", filepath.Join(parent.root, "scope-link")); err != nil {
				t.Fatal(err)
			}
			caller := &dispatchCaller{}
			d, err := NewDispatch(caller, agent.ContextManager{}, NewFileToolsForWorkspace(parent), DispatchLimits{})
			if err != nil {
				t.Fatal(err)
			}
			roots, _ := captureDispatchRoots(d)
			raw, _ := json.Marshal(map[string]any{"tasks": []any{map[string]string{"task": "one", "scope": "a"}, map[string]string{"task": "two", "scope": "b"}, map[string]string{"task": "blocked", "scope": scope}}})
			out, err := d.Invoke(t.Context(), raw)
			if err != nil || !out.IsError || out.Content != "path denied by workspace policy" {
				t.Errorf("symlink scope denial=%+v, %v", out, err)
			}
			if len(caller.requests()) != 0 {
				t.Errorf("symlink preflight started %d models", len(caller.requests()))
			}
			if len(roots()) != 2 {
				t.Fatalf("acquired roots=%d, want 2", len(roots()))
			}
			checkDispatchRoots(t, roots(), true)
		})
	}
}
