package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// ---------------------------------------------------------------------------
// Name and Capabilities
// ---------------------------------------------------------------------------

func TestOllamaProvider_Name(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	if got := p.Name(); got != "ollama" {
		t.Errorf("Name() = %q, want %q", got, "ollama")
	}
}

func TestOllamaProvider_Capabilities(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	caps := p.Capabilities()

	tests := []struct {
		name string
		cap  Capability
	}{
		{"CapChat", CapChat},
		{"CapGenerate", CapGenerate},
		{"CapEmbed", CapEmbed},
		{"CapStream", CapStream},
		{"CapToolCall", CapToolCall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !caps.Has(tt.cap) {
				t.Errorf("Capabilities() missing %s", tt.name)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Health
// ---------------------------------------------------------------------------

func TestOllamaProvider_Health(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{"healthy", http.StatusOK, false},
		{"unhealthy", http.StatusInternalServerError, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
			p := NewOllamaProvider(c)
			err := p.Health(context.Background())
			if (err != nil) != tt.wantErr {
				t.Errorf("Health() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), "provider: ollama:") {
				t.Errorf("error should contain 'provider: ollama:' prefix, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Chat (non-streaming)
// ---------------------------------------------------------------------------

func TestOllamaProvider_Chat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream {
			t.Error("expected stream=false for non-streaming chat")
		}
		if req.Model != "qwen3:8b" {
			t.Errorf("model = %q, want %q", req.Model, "qwen3:8b")
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "Hello" {
			t.Errorf("unexpected messages: %+v", req.Messages)
		}

		resp := ollama.ChatResponse{
			Model:              "qwen3:8b",
			Message:            ollama.ChatMessage{Role: "assistant", Content: "Hi there!"},
			Done:               true,
			PromptEvalCount:    5,
			EvalCount:          3,
			LoadDuration:       1000000,
			PromptEvalDuration: 2000000,
			EvalDuration:       3000000,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Content != "Hi there!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hi there!")
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q, want %q", resp.Provider, "ollama")
	}
	if resp.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", resp.Model, "qwen3:8b")
	}
	if !resp.Done {
		t.Error("expected Done=true")
	}
	if resp.Usage.PromptTokens != 5 {
		t.Errorf("Usage.PromptTokens = %d, want 5", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 3 {
		t.Errorf("Usage.CompletionTokens = %d, want 3", resp.Usage.CompletionTokens)
	}
	if resp.Usage.TotalTokens != 8 {
		t.Errorf("Usage.TotalTokens = %d, want 8", resp.Usage.TotalTokens)
	}
	if resp.Latency.LoadDuration != time.Duration(1000000) {
		t.Errorf("Latency.LoadDuration = %v, want 1ms", resp.Latency.LoadDuration)
	}
}

func TestOllamaProvider_Chat_ThinkExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me reason about this.</think>The answer is 42."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "What is the meaning of life?"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "Let me reason about this." {
		t.Errorf("Thinking = %q, want %q", resp.Thinking, "Let me reason about this.")
	}
	if resp.Content != "The answer is 42." {
		t.Errorf("Content = %q, want %q", resp.Content, "The answer is 42.")
	}
}

func TestOllamaProvider_Chat_NoThinkTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "Just a plain answer."},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	resp, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if resp.Thinking != "" {
		t.Errorf("Thinking = %q, want empty", resp.Thinking)
	}
	if resp.Content != "Just a plain answer." {
		t.Errorf("Content = %q, want %q", resp.Content, "Just a plain answer.")
	}
}

func TestOllamaProvider_Chat_WithOptions(t *testing.T) {
	var receivedOpts ollama.ModelOptions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollama.ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Options != nil {
			receivedOpts = *req.Options
		}
		resp := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "ok"},
			Done:    true,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c)

	_, err := p.Chat(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
		Options: ModelOptions{
			Temperature: Ptr(0.7),
			NumPredict:  100,
			NumCtx:      4096,
		},
	})
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if receivedOpts.Temperature != 0.7 {
		t.Errorf("Temperature = %f, want 0.7", receivedOpts.Temperature)
	}
	if receivedOpts.NumPredict != 100 {
		t.Errorf("NumPredict = %d, want 100", receivedOpts.NumPredict)
	}
	if receivedOpts.NumCtx != 4096 {
		t.Errorf("NumCtx = %d, want 4096", receivedOpts.NumCtx)
	}
}

// ---------------------------------------------------------------------------
// ChatStream
// ---------------------------------------------------------------------------

func TestOllamaProvider_ChatStream(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "Hello "}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "world!"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true, EvalCount: 4, PromptEvalCount: 2},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	// Use ThinkNone to get passthrough behavior for plain text.
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	var received []ChatResponse
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(resp ChatResponse) error {
		received = append(received, resp)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	// With ThinkNone passthrough, we expect: content("Hello "), content("world!"), done.
	if len(received) < 3 {
		t.Fatalf("expected at least 3 chunks, got %d", len(received))
	}

	// All should have provider set.
	for i, r := range received {
		if r.Provider != "ollama" {
			t.Errorf("chunk[%d].Provider = %q, want %q", i, r.Provider, "ollama")
		}
	}

	// Last chunk should be Done with usage.
	last := received[len(received)-1]
	if !last.Done {
		t.Error("last chunk should have Done=true")
	}
	if last.Usage.CompletionTokens != 4 {
		t.Errorf("last chunk Usage.CompletionTokens = %d, want 4", last.Usage.CompletionTokens)
	}
	if last.Usage.PromptTokens != 2 {
		t.Errorf("last chunk Usage.PromptTokens = %d, want 2", last.Usage.PromptTokens)
	}
}

func TestOllamaProvider_ChatStream_ThinkExtraction(t *testing.T) {
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "<think>Let me"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " think.</think>The answer"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: " is 42."}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))

	var thinkingChunks []string
	var contentChunks []string
	var gotDone bool

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Think about this"}},
	}, func(resp ChatResponse) error {
		if resp.Thinking != "" {
			thinkingChunks = append(thinkingChunks, resp.Thinking)
		}
		if resp.Content != "" {
			contentChunks = append(contentChunks, resp.Content)
		}
		if resp.Done {
			gotDone = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	if !gotDone {
		t.Error("expected final chunk with Done=true")
	}

	thinking := strings.Join(thinkingChunks, "")
	content := strings.Join(contentChunks, "")

	if thinking != "Let me think." {
		t.Errorf("accumulated thinking = %q, want %q", thinking, "Let me think.")
	}
	if content != "The answer is 42." {
		t.Errorf("accumulated content = %q, want %q", content, "The answer is 42.")
	}
}

func TestOllamaProvider_ChatStream_FragmentedTags(t *testing.T) {
	// The <think> tag is split across two chunks: "<thi" then "nk>..."
	chunks := []ollama.ChatResponse{
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "<thi"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: "nk>reasoning</think>content"}, Done: false},
		{Model: "qwen3:8b", Message: ollama.ChatMessage{Role: "assistant", Content: ""}, Done: true},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = fmt.Fprintf(w, "%s\n", data)
		}
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))

	var thinkingChunks []string
	var contentChunks []string

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "test"}},
	}, func(resp ChatResponse) error {
		if resp.Thinking != "" {
			thinkingChunks = append(thinkingChunks, resp.Thinking)
		}
		if resp.Content != "" {
			contentChunks = append(contentChunks, resp.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() error: %v", err)
	}

	thinking := strings.Join(thinkingChunks, "")
	content := strings.Join(contentChunks, "")

	if thinking != "reasoning" {
		t.Errorf("thinking = %q, want %q", thinking, "reasoning")
	}
	if content != "content" {
		t.Errorf("content = %q, want %q", content, "content")
	}
}

func TestOllamaProvider_ChatStream_GracefulCancellation(t *testing.T) {
	// Server sends one chunk then blocks until the serverDone channel is closed,
	// simulating a long-running stream that gets cancelled by the client.
	ready := make(chan struct{})
	serverDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "partial content"},
			Done:    false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
		// Flush to ensure the client sees the chunk.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Signal that the chunk was sent.
		close(ready)
		// Block until test cleanup signals us to exit.
		<-serverDone
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	ctx, cancel := context.WithCancel(context.Background())

	var received []ChatResponse
	done := make(chan error, 1)
	go func() {
		done <- p.ChatStream(ctx, ChatRequest{
			Model:    "qwen3:8b",
			Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
		}, func(resp ChatResponse) error {
			received = append(received, resp)
			return nil
		})
	}()

	// Wait for the first chunk to be processed, then cancel.
	<-ready
	// Small delay to ensure the chunk is processed by the stream handler.
	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	// Unblock the server handler so httptest.Server.Close() returns promptly.
	close(serverDone)

	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	if !strings.Contains(err.Error(), "provider: ollama:") {
		t.Errorf("error should contain 'provider: ollama:' prefix, got: %v", err)
	}

	// Should have at least the content chunk and the partial result.
	gotPartial := false
	for _, r := range received {
		if r.Partial && r.Done {
			gotPartial = true
			if r.Content != "partial content" {
				t.Errorf("partial Content = %q, want %q", r.Content, "partial content")
			}
		}
	}
	if !gotPartial {
		t.Error("expected a partial result chunk after cancellation")
	}
}

