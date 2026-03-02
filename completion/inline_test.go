package completion

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestComplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}

		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming complete")
		}
		if !strings.Contains(req.Prompt, "<|fim_prefix|>") {
			t.Error("prompt should contain FIM prefix token")
		}
		if !strings.Contains(req.Prompt, "<|fim_suffix|>") {
			t.Error("prompt should contain FIM suffix token")
		}
		if !strings.Contains(req.Prompt, "<|fim_middle|>") {
			t.Error("prompt should contain FIM middle token")
		}

		resp := ollama.GenerateResponse{
			Model:     "test-model",
			Response:  "fmt.Println(\"hello\")",
			Done:      true,
			EvalCount: 5,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")

	resp, err := provider.Complete(context.Background(), FIMRequest{
		Prefix: "func main() {\n\t",
		Suffix: "\n}",
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}
	if resp.Completion != "fmt.Println(\"hello\")" {
		t.Errorf("unexpected completion: %q", resp.Completion)
	}
	if resp.Tokens != 5 {
		t.Errorf("expected 5 tokens, got %d", resp.Tokens)
	}
	if resp.LatencyMs < 0 {
		t.Error("latency should be non-negative")
	}
}

func TestCompleteStream(t *testing.T) {
	chunks := []ollama.GenerateResponse{
		{Model: "test-model", Response: "fmt.", Done: false},
		{Model: "test-model", Response: "Println", Done: false},
		{Model: "test-model", Response: "()", Done: false},
		{Model: "test-model", Response: "", Done: true, EvalCount: 3},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true for streaming complete")
		}

		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")

	var tokens []string
	err := provider.CompleteStream(context.Background(), FIMRequest{
		Prefix: "func main() {\n\t",
		Suffix: "\n}",
	}, func(token string) error {
		tokens = append(tokens, token)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream() error: %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
	joined := strings.Join(tokens, "")
	if joined != "fmt.Println()" {
		t.Errorf("unexpected combined tokens: %q", joined)
	}
}

func TestCompleteStreamCallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := ollama.GenerateResponse{
			Model:    "test-model",
			Response: "token",
			Done:     false,
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")

	callbackErr := fmt.Errorf("stop streaming")
	err := provider.CompleteStream(context.Background(), FIMRequest{
		Prefix: "func main() {",
		Suffix: "}",
	}, func(token string) error {
		return callbackErr
	})
	if err == nil {
		t.Fatal("expected callback error")
	}
}

func TestCompleteValidation(t *testing.T) {
	client := ollama.NewClient()

	t.Run("model required", func(t *testing.T) {
		provider := NewProvider(client, "")
		_, err := provider.Complete(context.Background(), FIMRequest{
			Prefix: "code",
		})
		if err == nil {
			t.Fatal("expected error for missing model")
		}
	})

	t.Run("prefix required", func(t *testing.T) {
		provider := NewProvider(client, "test-model")
		_, err := provider.Complete(context.Background(), FIMRequest{})
		if err == nil {
			t.Fatal("expected error for missing prefix")
		}
	})

	t.Run("stream fn required", func(t *testing.T) {
		provider := NewProvider(client, "test-model")
		err := provider.CompleteStream(context.Background(), FIMRequest{
			Prefix: "code",
		}, nil)
		if err == nil {
			t.Fatal("expected error for nil callback")
		}
	})
}

func TestCompleteFIMPromptFormat(t *testing.T) {
	var capturedPrompt string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		json.NewDecoder(r.Body).Decode(&req)
		capturedPrompt = req.Prompt

		resp := ollama.GenerateResponse{
			Model:    "test-model",
			Response: "result",
			Done:     true,
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")

	provider.Complete(context.Background(), FIMRequest{
		Prefix: "BEFORE",
		Suffix: "AFTER",
	})

	expected := "<|fim_prefix|>BEFORE<|fim_suffix|>AFTER<|fim_middle|>"
	if capturedPrompt != expected {
		t.Errorf("FIM prompt = %q, want %q", capturedPrompt, expected)
	}
}

func TestCompleteModelOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Options == nil {
			t.Fatal("expected model options")
		}
		if req.Options.NumPredict != 128 {
			t.Errorf("NumPredict = %d, want 128", req.Options.NumPredict)
		}
		if req.Options.NumCtx != 2048 {
			t.Errorf("NumCtx = %d, want 2048", req.Options.NumCtx)
		}
		if req.Options.Temperature != 0.2 {
			t.Errorf("Temperature = %f, want 0.2", req.Options.Temperature)
		}

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")
	provider.Complete(context.Background(), FIMRequest{Prefix: "code"})
}

func TestCompleteCustomMaxTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		json.NewDecoder(r.Body).Decode(&req)

		if req.Options.NumPredict != 256 {
			t.Errorf("NumPredict = %d, want 256", req.Options.NumPredict)
		}

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")
	provider.Complete(context.Background(), FIMRequest{
		Prefix:    "code",
		MaxTokens: 256,
	})
}
