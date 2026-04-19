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
			FIM:           &provider.FIMConfig{StopTokens: []string{""}},
			ContextWindow: 8192,
		})
		if err == nil {
			t.Fatal("expected error for invalid FIM config")
		}
	})

	t.Run("zero context window rejected", func(t *testing.T) {
		_, err := NewProvider(client, "test", ProviderConfig{
			FIM:           &provider.FIMConfig{},
			ContextWindow: 0,
		})
		if err == nil {
			t.Fatal("expected error for zero context window")
		}
	})

	t.Run("valid config accepted", func(t *testing.T) {
		p, err := NewProvider(client, "test", ProviderConfig{
			FIM: &provider.FIMConfig{
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
		if req.Prompt != "func main() {\n\t" {
			t.Errorf("Prompt = %q, want prefix payload", req.Prompt)
		}
		if req.Suffix != "\n}" {
			t.Errorf("Suffix = %q, want suffix payload", req.Suffix)
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
	if len(tokens) == 0 {
		t.Fatal("expected at least one token callback")
	}
	joined := strings.Join(tokens, "")
	if joined != "fmt.Println()" {
		t.Errorf("unexpected combined tokens: %q", joined)
	}
}

func TestCompleteStreamCallbackError(t *testing.T) {
	// Send a chunk larger than the longest stop token so that the buffered
	// CompleteStream flushes at least once; otherwise the trailing buffer
	// would swallow the whole chunk until the done marker.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := ollama.GenerateResponse{
			Model:    "test-model",
			Response: strings.Repeat("token", 10),
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
	var capturedSuffix string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedPrompt = req.Prompt
		capturedSuffix = req.Suffix

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

	if capturedPrompt != "BEFORE" {
		t.Errorf("Prompt = %q, want %q", capturedPrompt, "BEFORE")
	}
	if capturedSuffix != "AFTER" {
		t.Errorf("Suffix = %q, want %q", capturedSuffix, "AFTER")
	}
}

func TestCompleteModelOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		if req.Options == nil {
			t.Fatal("expected model options")
		}
		// Values are now adaptive: NumCtx is pinned by ContextWindow=2048
		// in testProviderConfig, but NumPredict and Temperature come from
		// ComputeBudget and depend on the detected cursor context/shape.
		if req.Options.NumCtx != 2048 {
			t.Errorf("NumCtx = %d, want 2048 (bounded by ContextWindow)", req.Options.NumCtx)
		}
		if req.Options.NumPredict < 16 || req.Options.NumPredict > 512 {
			t.Errorf("NumPredict = %d, want within adaptive range [16, 512]", req.Options.NumPredict)
		}
		if req.Options.Temperature < 0.0 || req.Options.Temperature > 0.5 {
			t.Errorf("Temperature = %f, want within adaptive range [0.0, 0.5]", req.Options.Temperature)
		}
		if req.Options.NumPredict > req.Options.NumCtx {
			t.Errorf("NumPredict (%d) must not exceed NumCtx (%d)",
				req.Options.NumPredict, req.Options.NumCtx)
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
	var capturedSuffix string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode generate request: %v", err)
		}
		capturedPrompt = req.Prompt
		capturedSuffix = req.Suffix

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
	// Available = 2048 - 128 - 0 = 1920 tokens, split 75/25 into prefix/suffix.
	if len(capturedPrompt) > 5760 {
		t.Errorf("prompt length %d exceeds prefix budget %d", len(capturedPrompt), 5760)
	}
	if len(capturedSuffix) > 1920 {
		t.Errorf("suffix length %d exceeds suffix budget %d", len(capturedSuffix), 1920)
	}

	truncatedPrefix := capturedPrompt
	truncatedSuffix := capturedSuffix

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
	// MaxTokens exceeding (effectiveNumCtx - 1) must be clamped so that
	// num_predict never exceeds num_ctx. testProviderConfig uses
	// ContextWindow=2048, so effectiveNumCtx=2048 and maxAllowed=2047.
	maxAllowed := 2048 - 1

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

func TestCompleteAdaptive(t *testing.T) {
	var capturedPrompt string
	var capturedSuffix string
	var capturedOpts *ollama.ModelOptions

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedPrompt = req.Prompt
		capturedSuffix = req.Suffix
		capturedOpts = req.Options

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
	cfg := ProviderConfig{
		FIM: &provider.FIMConfig{
			StopTokens:      []string{"<|endoftext|>"},
			PrefixBudgetPct: 75,
		},
		ContextWindow: 32768,
		QualityTier:   provider.TierGood,
	}
	p, err := NewProvider(client, "test-model", cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	resp, err := p.Complete(context.Background(), FIMRequest{
		Prefix:   "func main() {\n\t",
		Suffix:   "\n}",
		FilePath: "main.go",
		Trace:    true,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if capturedPrompt != "func main() {\n\t" {
		t.Errorf("Prompt = %q, want prefix payload", capturedPrompt)
	}
	if capturedSuffix != "\n}" {
		t.Errorf("Suffix = %q, want suffix payload", capturedSuffix)
	}

	if resp.CursorContext == ContextUnknown && resp.CompletionShape == ShapeUnknown {
		t.Error("expected cursor analysis to populate context and shape")
	}

	if resp.BudgetTrace == nil {
		t.Fatal("expected BudgetTrace when Trace=true")
	}
	if resp.BudgetTrace.Language != "go" {
		t.Errorf("BudgetTrace.Language = %q, want %q", resp.BudgetTrace.Language, "go")
	}

	if capturedOpts == nil {
		t.Fatal("expected model options")
	}
	if len(capturedOpts.Stop) == 0 {
		t.Error("expected stop tokens in model options")
	}

	if resp.Completion != "fmt.Println(\"hello\")" {
		t.Errorf("Completion = %q", resp.Completion)
	}
}

func TestCompleteStopTokenStripping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ollama.GenerateResponse{
			Model:     "test-model",
			Response:  "result<|endoftext|>",
			Done:      true,
			EvalCount: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	cfg := ProviderConfig{
		FIM: &provider.FIMConfig{
			StopTokens: []string{"<|endoftext|>"},
		},
		ContextWindow: 8192,
		QualityTier:   provider.TierGood,
	}
	p, err := NewProvider(client, "test-model", cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	resp, err := p.Complete(context.Background(), FIMRequest{
		Prefix: "x = ", Suffix: "\n", FilePath: "main.go",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Completion != "result" {
		t.Errorf("expected stop token stripped, got %q", resp.Completion)
	}
}

func TestCompleteMaxTokensOverride(t *testing.T) {
	var capturedOpts *ollama.ModelOptions

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.GenerateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		capturedOpts = req.Options
		resp := ollama.GenerateResponse{Model: "test-model", Response: "x", Done: true}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := mustNewProvider(t, client, "test-model")

	_, _ = p.Complete(context.Background(), FIMRequest{
		Prefix:    "code",
		MaxTokens: 256,
		FilePath:  "main.go",
	})

	if capturedOpts.NumPredict != 256 {
		t.Errorf("NumPredict = %d, want 256 (manual override)", capturedOpts.NumPredict)
	}
}

func TestCompleteStreamStopSuppression(t *testing.T) {
	chunks := []ollama.GenerateResponse{
		{Model: "test-model", Response: "fmt.", Done: false},
		{Model: "test-model", Response: "Println()", Done: false},
		{Model: "test-model", Response: "<|endo", Done: false},
		{Model: "test-model", Response: "ftext|>", Done: false},
		{Model: "test-model", Response: "", Done: true, EvalCount: 4},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Prefix:   "func main() {\n\t",
		Suffix:   "\n}",
		FilePath: "main.go",
	}, func(token string) error {
		tokens = append(tokens, token)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	joined := strings.Join(tokens, "")
	if strings.Contains(joined, "<|endoftext|>") {
		t.Errorf("stop token leaked to callback: %q", joined)
	}
	if !strings.Contains(joined, "fmt.Println()") {
		t.Errorf("expected code content, got %q", joined)
	}
}

func TestCompleteStreamNoStopToken(t *testing.T) {
	chunks := []ollama.GenerateResponse{
		{Model: "test-model", Response: "hello", Done: false},
		{Model: "test-model", Response: " world", Done: false},
		{Model: "test-model", Response: "", Done: true, EvalCount: 2},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Prefix:   "x = ",
		Suffix:   "\n",
		FilePath: "main.go",
	}, func(token string) error {
		tokens = append(tokens, token)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}

	joined := strings.Join(tokens, "")
	if joined != "hello world" {
		t.Errorf("unexpected output: %q", joined)
	}
}
