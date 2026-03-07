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

func TestCompleteContextTruncation(t *testing.T) {
	// Create prefix and suffix that far exceed the context window budget.
	// With num_ctx=2048, num_predict=128, FIM overhead=3, available=1917 tokens.
	// At ~4 chars/token, that's ~7668 chars total (5748 prefix + 1920 suffix).
	longPrefix := strings.Repeat("a", 30000) // way over budget
	longSuffix := strings.Repeat("z", 10000) // way over budget

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		json.NewDecoder(r.Body).Decode(&req)
		capturedPrompt = req.Prompt

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")
	provider.Complete(context.Background(), FIMRequest{
		Prefix: longPrefix,
		Suffix: longSuffix,
	})

	// The prompt should be much shorter than the raw inputs.
	// Raw would be 40000+ chars; truncated should fit within budget.
	// Available = 2048 - 128 - 3 = 1917 tokens * 4 chars = 7668 chars max for prefix+suffix.
	// Plus FIM tokens themselves (~42 chars for the 3 token strings).
	maxExpectedLen := 7668 + len("<|fim_prefix|>") + len("<|fim_suffix|>") + len("<|fim_middle|>")
	if len(capturedPrompt) > maxExpectedLen {
		t.Errorf("prompt length %d exceeds budget %d", len(capturedPrompt), maxExpectedLen)
	}

	// Verify prefix was truncated to keep the END (most recent code)
	if !strings.HasSuffix(capturedPrompt, "<|fim_middle|>") {
		t.Error("prompt should end with FIM middle token")
	}
	// The prefix portion should end with 'a's (kept from the end of longPrefix)
	prefixStart := len("<|fim_prefix|>")
	suffixTokenIdx := strings.Index(capturedPrompt, "<|fim_suffix|>")
	truncatedPrefix := capturedPrompt[prefixStart:suffixTokenIdx]
	if len(truncatedPrefix) >= len(longPrefix) {
		t.Error("prefix should have been truncated")
	}

	// Verify suffix was truncated to keep the BEGINNING (nearest to cursor)
	middleTokenIdx := strings.Index(capturedPrompt, "<|fim_middle|>")
	truncatedSuffix := capturedPrompt[suffixTokenIdx+len("<|fim_suffix|>") : middleTokenIdx]
	if len(truncatedSuffix) >= len(longSuffix) {
		t.Error("suffix should have been truncated")
	}
	// Suffix should start with 'z's (kept from the beginning of longSuffix)
	if len(truncatedSuffix) > 0 && truncatedSuffix[0] != 'z' {
		t.Errorf("truncated suffix should start with 'z', got %q", truncatedSuffix[0:1])
	}
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
