package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGenerateToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/generate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","response":"generated text","done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "generate",
		Arguments: map[string]any{
			"model":  "m",
			"prompt": "Once upon a time",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	if got := extractText(result); got != "generated text" {
		t.Errorf("got %q, want %q", got, "generated text")
	}
}

func TestGenerateToolWithOptions(t *testing.T) {
	var receivedOpts struct {
		Temperature float64 `json:"temperature"`
		NumPredict  int     `json:"num_predict"`
	}
	var receivedSystem string

	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/generate":
			var body struct {
				System  string          `json:"system"`
				Options json.RawMessage `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				receivedSystem = body.System
				_ = json.Unmarshal(body.Options, &receivedOpts)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","response":"ok","done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	_, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "generate",
		Arguments: map[string]any{
			"model":       "m",
			"prompt":      "test",
			"system":      "You are a poet",
			"temperature": 0.5,
			"max_tokens":  200,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if receivedSystem != "You are a poet" {
		t.Errorf("system = %q, want %q", receivedSystem, "You are a poet")
	}
	if receivedOpts.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", receivedOpts.Temperature)
	}
	if receivedOpts.NumPredict != 200 {
		t.Errorf("num_predict = %v, want 200", receivedOpts.NumPredict)
	}
}

func TestGenerateToolEmptyPrompt(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "generate",
		Arguments: map[string]any{
			"model":  "m",
			"prompt": "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty prompt")
	}
	if text := extractText(result); !strings.Contains(text, "prompt must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "prompt must not be empty")
	}
}

func TestGenerateToolMissingModel(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "generate",
		Arguments: map[string]any{
			"prompt": "test",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when no model and no config")
	}
	if text := extractText(result); !strings.Contains(text, "model parameter required") {
		t.Errorf("error = %q, want to contain %q", text, "model parameter required")
	}
}

func TestGenerateToolOllamaError(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/generate":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"out of memory"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "generate",
		Arguments: map[string]any{
			"model":  "m",
			"prompt": "test",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true on Ollama 500")
	}
	if text := extractText(result); !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}
