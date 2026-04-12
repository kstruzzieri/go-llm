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

func TestListModelsToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b","size":4700000000},{"name":"nomic-embed-text","size":274000000}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "list_models",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var models []ollama.ModelInfo
	if err := json.Unmarshal([]byte(text), &models); err != nil {
		t.Fatalf("unmarshal models: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("model count = %d, want 2", len(models))
	}
}

func TestShowModelToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			// Ollama's /api/show response nests model details under "details".
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B","quantization_level":"Q4_K_M"},"digest":"abc123","template":"{{.Prompt}}"}`))

		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "show_model",
		Arguments: map[string]any{
			"name": "qwen3:8b",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var info ollama.ModelInfo
	if err := json.Unmarshal([]byte(text), &info); err != nil {
		t.Fatalf("unmarshal model info: %v", err)
	}
	if info.Family != "qwen3" {
		t.Errorf("family = %q, want %q", info.Family, "qwen3")
	}
}

func TestShowModelToolEmptyName(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "show_model",
		Arguments: map[string]any{
			"name": "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty name")
	}
	if text := extractText(result); !strings.Contains(text, "name must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "name must not be empty")
	}
}

func TestPullModelToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/api/pull":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"success"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "pull_model",
		Arguments: map[string]any{
			"name": "qwen3:8b",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	text := extractText(result)
	if !strings.Contains(text, "pulled successfully") {
		t.Errorf("result = %q, want to contain %q", text, "pulled successfully")
	}
}

func TestPullModelToolOllamaError(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/pull":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "pull_model",
		Arguments: map[string]any{
			"name": "nonexistent",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true on pull failure")
	}
	if text := extractText(result); !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}
