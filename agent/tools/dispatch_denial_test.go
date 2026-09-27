//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestScopedRequestDenials(t *testing.T) {
	t.Run("native requests and quiet filtering", func(t *testing.T) {
		parent := scopedFixture(t)
		if err := os.Symlink("visible.txt", filepath.Join(parent.root, "a", "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent.root, "a", "forged.txt"), []byte("path denied by workspace policy"), 0600); err != nil {
			t.Fatal(err)
		}
		unreadable := filepath.Join(parent.root, "a", "unreadable.txt")
		if err := os.WriteFile(unreadable, []byte("ONLY"), 0000); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(unreadable, 0600) }()
		ws, counts, cleanup, err := newScopedWorkspace(parent, "a")
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		if _, err := ws.readAll("private.txt"); !errors.Is(err, errScopeDenied) {
			t.Fatal(err)
		}
		if counts.requests.Load() != 0 {
			t.Fatal("host access counted as model request")
		}
		for _, tc := range []struct {
			name, raw string
			denied    bool
		}{
			{"read_file", `{"path":"private.txt"}`, true},
			{"read_file", `{"path":"../b/visible.txt"}`, true},
			{"read_file", `{"path":"link"}`, true},
			{"list", `{"path":"../b"}`, true},
			{"glob", `{"pattern":"/absolute"}`, true},
			{"glob", `{"pattern":"a/../*"}`, true},
			{"glob", `{"pattern":"bad\u0000pattern"}`, true},
			{"read_file", `{"path":"visible.txt"}`, false},
			{"read_file", `{"path":"forged.txt"}`, false},
			{"read_file", `{"path":"missing"}`, false},
			{"read_file", `{"path":3}`, false},
			{"read_file", `{"path":"visible.txt","start_line":-1}`, false},
			{"search", `{"pattern":"ONLY"}`, false},
			{"search", `{"pattern":"[","regex":true}`, false},
			{"glob", `{"pattern":"**"}`, false},
			{"list", `{}`, false},
		} {
			t.Run(tc.name+"/"+tc.raw, func(t *testing.T) {
				var tool agent.Tool
				for _, candidate := range NewFileToolsForWorkspace(ws) {
					if candidate.Spec().Name == tc.name {
						tool = candidate
					}
				}
				before := counts.requests.Load()
				evals := counts.evaluations.Load()
				out, err := tool.Invoke(t.Context(), json.RawMessage(tc.raw))
				if err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if tc.denied {
					want = 1
					if !out.IsError || out.Content != "path denied by workspace policy" {
						t.Fatalf("denial=%+v", out)
					}
				}
				if got := counts.requests.Load() - before; got != want {
					t.Fatalf("requests=%d, want %d", got, want)
				}
				if (tc.name == "list" && tc.raw == "{}") || (tc.name == "glob" && tc.raw == `{"pattern":"**"}`) || tc.name == "search" && !out.IsError {
					if counts.evaluations.Load() <= evals {
						t.Fatal("fixture did not exercise quiet pruning")
					}
				}
			})
		}
	})
	t.Run("child snapshots", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			scope *string
			calls []provider.ToolCall
			want  int64
		}{
			{"parallel", stringPointer("a"), []provider.ToolCall{denialCall("", "read_file", `{"path":"private.txt"}`), denialCall("", "list", `{"path":"../b"}`), denialCall("", "glob", `{"pattern":"../*"}`)}, 3},
			{"serial repeated IDs", stringPointer("a"), []provider.ToolCall{denialCall("reused", "read_file", `{"path":"private.txt"}`), denialCall("reused", "read_file", `{"path":"../b/visible.txt"}`)}, 2},
			{"unknown tool", stringPointer("a"), []provider.ToolCall{denialCall("", "unknown", "{}")}, 0},
			{"legacy guard", nil, []provider.ToolCall{denialCall("", "read_file", `{"path":"a/private.txt"}`)}, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d, err := NewDispatch(denialCaller(tc.calls), agent.ContextManager{}, NewFileToolsForWorkspace(scopedFixture(t)), DispatchLimits{})
				if err != nil {
					t.Fatal(err)
				}
				readers, counts, cleanup, err := d.childTools(tc.scope)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				out, err := d.runChild(t.Context(), dispatchTask{task: "inspect", scope: tc.scope}, dispatchChild{tools: readers, counter: counts, cleanup: cleanup})
				if err != nil || out.deniedRequests != tc.want || out.RiskScore != 0 {
					t.Fatalf("snapshot=%+v err=%v, want %d refusals with unchanged child risk", out, err, tc.want)
				}
				if tc.want == 3 {
					if out.StopReason != agent.ToolErrorCapReached.String() {
						t.Fatalf("three refusals must retain governor stop: %+v", out)
					}
				} else if out.Summary != "finished" {
					t.Fatalf("summary=%q", out.Summary)
				}
			})
		}
	})
}

func stringPointer(s string) *string { return &s }

func denialCall(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Type: "function", Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func denialCaller(calls []provider.ToolCall) dispatchModelFunc {
	return func(_ context.Context, req provider.ChatRequest) (agent.ModelResult, error) {
		out := dispatchBudgetAnswer(provider.Usage{})
		out.Response.Content = "finished"
		for _, m := range req.Messages {
			if m.Role == "tool" {
				return out, nil
			}
		}
		out.Response = provider.ChatResponse{ToolCalls: calls}
		return out, nil
	}
}