func TestOllamaProvider_ChatStream_CallbackError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := ollama.ChatResponse{
			Model:   "qwen3:8b",
			Message: ollama.ChatMessage{Role: "assistant", Content: "hello"},
			Done:    false,
		}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "%s\n", data)
	}))
	defer srv.Close()

	c := ollama.NewClient(ollama.WithBaseURL(srv.URL))
	p := NewOllamaProvider(c, WithThinkMode(ThinkNone))

	callbackErr := fmt.Errorf("consumer stopped")
	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, func(_ ChatResponse) error {
		return callbackErr
	})
	if err == nil {
		t.Fatal("expected error from callback")
	}
	if !strings.Contains(err.Error(), "consumer stopped") {
		t.Errorf("expected callback error in chain, got: %v", err)
	}
}

func TestOllamaProvider_ChatStream_NilCallback(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)

	err := p.ChatStream(context.Background(), ChatRequest{
		Model:    "qwen3:8b",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil callback")
	}
	if !strings.Contains(err.Error(), "callback function is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Functional options
// ---------------------------------------------------------------------------

func TestOllamaProvider_WithThinkMode(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c, WithThinkMode(ThinkAlways))
	if p.thinkMode != ThinkAlways {
		t.Errorf("thinkMode = %v, want ThinkAlways", p.thinkMode)
	}
}

func TestOllamaProvider_WithThinkTags(t *testing.T) {
	customTags := ThinkTags{Open: "<reasoning>", Close: "</reasoning>"}
	c := ollama.NewClient()
	p := NewOllamaProvider(c, WithThinkTags(customTags))
	if p.thinkTags != customTags {
		t.Errorf("thinkTags = %+v, want %+v", p.thinkTags, customTags)
	}
}

func TestOllamaProvider_DefaultThinkMode(t *testing.T) {
	c := ollama.NewClient()
	p := NewOllamaProvider(c)
	if p.thinkMode != ThinkAuto {
		t.Errorf("default thinkMode = %v, want ThinkAuto", p.thinkMode)
	}
	if p.thinkTags != DefaultThinkTags() {
		t.Errorf("default thinkTags = %+v, want %+v", p.thinkTags, DefaultThinkTags())
	}
}

// ---------------------------------------------------------------------------
// Type mapping helpers
// ---------------------------------------------------------------------------

func TestToOllamaChatRequest(t *testing.T) {
	req := ChatRequest{
		Model: "qwen3:8b",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
		},
		Options: ModelOptions{
			Temperature: Ptr(0.5),
			NumCtx:      8192,
		},
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "search",
					Description: "Search the web",
					Parameters:  json.RawMessage(`{"type":"object"}`),
				},
			},
		},
	}

	oReq := toOllamaChatRequest(req)

	if oReq.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", oReq.Model, "qwen3:8b")
	}
	if len(oReq.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(oReq.Messages))
	}
	if oReq.Messages[0].Role != "system" {
		t.Errorf("Messages[0].Role = %q, want %q", oReq.Messages[0].Role, "system")
	}
	if oReq.Options == nil {
		t.Fatal("Options should not be nil")
	}
	if oReq.Options.Temperature != 0.5 {
		t.Errorf("Options.Temperature = %f, want 0.5", oReq.Options.Temperature)
	}
	if oReq.Options.NumCtx != 8192 {
		t.Errorf("Options.NumCtx = %d, want 8192", oReq.Options.NumCtx)
	}
	if len(oReq.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1", len(oReq.Tools))
	}
	if oReq.Tools[0].Function.Name != "search" {
		t.Errorf("Tools[0].Function.Name = %q, want %q", oReq.Tools[0].Function.Name, "search")
	}
}

