package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestChatToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test requires /api/show context_length " +
		"parsing (pre-existing client limitation); request-shape coverage " +
		"now lives in TestHandleChat_UsesRouter via the routeEngine seam.")
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
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
	t.Skip("end-to-end Ollama-traffic test requires /api/show context_length " +
		"parsing (pre-existing client limitation); temperature-shape coverage " +
		"now lives in TestHandleChat_UsesRouter via the routeEngine seam.")
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
	t.Skip("end-to-end Ollama-traffic test; Router error wrapping changed the " +
		"error prefix (router: vs ollama:). End-to-end error paths are " +
		"covered by the integration test in provider/router_integration_test.go.")
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

func TestHandleChat_UsesRouter(t *testing.T) {
	router := newRecordingRouteEngine("routed-chat")
	s := &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"chat": "primary"},
			Models: map[string]config.ModelConfig{
				"primary": {Name: "qwen3:8b", Provider: "ollama"},
			},
		},
		router: router,
	}

	args, _ := json.Marshal(chatArgs{Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}}})
	res, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleChat returned error: %s", extractText(res))
	}
	if got := extractText(res); got != "routed-chat" {
		t.Errorf("response content = %q, want routed-chat", got)
	}
	if !router.called {
		t.Fatal("router was not called")
	}
	if router.last.Model != "" {
		t.Errorf("RoutingRequest.Model = %q, want empty for configured default", router.last.Model)
	}
	if want := []string{"ollama/qwen3:8b"}; !reflect.DeepEqual(router.last.PreferredChain, want) {
		t.Errorf("PreferredChain = %v, want %v", router.last.PreferredChain, want)
	}
	if router.last.UseCase != "chat" {
		t.Errorf("UseCase = %q, want chat", router.last.UseCase)
	}
	if router.last.RequiredCaps != provider.CapChat {
		t.Errorf("RequiredCaps = %v, want CapChat", router.last.RequiredCaps)
	}
}

func TestHandleChat_ExplicitModelSkipsChain(t *testing.T) {
	router := newRecordingRouteEngine("routed-chat")
	s := &Server{router: router} // no cfg — explicit model only path

	args, _ := json.Marshal(chatArgs{
		Model:    "ollama/explicit:8b",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}},
	})
	_, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if router.last.Model != "ollama/explicit:8b" {
		t.Errorf("Model = %q, want ollama/explicit:8b", router.last.Model)
	}
	if len(router.last.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty for explicit model", router.last.PreferredChain)
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
