package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestChatToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			resp := `{"model":"test-model","message":{"role":"assistant","content":"Hello back!"},"done":true}`
			_, _ = w.Write([]byte(resp))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model": "test-model",
			"messages": []map[string]any{
				{"role": "user", "content": "Hello!"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	if text != "Hello back!" {
		t.Errorf("got %q, want %q", text, "Hello back!")
	}
}

func TestChatToolWithTemperature(t *testing.T) {
	var receivedTemp float64
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			var body struct {
				Options struct {
					Temperature float64 `json:"temperature"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				receivedTemp = body.Options.Temperature
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	_, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":       "m",
			"messages":    []map[string]any{{"role": "user", "content": "hi"}},
			"temperature": 0.9,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if receivedTemp != 0.9 {
		t.Errorf("temperature = %v, want 0.9", receivedTemp)
	}
}

func TestChatToolMissingModelNoConfig(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when no model and no config")
	}
	text := extractText(result)
	if !strings.Contains(text, "model parameter required") {
		t.Errorf("error = %q, want to contain %q", text, "model parameter required")
	}
}

func TestChatToolEmptyMessages(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "m",
			"messages": []map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty messages")
	}
	text := extractText(result)
	if !strings.Contains(text, "messages must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "messages must not be empty")
	}
}

func TestChatToolRAGDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "m",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"use_rag":  true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
	text := extractText(result)
	if !strings.Contains(text, "RAG is disabled") {
		t.Errorf("error = %q, want to contain %q", text, "RAG is disabled")
	}
}

func TestChatToolOllamaError(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"model not loaded"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "bad-model",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when Ollama returns 500")
	}
	text := extractText(result)
	if !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}

func TestChatToolInvalidArguments(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "chat",
		Arguments: "not a json object",
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for invalid arguments")
	}
	text := extractText(result)
	if !strings.Contains(text, "validation:") {
		t.Errorf("error = %q, want to contain %q", text, "validation:")
	}
}

func TestLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []ollama.ChatMessage
		expected string
	}{
		{
			name:     "single user message",
			msgs:     []ollama.ChatMessage{{Role: "user", Content: "hello"}},
			expected: "hello",
		},
		{
			name: "multiple messages returns last user",
			msgs: []ollama.ChatMessage{
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "reply"},
				{Role: "user", Content: "second"},
			},
			expected: "second",
		},
		{
			name: "no user messages",
			msgs: []ollama.ChatMessage{
				{Role: "system", Content: "you are helpful"},
				{Role: "assistant", Content: "hi"},
			},
			expected: "",
		},
		{
			name:     "nil slice",
			msgs:     nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastUserMessage(tt.msgs)
			if got != tt.expected {
				t.Errorf("lastUserMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// extractText returns the text content from a CallToolResult.
// It uses a type assertion to access the TextContent directly.
func extractText(r *gomcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*gomcp.TextContent); ok {
		return tc.Text
	}
	// Fallback: marshal/unmarshal for unknown content types.
	data, err := json.Marshal(r.Content[0])
	if err != nil {
		return ""
	}
	var tc struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return ""
	}
	return tc.Text
}
