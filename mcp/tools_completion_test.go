package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCompletionToolBasic(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"}}`))
		case "/api/generate":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"qwen3:8b","response":"fmt.Println(x)","done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "complete_code",
		Arguments: map[string]any{
			"model":  "qwen3:8b",
			"prefix": "func main() {\n\tx := 42\n\t",
			"suffix": "\n}",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	if got := extractText(result); got != "fmt.Println(x)" {
		t.Errorf("got %q, want %q", got, "fmt.Println(x)")
	}
}

func TestCompletionToolWithOptions(t *testing.T) {
	var receivedMaxTokens int

	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"qwen3:8b"}]}`))
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"details":{"family":"qwen3","parameter_size":"8B"}}`))
		case "/api/generate":
			var body struct {
				Options struct {
					NumPredict int `json:"num_predict"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				receivedMaxTokens = body.Options.NumPredict
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"qwen3:8b","response":"code","done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "complete_code",
		Arguments: map[string]any{
			"model":      "qwen3:8b",
			"prefix":     "func ",
			"suffix":     "() {}",
			"max_tokens": 64,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}
	// completion.Provider.buildRequest clamps max_tokens and applies it via
	// num_predict in ModelOptions. The exact value may be adjusted by the
	// provider's budget logic, so just verify it was set to something > 0.
	if receivedMaxTokens <= 0 {
		t.Errorf("num_predict = %d, want > 0", receivedMaxTokens)
	}
}

func TestCompletionToolMissingModel(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "complete_code",
		Arguments: map[string]any{
			"prefix": "x",
			"suffix": "y",
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

func TestCompletionToolOllamaError(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/generate":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "complete_code",
		Arguments: map[string]any{
			"model":  "nonexistent",
			"prefix": "x",
			"suffix": "y",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true on Ollama error")
	}
	if text := extractText(result); !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}
