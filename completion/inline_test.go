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
	"github.com/kstruzzieri/go-llm/provider"
)

// testFIM returns a valid FIMConfig for tests using Qwen-style tokens.
func testFIM() *provider.FIMConfig {
	return &provider.FIMConfig{
		Prefix:          "<|fim_prefix|>",
		Suffix:          "<|fim_suffix|>",
		Middle:          "<|fim_middle|>",
		StopTokens:      []string{"<|endoftext|>"},
		PrefixBudgetPct: 75,
	}
}

// testProviderConfig returns a standard test ProviderConfig.
func testProviderConfig() ProviderConfig {
	return ProviderConfig{
		FIM:           testFIM(),
		ContextWindow: 2048,
		QualityTier:   provider.TierGood,
	}
}

// mustNewProvider creates a Provider for testing, failing on error.
func mustNewProvider(t *testing.T, client *ollama.Client, model string) *Provider {
	t.Helper()
	p, err := NewProvider(client, model, testProviderConfig())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

func TestNewProviderValidation(t *testing.T) {
	client := ollama.NewClient()

	t.Run("nil FIM config rejected", func(t *testing.T) {
		_, err := NewProvider(client, "test", ProviderConfig{
			ContextWindow: 8192,
			QualityTier:   provider.TierGood,
		})
		if err == nil {
			t.Fatal("expected error for nil FIM config")
		}
	})

	t.Run("invalid FIM config rejected", func(t *testing.T) {
		_, err := NewProvider(client, "test", ProviderConfig{
			FIM:           &provider.FIMConfig{Prefix: "", Suffix: "s", Middle: "m"},
			ContextWindow: 8192,
		})
		if err == nil {
			t.Fatal("expected error for invalid FIM config")
		}
	})

	t.Run("zero context window rejected", func(t *testing.T) {
		_, err := NewProvider(client, "test", ProviderConfig{
			FIM:           &provider.FIMConfig{Prefix: "p", Suffix: "s", Middle: "m"},
			ContextWindow: 0,
		})
		if err == nil {
			t.Fatal("expected error for zero context window")
		}
	})

	t.Run("valid config accepted", func(t *testing.T) {
		p, err := NewProvider(client, "test", ProviderConfig{
			FIM: &provider.FIMConfig{
				Prefix: "<|fim_prefix|>", Suffix: "<|fim_suffix|>", Middle: "<|fim_middle|>",
				StopTokens: []string{"<|endoftext|>"},
			},
			ContextWindow: 32768,
			QualityTier:   provider.TierGood,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p == nil {
			t.Fatal("expected non-nil provider")
		}
	})
}

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
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")

	resp, err := p.Complete(context.Background(), FIMRequest{
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
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true for streaming complete")
		}

		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")

	var tokens []string
	err := p.CompleteStream(context.Background(), FIMRequest{
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
		_, _ = fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")

	callbackErr := fmt.Errorf("stop streaming")
	err := p.CompleteStream(context.Background(), FIMRequest{
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
	t.Run("nil client caught at complete time", func(t *testing.T) {
		p, err := NewProvider(nil, "test-model", testProviderConfig())
		if err != nil {
			t.Fatalf("unexpected construction error: %v", err)
		}
		_, err = p.Complete(context.Background(), FIMRequest{Prefix: "code"})
		if err == nil {
			t.Fatal("expected error for nil client")
		}
		err = p.CompleteStream(context.Background(), FIMRequest{Prefix: "code"}, func(string) error { return nil })
		if err == nil {
			t.Fatal("expected error for nil client in stream")
		}
	})

	t.Run("model required", func(t *testing.T) {
		p, err := NewProvider(ollama.NewClient(), "", testProviderConfig())
		if err != nil {
			t.Fatalf("unexpected construction error: %v", err)
		}
		_, err = p.Complete(context.Background(), FIMRequest{Prefix: "code"})
		if err == nil {
			t.Fatal("expected error for missing model")
		}
	})

	t.Run("empty prefix is valid for FIM", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"response":   "generated",
				"done":       true,
				"eval_count": 1,
			})
		}))
		defer srv.Close()
		c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
		p := mustNewProvider(t, c, "test-model")
		resp, err := p.Complete(context.Background(), FIMRequest{Suffix: "func main() {}"})
		if err != nil {
			t.Fatalf("unexpected error for empty prefix: %v", err)
		}
		if resp.Completion != "generated" {
			t.Errorf("got %q, want %q", resp.Completion, "generated")
		}
	})

	t.Run("stream fn required", func(t *testing.T) {
		p, err := NewProvider(ollama.NewClient(), "test-model", testProviderConfig())
		if err != nil {
			t.Fatalf("unexpected construction error: %v", err)
		}
		err = p.CompleteStream(context.Background(), FIMRequest{Prefix: "code"}, nil)
		if err == nil {
			t.Fatal("expected error for nil callback")
		}
	})
}

func TestCompleteFIMPromptFormat(t *testing.T) {
	var capturedPrompt string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedPrompt = req.Prompt

		resp := ollama.GenerateResponse{
			Model:    "test-model",
			Response: "result",
			Done:     true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")

	_, _ = p.Complete(context.Background(), FIMRequest{
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
		_ = json.NewDecoder(r.Body).Decode(&req)

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
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")
	_, _ = p.Complete(context.Background(), FIMRequest{Prefix: "code"})
}

func TestCompleteContextTruncation(t *testing.T) {
	// Build distinguishable prefix and suffix that far exceed the context budget.
	var prefixBuilder, suffixBuilder strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&prefixBuilder, "P%05d", i)
	}
	for i := 0; i < 1667; i++ {
		fmt.Fprintf(&suffixBuilder, "S%05d", i)
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
	p := mustNewProvider(t, client, "test-model")
	_, err := p.Complete(context.Background(), FIMRequest{
		Prefix: longPrefix,
		Suffix: longSuffix,
	})
	if err != nil {
		t.Fatalf("Complete() error: %v", err)
	}

	// Input exceeds fimCtxCeiling, so effectiveNumCtx returns min(16384, 2048) = 2048.
	// Available = 2048 - 128 - 3 = 1917 tokens * 4 chars = 7668 chars.
	maxExpectedLen := 7668 + len("<|fim_prefix|>") + len("<|fim_suffix|>") + len("<|fim_middle|>")
	if len(capturedPrompt) > maxExpectedLen {
		t.Errorf("prompt length %d exceeds budget %d", len(capturedPrompt), maxExpectedLen)
	}

	suffixTokenIdx := strings.Index(capturedPrompt, "<|fim_suffix|>")
	middleTokenIdx := strings.Index(capturedPrompt, "<|fim_middle|>")
	if suffixTokenIdx == -1 || middleTokenIdx == -1 {
		t.Fatal("prompt missing FIM tokens")
	}
	truncatedPrefix := capturedPrompt[len("<|fim_prefix|>"):suffixTokenIdx]
	truncatedSuffix := capturedPrompt[suffixTokenIdx+len("<|fim_suffix|>") : middleTokenIdx]

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
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Options.NumPredict != 256 {
			t.Errorf("NumPredict = %d, want 256", req.Options.NumPredict)
		}

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")
	_, _ = p.Complete(context.Background(), FIMRequest{
		Prefix:    "code",
		MaxTokens: 256,
	})
}

func TestCompleteMaxTokensClamped(t *testing.T) {
	// MaxTokens exceeding (effectiveNumCtx - overhead - 1) must be clamped
	// so that num_predict never exceeds num_ctx. testProviderConfig uses
	// ContextWindow=2048, so effectiveNumCtx=2048 and maxAllowed=2044.
	overhead := testFIM().TokenOverhead()
	maxAllowed := 2048 - overhead - 1 // 2044

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.Options.NumPredict != maxAllowed {
			t.Errorf("NumPredict = %d, want %d (clamped)", req.Options.NumPredict, maxAllowed)
		}
		if req.Options.NumPredict > req.Options.NumCtx {
			t.Errorf("NumPredict (%d) > NumCtx (%d)", req.Options.NumPredict, req.Options.NumCtx)
		}

		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")
	resp, err := p.Complete(context.Background(), FIMRequest{
		Prefix:    "code",
		MaxTokens: 99999,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}