func TestToOllamaOptions_NilOnZero(t *testing.T) {
	opts := toOllamaOptions(ModelOptions{})
	if opts != nil {
		t.Errorf("expected nil for zero-value options, got %+v", opts)
	}
}

func TestToProviderToolCalls(t *testing.T) {
	calls := []ollama.ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: ollama.ToolCallFunction{
				Index:     0,
				Name:      "get_weather",
				Arguments: map[string]any{"city": "Tokyo"},
			},
		},
	}

	result := toProviderToolCalls(calls)
	if len(result) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result))
	}
	if result[0].ID != "call_1" {
		t.Errorf("ID = %q, want %q", result[0].ID, "call_1")
	}
	if result[0].Function.Name != "get_weather" {
		t.Errorf("Name = %q, want %q", result[0].Function.Name, "get_weather")
	}
	// Arguments should be valid JSON.
	var args map[string]any
	if err := json.Unmarshal(result[0].Function.Arguments, &args); err != nil {
		t.Fatalf("failed to unmarshal arguments: %v", err)
	}
	if args["city"] != "Tokyo" {
		t.Errorf("args[city] = %v, want %q", args["city"], "Tokyo")
	}
}

func TestToProviderToolCalls_Empty(t *testing.T) {
	result := toProviderToolCalls(nil)
	if result != nil {
		t.Errorf("expected nil for empty input, got %v", result)
	}
}

func TestToOllamaToolCalls_RoundTrip(t *testing.T) {
	providerCalls := []ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: ToolCallFunction{
				Index:     0,
				Name:      "search",
				Arguments: json.RawMessage(`{"query":"test"}`),
			},
		},
	}

	ollamaCalls := toOllamaToolCalls(providerCalls)
	if len(ollamaCalls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(ollamaCalls))
	}
	if ollamaCalls[0].Function.Name != "search" {
		t.Errorf("Name = %q, want %q", ollamaCalls[0].Function.Name, "search")
	}
	if ollamaCalls[0].Function.Arguments["query"] != "test" {
		t.Errorf("Arguments[query] = %v, want %q", ollamaCalls[0].Function.Arguments["query"], "test")
	}

	// Round-trip back to provider.
	back := toProviderToolCalls(ollamaCalls)
	if len(back) != 1 {
		t.Fatalf("round-trip: expected 1 call, got %d", len(back))
	}
	if back[0].Function.Name != "search" {
		t.Errorf("round-trip: Name = %q, want %q", back[0].Function.Name, "search")
	}
}
