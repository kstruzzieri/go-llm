package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

func TestReplayRefusesMultipleUserTurns(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{
		ID:     "multi-turn",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "ack"},
			{Role: "user", Content: "second"},
		},
	}

	_, err := replay(context.Background(), client, "some-model", trace)
	if !errors.Is(err, errMultiUserTurn) {
		t.Fatalf("err = %v, want errMultiUserTurn", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
}

func TestReplayRefusesTraceWithUnsupportedExtraTurns(t *testing.T) {
	srv, called := newBlockedServer(t)
	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))

	trace := Trace{
		ID:     "tool-loop-trace",
		System: "sys",
		Turns: []Turn{
			{Role: "user", Content: "first"},
			{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
			{Role: "tool", Name: "read_file", Content: "contents"},
			{Role: "assistant", Content: "final"},
		},
	}

	_, err := replay(context.Background(), client, "some-model", trace)
	if !errors.Is(err, errUnsupportedTurns) {
		t.Fatalf("err = %v, want errUnsupportedTurns", err)
	}
	if *called {
		t.Error("replay reached Ollama despite refusing the trace")
	}
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
