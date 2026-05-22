package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// newBlockedServer returns a test server that flags any received request.
// Callers assert !called to prove replay refused the trace before reaching
// the network.
func newBlockedServer(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &called
}

func rolesAndContent(messages []ollama.ChatMessage) []string {
	result := make([]string, 0, len(messages))
	for _, msg := range messages {
		result = append(result, msg.Role+":"+msg.Content)
	}
	return result
}

func TestReplayRefusesTraceWithoutUserTurn(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{ID: "no-user", Turns: []Turn{{Role: "assistant", Content: "hi"}}}

	_, err := replay(context.Background(), client, "m", trace)
	if !errors.Is(err, errNoUserTurn) {
		t.Fatalf("err = %v, want errNoUserTurn", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}

func TestReplaySupportsMultipleUserTurns(t *testing.T) {
	var requests []ollama.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, req)

		resp := ollama.ChatResponse{
			Model: "test-model",
			Done:  true,
		}
		switch len(requests) {
		case 1:
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "model ack"}
		case 2:
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "final answer"}
		default:
			t.Fatalf("unexpected request %d", len(requests))
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "multi-turn",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "captured ack"},
			{Role: "user", Content: "second"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 2 {
		t.Fatalf("transcript len = %d, want 2: %#v", len(transcript), transcript)
	}
	if transcript[0].Content != "model ack" || transcript[1].Content != "final answer" {
		t.Fatalf("transcript = %#v, want candidate assistant turns", transcript)
	}
	if len(requests) != 2 {
		t.Fatalf("requests len = %d, want 2", len(requests))
	}
	gotRoles := rolesAndContent(requests[1].Messages)
	wantRoles := []string{
		"system:sys",
		"user:first",
		"assistant:model ack",
		"user:second",
	}
	if !reflect.DeepEqual(gotRoles, wantRoles) {
		t.Fatalf("second request messages = %#v, want %#v", gotRoles, wantRoles)
	}
}

func TestReplayInjectsFrozenToolResults(t *testing.T) {
	var requests []ollama.ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, req)

		resp := ollama.ChatResponse{
			Model: "test-model",
			Done:  true,
		}
		switch len(requests) {
		case 1:
			if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read_file" {
				t.Fatalf("tools = %#v, want read_file", req.Tools)
			}
			resp.Message = ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:   "candidate-call",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "read_file",
							Arguments: map[string]any{"path": "provider/router.go"},
						},
					},
				},
			}
		case 2:
			if len(req.Messages) != 4 {
				t.Fatalf("second request messages len = %d, want 4: %#v", len(req.Messages), req.Messages)
			}
			tool := req.Messages[3]
			if tool.Role != "tool" || tool.ToolName != "read_file" {
				t.Fatalf("tool message = %#v, want read_file result", tool)
			}
			if tool.ToolCallID != "candidate-call" {
				t.Fatalf("tool_call_id = %q, want candidate-call", tool.ToolCallID)
			}
			if tool.Content != "package provider" {
				t.Fatalf("tool content = %q, want frozen result", tool.Content)
			}
			resp.Message = ollama.ChatMessage{Role: "assistant", Content: "router summary"}
		default:
			t.Fatalf("unexpected request %d", len(requests))
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-loop",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{
				Role:      "assistant",
				ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}},
			},
			{Role: "tool", Name: "read_file", ToolCallID: "captured-call", Content: "package provider"},
		},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 3 {
		t.Fatalf("transcript len = %d, want 3: %#v", len(transcript), transcript)
	}
	if got := extractToolNames(transcript); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("extractToolNames() = %v, want [read_file]", got)
	}
	if transcript[1].Role != "tool" || transcript[1].Name != "read_file" {
		t.Fatalf("tool transcript turn = %#v, want read_file result", transcript[1])
	}
	if transcript[1].ToolCallID != "candidate-call" {
		t.Fatalf("transcript tool_call_id = %q, want candidate-call", transcript[1].ToolCallID)
	}
	if transcript[2].Content != "router summary" {
		t.Fatalf("final transcript = %#v, want router summary", transcript[2])
	}
}

func TestReplayErrorsWhenCandidateToolCallDoesNotMatchFrozenResult(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		resp := ollama.ChatResponse{
			Model: "test-model",
			Message: ollama.ChatMessage{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:   "candidate-call",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Name:      "write_file",
							Arguments: map[string]any{"path": "provider/router.go"},
						},
					},
				},
			},
			Done: true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-mismatch",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{
			{Role: "user", Content: "Read provider/router.go"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"provider/router.go"}`)}}},
			{Role: "tool", Name: "read_file", Content: "package provider"},
		},
	}

	_, err := replay(context.Background(), client, "test-model", trace)
	if !errors.Is(err, errToolCallMismatch) {
		t.Fatalf("err = %v, want errToolCallMismatch", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestReplayForwardsTraceTools(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(req.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(req.Tools))
		}
		if req.Tools[0].Type != "function" {
			t.Fatalf("tool type = %q, want function", req.Tools[0].Type)
		}
		if req.Tools[0].Function.Name != "read_file" {
			t.Fatalf("tool name = %q, want read_file", req.Tools[0].Function.Name)
		}

		var schema map[string]any
		if err := json.Unmarshal(req.Tools[0].Function.Parameters, &schema); err != nil {
			t.Fatalf("unmarshal schema: %v", err)
		}
		if schema["type"] != "object" {
			t.Fatalf("schema type = %v, want object", schema["type"])
		}

		resp := ollama.ChatResponse{
			Model:   "test-model",
			Message: ollama.ChatMessage{Role: "assistant", Content: "done"},
			Done:    true,
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	trace := Trace{
		ID:     "tool-trace",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file from disk","inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}`),
		},
		Turns: []Turn{{Role: "user", Content: "Read provider/router.go"}},
	}

	transcript, err := replay(context.Background(), client, "test-model", trace)
	if err != nil {
		t.Fatalf("replay() error: %v", err)
	}
	if len(transcript) != 1 || transcript[0].Content != "done" {
		t.Fatalf("transcript = %#v, want single assistant turn with content", transcript)
	}
}

func TestReplayRejectsInvalidTraceTool(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{
		ID:     "bad-tool",
		System: "sys",
		Tools: []json.RawMessage{
			json.RawMessage(`{"name":"read_file","description":"Read a file","inputSchema":123}`),
		},
		Turns: []Turn{{Role: "user", Content: "hi"}},
	}

	_, err := replay(context.Background(), client, "m", trace)
	if !errors.Is(err, errInvalidTraceTool) {
		t.Fatalf("err = %v, want errInvalidTraceTool", err)
	}
	if *called {
		t.Error("replay reached Ollama despite invalid tool definition")
	}
}
