package golem

// These tests live in package golem (not golem_test) on purpose: the focused
// ScopeGuard contract inspects the private rt.tools slice, and keeping the
// integration variant here avoids an agent/tools -> golem -> agent/tools test
// import cycle.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestScopeGuardCoversEveryBuiltinFileTool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("NEVER_LEAK"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "private"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "private", "nested.txt"), []byte("NEVER_LEAK"), 0o600); err != nil {
		t.Fatal(err)
	}

	rt, err := New(context.Background(), Options{
		Root: root,
		ScopeGuard: func(rel string, _ bool) error {
			if rel == "secret.txt" || rel == "private" || strings.HasPrefix(rel, "private/") {
				return errors.New("blocked by test guard")
			}
			return nil
		},
		Orchestrator: agent.New(nil, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })

	tools := make(map[string]agent.Tool, len(rt.tools))
	for _, tool := range rt.tools {
		tools[tool.Spec().Name] = tool
	}
	for _, name := range []string{"read_file", "search", "glob", "list"} {
		if tools[name] == nil {
			t.Fatalf("missing built-in tool %q", name)
		}
		effect := tools[name].Effect()
		if !effect.Class.Has(agent.Read) || effect.Class.IsMutating() {
			t.Fatalf("built-in tool %q effect = %#v", name, effect)
		}
	}

	read, err := tools["read_file"].Invoke(context.Background(), json.RawMessage(`{"path":"secret.txt"}`))
	if err != nil || !read.IsError || read.Content != "path denied by workspace policy" || strings.Contains(read.Content, "blocked by test guard") {
		t.Fatalf("read_file = %#v, %v", read, err)
	}
	search, err := tools["search"].Invoke(context.Background(), json.RawMessage(`{"pattern":"NEVER_LEAK"}`))
	if err != nil || search.IsError || search.Content != "no matches" {
		t.Fatalf("search = %#v, %v", search, err)
	}
	glob, err := tools["glob"].Invoke(context.Background(), json.RawMessage(`{"pattern":"**"}`))
	if err != nil || glob.IsError || strings.Contains(glob.Content, "secret.txt") || strings.Contains(glob.Content, "private") {
		t.Fatalf("glob = %#v, %v", glob, err)
	}
	list, err := tools["list"].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || list.IsError || strings.Contains(list.Content, "secret.txt") || strings.Contains(list.Content, "private") {
		t.Fatalf("list = %#v, %v", list, err)
	}
	// Direct point lookup of the guarded directory: denial text is the stable
	// scope message, exactly as for read_file.
	denied, err := tools["list"].Invoke(context.Background(), json.RawMessage(`{"path":"private"}`))
	if err != nil || !denied.IsError || denied.Content != "path denied by workspace policy" {
		t.Fatalf("list(private) = %#v, %v", denied, err)
	}
}

func TestNilScopeGuardPreservesBuiltinReads(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("VISIBLE"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt, err := New(context.Background(), Options{
		Root:         root,
		Orchestrator: agent.New(nil, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestRuntime(t, rt) })

	var read agent.Tool
	for _, tool := range rt.tools {
		if tool.Spec().Name == "read_file" {
			read = tool
			break
		}
	}
	if read == nil {
		t.Fatal("missing read_file")
	}
	got, err := read.Invoke(context.Background(), json.RawMessage(`{"path":"visible.txt"}`))
	if err != nil || got.IsError || got.Content != "VISIBLE" {
		t.Fatalf("read_file = %#v, %v", got, err)
	}
}

// oneToolCaller issues a single scripted tool call on step one, then answers.
// It records every request so tests can inspect the model-visible observations.
type oneToolCaller struct {
	call     provider.ToolCall
	step     int
	requests []provider.ChatRequest
}

func (c *oneToolCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.requests = append(c.requests, req)
	if c.step == 0 {
		c.step++
		return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{c.call}}}, nil
	}
	if err := onToken(provider.ChatResponse{Content: "done"}); err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
}

// TestScopeGuardSanitizedToolFailuresThroughRealRun drives the four built-in
// read tools' failure shapes through real Golem runs: read_file and list
// against missing paths, glob and search after the workspace root itself is
// removed (the TOCTOU shape a walk error would otherwise disclose). The
// canonical root carries a distinctive marker; no model observation and no
// marshaled event may contain it.
func TestScopeGuardSanitizedToolFailuresThroughRealRun(t *testing.T) {
	const marker = "golem-scopeguard-secret-marker"
	cases := []struct {
		name       string
		tool       string
		args       string
		removeRoot bool
	}{
		{name: "read_file missing path", tool: "read_file", args: `{"path":"missing.txt"}`},
		{name: "list missing path", tool: "list", args: `{"path":"missing-dir"}`},
		{name: "glob removed root", tool: "glob", args: `{"pattern":"**"}`, removeRoot: true},
		{name: "search removed root", tool: "search", args: `{"pattern":"anything"}`, removeRoot: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), marker)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			caller := &oneToolCaller{call: provider.ToolCall{
				ID:   "call-1",
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      tc.tool,
					Arguments: json.RawMessage(tc.args),
				},
			}}
			rt, err := New(context.Background(), Options{
				Root:         root,
				Orchestrator: agent.New(caller, agent.ContextManager{}),
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { closeTestRuntime(t, rt) })
			if tc.removeRoot {
				if err := os.RemoveAll(root); err != nil {
					t.Fatal(err)
				}
			}

			var events []Event
			result, err := rt.Run(context.Background(), Turn{RunID: "run-" + tc.tool, Message: "go"}, func(event Event) error {
				events = append(events, event)
				return nil
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Answer != "done" {
				t.Fatalf("Answer = %q", result.Answer)
			}

			finished := 0
			for _, event := range events {
				raw, merr := json.Marshal(event)
				if merr != nil {
					t.Fatalf("marshal event: %v", merr)
				}
				if strings.Contains(string(raw), marker) {
					t.Fatalf("event %s leaks the workspace root: %s", event.Type, raw)
				}
				if event.Type != "tool.finished" {
					continue
				}
				finished++
				var payload struct {
					IsError bool `json:"isError"`
				}
				if uerr := json.Unmarshal(event.Payload, &payload); uerr != nil {
					t.Fatalf("decode tool.finished: %v", uerr)
				}
				if !payload.IsError {
					t.Fatalf("tool.finished = %s, want isError", event.Payload)
				}
			}
			if finished != 1 {
				t.Fatalf("tool.finished events = %d, want 1", finished)
			}

			if len(caller.requests) < 2 {
				t.Fatalf("model requests = %d, want the post-tool observation request", len(caller.requests))
			}
			observed := ""
			for _, req := range caller.requests {
				for _, msg := range req.Messages {
					if strings.Contains(msg.Content, marker) {
						t.Fatalf("model observation leaks the workspace root: %q", msg.Content)
					}
					if msg.Role == "tool" {
						observed = msg.Content
					}
				}
			}
			if observed != "path not found" {
				t.Fatalf("tool observation = %q, want %q", observed, "path not found")
			}
		})
	}
}
