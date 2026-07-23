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
	"github.com/kstruzzieri/go-llm/provider"
)

func TestGenerateToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; request-shape coverage now lives " +
		"in TestHandleGenerate_UsesRouter via the routeEngine seam.")
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
	t.Skip("end-to-end Ollama-traffic test; option shape coverage now lives " +
		"in TestHandleGenerate_UsesRouter via the routeEngine seam.")
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
	t.Skip("end-to-end Ollama-traffic test; Router error wrapping changed " +
		"the error prefix (router: vs ollama:). End-to-end error paths are " +
		"covered by the integration test in provider/router_integration_test.go.")
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

func TestHandleGenerate_UsesRouter(t *testing.T) {
	router := newRecordingRouteEngine("routed-gen")
	temp := 0.7
	s := &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"chat": "primary"},
			Models: map[string]config.ModelConfig{
				"primary": {Name: "qwen3:8b", Provider: "ollama"},
			},
		},
		router: router,
	}

	args, _ := json.Marshal(generateArgs{
		Prompt:      "write a haiku",
		System:      "you are a poet",
		Temperature: &temp,
		MaxTokens:   200,
	})
	res, err := s.handleGenerate(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleGenerate: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleGenerate returned error: %s", extractText(res))
	}
	if !router.called {
		t.Fatal("router was not called")
	}
	if router.last.Model != "" {
		t.Errorf("RoutingRequest.Model = %q, want empty for chain", router.last.Model)
	}
	if want := []string{"ollama/qwen3:8b"}; !reflect.DeepEqual(router.last.PreferredChain, want) {
		t.Errorf("PreferredChain = %v, want %v", router.last.PreferredChain, want)
	}
	if router.last.RequiredCaps != provider.CapGenerate {
		t.Errorf("RequiredCaps = %v, want CapGenerate", router.last.RequiredCaps)
	}
	if router.last.Prompt != "write a haiku" {
		t.Errorf("Prompt = %q, want %q", router.last.Prompt, "write a haiku")
	}
	if router.last.System != "you are a poet" {
		t.Errorf("System = %q, want %q", router.last.System, "you are a poet")
	}
	if router.last.Options.NumPredict != 200 {
		t.Errorf("NumPredict = %d, want 200", router.last.Options.NumPredict)
	}
	if router.last.ExpectedOutput != 200 {
		t.Errorf("ExpectedOutput = %d, want 200 (from max_tokens)", router.last.ExpectedOutput)
	}
	if router.last.Options.Temperature == nil || *router.last.Options.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", router.last.Options.Temperature)
	}
}
