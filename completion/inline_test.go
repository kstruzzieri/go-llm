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

	t.Run("nil client", func(t *testing.T) {
		provider := NewProvider(nil, "test-model")
		_, err := provider.Complete(context.Background(), FIMRequest{Prefix: "code"})
		if err == nil {
			t.Fatal("expected error for nil client")
		}
		err = provider.CompleteStream(context.Background(), FIMRequest{Prefix: "code"}, func(string) error { return nil })
		if err == nil {
			t.Fatal("expected error for nil client in stream")
		}
	})

	t.Run("model required", func(t *testing.T) {
		provider := NewProvider(client, "")
		_, err := provider.Complete(context.Background(), FIMRequest{
			Prefix: "code",
		})
		if err == nil {
			t.Fatal("expected error for missing model")
		}
	})

	t.Run("empty prefix is valid for FIM", func(t *testing.T) {
		// Empty prefix is valid — represents cursor at the start of a file or new buffer.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]any{
				"response":   "generated",
				"done":       true,
				"eval_count": 1,
			})
		}))
		defer srv.Close()
		c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		provider := NewProvider(c, "test-model")
		resp, err := provider.Complete(context.Background(), FIMRequest{Suffix: "func main() {}"})
		if err != nil {
			t.Fatalf("unexpected error for empty prefix: %v", err)
		}
		if resp.Completion != "generated" {
			t.Errorf("got %q, want %q", resp.Completion, "generated")
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
	// Build distinguishable prefix and suffix that far exceed the context budget.
	// Prefix: "P00000...P29999" — each segment is 6 chars, so we can verify
	// that truncation keeps the END of the prefix (high-numbered segments).
	// Suffix: "S00000...S09999" — verify truncation keeps the BEGINNING (low-numbered).
	var prefixBuilder, suffixBuilder strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&prefixBuilder, "P%05d", i) // 6 chars each = 30000 total
	}
	for i := 0; i < 1667; i++ {
		fmt.Fprintf(&suffixBuilder, "S%05d", i) // 6 chars each = 10002 total
	}
	longPrefix := prefixBuilder.String()
	longSuffix := suffixBuilder.String()

	var capturedPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode generate request: %v", err)
		}
		capturedPrompt = req.Prompt

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Fatalf("failed to encode generate response: %v", err)
		}
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	provider := NewProvider(client, "test-model")
	_, err := provider.Complete(context.Background(), FIMRequest{
		Prefix: longPrefix,
		Suffix: longSuffix,
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	// The prompt should be much shorter than the raw inputs.
	// Available = 2048 - 128 - 3 = 1917 tokens * 4 chars = 7668 chars max for prefix+suffix.
	// Plus FIM tokens themselves (~42 chars for the 3 token strings).
	maxExpectedLen := 7668 + len("<|fim_prefix|>") + len("<|fim_suffix|>") + len("<|fim_middle|>")
	if len(capturedPrompt) > maxExpectedLen {
		t.Errorf("prompt length %d exceeds budget %d", len(capturedPrompt), maxExpectedLen)
	}

	// Extract prefix and suffix portions from the prompt
	suffixTokenIdx := strings.Index(capturedPrompt, "<|fim_suffix|>")
	middleTokenIdx := strings.Index(capturedPrompt, "<|fim_middle|>")
	if suffixTokenIdx == -1 || middleTokenIdx == -1 {
		t.Fatal("prompt missing FIM tokens")
	}
	truncatedPrefix := capturedPrompt[len("<|fim_prefix|>"):suffixTokenIdx]
	truncatedSuffix := capturedPrompt[suffixTokenIdx+len("<|fim_suffix|>") : middleTokenIdx]

	// Verify prefix was truncated and keeps the END (high-numbered segments)
	if len(truncatedPrefix) >= len(longPrefix) {
		t.Error("prefix should have been truncated")
	}
	if !strings.HasSuffix(truncatedPrefix, "P04999") {
		t.Errorf("prefix should end with last segment P04999, got suffix %q",
			truncatedPrefix[max(0, len(truncatedPrefix)-6):])
	}
	if strings.HasPrefix(truncatedPrefix, "P00000") {
		t.Error("prefix should NOT start with P00000 (beginning should be trimmed)")
	}

	// Verify suffix was truncated and keeps the BEGINNING (low-numbered segments)
	if len(truncatedSuffix) >= len(longSuffix) {
		t.Error("suffix should have been truncated")
	}
	if !strings.HasPrefix(truncatedSuffix, "S00000") {
		t.Errorf("suffix should start with first segment S00000, got prefix %q",
			truncatedSuffix[:min(6, len(truncatedSuffix))])
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
